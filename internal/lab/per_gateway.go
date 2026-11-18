package lab

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

// gatewayProbeGroups resolves probes before any cluster is created. A request
// that could go through multiple independent proxies has no single candidate
// address: guessing would measure a different configuration.
func gatewayProbeGroups(c *Candidate, probes []model.Probe) ([][]model.Probe, error) {
	if len(c.Gateways) == 0 {
		return nil, fmt.Errorf("this implementation needs a Gateway in the candidate configuration")
	}
	groups := make([][]model.Probe, len(c.Gateways))
	for _, gw := range c.Gateways {
		if _, err := gatewaySentinelListener(gw, "readiness.routeparity.invalid"); err != nil {
			return nil, err
		}
	}
	for _, p := range probes {
		if p.Origin.Source == model.OriginControl {
			groups[0] = append(groups[0], p)
			continue
		}
		var routed, listening []int
		for i, gw := range c.Gateways {
			for _, l := range gw.Spec.Listeners {
				if !listenerMatches(l, p) {
					continue
				}
				listening = appendUnique(listening, i)
				for _, r := range c.HTTPRoutes {
					if !routeHostMatches(r.Spec.Hostnames, p.Host) {
						continue
					}
					for _, parent := range r.Spec.ParentRefs {
						ns := r.Namespace
						if parent.Namespace != nil {
							ns = string(*parent.Namespace)
						}
						if (parent.Kind != nil && *parent.Kind != "Gateway") || (parent.Group != nil && *parent.Group != gatewayv1.GroupName) {
							continue
						}
						if ns == gw.Namespace && string(parent.Name) == gw.Name &&
							(parent.SectionName == nil || *parent.SectionName == l.Name) && (parent.Port == nil || *parent.Port == l.Port) {
							routed = appendUnique(routed, i)
						}
					}
				}
			}
		}
		choices := routed
		if len(choices) == 0 {
			choices = listening
		}
		// A missing listener is still worth measuring: send to the sole Gateway
		// and let the ordinary missing-address/unrouted guards describe the result.
		if len(choices) == 0 && len(c.Gateways) == 1 {
			choices = []int{0}
		}
		if len(choices) != 1 {
			return nil, fmt.Errorf("cannot select a unique Gateway for %s://%s%s (%d candidates); run each independent Gateway configuration separately", p.Scheme, p.Host, p.RawTarget, len(choices))
		}
		groups[choices[0]] = append(groups[choices[0]], p)
	}
	return groups, nil
}

func appendUnique(values []int, value int) []int {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

func hostMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if strings.HasPrefix(pattern, "*.") {
		return len(host) > len(pattern)-1 && strings.HasSuffix(host, pattern[1:])
	}
	return pattern == host
}

func routeHostMatches(hostnames []gatewayv1.Hostname, host string) bool {
	if len(hostnames) == 0 {
		return true
	}
	for _, h := range hostnames {
		if hostMatches(string(h), host) {
			return true
		}
	}
	return false
}

func listenerMatches(l gatewayv1.Listener, p model.Probe) bool {
	protocol := (p.Scheme == "http" && l.Protocol == gatewayv1.HTTPProtocolType && l.Port == 80) ||
		(p.Scheme == "https" && l.Protocol == gatewayv1.HTTPSProtocolType && l.Port == 443)
	return protocol && (l.Hostname == nil || hostMatches(string(*l.Hostname), p.Host))
}

// gatewaySentinelListener uses a transport the Gateway already exposes. In
// particular, an HTTPS-only Gateway must not acquire a plain HTTP data port
// merely to let the lab test readiness.
func gatewaySentinelListener(gw gatewayv1.Gateway, host string) (gatewayv1.Listener, error) {
	var transport *gatewayv1.Listener
	for i := range gw.Spec.Listeners {
		l := &gw.Spec.Listeners[i]
		if l.Name == "routeparity-sentinel" {
			return gatewayv1.Listener{}, fmt.Errorf("Gateway %s/%s uses the reserved listener name routeparity-sentinel", gw.Namespace, gw.Name)
		}
		if l.Protocol == gatewayv1.HTTPProtocolType && l.Port == 80 {
			transport = l
		} else if transport == nil && l.Protocol == gatewayv1.HTTPSProtocolType && l.Port == 443 && l.TLS != nil {
			transport = l
		}
	}
	if transport == nil {
		return gatewayv1.Listener{}, fmt.Errorf("Gateway %s/%s needs an HTTP listener on 80 or a terminating HTTPS listener on 443 for the lab readiness check", gw.Namespace, gw.Name)
	}
	all := gatewayv1.NamespacesFromAll
	hostname := gatewayv1.Hostname(host)
	return gatewayv1.Listener{
		Name: "routeparity-sentinel", Port: transport.Port, Protocol: transport.Protocol,
		Hostname: &hostname, TLS: transport.TLS.DeepCopy(),
		AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &all}},
	}, nil
}

// candidatePerGateway records one independent proxy at a time using the same
// host NodePorts. A sentinel listener is added to each actual Gateway under a
// unique hostname. Serving a separate sentinel Gateway would prove nothing
// about these proxies' readiness.
func (s *session) candidatePerGateway(ctx context.Context, impl Implementation, c *Candidate) (*model.Snapshot, error) {
	groups, err := gatewayProbeGroups(c, s.probes.Probes)
	if err != nil {
		return nil, err
	}
	class, err := decodeObjects([]byte(impl.class))
	if err != nil {
		return nil, err
	}
	if err := s.k.apply(ctx, label(class)); err != nil {
		return nil, err
	}
	patched := *c
	patched.Gateways = make([]gatewayv1.Gateway, len(c.Gateways))
	var sentinels []*gatewayv1.HTTPRoute
	var hosts []string
	for i := range c.Gateways {
		gw := c.Gateways[i].DeepCopy()
		nonce, err := newNonce()
		if err != nil {
			return nil, err
		}
		_, sr, host := s.plan.GatewaySentinel(nonce, impl.ClassName)
		listener, err := gatewaySentinelListener(*gw, host)
		if err != nil {
			return nil, err
		}
		gw.Spec.Listeners = append(gw.Spec.Listeners, listener)
		ns, section := gatewayv1.Namespace(gw.Namespace), listener.Name
		sr.Name = fmt.Sprintf("%s-%d", sentinelName, i)
		sr.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(gw.Name), Namespace: &ns, SectionName: &section}}
		patched.Gateways[i] = *gw
		sentinels = append(sentinels, sr)
		hosts = append(hosts, host)
	}
	objs, gateways, routes, err := patched.labObjects(impl.ClassName)
	if err != nil {
		return nil, err
	}
	s.opts.progress("applying %d Gateway(s) and %d HTTPRoute(s) from %s", len(gateways), len(routes), c.Source)
	if err := s.k.apply(ctx, label(objs)); err != nil {
		return nil, err
	}
	for _, sr := range sentinels {
		objs, err := labelled(sr)
		if err != nil {
			return nil, err
		}
		if err := s.k.apply(ctx, objs); err != nil {
			return nil, err
		}
	}
	meta := model.TargetMeta{
		Name: impl.Name, Kind: model.TargetGatewayAPI, Mode: model.ModeLab,
		Address: "kind cluster " + s.cluster.Name, Implementation: impl.Name, ToolVersion: s.opts.ToolVersion,
		Versions: map[string]string{impl.Name: impl.Version, "gateway-api": impl.GatewayAPIVersion, "kubernetes": KubernetesVersion, "echo": s.opts.EchoImage, "routes": c.Source},
	}
	result := &model.Snapshot{SchemaVersion: model.SnapshotSchemaVersion, Meta: meta, Probes: s.probes.Probes}
	result.Unverified = append(result.Unverified, s.plan.Unverified...)
	result.Unverified = append(result.Unverified, s.probes.Unverified...)
	result.Unverified = append(result.Unverified, c.Unverified...)
	for i, gw := range gateways {
		selected := impl
		selected.proxyNamespace = gw.Namespace
		selected.proxySelector = "gateway.networking.k8s.io/gateway-name=" + gw.Name
		s.opts.progress("waiting for the %s proxy for Gateway %s", impl.Name, gw)
		target, err := s.exposeProxy(ctx, selected)
		if err != nil {
			return nil, err
		}
		if err := waitForSentinel(ctx, impl.Name+" "+gw.String(), target, hosts[i], s.opts.Send); err != nil {
			return nil, err
		}
		// Keep the unknown-host control in the snapshot only once. All remaining
		// groups are disjoint and retain their original request IDs.
		s.opts.progress("sending %d probe(s) to Gateway %s", len(groups[i]), gw)
		snap, settled, err := recordSettled(ctx, target, meta, groups[i], nil, s.opts.Send)
		if err != nil {
			return nil, err
		}
		if !settled {
			result.Unverified = append(result.Unverified, model.Unverified{Reason: ReasonNotLoaded, Feature: gw.String(), Detail: "proxy did not settle between consecutive recordings"})
		}
		result.Meta.RecordedAt = snap.Meta.RecordedAt
		result.Observations = append(result.Observations, snap.Observations...)
		result.Unverified = append(result.Unverified, snap.Unverified...)
	}
	result.Unverified = append(result.Unverified, s.gatewayStatus(ctx, gateways, routes)...)
	return result, nil
}

// clearGatewayEntry releases the fixed candidate NodePorts before selecting
// another Gateway or implementation. Baseline ingress-nginx is untouched.
func (s *session) clearGatewayEntry(ctx context.Context) error {
	services, err := s.k.client.CoreV1().Services("").List(ctx, metav1.ListOptions{LabelSelector: managedByLabel + "=" + managedByValue})
	if err != nil {
		return err
	}
	for _, svc := range services.Items {
		if svc.Name != "routeparity-entry" || svc.Namespace == ingressNGINXNamespace {
			continue
		}
		entry := false
		for _, port := range svc.Spec.Ports {
			entry = entry || port.NodePort == nodePortGatewayHTTP || port.NodePort == nodePortGatewayHTTPS
		}
		if !entry {
			continue // an echo backend can legitimately have this Service name
		}
		if err := s.k.client.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("release Gateway entry Service %s: %w", types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, err)
		}
	}
	return nil
}

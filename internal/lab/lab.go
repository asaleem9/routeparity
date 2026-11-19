package lab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/probe"
	"github.com/asaleem9/routeparity/internal/prober"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "routeparity"

	ingressNGINXNamespace  = "ingress-nginx"
	ingressNGINXDeployment = "ingress-nginx-controller"

	controllerTimeout = 5 * time.Minute // includes pulling the controller image
	echoTimeout       = 2 * time.Minute
	proxyTimeout      = 5 * time.Minute // includes pulling the proxy image
	routesTimeout     = 90 * time.Second
	hostCheckTimeout  = 5 * time.Second
	statusTimeout     = 20 * time.Second
	// settleRounds bounds how often a target is re-recorded while waiting
	// for two consecutive recordings to agree.
	settleRounds = 8
)

// ingressNGINXSelector matches the controller pods of the vendored manifest.
var ingressNGINXSelector = map[string]string{
	"app.kubernetes.io/name":      "ingress-nginx",
	"app.kubernetes.io/instance":  "ingress-nginx",
	"app.kubernetes.io/component": "controller",
}

// ReasonNotLoaded marks configuration that a controller never put into effect.
const ReasonNotLoaded model.UnverifiedReason = "not-loaded"

// Options controls a lab run.
type Options struct {
	// ClusterName is the kind cluster to create or reuse.
	ClusterName string
	// EchoImage is the echo backend image. A local Docker image of that name
	// is copied into the cluster; otherwise the cluster pulls it.
	EchoImage string
	// Keep leaves the cluster running when the run ends.
	Keep bool
	// Probes selects which probes are sent.
	Probes probe.Options
	// Send controls how they are sent.
	Send prober.Options
	// ToolVersion is recorded in the snapshots.
	ToolVersion string
	// Progress receives one line per step. It may be nil.
	Progress func(format string, args ...any)
}

func (o Options) progress(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(format, args...)
	}
}

// session is one lab run: a cluster, what to put in it, and what to ask it.
type session struct {
	opts    Options
	cluster *Cluster
	k       *kube
	plan    *Plan
	probes  probe.Set
}

// RecordBaseline reproduces a World on ingress-nginx in a local cluster, with
// echo backends, and records how it behaves.
func RecordBaseline(ctx context.Context, w *model.World, opts Options) (snap *model.Snapshot, err error) {
	s, done, err := open(ctx, w, nil, opts)
	if err != nil {
		return nil, err
	}
	defer done(&err)
	if err := s.install(ctx, nil); err != nil {
		return nil, err
	}
	return s.baseline(ctx)
}

// Run reproduces a World on ingress-nginx and a Gateway API configuration on
// an implementation, side by side in one local cluster with the same echo
// backends behind both, and records how each behaves.
func Run(ctx context.Context, w *model.World, candidate *Candidate, impl Implementation, opts Options) (base, cand *model.Snapshot, err error) {
	if err := impl.validate(); err != nil {
		return nil, nil, err
	}
	if candidate.Empty() {
		return nil, nil, errors.New("the Gateway configuration has no Gateway and no HTTPRoute, so there is nothing to compare ingress-nginx with")
	}
	if impl.perGateway {
		if _, err := gatewayProbeGroups(candidate, probe.Generate(w, opts.Probes).Probes); err != nil {
			return nil, nil, err
		}
	}
	s, done, err := open(ctx, w, candidate, opts)
	if err != nil {
		return nil, nil, err
	}
	defer done(&err)
	if err := s.install(ctx, &impl); err != nil {
		return nil, nil, err
	}
	if base, err = s.baseline(ctx); err != nil {
		return nil, nil, err
	}
	if cand, err = s.candidate(ctx, impl, candidate); err != nil {
		return nil, nil, err
	}
	return base, cand, nil
}

// open plans the run and gets a cluster for it. The returned function ends
// the run: it deletes the cluster unless it is to be kept, and reports a
// failed deletion through errp when the run itself succeeded.
func open(ctx context.Context, w *model.World, candidate *Candidate, opts Options) (*session, func(errp *error), error) {
	if opts.ClusterName == "" {
		opts.ClusterName = DefaultClusterName
	}
	plan, err := BuildPlan(w, candidate, opts.EchoImage)
	if err != nil {
		return nil, nil, err
	}
	if len(plan.EchoDeployments) == 0 {
		return nil, nil, errors.New("the Ingresses route to no Service, so there is nothing to reproduce")
	}

	opts.progress("starting kind cluster %s (Kubernetes %s)", opts.ClusterName, KubernetesVersion)
	cluster, err := EnsureCluster(opts.ClusterName)
	if err != nil {
		return nil, nil, err
	}
	if !cluster.Created {
		opts.progress("reusing the existing cluster")
	}
	done := func(errp *error) {
		if opts.Keep {
			opts.progress("cluster %s kept; remove it with `routeparity lab down`", cluster.Name)
			return
		}
		opts.progress("deleting cluster %s", cluster.Name)
		if delErr := cluster.Delete(); delErr != nil && *errp == nil {
			*errp = delErr
		}
	}

	s := &session{opts: opts, cluster: cluster, plan: plan, probes: probe.Generate(w, opts.Probes)}
	if s.k, err = newKube(cluster.Config); err != nil {
		done(&err)
		return nil, nil, err
	}

	loaded, err := cluster.LoadLocalImage(ctx, opts.EchoImage)
	switch {
	case err != nil:
	case loaded:
		opts.progress("loaded the local image %s into the cluster", opts.EchoImage)
	case !strings.Contains(opts.EchoImage, "/"):
		// A name without a registry can only come from the local daemon.
		err = fmt.Errorf("the echo image %q is not in the local Docker daemon; build it with `make echo-image` or pass --echo-image", opts.EchoImage)
	}
	if err != nil {
		done(&err)
		return nil, nil, err
	}
	return s, done, nil
}

// install puts the controllers and the echo backends in place and waits for
// them. impl is nil when only ingress-nginx is needed.
func (s *session) install(ctx context.Context, impl *Implementation) error {
	if impl != nil {
		if err := s.claimImplementation(ctx, *impl); err != nil {
			return err
		}
	}
	s.opts.progress("installing ingress-nginx %s", IngressNGINXVersion)
	controller, err := decodeObjects(ingressNGINXManifest)
	if err != nil {
		return fmt.Errorf("embedded ingress-nginx manifest: %w", err)
	}
	if err := s.k.apply(ctx, controller); err != nil {
		return err
	}
	// The admission webhook races with the job that issues its certificate
	// and would reject Ingresses until that finishes. Routing does not
	// depend on it.
	err = s.k.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, "ingress-nginx-admission", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove the ingress-nginx admission webhook: %w", err)
	}

	entry, err := labelled(entryService(ingressNGINXNamespace, ingressNGINXSelector, []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), NodePort: nodePortNGINXHTTP},
		{Name: "https", Port: 443, TargetPort: intstr.FromString("https"), NodePort: nodePortNGINXHTTPS},
	}))
	if err != nil {
		return err
	}
	if err := s.k.apply(ctx, entry); err != nil {
		return err
	}

	if impl != nil {
		s.opts.progress("installing %s %s (Gateway API %s)", impl.Name, impl.Version, impl.GatewayAPIVersion)
		data, err := impl.manifest()
		if err != nil {
			return err
		}
		objs, err := decodeObjects(data)
		if err != nil {
			return fmt.Errorf("embedded %s manifest: %w", impl.Name, err)
		}
		if err := s.k.apply(ctx, objs); err != nil {
			return err
		}
	}

	if err := s.removeStale(ctx, impl != nil); err != nil {
		return err
	}
	support, err := labelled(s.plan.Support...)
	if err != nil {
		return err
	}
	if err := s.k.apply(ctx, support); err != nil {
		return err
	}

	if err := s.k.waitForDeployment(ctx, ingressNGINXNamespace, ingressNGINXDeployment, controllerTimeout); err != nil {
		return err
	}
	if impl != nil {
		if err := s.k.waitForDeployment(ctx, impl.controller.Namespace, impl.controller.Name, controllerTimeout); err != nil {
			return err
		}
	}
	for _, d := range s.plan.EchoDeployments {
		if err := s.k.waitForDeployment(ctx, d.Namespace, d.Name, echoTimeout); err != nil {
			return err
		}
	}
	return nil
}

// baseline applies the Ingresses and records ingress-nginx.
func (s *session) baseline(ctx context.Context) (*model.Snapshot, error) {
	s.opts.progress("applying %d Ingress(es) with %d Service(s) replaced by the echo backend", len(s.plan.Ingresses), countServices(s.plan))
	ingresses := make([]runtime.Object, len(s.plan.Ingresses))
	for i, ing := range s.plan.Ingresses {
		ingresses[i] = ing
	}
	objs, err := labelled(ingresses...)
	if err != nil {
		return nil, err
	}
	// One at a time and in order: ingress-nginx gives precedence to the
	// oldest Ingress, and creation order is the only way to reproduce that.
	if err := s.k.apply(ctx, objs); err != nil {
		return nil, err
	}

	target := prober.Target{
		HTTP:  hostPort(s.cluster.Ports.NGINXHTTP),
		HTTPS: hostPort(s.cluster.Ports.NGINXHTTPS),
	}

	// ingress-nginx reloads a few seconds after Ingresses change, not at
	// once. Until then it serves the previous configuration, consistently,
	// so neither "the routes answer" nor "two recordings agree" shows that
	// the change has taken effect: in a reused cluster both hold for the
	// configuration of the run before.
	s.opts.progress("waiting for ingress-nginx to load the configuration")
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	sentinel, host := s.plan.Sentinel(nonce)
	sentinelObjs, err := labelled(sentinel)
	if err != nil {
		return nil, err
	}
	if err := s.k.apply(ctx, sentinelObjs); err != nil {
		return nil, err
	}
	if err := waitForSentinel(ctx, "ingress-nginx", target, host, s.opts.Send); err != nil {
		return nil, err
	}

	unverified := append(append([]model.Unverified{}, s.plan.Unverified...), s.probes.Unverified...)
	// The configuration is current, so a host that is not routed now never
	// will be; one short pass is enough to find those.
	for _, host := range waitForRoutes(ctx, target, s.probes.Probes, s.opts.Send, hostCheckTimeout) {
		unverified = append(unverified, model.Unverified{
			Reason:  ReasonNotLoaded,
			Feature: host,
			Detail:  "ingress-nginx never started routing this host; it may have rejected the Ingress",
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.opts.progress("sending %d probe(s) to ingress-nginx", len(s.probes.Probes))
	meta := model.TargetMeta{
		Name:           "ingress-nginx",
		Kind:           model.TargetIngressNGINX,
		Mode:           model.ModeLab,
		Address:        "kind cluster " + s.cluster.Name,
		Implementation: "ingress-nginx",
		Versions: map[string]string{
			"ingress-nginx": IngressNGINXVersion,
			"kubernetes":    KubernetesVersion,
			"echo":          s.opts.EchoImage,
		},
		ToolVersion: s.opts.ToolVersion,
	}
	return s.record(ctx, target, meta, unverified)
}

// candidate applies the Gateway configuration and records the implementation.
func (s *session) candidate(ctx context.Context, impl Implementation, candidate *Candidate) (*model.Snapshot, error) {
	if impl.perGateway {
		return s.candidatePerGateway(ctx, impl, candidate)
	}
	class, err := decodeObjects([]byte(impl.class))
	if err != nil {
		return nil, fmt.Errorf("%s GatewayClass: %w", impl.Name, err)
	}
	objs, gateways, routes, err := candidate.labObjects(impl.ClassName)
	if err != nil {
		return nil, err
	}
	s.opts.progress("applying %d Gateway(s) and %d HTTPRoute(s) from %s", len(gateways), len(routes), candidate.Source)
	if err := s.k.apply(ctx, label(class)); err != nil {
		return nil, err
	}
	if err := s.k.apply(ctx, label(objs)); err != nil {
		return nil, err
	}

	// The sentinel route goes last, for the reason given in baseline: a
	// proxy that serves it has a configuration built after everything above.
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	sentinelGateway, sentinelRoute, host := s.plan.GatewaySentinel(nonce, impl.ClassName)
	sentinelObjs, err := labelled(sentinelGateway, sentinelRoute)
	if err != nil {
		return nil, err
	}
	if err := s.k.apply(ctx, sentinelObjs); err != nil {
		return nil, err
	}

	s.opts.progress("waiting for the %s proxy", impl.Name)
	target, err := s.exposeProxy(ctx, impl)
	if err != nil {
		return nil, err
	}
	if err := waitForSentinel(ctx, impl.Name, target, host, s.opts.Send); err != nil {
		return nil, err
	}

	unverified := append(append([]model.Unverified{}, s.plan.Unverified...), s.probes.Unverified...)
	unverified = append(unverified, candidate.Unverified...)
	unverified = append(unverified, s.gatewayStatus(ctx, gateways, routes)...)

	s.opts.progress("sending %d probe(s) to %s", len(s.probes.Probes), impl.Name)
	meta := model.TargetMeta{
		Name:           impl.Name,
		Kind:           model.TargetGatewayAPI,
		Mode:           model.ModeLab,
		Address:        "kind cluster " + s.cluster.Name,
		Implementation: impl.Name,
		Versions: map[string]string{
			impl.Name:     impl.Version,
			"gateway-api": impl.GatewayAPIVersion,
			"kubernetes":  KubernetesVersion,
			"echo":        s.opts.EchoImage,
			"routes":      candidate.Source,
		},
		ToolVersion: s.opts.ToolVersion,
	}
	return s.record(ctx, target, meta, unverified)
}

// record records a target once it answers consistently.
func (s *session) record(ctx context.Context, target prober.Target, meta model.TargetMeta, unverified []model.Unverified) (*model.Snapshot, error) {
	snap, settled, err := recordSettled(ctx, target, meta, s.probes.Probes, unverified, s.opts.Send)
	if err != nil {
		return nil, err
	}
	if !settled {
		snap.Unverified = append(snap.Unverified, model.Unverified{
			Reason:  ReasonNotLoaded,
			Feature: "stable behaviour of " + meta.Name,
			Detail: fmt.Sprintf("it still answered differently between two consecutive recordings after %d attempts; "+
				"the snapshot is the last recording", settleRounds),
		})
	}
	return snap, nil
}

// waitForProxy waits for the implementation to create the Service in front
// of its data plane and for a pod behind it to be ready.
func (s *session) waitForProxy(ctx context.Context, impl Implementation) (*corev1.Service, error) {
	var found *corev1.Service
	var last string
	err := wait.PollUntilContextTimeout(ctx, time.Second, proxyTimeout, true, func(ctx context.Context) (bool, error) {
		services, err := s.k.client.CoreV1().Services(impl.proxyNamespace).List(ctx, metav1.ListOptions{LabelSelector: impl.proxySelector})
		if err != nil {
			last = err.Error()
			return false, nil //nolint:nilerr // asked again until the timeout
		}
		if len(services.Items) == 0 {
			last = "no Service matches " + impl.proxySelector
			return false, nil
		}
		if len(services.Items) != 1 {
			return false, fmt.Errorf("%s proxy selector %q matches %d Services in %s; refusing to select one arbitrarily", impl.Name, impl.proxySelector, len(services.Items), impl.proxyNamespace)
		}
		svc := services.Items[0]
		if len(svc.Spec.Selector) == 0 {
			return false, fmt.Errorf("%s proxy Service %s/%s has no pod selector", impl.Name, svc.Namespace, svc.Name)
		}
		selector := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: svc.Spec.Selector})
		if _, err := s.k.runningPod(ctx, impl.proxyNamespace, selector); err != nil {
			last = s.k.describePods(ctx, impl.proxyNamespace, selector)
			return false, nil //nolint:nilerr // asked again until the timeout
		}
		found = &svc
		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the %s proxy was not ready: %w (%s)", impl.Name, err, last)
	}
	return found, nil
}

// exposeProxy waits for the implementation's proxy and puts an entry Service
// in front of it on the node ports that are published to the host. It returns
// where to send probes. A proxy with no listener on 443 gets no HTTPS address.
func (s *session) exposeProxy(ctx context.Context, impl Implementation) (prober.Target, error) {
	if err := s.clearGatewayEntry(ctx); err != nil {
		return prober.Target{}, err
	}
	proxy, err := s.waitForProxy(ctx, impl)
	if err != nil {
		return prober.Target{}, err
	}
	var ports []corev1.ServicePort
	var target prober.Target
	for _, p := range proxy.Spec.Ports {
		switch p.Port {
		case 80:
			ports = append(ports, corev1.ServicePort{Name: "http", Port: 80, TargetPort: p.TargetPort, NodePort: nodePortGatewayHTTP})
			target.HTTP = hostPort(s.cluster.Ports.GatewayHTTP)
		case 443:
			ports = append(ports, corev1.ServicePort{Name: "https", Port: 443, TargetPort: p.TargetPort, NodePort: nodePortGatewayHTTPS})
			target.HTTPS = hostPort(s.cluster.Ports.GatewayHTTPS)
		}
	}
	if len(ports) == 0 {
		return prober.Target{}, fmt.Errorf("the %s proxy listens on neither port 80 nor port 443", impl.Name)
	}
	entry, err := labelled(entryService(impl.proxyNamespace, proxy.Spec.Selector, ports))
	if err != nil {
		return prober.Target{}, err
	}
	if err := s.k.apply(ctx, entry); err != nil {
		return prober.Target{}, err
	}
	return target, nil
}

// entryService is a NodePort Service in front of a proxy's pods. Its node
// ports are the ones published to the host when the cluster was created.
func entryService(namespace string, selector map[string]string, ports []corev1.ServicePort) *corev1.Service {
	for i := range ports {
		ports[i].Protocol = corev1.ProtocolTCP
	}
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "routeparity-entry"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: selector, Ports: ports},
	}
}

func hostPort(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// gatewayStatus reports the Gateways and HTTPRoutes that the implementation
// did not accept. Such a route is not in effect, and the probes for it will
// show up as differences; saying why here is what makes those readable.
func (s *session) gatewayStatus(ctx context.Context, gateways, routes []types.NamespacedName) []model.Unverified {
	group, version := gatewayv1.GroupVersion.Group, gatewayv1.GroupVersion.Version
	gatewayKind := schema.GroupVersionKind{Group: group, Version: version, Kind: "Gateway"}
	routeKind := schema.GroupVersionKind{Group: group, Version: version, Kind: "HTTPRoute"}

	check := func() []model.Unverified {
		var out []model.Unverified
		for _, key := range gateways {
			ref := model.ObjectRef{Kind: "Gateway", Namespace: key.Namespace, Name: key.Name}
			problems := s.objectProblems(ctx, gatewayKind, key, func(u *unstructured.Unstructured) []string {
				conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
				problems := conditionProblems(conditions, "Accepted", "Programmed")
				listeners, _, _ := unstructured.NestedSlice(u.Object, "status", "listeners")
				for _, raw := range listeners {
					listener, _ := raw.(map[string]any)
					name, _ := listener["name"].(string)
					conds, _ := listener["conditions"].([]any)
					for _, p := range conditionProblems(conds, "Accepted", "Programmed") {
						problems = append(problems, "listener "+name+": "+p)
					}
				}
				return problems
			})
			for _, p := range problems {
				out = append(out, model.Unverified{Reason: ReasonNotLoaded, Feature: ref.String(), Object: &ref, Detail: p})
			}
		}
		for _, key := range routes {
			ref := model.ObjectRef{Kind: "HTTPRoute", Namespace: key.Namespace, Name: key.Name}
			problems := s.objectProblems(ctx, routeKind, key, func(u *unstructured.Unstructured) []string {
				parents, _, _ := unstructured.NestedSlice(u.Object, "status", "parents")
				if len(parents) == 0 {
					return []string{"no Gateway has reported on this route, so it is attached to none"}
				}
				var problems []string
				for _, raw := range parents {
					parent, _ := raw.(map[string]any)
					conds, _ := parent["conditions"].([]any)
					problems = append(problems, conditionProblems(conds, "Accepted", "ResolvedRefs")...)
				}
				return problems
			})
			for _, p := range problems {
				out = append(out, model.Unverified{Reason: ReasonNotLoaded, Feature: ref.String(), Object: &ref, Detail: p})
			}
		}
		return out
	}

	// Status trails the data plane slightly; give it a moment to catch up
	// before reporting a problem that is about to resolve itself.
	var problems []model.Unverified
	_ = wait.PollUntilContextTimeout(ctx, time.Second, statusTimeout, true, func(context.Context) (bool, error) {
		problems = check()
		return len(problems) == 0, nil
	})
	return problems
}

func (s *session) objectProblems(ctx context.Context, gvk schema.GroupVersionKind, key types.NamespacedName, read func(*unstructured.Unstructured) []string) []string {
	resource, err := s.k.resource(ctx, gvk, key.Namespace)
	if err != nil {
		return []string{err.Error()}
	}
	obj, err := resource.Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return []string{"could not be read back: " + err.Error()}
	}
	return read(obj)
}

// removeStale deletes what a previous run left in a reused cluster. Left in
// place it would keep routing, and the recording would describe two
// configurations at once.
func (s *session) removeStale(ctx context.Context, gatewayAPI bool) error {
	selector := managedByLabel + "=" + managedByValue
	list, err := s.k.client.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list Ingresses from a previous run: %w", err)
	}
	for _, ing := range list.Items {
		err := s.k.client.NetworkingV1().Ingresses(ing.Namespace).Delete(ctx, ing.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Ingress %s/%s from a previous run: %w", ing.Namespace, ing.Name, err)
		}
	}
	if !gatewayAPI {
		return nil
	}

	// Everything in the Gateway API groups that carries the lab's label,
	// except the class and its proxy settings: those are the same in every
	// run, and deleting them would restart the proxy for nothing.
	_, lists, err := s.k.client.Discovery().ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		return fmt.Errorf("discover API resources: %w", err)
	}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil || !staleGroups[gv.Group] {
			continue
		}
		for _, res := range list.APIResources {
			if strings.Contains(res.Name, "/") || res.Kind == "GatewayClass" || res.Kind == "EnvoyProxy" || res.Kind == "NginxProxy" || !hasVerbs(res.Verbs, "list", "delete") {
				continue
			}
			client := s.k.dyn.Resource(gv.WithResource(res.Name))
			items, err := client.Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				continue // a kind that cannot be listed holds nothing of ours
			}
			for _, item := range items.Items {
				err := client.Namespace(item.GetNamespace()).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("delete %s %s/%s from a previous run: %w", res.Kind, item.GetNamespace(), item.GetName(), err)
				}
			}
		}
	}
	return nil
}

// staleGroups are the API groups a previous run's Gateway configuration lives in.
var staleGroups = map[string]bool{
	gatewayv1.GroupName:           true,
	"gateway.networking.x-k8s.io": true,
	"gateway.envoyproxy.io":       true,
	"gateway.nginx.org":           true,
	"networking.istio.io":         true,
	"security.istio.io":           true,
	"telemetry.istio.io":          true,
	"extensions.istio.io":         true,
}

func hasVerbs(verbs []string, want ...string) bool {
	have := map[string]bool{}
	for _, v := range verbs {
		have[v] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

func newNonce() (string, error) {
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

// waitForSentinel waits until a proxy routes the sentinel host to the echo.
// The host is new in every run, so the only configuration that can route it
// is one built after this run's changes were applied.
func waitForSentinel(ctx context.Context, name string, target prober.Target, host string, send prober.Options) error {
	probe := model.Probe{Host: host, RawTarget: "/", Origin: model.Origin{Source: model.OriginControl, Mutation: "sentinel"}}
	address := target.HTTP
	if address == "" {
		address, probe.Scheme = target.HTTPS, "https"
	}
	if address == "" {
		return fmt.Errorf("%s exposes no HTTP or HTTPS port to send the sentinel request to", name)
	}
	if err := probe.Seal(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, routesTimeout)
	defer cancel()
	var last string
	for {
		obs := prober.Send(ctx, address, probe, send)
		switch {
		case obs.Backend != nil:
			return nil
		case obs.Client.Error != "":
			last = obs.Client.Error
		default:
			last = fmt.Sprintf("status %d", obs.Client.Status)
		}
		if sleep(ctx, 500*time.Millisecond) != nil {
			return fmt.Errorf("%s did not load the configuration within %s (last answer for the sentinel host: %s)", name, routesTimeout, last)
		}
	}
}

// recordSettled records the target until two consecutive recordings agree.
// The sentinel has already established that this run's configuration is in
// effect. This is a second check, against whatever else can make a target
// answer differently from one moment to the next, such as a Secret that a
// controller picks up after the route that uses it.
func recordSettled(ctx context.Context, target prober.Target, meta model.TargetMeta, probes []model.Probe,
	unverified []model.Unverified, send prober.Options) (*model.Snapshot, bool, error) {
	var previous *model.Snapshot
	for round := 0; round < settleRounds; round++ {
		if round > 0 {
			if err := sleep(ctx, time.Second); err != nil {
				return nil, false, err
			}
		}
		snap, err := prober.Record(ctx, target, meta, probes, unverified, send, time.Now())
		if err != nil {
			return nil, false, err
		}
		if previous != nil {
			res := diff.Compare(previous, snap, diff.Options{})
			if res.Summary.Diff == 0 && res.Summary.Error == 0 {
				return snap, true, nil
			}
		}
		previous = snap
	}
	return previous, false, nil
}

// waitForRoutes polls until every host has a configured path that the target
// answers differently from a request for an unknown host, which is the sign
// that the host's routes are loaded. It returns the hosts that never got there.
func waitForRoutes(ctx context.Context, target prober.Target, probes []model.Probe, send prober.Options, timeout time.Duration) []string {
	var control *model.Probe
	pending := map[string]model.Probe{}
	var order []string
	for i, p := range probes {
		switch {
		case p.Origin.Source == model.OriginControl:
			control = &probes[i]
		case p.Origin.Source == model.OriginDerived && p.Scheme == "http":
			if _, ok := pending[p.Host]; !ok {
				pending[p.Host] = p
				order = append(order, p.Host)
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for len(pending) > 0 {
		unroutedStatus, unroutedBody, haveControl := 0, "", false
		if control != nil {
			if obs := prober.Send(ctx, target.HTTP, *control, send); obs.Client.Error == "" && obs.Backend == nil {
				unroutedStatus, unroutedBody, haveControl = obs.Client.Status, obs.Client.BodySHA256, true
			}
		}
		for host, p := range pending {
			obs := prober.Send(ctx, target.HTTP, p, send)
			if obs.Client.Error != "" {
				continue
			}
			unrouted := obs.Backend == nil && (obs.Client.Status == 404 || obs.Client.Status == 503)
			if haveControl {
				unrouted = obs.Backend == nil && obs.Client.Status == unroutedStatus && obs.Client.BodySHA256 == unroutedBody
			}
			if !unrouted {
				delete(pending, host)
			}
		}
		if len(pending) == 0 || sleep(ctx, time.Second) != nil {
			break
		}
	}

	var notLoaded []string
	for _, host := range order {
		if _, still := pending[host]; still {
			notLoaded = append(notLoaded, host)
		}
	}
	return notLoaded
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// labelled converts objects for applying and marks them as the lab's, so
// that a later run in a reused cluster can find what an earlier one left.
func labelled(objs ...runtime.Object) ([]*unstructured.Unstructured, error) {
	out, err := toUnstructured(objs...)
	if err != nil {
		return nil, err
	}
	return label(out), nil
}

// label marks objects as the lab's and removes fields that server-side apply
// rejects when they are present but empty.
func label(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	for _, obj := range objs {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[managedByLabel] = managedByValue
		obj.SetLabels(labels)
		unstructured.RemoveNestedField(obj.Object, "metadata", "creationTimestamp")
		unstructured.RemoveNestedField(obj.Object, "status")
	}
	return objs
}

func countServices(plan *Plan) int {
	n := 0
	for _, obj := range plan.Support {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Service" {
			n++
		}
	}
	return n
}

package lab

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

const (
	echoName = "routeparity-echo"
	// echoAdminPort serves the echo's health endpoint, apart from the ports
	// that stand in for Services.
	echoAdminPort = 9090
	// echoFirstPort is the first port an echo pod listens on for a Service.
	echoFirstPort = 8000
	// labIngressClass is the class the lab's ingress-nginx serves.
	labIngressClass = "nginx"
)

// Plan is what the lab applies to reproduce a World: the Ingresses as they
// are, optionally a Gateway API configuration next to them, and every Service
// either side routes to replaced by the echo backend.
type Plan struct {
	// Support holds namespaces, TLS secrets, echo Deployments and the
	// Services that point at them. It is applied before anything that routes.
	Support []runtime.Object
	// Ingresses are the World's Ingresses, cleaned of cluster-specific
	// metadata, in the order ingress-nginx gives them precedence.
	Ingresses []*networkingv1.Ingress
	// EchoDeployments are the Deployments to wait for, by namespace.
	EchoDeployments []types.NamespacedName
	// Unverified lists configuration the lab cannot reproduce, with the
	// reason. What is listed here was removed before applying, so it is not
	// part of what gets measured.
	Unverified []model.Unverified

	// sentinel is an echo Service port for sentinel routes to point at.
	sentinel *sentinelBackend
}

type sentinelBackend struct {
	namespace string
	service   string
	port      int32
}

// SentinelHostSuffix ends the host of every sentinel route. The .invalid
// top-level domain is reserved, so it cannot collide with a real host.
const SentinelHostSuffix = ".sentinel.routeparity.invalid"

const sentinelName = "routeparity-sentinel"

// Sentinel returns an Ingress for a host unique to this run, routed to one
// of the echo Services. It is applied after everything else. Once
// ingress-nginx routes its host, the configuration in effect includes every
// change made before it, because the controller builds each configuration
// from all the Ingresses it knows and learns of them in order.
//
// It returns nil when the plan has no echo Service to point at.
func (p *Plan) Sentinel(nonce string) (*networkingv1.Ingress, string) {
	if p.sentinel == nil {
		return nil, ""
	}
	host := "rp-" + nonce + SentinelHostSuffix
	class := labIngressClass
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Namespace: p.sentinel.namespace, Name: sentinelName},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: p.sentinel.service, Port: networkingv1.ServiceBackendPort{Number: p.sentinel.port},
						}},
					}},
				}},
			}},
		},
	}, host
}

// GatewaySentinel is the Gateway API counterpart of Sentinel: a Gateway with
// one listener for a host unique to this run, and an HTTPRoute sending that
// host to an echo Service. The route is applied last.
func (p *Plan) GatewaySentinel(nonce, className string) (gateway *gatewayv1.Gateway, route *gatewayv1.HTTPRoute, host string) {
	if p.sentinel == nil {
		return nil, nil, ""
	}
	host = "rp-" + nonce + SentinelHostSuffix
	hostname := gatewayv1.Hostname(host)
	port := p.sentinel.port
	gateway = &gatewayv1.Gateway{
		TypeMeta:   metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway"},
		ObjectMeta: metav1.ObjectMeta{Namespace: p.sentinel.namespace, Name: sentinelName},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(className),
			Listeners: []gatewayv1.Listener{{
				Name: "sentinel", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &hostname,
			}},
		},
	}
	route = &gatewayv1.HTTPRoute{
		TypeMeta:   metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{Namespace: p.sentinel.namespace, Name: sentinelName},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: sentinelName}}},
			Hostnames:       []gatewayv1.Hostname{hostname},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(p.sentinel.service), Port: &port},
				}}},
			}},
		},
	}
	return gateway, route, host
}

// backendPort is one Service port that an Ingress or a route sends traffic to.
type backendPort struct {
	service string
	// id names the port in echo responses: its number when that is known,
	// otherwise its name. An Ingress may name a port "http" where the
	// converted HTTPRoute says 80; resolving both to one id is what lets the
	// two sides be seen to reach the same backend.
	id string
	// number is the Service port number, or zero when only a name is known.
	number int32
	// name is the Service port name, or empty when only a number is known.
	name string
}

// backendSet collects the backend ports per namespace.
type backendSet struct {
	world *model.World
	ports map[string]map[string]*backendPort
}

// add records a backend given by port name or by port number, resolving one
// to the other through the original Service when the manifests include it.
func (s *backendSet) add(namespace, service, name string, number int32) {
	if original, ok := s.world.Services[types.NamespacedName{Namespace: namespace, Name: service}]; ok {
		for _, p := range original.Spec.Ports {
			switch {
			case number == 0 && name != "" && p.Name == name:
				number = p.Port
			case number != 0 && name == "" && p.Port == number:
				name = p.Name
			}
		}
	}
	id := name
	if number != 0 {
		id = strconv.Itoa(int(number))
	}
	if id == "" {
		return
	}
	if s.ports[namespace] == nil {
		s.ports[namespace] = map[string]*backendPort{}
	}
	key := service + "\x00" + id
	if existing := s.ports[namespace][key]; existing != nil {
		if existing.name == "" {
			existing.name = name
		}
		return
	}
	s.ports[namespace][key] = &backendPort{service: service, id: id, number: number, name: name}
}

func (s *backendSet) sorted(namespace string) []*backendPort {
	out := make([]*backendPort, 0, len(s.ports[namespace]))
	for _, p := range s.ports[namespace] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].service != out[j].service {
			return out[i].service < out[j].service
		}
		return out[i].id < out[j].id
	})
	return out
}

// BuildPlan derives the lab objects for a World. candidate is the Gateway
// API configuration to run next to the Ingresses, or nil to reproduce the
// Ingresses alone.
func BuildPlan(w *model.World, candidate *Candidate, echoImage string) (*Plan, error) {
	plan := &Plan{}
	backends := &backendSet{world: w, ports: map[string]map[string]*backendPort{}}
	secrets := map[types.NamespacedName]map[string]bool{}
	namespaces := map[string]bool{}
	wantSecret := func(key types.NamespacedName, hosts ...string) {
		if secrets[key] == nil {
			secrets[key] = map[string]bool{}
		}
		for _, host := range hosts {
			if host != "" {
				secrets[key][host] = true
			}
		}
		namespaces[key.Namespace] = true
	}

	note := func(ing *networkingv1.Ingress, b *networkingv1.IngressBackend, where string) {
		ref := model.IngressRef(ing)
		switch {
		case b.Service != nil:
			backends.add(ing.Namespace, b.Service.Name, b.Service.Port.Name, b.Service.Port.Number)
		case b.Resource != nil:
			plan.Unverified = append(plan.Unverified, model.Unverified{
				Reason:  model.ReasonUnsupported,
				Feature: where + " routes to a " + b.Resource.Kind + " resource",
				Object:  &ref,
				Detail:  "only Service backends are replaced by the echo",
			})
		}
	}
	for i := range w.Ingresses {
		ing := &w.Ingresses[i]
		namespaces[ing.Namespace] = true
		if ing.Spec.DefaultBackend != nil {
			note(ing, ing.Spec.DefaultBackend, "the default backend")
		}
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for j := range rule.HTTP.Paths {
				note(ing, &rule.HTTP.Paths[j].Backend, "path "+rule.HTTP.Paths[j].Path)
			}
		}
		for _, tls := range ing.Spec.TLS {
			if tls.SecretName != "" {
				wantSecret(types.NamespacedName{Namespace: ing.Namespace, Name: tls.SecretName}, tls.Hosts...)
			}
		}
		clean, unverified := labIngress(ing)
		plan.Ingresses = append(plan.Ingresses, clean)
		plan.Unverified = append(plan.Unverified, unverified...)
	}

	if candidate != nil {
		plan.Unverified = append(plan.Unverified, candidate.backends(backends, wantSecret, namespaces)...)
	}

	for _, ns := range sortedKeys(backends.ports) {
		namespaces[ns] = true
	}
	for _, ns := range sortedKeys(namespaces) {
		plan.Support = append(plan.Support, &corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
	}

	secretKeys := make([]types.NamespacedName, 0, len(secrets))
	for key := range secrets {
		secretKeys = append(secretKeys, key)
	}
	sort.Slice(secretKeys, func(i, j int) bool { return secretKeys[i].String() < secretKeys[j].String() })
	for _, key := range secretKeys {
		secret, err := selfSignedSecret(key, sortedKeys(secrets[key]))
		if err != nil {
			return nil, fmt.Errorf("certificate for secret %s: %w", key, err)
		}
		plan.Support = append(plan.Support, secret)
	}

	for _, ns := range sortedKeys(backends.ports) {
		deployment, services, first := echoBackends(ns, backends.sorted(ns), echoImage)
		if plan.sentinel == nil {
			plan.sentinel = first
		}
		plan.Support = append(plan.Support, deployment)
		for _, svc := range services {
			plan.Support = append(plan.Support, svc)
		}
		plan.EchoDeployments = append(plan.EchoDeployments, types.NamespacedName{Namespace: ns, Name: echoName})
	}
	return plan, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// echoBackends builds one echo Deployment for a namespace and a Service for
// each Service name that is routed to. Each backend port gets its own echo
// port, which is how a response says which Service port it stands in for. It
// also returns the first of them as a target for sentinel routes.
func echoBackends(namespace string, ports []*backendPort, image string) (*appsv1.Deployment, []*corev1.Service, *sentinelBackend) {
	labels := map[string]string{"app.kubernetes.io/name": echoName}
	container := corev1.Container{
		Name:            "echo",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"--admin", fmt.Sprintf(":%d", echoAdminPort)},
		Env: []corev1.EnvVar{{
			Name:      "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(echoAdminPort)}},
			PeriodSeconds: 1,
		},
	}

	byService := map[string]*corev1.Service{}
	var services []*corev1.Service
	var first *sentinelBackend
	for i, bp := range ports {
		echoPort := int32(echoFirstPort + i)
		container.Args = append(container.Args, "--listen", fmt.Sprintf("%s/%s:%s=:%d", namespace, bp.service, bp.id, echoPort))

		svc := byService[bp.service]
		if svc == nil {
			svc = &corev1.Service{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: bp.service},
				Spec:       corev1.ServiceSpec{Selector: labels},
			}
			byService[bp.service] = svc
			services = append(services, svc)
		}
		spec := corev1.ServicePort{
			Name:       bp.name,
			Port:       bp.number,
			TargetPort: intstr.FromInt32(echoPort),
			Protocol:   corev1.ProtocolTCP,
		}
		if spec.Port == 0 {
			// Only the name is known. The name is what gets resolved, so
			// any number serves.
			spec.Port = echoPort
		}
		if spec.Name == "" {
			// Port names are required as soon as a Service has two ports.
			spec.Name = "p" + bp.id
		}
		svc.Spec.Ports = append(svc.Spec.Ports, spec)
		if first == nil {
			first = &sentinelBackend{namespace: namespace, service: bp.service, port: spec.Port}
		}
	}

	replicas := int32(1)
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: echoName, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}
	return deployment, services, first
}

// strippedAnnotation describes ingress-nginx annotations the lab removes,
// because keeping them would measure the lab's limits instead of the routing.
type strippedAnnotation struct {
	matches func(name, value string) bool
	reason  model.UnverifiedReason
	detail  string
}

var strippedAnnotations = []strippedAnnotation{
	{
		matches: func(name, _ string) bool { return strings.HasPrefix(name, "auth-") },
		reason:  model.ReasonBackendDependent,
		detail:  "needs the authentication service, secret or CA it refers to, which the lab does not have",
	},
	{
		matches: func(name, _ string) bool { return name == "whitelist-source-range" || name == "denylist-source-range" },
		reason:  model.ReasonClientIdentity,
		detail:  "depends on the client address, and every lab request arrives through a port-forward",
	},
	{
		matches: func(name, _ string) bool { return strings.HasPrefix(name, "limit-") },
		reason:  model.ReasonUnsupported,
		detail:  "a rate or connection limit would throttle the probes themselves",
	},
	{
		matches: func(name, _ string) bool { return strings.HasSuffix(name, "-snippet") },
		reason:  model.ReasonUnsupported,
		detail:  "snippets are disabled by default in ingress-nginx and the lab does not enable them",
	},
	{
		matches: func(name, value string) bool {
			return name == "backend-protocol" && !strings.EqualFold(strings.TrimSpace(value), "HTTP")
		},
		reason: model.ReasonBackendDependent,
		detail: "the echo backend speaks plain HTTP only",
	},
	{
		matches: func(name, _ string) bool { return strings.HasPrefix(name, "proxy-ssl-") },
		reason:  model.ReasonBackendDependent,
		detail:  "configures TLS to the backend, and the echo backend speaks plain HTTP only",
	},
	{
		matches: func(name, _ string) bool { return name == "custom-headers" || name == "mirror-target" },
		reason:  model.ReasonBackendDependent,
		detail:  "refers to an object or a service that the lab does not have",
	},
}

// labIngress returns the Ingress as the lab applies it: without the metadata
// a cluster export carries, assigned to the lab's Ingress class, and without
// the annotations the lab cannot honour.
func labIngress(ing *networkingv1.Ingress) (*networkingv1.Ingress, []model.Unverified) {
	ref := model.IngressRef(ing)
	out := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ing.Namespace,
			Name:        ing.Name,
			Labels:      ing.Labels,
			Annotations: map[string]string{},
		},
		Spec: *ing.Spec.DeepCopy(),
	}
	class := labIngressClass
	out.Spec.IngressClassName = &class

	var unverified []model.Unverified
	names := sortedKeys(ing.Annotations)
	for _, key := range names {
		value := ing.Annotations[key]
		if key == model.LegacyIngressClassAnnotation || key == "kubectl.kubernetes.io/last-applied-configuration" {
			continue
		}
		name, isNGINX := strings.CutPrefix(key, model.NGINXAnnotationPrefix)
		if isNGINX {
			if s, ok := stripped(name, value); ok {
				unverified = append(unverified, model.Unverified{Reason: s.reason, Feature: key, Object: &ref, Detail: s.detail})
				continue
			}
		}
		out.Annotations[key] = value
	}
	return out, unverified
}

func stripped(name, value string) (strippedAnnotation, bool) {
	for _, s := range strippedAnnotations {
		if s.matches(name, value) {
			return s, true
		}
	}
	return strippedAnnotation{}, false
}

// selfSignedSecret creates a TLS secret for the hosts. ingress-nginx only
// treats a host as having TLS, with the redirect that implies, when the
// secret its Ingress names exists and holds a usable certificate.
func selfSignedSecret(key types.NamespacedName, hosts []string) (*corev1.Secret, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "routeparity lab", Organization: []string{"routeparity"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hosts,
	}
	if len(hosts) == 0 {
		template.DNSNames = []string{"routeparity.invalid"}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		},
	}, nil
}

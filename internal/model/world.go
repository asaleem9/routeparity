package model

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// NGINXAnnotationPrefix is the default prefix of ingress-nginx annotations.
	NGINXAnnotationPrefix = "nginx.ingress.kubernetes.io/"
	// LegacyIngressClassAnnotation is the pre-IngressClass way of selecting a controller.
	LegacyIngressClassAnnotation = "kubernetes.io/ingress.class"
	// DefaultIngressClass is the class name ingress-nginx installs by default.
	DefaultIngressClass = "nginx"
)

// Objects is the raw input to a World: everything read from files or a cluster.
type Objects struct {
	Ingresses  []networkingv1.Ingress
	Services   []corev1.Service
	Gateways   []gatewayv1.Gateway
	HTTPRoutes []gatewayv1.HTTPRoute
}

// WorldOptions controls which Ingresses are considered.
type WorldOptions struct {
	// IngressClass selects Ingresses by spec.ingressClassName or the legacy
	// annotation. Empty means DefaultIngressClass.
	IngressClass string
	// IncludeClassless also selects Ingresses that name no class, which
	// ingress-nginx serves when it is the default class or runs with
	// --watch-ingress-without-class.
	IncludeClassless bool
}

// World is the routing configuration under test, indexed the way
// ingress-nginx sees it: by host, across all Ingresses of one class.
type World struct {
	IngressClass string
	// Ingresses are the selected Ingresses in precedence order, oldest first.
	Ingresses []networkingv1.Ingress
	// Skipped are Ingresses that were read and not selected.
	Skipped []SkippedIngress
	// UndatedIngresses counts selected Ingresses with no creationTimestamp.
	// ingress-nginx resolves conflicts in favour of the oldest Ingress, so
	// when this is non-zero the precedence order is a guess.
	UndatedIngresses int

	Services   map[types.NamespacedName]corev1.Service
	Gateways   []gatewayv1.Gateway
	HTTPRoutes []gatewayv1.HTTPRoute

	// Hosts is every host served, sorted by name. Rules without a host are
	// collected under the empty name.
	Hosts []*Host
	// DefaultBackends are spec.defaultBackend entries of selected Ingresses.
	DefaultBackends []DefaultBackend
}

// SkippedIngress is an Ingress that was read and left out, with the reason.
type SkippedIngress struct {
	Ref    ObjectRef `json:"ref"`
	Reason string    `json:"reason"`
}

// Host is one server name and every path routed under it.
type Host struct {
	// Name is the host as written in the Ingress rule, or empty for rules
	// that match any host.
	Name string
	// Paths are in precedence order of their owning Ingress.
	Paths []PathRule
	// TLS is true when any selected Ingress lists this host under spec.tls.
	TLS bool
	// RegexTriggers lists the Ingresses that put this host into regex mode.
	// In regex mode ingress-nginx treats every path on the host, from every
	// Ingress, as a case-insensitive regular expression matched as a prefix.
	RegexTriggers []RegexTrigger
}

// RegexMode reports whether any Ingress put this host into regex mode.
func (h *Host) RegexMode() bool { return len(h.RegexTriggers) > 0 }

// DisplayName is the host name, or a placeholder for the catch-all host.
func (h *Host) DisplayName() string {
	if h.Name == "" {
		return "*"
	}
	return h.Name
}

// RegexTrigger records which annotation on which Ingress enabled regex mode.
type RegexTrigger struct {
	Ingress    ObjectRef
	Annotation string
}

// Backend is where a path sends traffic.
type Backend struct {
	Namespace string
	// Service is empty when the backend is a resource reference.
	Service string
	// Port is the port name or number as written.
	Port string
	// Resource is "Kind/name" for resource backends.
	Resource string
}

// PathRule is a single path of a single Ingress rule.
type PathRule struct {
	Ingress  ObjectRef
	Path     string
	PathType networkingv1.PathType
	Backend  Backend
	// Canary is true when the owning Ingress is a canary. Canary Ingresses
	// add alternative backends to existing paths instead of defining paths.
	Canary bool

	annotations map[string]string
}

// EffectivePath is the path ingress-nginx uses: an empty path means "/".
func (p PathRule) EffectivePath() string {
	if p.Path == "" {
		return "/"
	}
	return p.Path
}

// Annotation returns an ingress-nginx annotation of the owning Ingress by its
// short name, for example "rewrite-target".
func (p PathRule) Annotation(name string) (string, bool) {
	v, ok := p.annotations[NGINXAnnotationPrefix+name]
	return v, ok
}

// AnnotationBool reads a boolean ingress-nginx annotation of the owning
// Ingress. Missing and unparseable values are false.
func (p PathRule) AnnotationBool(name string) bool {
	v, _ := p.Annotation(name)
	b, _ := strconv.ParseBool(v)
	return b
}

// DefaultBackend is a spec.defaultBackend of a selected Ingress.
type DefaultBackend struct {
	Ingress ObjectRef
	Backend Backend
}

// IngressRef builds the reference for an Ingress.
func IngressRef(ing *networkingv1.Ingress) ObjectRef {
	return ObjectRef{Kind: "Ingress", Namespace: ing.Namespace, Name: ing.Name}
}

// NGINXAnnotation returns an ingress-nginx annotation by its short name.
func NGINXAnnotation(ing *networkingv1.Ingress, name string) (string, bool) {
	v, ok := ing.Annotations[NGINXAnnotationPrefix+name]
	return v, ok
}

// NGINXAnnotationBool reads a boolean ingress-nginx annotation. Missing and
// unparseable values are false, which is how ingress-nginx treats them.
func NGINXAnnotationBool(ing *networkingv1.Ingress, name string) bool {
	v, _ := NGINXAnnotation(ing, name)
	b, _ := strconv.ParseBool(v)
	return b
}

// IngressClassOf returns the class an Ingress asks for, or empty when it
// names none. spec.ingressClassName wins over the legacy annotation.
func IngressClassOf(ing *networkingv1.Ingress) string {
	if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
		return *ing.Spec.IngressClassName
	}
	return ing.Annotations[LegacyIngressClassAnnotation]
}

// NewWorld selects the Ingresses of one class and indexes them by host.
func NewWorld(objs Objects, opts WorldOptions) *World {
	class := opts.IngressClass
	if class == "" {
		class = DefaultIngressClass
	}
	w := &World{
		IngressClass: class,
		Services:     make(map[types.NamespacedName]corev1.Service, len(objs.Services)),
		Gateways:     objs.Gateways,
		HTTPRoutes:   objs.HTTPRoutes,
	}
	for _, svc := range objs.Services {
		w.Services[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}] = svc
	}

	for i := range objs.Ingresses {
		ing := objs.Ingresses[i]
		switch got := IngressClassOf(&ing); {
		case got == class:
			w.Ingresses = append(w.Ingresses, ing)
		case got == "" && opts.IncludeClassless:
			w.Ingresses = append(w.Ingresses, ing)
		case got == "":
			w.Skipped = append(w.Skipped, SkippedIngress{Ref: IngressRef(&ing), Reason: "names no ingress class"})
		default:
			w.Skipped = append(w.Skipped, SkippedIngress{Ref: IngressRef(&ing), Reason: "ingress class is " + strconv.Quote(got)})
		}
	}

	// Oldest first, then namespace/name, matching how ingress-nginx orders
	// Ingresses when their rules conflict.
	sort.SliceStable(w.Ingresses, func(i, j int) bool {
		a, b := &w.Ingresses[i], &w.Ingresses[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	hosts := map[string]*Host{}
	host := func(name string) *Host {
		h, ok := hosts[name]
		if !ok {
			h = &Host{Name: name}
			hosts[name] = h
		}
		return h
	}

	for i := range w.Ingresses {
		ing := &w.Ingresses[i]
		ref := IngressRef(ing)
		if ing.CreationTimestamp.IsZero() {
			w.UndatedIngresses++
		}
		canary := NGINXAnnotationBool(ing, "canary")

		if ing.Spec.DefaultBackend != nil {
			w.DefaultBackends = append(w.DefaultBackends, DefaultBackend{
				Ingress: ref,
				Backend: backendOf(ing.Namespace, *ing.Spec.DefaultBackend),
			})
		}

		for _, rule := range ing.Spec.Rules {
			h := host(rule.Host)
			if !canary {
				if NGINXAnnotationBool(ing, "use-regex") {
					h.RegexTriggers = append(h.RegexTriggers, RegexTrigger{Ingress: ref, Annotation: "use-regex"})
				}
				if v, _ := NGINXAnnotation(ing, "rewrite-target"); v != "" {
					h.RegexTriggers = append(h.RegexTriggers, RegexTrigger{Ingress: ref, Annotation: "rewrite-target"})
				}
			}
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				pt := networkingv1.PathTypeImplementationSpecific
				if p.PathType != nil {
					pt = *p.PathType
				}
				h.Paths = append(h.Paths, PathRule{
					Ingress:     ref,
					Path:        p.Path,
					PathType:    pt,
					Backend:     backendOf(ing.Namespace, p.Backend),
					Canary:      canary,
					annotations: ing.Annotations,
				})
			}
		}

		for _, tls := range ing.Spec.TLS {
			for _, name := range tls.Hosts {
				host(name).TLS = true
			}
		}
	}

	for _, h := range hosts {
		// A host that only appears under spec.tls routes nothing.
		if len(h.Paths) == 0 && len(h.RegexTriggers) == 0 {
			continue
		}
		w.Hosts = append(w.Hosts, h)
	}
	sort.Slice(w.Hosts, func(i, j int) bool { return w.Hosts[i].Name < w.Hosts[j].Name })
	return w
}

func backendOf(namespace string, b networkingv1.IngressBackend) Backend {
	out := Backend{Namespace: namespace}
	switch {
	case b.Service != nil:
		out.Service = b.Service.Name
		if b.Service.Port.Name != "" {
			out.Port = b.Service.Port.Name
		} else if b.Service.Port.Number != 0 {
			out.Port = strconv.Itoa(int(b.Service.Port.Number))
		}
	case b.Resource != nil:
		out.Resource = b.Resource.Kind + "/" + b.Resource.Name
	}
	return out
}

// String renders the backend as namespace/service:port.
func (b Backend) String() string {
	if b.Resource != "" {
		return b.Namespace + "/" + b.Resource
	}
	var sb strings.Builder
	sb.WriteString(b.Namespace)
	sb.WriteByte('/')
	sb.WriteString(b.Service)
	if b.Port != "" {
		sb.WriteByte(':')
		sb.WriteString(b.Port)
	}
	return sb.String()
}

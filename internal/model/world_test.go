package model

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ingressOpt func(*networkingv1.Ingress)

func class(name string) ingressOpt {
	return func(ing *networkingv1.Ingress) { ing.Spec.IngressClassName = &name }
}

func annotate(name, value string) ingressOpt {
	return func(ing *networkingv1.Ingress) {
		if ing.Annotations == nil {
			ing.Annotations = map[string]string{}
		}
		ing.Annotations[name] = value
	}
}

func nginx(name, value string) ingressOpt { return annotate(NGINXAnnotationPrefix+name, value) }

func created(t time.Time) ingressOpt {
	return func(ing *networkingv1.Ingress) { ing.CreationTimestamp = metav1.NewTime(t) }
}

func tls(hosts ...string) ingressOpt {
	return func(ing *networkingv1.Ingress) {
		ing.Spec.TLS = append(ing.Spec.TLS, networkingv1.IngressTLS{Hosts: hosts, SecretName: "cert"})
	}
}

func rule(host string, paths ...networkingv1.HTTPIngressPath) ingressOpt {
	return func(ing *networkingv1.Ingress) {
		ing.Spec.Rules = append(ing.Spec.Rules, networkingv1.IngressRule{
			Host:             host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}},
		})
	}
}

func path(p string, pt networkingv1.PathType, service string) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{
		Path:     p,
		PathType: &pt,
		Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: service, Port: networkingv1.ServiceBackendPort{Number: 80},
		}},
	}
}

func ingress(namespace, name string, opts ...ingressOpt) networkingv1.Ingress {
	ing := networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	for _, opt := range opts {
		opt(&ing)
	}
	return ing
}

func names(ings []networkingv1.Ingress) []string {
	out := make([]string, len(ings))
	for i, ing := range ings {
		out[i] = ing.Namespace + "/" + ing.Name
	}
	return out
}

func hostNamed(t *testing.T, w *World, name string) *Host {
	t.Helper()
	for _, h := range w.Hosts {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("world has no host %q", name)
	return nil
}

func TestNewWorldSelectsByClass(t *testing.T) {
	objs := Objects{Ingresses: []networkingv1.Ingress{
		ingress("a", "by-field", class("nginx")),
		ingress("a", "by-annotation", annotate(LegacyIngressClassAnnotation, "nginx")),
		ingress("a", "field-wins", class("traefik"), annotate(LegacyIngressClassAnnotation, "nginx")),
		ingress("a", "other", class("traefik")),
		ingress("a", "classless"),
	}}

	t.Run("default class", func(t *testing.T) {
		w := NewWorld(objs, WorldOptions{})
		if diff := cmp.Diff([]string{"a/by-annotation", "a/by-field"}, names(w.Ingresses)); diff != "" {
			t.Errorf("selected (-want +got):\n%s", diff)
		}
		if len(w.Skipped) != 3 {
			t.Errorf("skipped %d Ingresses, want 3: %+v", len(w.Skipped), w.Skipped)
		}
	})
	t.Run("another class", func(t *testing.T) {
		w := NewWorld(objs, WorldOptions{IngressClass: "traefik"})
		if diff := cmp.Diff([]string{"a/field-wins", "a/other"}, names(w.Ingresses)); diff != "" {
			t.Errorf("selected (-want +got):\n%s", diff)
		}
	})
	t.Run("classless included on request", func(t *testing.T) {
		w := NewWorld(objs, WorldOptions{IncludeClassless: true})
		if diff := cmp.Diff([]string{"a/by-annotation", "a/by-field", "a/classless"}, names(w.Ingresses)); diff != "" {
			t.Errorf("selected (-want +got):\n%s", diff)
		}
	})
}

func TestNewWorldOrdersIngressesOldestFirst(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	w := NewWorld(Objects{Ingresses: []networkingv1.Ingress{
		ingress("b", "newest", class("nginx"), created(t0.Add(2*time.Hour)), rule("h", path("/x", networkingv1.PathTypeExact, "new"))),
		ingress("b", "same-time", class("nginx"), created(t0), rule("h", path("/x", networkingv1.PathTypeExact, "tie-b"))),
		ingress("a", "same-time", class("nginx"), created(t0), rule("h", path("/x", networkingv1.PathTypeExact, "tie-a"))),
	}}, WorldOptions{})

	if diff := cmp.Diff([]string{"a/same-time", "b/same-time", "b/newest"}, names(w.Ingresses)); diff != "" {
		t.Errorf("order (-want +got):\n%s", diff)
	}
	// Paths follow the same order, so the first entry for a path is the one
	// ingress-nginx serves.
	h := hostNamed(t, w, "h")
	if got := h.Paths[0].Backend.Service; got != "tie-a" {
		t.Errorf("first path belongs to backend %q, want tie-a", got)
	}
	if w.UndatedIngresses != 0 {
		t.Errorf("UndatedIngresses = %d, want 0", w.UndatedIngresses)
	}
}

func TestNewWorldCountsUndatedIngresses(t *testing.T) {
	w := NewWorld(Objects{Ingresses: []networkingv1.Ingress{
		ingress("a", "one", class("nginx")),
		ingress("a", "two", class("nginx"), created(time.Now())),
	}}, WorldOptions{})
	if w.UndatedIngresses != 1 {
		t.Errorf("UndatedIngresses = %d, want 1", w.UndatedIngresses)
	}
}

func TestRegexModeIsPerHostAcrossIngresses(t *testing.T) {
	w := NewWorld(Objects{Ingresses: []networkingv1.Ingress{
		ingress("a", "regex", class("nginx"), nginx("use-regex", "true"),
			rule("shared.example.com", path("/a/[0-9]+", networkingv1.PathTypeImplementationSpecific, "a"))),
		ingress("a", "bystander", class("nginx"),
			rule("shared.example.com", path("/b", networkingv1.PathTypeExact, "b")),
			rule("alone.example.com", path("/b", networkingv1.PathTypeExact, "b"))),
		ingress("a", "rewrite", class("nginx"), nginx("rewrite-target", "/"),
			rule("rewrite.example.com", path("/c", networkingv1.PathTypePrefix, "c"))),
		ingress("a", "disabled", class("nginx"), nginx("use-regex", "false"),
			rule("off.example.com", path("/d", networkingv1.PathTypePrefix, "d"))),
		ingress("a", "canary", class("nginx"), nginx("canary", "true"), nginx("use-regex", "true"),
			rule("canary.example.com", path("/e", networkingv1.PathTypePrefix, "e"))),
	}}, WorldOptions{})

	want := map[string]bool{
		"shared.example.com":  true,
		"alone.example.com":   false,
		"rewrite.example.com": true,
		"off.example.com":     false,
		"canary.example.com":  false, // a canary Ingress does not define locations
	}
	for name, regex := range want {
		if got := hostNamed(t, w, name).RegexMode(); got != regex {
			t.Errorf("%s: RegexMode = %v, want %v", name, got, regex)
		}
	}

	shared := hostNamed(t, w, "shared.example.com")
	if len(shared.Paths) != 2 {
		t.Fatalf("shared host has %d paths, want 2 (one from each Ingress)", len(shared.Paths))
	}
	wantTrigger := []RegexTrigger{{Ingress: ObjectRef{Kind: "Ingress", Namespace: "a", Name: "regex"}, Annotation: "use-regex"}}
	if diff := cmp.Diff(wantTrigger, shared.RegexTriggers); diff != "" {
		t.Errorf("triggers (-want +got):\n%s", diff)
	}
	if !hostNamed(t, w, "canary.example.com").Paths[0].Canary {
		t.Error("path of a canary Ingress is not marked as canary")
	}
}

func TestHostDetails(t *testing.T) {
	implicit := networkingv1.HTTPIngressPath{ // no pathType, empty path
		Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
			Name: "web", Port: networkingv1.ServiceBackendPort{Name: "http"},
		}},
	}
	w := NewWorld(Objects{Ingresses: []networkingv1.Ingress{
		ingress("a", "tls", class("nginx"), nginx("ssl-redirect", "false"),
			tls("secure.example.com", "cert-only.example.com"),
			rule("secure.example.com", path("/", networkingv1.PathTypePrefix, "web")),
			rule("", implicit)),
	}}, WorldOptions{})

	if !hostNamed(t, w, "secure.example.com").TLS {
		t.Error("host listed under spec.tls is not marked TLS")
	}
	for _, h := range w.Hosts {
		if h.Name == "cert-only.example.com" {
			t.Error("a host that only appears under spec.tls is listed, but it routes nothing")
		}
	}

	catchAll := hostNamed(t, w, "")
	if catchAll.DisplayName() != "*" {
		t.Errorf("DisplayName of the catch-all host = %q, want *", catchAll.DisplayName())
	}
	p := catchAll.Paths[0]
	if p.EffectivePath() != "/" {
		t.Errorf("EffectivePath of an empty path = %q, want /", p.EffectivePath())
	}
	if p.PathType != networkingv1.PathTypeImplementationSpecific {
		t.Errorf("PathType of a path with none = %q, want ImplementationSpecific", p.PathType)
	}
	if got := p.Backend.String(); got != "a/web:http" {
		t.Errorf("Backend = %q, want a/web:http", got)
	}
	if v, ok := p.Annotation("ssl-redirect"); !ok || v != "false" {
		t.Errorf(`Annotation("ssl-redirect") = %q, %v; want "false", true`, v, ok)
	}
	if p.AnnotationBool("ssl-redirect") || p.AnnotationBool("missing") {
		t.Error("AnnotationBool is true for a false or missing annotation")
	}
}

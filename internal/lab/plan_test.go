package lab

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/source"
)

func world(t *testing.T, manifests string, opts model.WorldOptions) *model.World {
	t.Helper()
	res, err := source.Load(strings.NewReader(manifests), "test")
	if err != nil {
		t.Fatal(err)
	}
	return model.NewWorld(res.Objects, opts)
}

func plan(t *testing.T, manifests string) *Plan {
	t.Helper()
	p, err := BuildPlan(world(t, manifests, model.WorldOptions{}), "echo:test")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

const backendsManifests = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: shop
  namespace: shop
spec:
  ingressClassName: nginx
  defaultBackend:
    service: {name: fallback, port: {number: 80}}
  rules:
  - host: shop.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service: {name: web, port: {number: 80}}
      - path: /metrics
        pathType: Exact
        backend:
          service: {name: web, port: {name: metrics}}
      - path: /api
        pathType: Prefix
        backend:
          service: {name: api, port: {name: http}}
      - path: /again
        pathType: Prefix
        backend:
          service: {name: web, port: {number: 80}}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: blog
  namespace: blog
spec:
  ingressClassName: nginx
  rules:
  - host: blog.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service: {name: web, port: {number: 8080}}
---
# The original Service tells the lab which number the named port has.
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
spec:
  ports:
  - {name: http, port: 80}
  - {name: metrics, port: 9102}
`

func TestBuildPlanReplacesEveryServiceWithTheEcho(t *testing.T) {
	p := plan(t, backendsManifests)

	var namespaces []string
	deployments := map[string]*appsv1.Deployment{}
	services := map[string]*corev1.Service{}
	for _, obj := range p.Support {
		switch o := obj.(type) {
		case *corev1.Namespace:
			namespaces = append(namespaces, o.Name)
		case *appsv1.Deployment:
			deployments[o.Namespace] = o
		case *corev1.Service:
			services[o.Namespace+"/"+o.Name] = o
		}
	}
	if diff := cmp.Diff([]string{"blog", "shop"}, namespaces); diff != "" {
		t.Errorf("namespaces (-want +got):\n%s", diff)
	}

	// One echo per namespace, listening on one port per (Service, port) the
	// Ingresses route to. The same pair used twice gets one listener.
	wantArgs := []string{
		"--admin", ":9090",
		"--listen", "shop/api:http=:8000",
		"--listen", "shop/fallback:80=:8001",
		"--listen", "shop/web:80=:8002",
		"--listen", "shop/web:metrics=:8003",
	}
	shop := deployments["shop"]
	if shop == nil {
		t.Fatal("no echo Deployment in namespace shop")
	}
	if diff := cmp.Diff(wantArgs, shop.Spec.Template.Spec.Containers[0].Args); diff != "" {
		t.Errorf("echo arguments in shop (-want +got):\n%s", diff)
	}
	if got := shop.Spec.Template.Spec.Containers[0].Image; got != "echo:test" {
		t.Errorf("echo image = %q", got)
	}
	if deployments["blog"] == nil {
		t.Error("no echo Deployment in namespace blog")
	}

	type port struct {
		Name   string
		Port   int32
		Target int32
	}
	ports := func(key string) []port {
		svc := services[key]
		if svc == nil {
			t.Fatalf("no Service %s", key)
		}
		if svc.Spec.Selector["app.kubernetes.io/name"] != echoName {
			t.Errorf("Service %s does not select the echo", key)
		}
		var out []port
		for _, p := range svc.Spec.Ports {
			out = append(out, port{p.Name, p.Port, p.TargetPort.IntVal})
		}
		return out
	}
	// A numbered backend port keeps its number. A named one keeps its name,
	// with the number from the original Service when the manifests have it.
	if diff := cmp.Diff([]port{{"p80", 80, 8002}, {"metrics", 9102, 8003}}, ports("shop/web")); diff != "" {
		t.Errorf("shop/web ports (-want +got):\n%s", diff)
	}
	// Without the original Service any number will do; the name is what the
	// Ingress resolves.
	if diff := cmp.Diff([]port{{"http", 8000, 8000}}, ports("shop/api")); diff != "" {
		t.Errorf("shop/api ports (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]port{{"p8080", 8080, 8000}}, ports("blog/web")); diff != "" {
		t.Errorf("blog/web ports (-want +got):\n%s", diff)
	}
	if len(p.EchoDeployments) != 2 {
		t.Errorf("EchoDeployments = %v, want one per namespace", p.EchoDeployments)
	}
	if len(p.Unverified) != 0 {
		t.Errorf("unexpected unverified entries: %+v", p.Unverified)
	}
}

func TestBuildPlanIsDeterministic(t *testing.T) {
	// Echo ports are assigned in sorted order, so two runs lay the backends
	// out identically and a kept cluster is not reshuffled on the next run.
	a, b := plan(t, backendsManifests), plan(t, backendsManifests)
	args := func(p *Plan) [][]string {
		var out [][]string
		for _, obj := range p.Support {
			if d, ok := obj.(*appsv1.Deployment); ok {
				out = append(out, d.Spec.Template.Spec.Containers[0].Args)
			}
		}
		return out
	}
	if diff := cmp.Diff(args(a), args(b)); diff != "" {
		t.Errorf("two plans differ (-first +second):\n%s", diff)
	}
}

const annotatedIngress = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: admin
  namespace: demo
  resourceVersion: "12345"
  uid: 0b0b0b0b-0000-0000-0000-000000000000
  creationTimestamp: "2024-05-01T10:00:00Z"
  generation: 7
  labels: {team: platform}
  annotations:
    kubernetes.io/ingress.class: nginx-internal
    kubectl.kubernetes.io/last-applied-configuration: '{"big":"blob"}'
    example.com/owner: platform
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$1
    nginx.ingress.kubernetes.io/upstream-vhost: internal.svc
    nginx.ingress.kubernetes.io/backend-protocol: HTTP
    nginx.ingress.kubernetes.io/auth-url: https://auth.example.com/verify
    nginx.ingress.kubernetes.io/auth-signin: https://auth.example.com/start
    nginx.ingress.kubernetes.io/whitelist-source-range: 10.0.0.0/8
    nginx.ingress.kubernetes.io/limit-rps: "5"
    nginx.ingress.kubernetes.io/configuration-snippet: 'more_set_headers "X: y";'
    nginx.ingress.kubernetes.io/proxy-ssl-secret: demo/ca
spec:
  rules:
  - host: admin.example.com
    http:
      paths:
      - path: /svc/(.*)
        pathType: ImplementationSpecific
        backend:
          service: {name: admin, port: {number: 8080}}
status:
  loadBalancer:
    ingress: [{ip: 203.0.113.9}]
`

func TestLabIngress(t *testing.T) {
	w := world(t, annotatedIngress, model.WorldOptions{IngressClass: "nginx-internal"})
	p, err := BuildPlan(w, "echo:test")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Ingresses) != 1 {
		t.Fatalf("got %d Ingresses, want 1", len(p.Ingresses))
	}
	ing := p.Ingresses[0]

	t.Run("routing annotations are kept, the rest of the list is removed", func(t *testing.T) {
		want := map[string]string{
			"example.com/owner":                            "platform",
			"nginx.ingress.kubernetes.io/use-regex":        "true",
			"nginx.ingress.kubernetes.io/rewrite-target":   "/$1",
			"nginx.ingress.kubernetes.io/upstream-vhost":   "internal.svc",
			"nginx.ingress.kubernetes.io/backend-protocol": "HTTP",
		}
		if diff := cmp.Diff(want, ing.Annotations); diff != "" {
			t.Errorf("annotations (-want +got):\n%s", diff)
		}
	})

	t.Run("everything removed is reported, with a reason", func(t *testing.T) {
		got := map[string]model.UnverifiedReason{}
		for _, u := range p.Unverified {
			if u.Object == nil || u.Object.Name != "admin" || u.Detail == "" {
				t.Errorf("unverified entry lacks its Ingress or a detail: %+v", u)
			}
			got[strings.TrimPrefix(u.Feature, model.NGINXAnnotationPrefix)] = u.Reason
		}
		want := map[string]model.UnverifiedReason{
			"auth-url":               model.ReasonBackendDependent,
			"auth-signin":            model.ReasonBackendDependent,
			"whitelist-source-range": model.ReasonClientIdentity,
			"limit-rps":              model.ReasonUnsupported,
			"configuration-snippet":  model.ReasonUnsupported,
			"proxy-ssl-secret":       model.ReasonBackendDependent,
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("unverified (-want +got):\n%s", diff)
		}
	})

	t.Run("cluster-specific metadata is dropped and the class is the lab's", func(t *testing.T) {
		if ing.ResourceVersion != "" || ing.UID != "" || ing.Generation != 0 || !ing.CreationTimestamp.IsZero() {
			t.Errorf("metadata from the export survived: %+v", ing.ObjectMeta)
		}
		if len(ing.Status.LoadBalancer.Ingress) != 0 {
			t.Error("status from the export survived")
		}
		if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
			t.Errorf("ingressClassName = %v, want nginx whatever the source class was", ing.Spec.IngressClassName)
		}
		if ing.Labels["team"] != "platform" || ing.Namespace != "demo" || ing.Name != "admin" {
			t.Errorf("identity or labels changed: %+v", ing.ObjectMeta)
		}
		if ing.Spec.Rules[0].HTTP.Paths[0].Path != "/svc/(.*)" {
			t.Error("the rules changed")
		}
	})

	t.Run("the source Ingress is left untouched", func(t *testing.T) {
		if _, ok := w.Ingresses[0].Annotations["nginx.ingress.kubernetes.io/auth-url"]; !ok {
			t.Error("BuildPlan removed an annotation from the World's own Ingress")
		}
	})
}

func TestBackendProtocolOtherThanHTTPIsRemoved(t *testing.T) {
	manifest := strings.Replace(annotatedIngress, "backend-protocol: HTTP", "backend-protocol: GRPC", 1)
	p, err := BuildPlan(world(t, manifest, model.WorldOptions{IngressClass: "nginx-internal"}), "echo:test")
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := p.Ingresses[0].Annotations["nginx.ingress.kubernetes.io/backend-protocol"]; kept {
		t.Error("backend-protocol: GRPC was kept, but the echo only speaks HTTP")
	}
}

func TestBuildPlanGeneratesTLSSecrets(t *testing.T) {
	p := plan(t, `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: secure
  namespace: demo
spec:
  ingressClassName: nginx
  tls:
  - hosts: [a.example.com, b.example.com]
    secretName: site-cert
  rules:
  - host: a.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service: {name: web, port: {number: 80}}
`)
	var secret *corev1.Secret
	for _, obj := range p.Support {
		if s, ok := obj.(*corev1.Secret); ok {
			secret = s
		}
	}
	if secret == nil {
		t.Fatal("no Secret was generated for the Ingress's TLS section")
	}
	if secret.Namespace != "demo" || secret.Name != "site-cert" || secret.Type != corev1.SecretTypeTLS {
		t.Errorf("secret = %s/%s of type %s", secret.Namespace, secret.Name, secret.Type)
	}
	// The pair has to be one a server can actually load, or ingress-nginx
	// falls back to its default certificate and the host is not TLS at all.
	if _, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]); err != nil {
		t.Fatalf("the generated certificate and key do not form a pair: %v", err)
	}
	block, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"a.example.com", "b.example.com"}, cert.DNSNames); diff != "" {
		t.Errorf("certificate names (-want +got):\n%s", diff)
	}
}

func TestBuildPlanReportsBackendsItCannotReplace(t *testing.T) {
	p := plan(t, `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: bucket
  namespace: demo
spec:
  ingressClassName: nginx
  rules:
  - host: files.example.com
    http:
      paths:
      - path: /assets
        pathType: Prefix
        backend:
          resource: {apiGroup: storage.example.com, kind: Bucket, name: assets}
`)
	if len(p.EchoDeployments) != 0 {
		t.Errorf("an echo was planned for a resource backend: %v", p.EchoDeployments)
	}
	if len(p.Unverified) != 1 || p.Unverified[0].Reason != model.ReasonUnsupported || !strings.Contains(p.Unverified[0].Feature, "Bucket") {
		t.Errorf("unverified = %+v, want one entry naming the Bucket backend", p.Unverified)
	}
}

func TestSentinel(t *testing.T) {
	p := plan(t, backendsManifests)
	first, hostA := p.Sentinel("aaaa")
	second, hostB := p.Sentinel("bbbb")
	if first == nil || second == nil {
		t.Fatal("no sentinel for a plan that has echo Services")
	}
	// A host per run is what makes the sentinel evidence of this run's
	// configuration and not of an earlier one.
	if hostA == hostB || !strings.HasSuffix(hostA, SentinelHostSuffix) || first.Spec.Rules[0].Host != hostA {
		t.Errorf("hosts = %q and %q", hostA, hostB)
	}
	// The same object name every run, so that the previous run's sentinel is
	// replaced rather than accumulated.
	if first.Name != second.Name || first.Namespace != second.Namespace {
		t.Errorf("sentinel names differ between runs: %s/%s and %s/%s", first.Namespace, first.Name, second.Namespace, second.Name)
	}
	backend := first.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if first.Namespace != "blog" || backend.Name != "web" || backend.Port.Number != 8080 {
		t.Errorf("sentinel routes to %s/%s:%d, want an echo Service that the plan creates", first.Namespace, backend.Name, backend.Port.Number)
	}
	if *first.Spec.IngressClassName != "nginx" {
		t.Errorf("sentinel class = %q", *first.Spec.IngressClassName)
	}

	empty := &Plan{}
	if ing, _ := empty.Sentinel("x"); ing != nil {
		t.Error("a plan with no echo Service returned a sentinel")
	}
}

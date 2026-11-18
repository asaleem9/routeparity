package lab

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/echo"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/prober"
)

func TestImplementationsInstallTheirOwnClass(t *testing.T) {
	classes := map[string]bool{}
	for _, name := range ImplementationNames() {
		t.Run(name, func(t *testing.T) {
			impl, _ := ImplementationByName(name)
			if err := impl.validate(); err != nil {
				t.Fatal(err)
			}
			if classes[impl.ClassName] {
				t.Fatalf("class %s is shared: controllerName is immutable", impl.ClassName)
			}
			classes[impl.ClassName] = true
			data, err := impl.manifest()
			if err != nil {
				t.Fatal(err)
			}
			objs, err := decodeObjects(data)
			if err != nil {
				t.Fatal(err)
			}
			var deployment, crds bool
			for _, obj := range objs {
				switch obj.GetKind() {
				case "CustomResourceDefinition":
					crds = true
				case "Deployment":
					if obj.GetName() == impl.controller.Name && obj.GetNamespace() == impl.controller.Namespace {
						deployment = true
						if impl.Name == NGINXGatewayFabric.Name && !strings.Contains(string(data), "--gatewayclass="+impl.ClassName) {
							t.Error("controller watches a different GatewayClass")
						}
					}
				case "GatewayClass":
					if impl.perGateway && obj.GetName() != impl.ClassName {
						t.Errorf("installed class %s, want %s", obj.GetName(), impl.ClassName)
					}
				case "NginxProxy":
					serviceType, _, _ := unstructured.NestedString(obj.Object, "spec", "kubernetes", "service", "type")
					if serviceType != "ClusterIP" {
						t.Errorf("lab proxy service type = %s", serviceType)
					}
					if _, found, _ := unstructured.NestedString(obj.Object, "spec", "kubernetes", "service", "externalTrafficPolicy"); found {
						t.Error("ClusterIP has an external traffic policy")
					}
				}
			}
			if !deployment || !crds {
				t.Errorf("manifest has controller=%v, CRDs=%v", deployment, crds)
			}
		})
	}
}

func TestGatewayProbeGroups(t *testing.T) {
	c := &Candidate{}
	for _, ns := range []string{"a", "b"} {
		c.Gateways = append(c.Gateways, gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway"},
			Spec:       gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}}},
		})
		c.HTTPRoutes = append(c.HTTPRoutes, gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "route"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gateway"}}},
				Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(ns + ".example.com")},
			},
		})
	}
	probes := []model.Probe{{Scheme: "http", Host: "a.example.com", RawTarget: "/"}, {Scheme: "http", Host: "b.example.com", RawTarget: "//app"}}
	groups, err := gatewayProbeGroups(c, probes)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups[0]) != 1 || groups[0][0].Host != "a.example.com" || len(groups[1]) != 1 || groups[1][0].Host != "b.example.com" {
		t.Fatalf("probes assigned to wrong proxy: %+v", groups)
	}
	// A shared hostname on independent proxies must never pick the first one.
	c.HTTPRoutes[1].Spec.Hostnames = c.HTTPRoutes[0].Spec.Hostnames
	if _, err := gatewayProbeGroups(c, probes[:1]); err == nil {
		t.Error("ambiguous proxy was accepted")
	}
	// Listener section names must participate in parent selection.
	section := gatewayv1.SectionName("absent")
	c.HTTPRoutes[0].Spec.ParentRefs[0].SectionName = &section
	groups, err = gatewayProbeGroups(c, probes[:1])
	if err != nil || len(groups[1]) != 1 {
		t.Errorf("sectionName was ignored: %+v, %v", groups, err)
	}
}

func TestWaitForProxyRefusesAmbiguousOrSelectorlessServices(t *testing.T) {
	impl := EnvoyGateway
	impl.proxySelector = "test=proxy"
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			client := fake.NewClientset()
			for i := 0; i < count; i++ {
				_, err := client.CoreV1().Services(impl.proxyNamespace).Create(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("proxy-%d", i), Labels: map[string]string{"test": "proxy"}}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			s := session{k: &kube{client: client}}
			if _, err := s.waitForProxy(context.Background(), impl); err == nil {
				t.Fatal("unsafe Service selection was accepted")
			}
		})
	}
}

func TestClearGatewayEntryPreservesBaselineAndUnmanagedServices(t *testing.T) {
	client := fake.NewClientset()
	for _, item := range []struct {
		ns      string
		managed bool
	}{{ingressNGINXNamespace, true}, {"candidate", true}, {"other", false}, {"echo", true}} {
		labels := map[string]string{}
		if item.managed {
			labels[managedByLabel] = managedByValue
		}
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "routeparity-entry", Labels: labels}}
		if item.ns != "echo" {
			svc.Spec.Ports = []corev1.ServicePort{{Port: 80, NodePort: nodePortGatewayHTTP}}
		}
		_, err := client.CoreV1().Services(item.ns).Create(context.Background(), svc, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	s := session{k: &kube{client: client}}
	if err := s.clearGatewayEntry(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []types.NamespacedName{{Namespace: ingressNGINXNamespace, Name: "routeparity-entry"}, {Namespace: "other", Name: "routeparity-entry"}, {Namespace: "echo", Name: "routeparity-entry"}} {
		if _, err := client.CoreV1().Services(key.Namespace).Get(context.Background(), key.Name, metav1.GetOptions{}); err != nil {
			t.Errorf("removed unrelated Service %s: %v", key, err)
		}
	}
	if _, err := client.CoreV1().Services("candidate").Get(context.Background(), "routeparity-entry", metav1.GetOptions{}); err == nil {
		t.Error("candidate NodePorts were not released")
	}
}

func TestGatewaySentinelPreservesHTTPSOnlyTransport(t *testing.T) {
	gw := gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "secure", Namespace: "demo"},
		Spec:       gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, TLS: &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "certificate"}}}}}},
	}
	l, err := gatewaySentinelListener(gw, "readiness.routeparity.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if l.Port != 443 || l.Protocol != gatewayv1.HTTPSProtocolType || l.TLS == nil || l.TLS.CertificateRefs[0].Name != "certificate" {
		t.Fatalf("readiness changed the Gateway's transport: %+v", l)
	}
	l.TLS.CertificateRefs[0].Name = "changed"
	if gw.Spec.Listeners[0].TLS.CertificateRefs[0].Name != "certificate" {
		t.Fatal("sentinel mutated the candidate's certificate")
	}
	gw.Spec.Listeners[0].Port = 8443
	if _, err := gatewaySentinelListener(gw, "readiness.routeparity.invalid"); err == nil {
		t.Fatal("unsupported data port was silently replaced by port 80")
	}
}

func TestSentinelOverHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(echo.Handler(echo.Identity{Namespace: "test", Service: "sentinel", Port: "80"}, "pod"))
	defer server.Close()
	target := prober.Target{HTTPS: strings.TrimPrefix(server.URL, "https://")}
	if err := waitForSentinel(context.Background(), "secure", target, "readiness.routeparity.invalid", prober.Options{}); err != nil {
		t.Fatal(err)
	}
}

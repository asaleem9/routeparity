package convert

import (
	"context"
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const regexIngress = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: regex
  namespace: shop
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
spec:
  ingressClassName: nginx
  rules:
  - host: regex.example.com
    http:
      paths:
      - path: "/[A-Z]{3}"
        pathType: ImplementationSpecific
        backend:
          service:
            name: httpbin
            port:
              number: 8000
`

func TestInProcessConvertsRegexHost(t *testing.T) {
	out, err := InProcess{}.Convert(context.Background(), strings.NewReader(regexIngress), Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(out.Gateways) != 1 {
		t.Fatalf("got %d Gateways, want 1", len(out.Gateways))
	}
	if len(out.HTTPRoutes) == 0 {
		t.Fatal("got no HTTPRoutes")
	}
	if out.Gateways[0].Kind != "Gateway" || out.HTTPRoutes[0].Kind != "HTTPRoute" {
		t.Errorf("output objects are missing their kind: %q, %q", out.Gateways[0].Kind, out.HTTPRoutes[0].Kind)
	}

	// The converter is expected to carry ingress-nginx's regex semantics over
	// by making the pattern case-insensitive and open-ended. If this stops
	// holding after a dependency bump, the audit rules that describe the
	// converter's behaviour need revisiting.
	var patterns []string
	for _, route := range out.HTTPRoutes {
		for _, rule := range route.Spec.Rules {
			for _, m := range rule.Matches {
				if m.Path != nil && m.Path.Type != nil && *m.Path.Type == gatewayv1.PathMatchRegularExpression {
					patterns = append(patterns, *m.Path.Value)
				}
			}
		}
	}
	want := "(?i)/[A-Z]{3}.*"
	found := false
	for _, p := range patterns {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Errorf("regex path matches = %q, want one equal to %q", patterns, want)
	}
}

func TestInProcessSelectsOnlyTheRequestedClass(t *testing.T) {
	out, err := InProcess{}.Convert(context.Background(), strings.NewReader(regexIngress), Options{IngressClass: "other"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(out.HTTPRoutes) != 0 {
		t.Errorf("got %d HTTPRoutes for a class no Ingress uses, want 0", len(out.HTTPRoutes))
	}
}

func TestInProcessReportsUnconvertedAnnotations(t *testing.T) {
	manifest := strings.Replace(regexIngress,
		`nginx.ingress.kubernetes.io/use-regex: "true"`,
		`nginx.ingress.kubernetes.io/proxy-redirect-from: "http://old/"`, 1)
	out, err := InProcess{}.Convert(context.Background(), strings.NewReader(manifest), Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, n := range out.Notices {
		if strings.Contains(n.Message, "proxy-redirect-from") {
			return
		}
	}
	t.Errorf("no notice mentions proxy-redirect-from; notices: %+v", out.Notices)
}

func TestInProcessReportsConverterVersion(t *testing.T) {
	out, err := InProcess{}.Convert(context.Background(), strings.NewReader(regexIngress), Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.HasPrefix(out.Converter, i2gwModule+"@") {
		t.Errorf("Converter = %q, want prefix %q", out.Converter, i2gwModule+"@")
	}
}

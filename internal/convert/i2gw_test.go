package convert

import (
	"context"
	"os"
	"regexp"
	"runtime/debug"
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
	// Whether or not this test binary carries dependency information (it
	// depends on the Go release), the answer is the version in go.mod.
	if want := i2gwModule + "@" + i2gwPinnedVersion; out.Converter != want {
		t.Errorf("Converter = %q, want %q", out.Converter, want)
	}
}

// TestPinnedVersionMatchesGoMod fails when the dependency is upgraded without
// updating the fallback constant, which would make reports name the wrong
// converter version.
func TestPinnedVersionMatchesGoMod(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(i2gwModule) + ` (\S+)`).FindSubmatch(gomod)
	if m == nil {
		t.Fatalf("go.mod does not require %s", i2gwModule)
	}
	if got := string(m[1]); got != i2gwPinnedVersion {
		t.Errorf("go.mod requires %s %s but i2gwPinnedVersion is %s; update the constant", i2gwModule, got, i2gwPinnedVersion)
	}
}

func TestVersionFromDeps(t *testing.T) {
	other := &debug.Module{Path: "example.com/other", Version: "v9.9.9"}
	tests := []struct {
		name string
		deps []*debug.Module
		want string
	}{
		{"no dependency information", nil, ""},
		{"not listed", []*debug.Module{other}, ""},
		{"listed", []*debug.Module{other, {Path: i2gwModule, Version: "v1.2.0"}}, "v1.2.0"},
		{"replaced by another version", []*debug.Module{{Path: i2gwModule, Version: "v1.2.0",
			Replace: &debug.Module{Path: "example.com/fork", Version: "v1.2.1-fork"}}}, "v1.2.1-fork"},
		{"replaced by a local directory", []*debug.Module{{Path: i2gwModule, Version: "v1.2.0",
			Replace: &debug.Module{Path: "../ingress2gateway"}}}, "v1.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionFromDeps(tt.deps); got != tt.want {
				t.Errorf("versionFromDeps = %q, want %q", got, tt.want)
			}
		})
	}
}

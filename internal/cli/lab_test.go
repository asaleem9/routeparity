package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/source"
)

func TestDefaultEchoImage(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		// A release uses the image published for exactly that release.
		{"v0.1.0", "ghcr.io/asaleem9/routeparity-echo:v0.1.0"},
		{"v12.34.56", "ghcr.io/asaleem9/routeparity-echo:v12.34.56"},
		// Anything else has no published image to match, so it uses the one
		// built from the same source tree.
		{"dev", "routeparity-echo:dev"},
		{"", "routeparity-echo:dev"},
		{"240afc6-dirty", "routeparity-echo:dev"},
		{"v0.1.0-3-g240afc6", "routeparity-echo:dev"},
		{"v0.1.0-dirty", "routeparity-echo:dev"},
		{"v0.1.0-rc1", "routeparity-echo:dev"},
	}
	for _, tt := range tests {
		if got := defaultEchoImage(tt.version); got != tt.want {
			t.Errorf("defaultEchoImage(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

// These fail before the lab starts anything, so they need no Docker.
func TestLabRecordRejectsBadInputBeforeStartingACluster(t *testing.T) {
	out := filepath.Join(t.TempDir(), "snap.json")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no output file", []string{"lab", "record", "-f", exampleDir}, `required flag(s) "output" not set`},
		{"no manifests", []string{"lab", "record", "-o", out}, `required flag(s) "filename" not set`},
		{"zero samples", []string{"lab", "record", "-f", exampleDir, "-o", out, "--samples", "0"}, "--samples must be at least 1"},
		{"zero concurrency", []string{"lab", "record", "-f", exampleDir, "-o", out, "--concurrency", "0"}, "--concurrency must be at least 1"},
		{"no Ingress of the class", []string{"lab", "record", "-f", exampleDir, "-o", out, "--ingress-class", "traefik"}, "no Ingress of class"},
		{"missing manifests", []string{"lab", "record", "-f", "does-not-exist", "-o", out}, "does-not-exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := run(t, "", tt.args...)
			if got.code != ExitError || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("exit %d, stderr %q; want exit %d mentioning %q", got.code, got.stderr, ExitError, tt.want)
			}
		})
	}
}

func TestLabHelp(t *testing.T) {
	got := run(t, "", "lab", "--help")
	if got.code != ExitOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{"run", "record", "down", "Nothing is sent to"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("lab --help lacks %q:\n%s", want, got.stdout)
		}
	}
	// The lab's cluster serves nobody else, so its defaults are not the
	// cautious ones `live` uses against an endpoint.
	labHelp, liveHelp := run(t, "", "lab", "record", "--help").stdout, run(t, "", "live", "--help").stdout
	if !strings.Contains(liveHelp, "0 removes the limit (default 20)") || !strings.Contains(liveHelp, "probes in flight at once (default 4)") {
		t.Errorf("live no longer has its cautious defaults:\n%s", liveHelp)
	}
	if strings.Contains(labHelp, "(default 20)") || !strings.Contains(labHelp, "probes in flight at once (default 8)") {
		t.Errorf("lab record does not default to 8 at a time with no rate limit:\n%s", labHelp)
	}
}

const routeManifest = `
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: gw, namespace: demo}
spec:
  gatewayClassName: anything
  listeners: [{name: http, port: 80, protocol: HTTP}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: handwritten, namespace: demo}
spec:
  parentRefs: [{name: gw}]
  hostnames: [regex-match.example.com]
  rules:
  - backendRefs: [{name: httpbin, port: 8000}]
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata: {name: grant, namespace: demo}
spec:
  from: [{group: gateway.networking.k8s.io, kind: HTTPRoute, namespace: other}]
  to: [{group: "", kind: Service}]
`

func exampleWorld(t *testing.T, extra ...string) *model.World {
	t.Helper()
	paths := append([]string{exampleDir}, extra...)
	loaded, err := source.LoadPaths(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	return model.NewWorld(loaded.Objects, model.WorldOptions{})
}

func TestResolveCandidate(t *testing.T) {
	routes := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(routes, []byte(routeManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	streams := Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}

	t.Run("--gateway files win", func(t *testing.T) {
		c, err := resolveCandidate(context.Background(), streams, exampleWorld(t), []string{routes}, "standard")
		if err != nil {
			t.Fatal(err)
		}
		if len(c.HTTPRoutes) != 1 || c.HTTPRoutes[0].Name != "handwritten" || len(c.Gateways) != 1 {
			t.Errorf("candidate has %d Gateways and %d HTTPRoutes, want the ones in the file", len(c.Gateways), len(c.HTTPRoutes))
		}
		// Objects the tool has no type for still belong to the configuration.
		if len(c.Other) != 1 || c.Other[0].GetKind() != "ReferenceGrant" {
			t.Errorf("Other = %v, want the ReferenceGrant carried along", c.Other)
		}
		if c.Source != "the --gateway manifests" || len(c.Unverified) != 0 {
			t.Errorf("source %q, unverified %v", c.Source, c.Unverified)
		}
	})

	t.Run("Gateway objects among the -f manifests are used", func(t *testing.T) {
		c, err := resolveCandidate(context.Background(), streams, exampleWorld(t, routes), nil, "standard")
		if err != nil {
			t.Fatal(err)
		}
		if len(c.HTTPRoutes) != 1 || c.HTTPRoutes[0].Name != "handwritten" {
			t.Errorf("candidate routes = %d, want the hand-written one and no conversion", len(c.HTTPRoutes))
		}
	})

	t.Run("otherwise the Ingresses are converted", func(t *testing.T) {
		c, err := resolveCandidate(context.Background(), streams, exampleWorld(t), nil, "standard")
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Gateways) == 0 || len(c.HTTPRoutes) == 0 {
			t.Fatalf("conversion produced %d Gateways and %d HTTPRoutes", len(c.Gateways), len(c.HTTPRoutes))
		}
		if !strings.Contains(c.Source, "ingress2gateway@v") || !strings.Contains(c.Source, "standard emitter") {
			t.Errorf("source = %q, want the converter, its version and the emitter", c.Source)
		}
		// What the converter did not translate is missing from what gets
		// tested, and the run has to say so.
		var notConverted []string
		for _, u := range c.Unverified {
			if u.Reason != model.ReasonNotConverted {
				t.Errorf("unverified entry has reason %q", u.Reason)
			}
			notConverted = append(notConverted, u.Feature)
		}
		all := strings.Join(notConverted, "\n")
		for _, want := range []string{"auth-url", "limit-rps"} {
			if !strings.Contains(all, want) {
				t.Errorf("the untranslated annotation %s is not listed:\n%s", want, all)
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.yaml")
		if err := os.WriteFile(empty, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveCandidate(context.Background(), streams, exampleWorld(t), []string{empty}, "standard"); err == nil ||
			!strings.Contains(err.Error(), "no Gateway and no HTTPRoute") {
			t.Errorf("--gateway file without routes: err = %v", err)
		}
		if _, err := resolveCandidate(context.Background(), streams, exampleWorld(t), []string{"missing.yaml"}, "standard"); err == nil {
			t.Error("missing --gateway file: no error")
		}
		if _, err := resolveCandidate(context.Background(), streams, exampleWorld(t), nil, "no-such-emitter"); err == nil ||
			!strings.Contains(err.Error(), "could not be converted") {
			t.Errorf("unknown emitter: err = %v", err)
		}
	})
}

func TestLabRunRejectsBadInputBeforeStartingACluster(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown implementation", []string{"lab", "run", "-f", exampleDir, "--implementation", "traefik"}, "the lab can run envoy-gateway"},
		{"unknown output", []string{"lab", "run", "-f", exampleDir, "-o", "yaml"}, "unknown output format"},
		{"zero samples", []string{"lab", "run", "-f", exampleDir, "--samples", "0"}, "--samples must be at least 1"},
		{"missing gateway file", []string{"lab", "run", "-f", exampleDir, "--gateway", "does-not-exist.yaml"}, "does-not-exist.yaml"},
		{"no manifests", []string{"lab", "run"}, `required flag(s) "filename" not set`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := run(t, "", tt.args...)
			if got.code != ExitError || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("exit %d, stderr %q; want exit %d mentioning %q", got.code, got.stderr, ExitError, tt.want)
			}
		})
	}
}

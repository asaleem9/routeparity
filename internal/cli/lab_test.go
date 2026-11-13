package cli

import (
	"path/filepath"
	"strings"
	"testing"
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
	for _, want := range []string{"record", "down", "Nothing is sent to"} {
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

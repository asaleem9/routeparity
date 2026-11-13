package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/echo"
	"github.com/asaleem9/routeparity/internal/model"
)

// Two Ingresses: one regex host and one host with a slash-terminated path.
const proxyManifests = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: regex
  namespace: demo
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
          service: {name: web, port: {number: 80}}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: slash
  namespace: demo
spec:
  ingressClassName: nginx
  rules:
  - host: slash.example.com
    http:
      paths:
      - path: "/app/"
        pathType: Prefix
        backend:
          service: {name: web, port: {number: 80}}
`

var backendEcho = echo.Handler(echo.Identity{Namespace: "demo", Service: "web", Port: "80"}, "web-1")

// forward hands the request to the echo backend, optionally with a different
// request-target, the way a proxy that normalises paths would.
func forward(w http.ResponseWriter, r *http.Request, target string) {
	out := r.Clone(r.Context())
	out.RequestURI = target
	backendEcho.ServeHTTP(w, out)
}

// oldProxy imitates the ingress-nginx behaviours the manifests rely on:
// case-insensitive prefix regex matching, matching on the normalised path
// while forwarding the original, and the trailing-slash redirect.
func oldProxy() http.Handler {
	regex := regexp.MustCompile(`(?i)^/[A-Z]{3}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.RequestURI
		clean := path.Clean(r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/") && clean != "/" {
			clean += "/"
		}
		switch r.Host {
		case "regex.example.com":
			if regex.MatchString(clean) {
				forward(w, r, raw)
				return
			}
		case "slash.example.com":
			if strings.HasPrefix(clean, "/app/") {
				forward(w, r, raw)
				return
			}
			if clean == "/app" {
				w.Header().Set("Location", "http://slash.example.com/app/")
				w.WriteHeader(http.StatusMovedPermanently)
				return
			}
		}
		w.Header().Set("Server", "old")
		http.Error(w, "old proxy: no route", http.StatusNotFound)
	})
}

// newProxy imitates a literal translation on an Envoy-based gateway: the
// regex must match the whole path with its case, there is no implicit
// redirect, and the backend receives the normalised path.
func newProxy() http.Handler {
	regex := regexp.MustCompile(`^/[A-Z]{3}$`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/") && clean != "/" {
			clean += "/"
		}
		switch r.Host {
		case "regex.example.com":
			if regex.MatchString(clean) {
				forward(w, r, clean)
				return
			}
		case "slash.example.com":
			if strings.HasPrefix(clean, "/app/") {
				forward(w, r, clean)
				return
			}
		}
		w.Header().Set("Server", "new")
		w.WriteHeader(http.StatusNotFound)
	})
}

func startProxies(t *testing.T) (oldAddr, newAddr, manifests string) {
	t.Helper()
	oldSrv, newSrv := httptest.NewServer(oldProxy()), httptest.NewServer(newProxy())
	t.Cleanup(oldSrv.Close)
	t.Cleanup(newSrv.Close)
	manifests = filepath.Join(t.TempDir(), "ingress.yaml")
	if err := os.WriteFile(manifests, []byte(proxyManifests), 0o644); err != nil {
		t.Fatal(err)
	}
	return oldSrv.Listener.Addr().String(), newSrv.Listener.Addr().String(), manifests
}

func decodeDiff(t *testing.T, out string) *diff.Result {
	t.Helper()
	var res diff.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not a diff result: %v\n%s", err, out)
	}
	return &res
}

// verdicts maps "host target" to the verdict for that probe.
func verdicts(res *diff.Result) map[string]model.Verdict {
	out := map[string]model.Verdict{}
	for _, c := range res.Comparisons {
		out[c.Probe.Host+" "+c.Probe.RawTarget] = c.Verdict
	}
	return out
}

func TestLiveFindsEachDivergenceBetweenTwoProxies(t *testing.T) {
	oldAddr, newAddr, manifests := startProxies(t)
	got := run(t, "", "live", "-f", manifests, "--baseline", oldAddr, "--candidate", newAddr,
		"--mutations", "--rate", "0", "-o", "json")
	if got.code != ExitFindings {
		t.Fatalf("exit code = %d, want %d\nstderr: %s\nstdout: %s", got.code, ExitFindings, got.stderr, got.stdout)
	}
	res := decodeDiff(t, got.stdout)

	want := map[string]model.Verdict{
		// The paths written in the manifests work on both.
		"regex.example.com /AAA":                            model.VerdictMatch,
		"slash.example.com /app/":                           model.VerdictMatch,
		"slash.example.com /app/rp-sub":                     model.VerdictMatch,
		"unrouted.routeparity.invalid /rp-unrouted-control": model.VerdictMatch,
		// NGX-REGEX-001: case-insensitive, prefix.
		"regex.example.com /aaa":          model.VerdictDiff,
		"regex.example.com /AAA/rp-extra": model.VerdictDiff,
		// NGX-SLASH-001: the implicit redirect.
		"slash.example.com /app": model.VerdictDiff,
		// NGX-NORM-001: same status, different path at the backend.
		"regex.example.com /rp-x/../AAA":  model.VerdictDiff,
		"regex.example.com //AAA":         model.VerdictDiff,
		"regex.example.com /./AAA":        model.VerdictDiff,
		"slash.example.com /rp-x/../app/": model.VerdictDiff,
		// Neither proxy treats this path case-insensitively.
		"slash.example.com /APP/": model.VerdictMatch,
	}
	gotVerdicts := verdicts(res)
	for probe, verdict := range want {
		if gotVerdicts[probe] != verdict {
			t.Errorf("%s: verdict = %q, want %s", probe, gotVerdicts[probe], verdict)
		}
	}

	// A difference has to say what differed, and be traceable to its rule.
	for _, c := range res.Comparisons {
		switch c.Probe.Host + " " + c.Probe.RawTarget {
		case "regex.example.com /aaa":
			if c.Probe.Origin.RuleID != "NGX-REGEX-001" {
				t.Errorf("/aaa is attributed to %q, want NGX-REGEX-001", c.Probe.Origin.RuleID)
			}
			if diff := cmp.Diff([]string{"routed", "client.status", "backend.reached"}, deltaFields(c)); diff != "" {
				t.Errorf("/aaa deltas (-want +got):\n%s", diff)
			}
		case "regex.example.com //AAA":
			want := []model.Delta{{Field: "backend.rawTarget", Baseline: "//AAA", Candidate: "/AAA"}}
			if diff := cmp.Diff(want, c.Deltas); diff != "" {
				t.Errorf("//AAA deltas (-want +got):\n%s", diff)
			}
		case "slash.example.com /app":
			if c.Probe.Origin.RuleID != "NGX-SLASH-001" {
				t.Errorf("/app is attributed to %q, want NGX-SLASH-001", c.Probe.Origin.RuleID)
			}
		}
	}

	if res.Summary.Probes != len(res.Comparisons) || res.Summary.Error != 0 || res.Summary.Unverified != 0 {
		t.Errorf("summary = %+v", res.Summary)
	}
	if res.Baseline.Mode != model.ModeLab {
		t.Errorf("baseline mode = %q, want lab: the echo backend reported its view", res.Baseline.Mode)
	}
}

func deltaFields(c model.Comparison) []string {
	var out []string
	for _, d := range c.Deltas {
		out = append(out, d.Field)
	}
	return out
}

func TestLiveWithoutMutationsOnlySendsConfiguredPaths(t *testing.T) {
	oldAddr, newAddr, manifests := startProxies(t)
	got := run(t, "", "live", "-f", manifests, "--baseline", oldAddr, "--candidate", newAddr, "--rate", "0", "-o", "json")
	// The configured paths behave alike, which is exactly why a migration
	// that only tests them looks fine.
	if got.code != ExitOK {
		t.Fatalf("exit code = %d, want %d\nstdout: %s", got.code, ExitOK, got.stdout)
	}
	res := decodeDiff(t, got.stdout)
	for _, c := range res.Comparisons {
		if c.Probe.Origin.Source == model.OriginMutation {
			t.Errorf("mutation probe %s sent without --mutations", c.Probe.RawTarget)
		}
	}
	if res.Summary.Diff != 0 || res.Summary.Match != res.Summary.Probes {
		t.Errorf("summary = %+v, want everything to match", res.Summary)
	}

	// A clean result from configured paths alone must not read as parity.
	text := run(t, "", "live", "-f", manifests, "--baseline", oldAddr, "--candidate", newAddr, "--rate", "0")
	if !strings.Contains(text.stdout, "Only paths written in the manifests were requested") || !strings.Contains(text.stdout, "--mutations") {
		t.Errorf("report does not say that only configured paths were tested:\n%s", text.stdout)
	}
}

func TestLiveTextReport(t *testing.T) {
	oldAddr, newAddr, manifests := startProxies(t)
	got := run(t, "", "live", "-f", manifests, "--baseline", oldAddr, "--candidate", newAddr, "--mutations", "--rate", "0")
	if got.code != ExitFindings {
		t.Fatalf("exit code = %d, want %d\n%s", got.code, ExitFindings, got.stderr)
	}
	for _, want := range []string{
		"DIFF",
		"GET http://regex.example.com/aaa",
		"NGX-REGEX-001, case-flip",
		"client.status",
		"200  →  404",
		"backend.rawTarget",
		"client.location",
		"Differences recorded and not counted",
		"The candidate behaves differently from the baseline on",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "MATCH") {
		t.Errorf("the report lists matching probes; they should only be counted:\n%s", got.stdout)
	}
}

func TestRecordThenDiff(t *testing.T) {
	oldAddr, newAddr, manifests := startProxies(t)
	dir := t.TempDir()
	oldSnap, newSnap := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")

	rec := run(t, "", "record", "-f", manifests, "--address", oldAddr, "--name", "old", "--kind", "ingress-nginx",
		"--mutations", "--rate", "0", "-o", oldSnap)
	if rec.code != ExitOK {
		t.Fatalf("record old: exit %d: %s", rec.code, rec.stderr)
	}
	if !strings.Contains(rec.stderr, "recorded") || rec.stdout != "" {
		t.Errorf("record with -o FILE: stdout %q, stderr %q; want only a note on stderr", rec.stdout, rec.stderr)
	}
	// A URL is accepted for the address, as people paste them.
	if rec := run(t, "", "record", "-f", manifests, "--address", "http://"+newAddr+"/", "--name", "new",
		"--mutations", "--rate", "0", "-o", newSnap); rec.code != ExitOK {
		t.Fatalf("record new: exit %d: %s", rec.code, rec.stderr)
	}

	t.Run("old against new differs", func(t *testing.T) {
		got := run(t, "", "diff", oldSnap, newSnap, "-o", "json")
		if got.code != ExitFindings {
			t.Fatalf("exit code = %d, want %d: %s", got.code, ExitFindings, got.stderr)
		}
		res := decodeDiff(t, got.stdout)
		if res.Baseline.Name != "old" || res.Baseline.Kind != model.TargetIngressNGINX || res.Candidate.Name != "new" {
			t.Errorf("metadata = %+v → %+v", res.Baseline, res.Candidate)
		}
		if res.Summary.Diff == 0 {
			t.Error("no differences between the two proxies' snapshots")
		}
	})
	t.Run("a snapshot against itself is clean", func(t *testing.T) {
		got := run(t, "", "diff", oldSnap, oldSnap, "--strict")
		if got.code != ExitOK {
			t.Fatalf("exit code = %d, want %d\n%s", got.code, ExitOK, got.stdout)
		}
		if !strings.Contains(got.stdout, "No differences on the") {
			t.Errorf("report does not state the result:\n%s", got.stdout)
		}
		if strings.Contains(got.stdout, "not a statement of parity") {
			t.Errorf("a clean comparison is hedged as incomplete:\n%s", got.stdout)
		}
	})
	t.Run("a snapshot recorded with fewer probes leaves the rest unverified", func(t *testing.T) {
		fewer := filepath.Join(dir, "fewer.json")
		if rec := run(t, "", "record", "-f", manifests, "--address", oldAddr, "--rate", "0", "-o", fewer); rec.code != ExitOK {
			t.Fatalf("record: exit %d: %s", rec.code, rec.stderr)
		}
		plain := run(t, "", "diff", oldSnap, fewer)
		if plain.code != ExitOK {
			t.Errorf("exit code = %d, want %d without --strict", plain.code, ExitOK)
		}
		if !strings.Contains(plain.stdout, "not a statement of parity") {
			t.Errorf("report does not say the comparison is incomplete:\n%s", plain.stdout)
		}
		if strict := run(t, "", "diff", oldSnap, fewer, "--strict"); strict.code != ExitUnverified {
			t.Errorf("exit code = %d, want %d with --strict", strict.code, ExitUnverified)
		}
	})
}

func TestCompareCommandsRejectBadInput(t *testing.T) {
	oldAddr, _, manifests := startProxies(t)
	// A port nothing listens on.
	deadSrv := httptest.NewServer(http.NotFoundHandler())
	dead := deadSrv.Listener.Addr().String()
	deadSrv.Close()

	tests := []struct {
		name string
		args []string
		want string // substring of stderr
	}{
		{"live without a candidate", []string{"live", "-f", manifests, "--baseline", oldAddr}, "--candidate or --candidate-https is required"},
		{"address without a port", []string{"record", "-f", manifests, "--address", "localhost"}, "want HOST:PORT"},
		{"address with a path", []string{"record", "-f", manifests, "--address", "http://localhost:80/api"}, "not a URL with a path"},
		{"unknown kind", []string{"record", "-f", manifests, "--address", oldAddr, "--kind", "apache"}, "unknown --kind"},
		{"zero concurrency", []string{"record", "-f", manifests, "--address", oldAddr, "--concurrency", "0"}, "--concurrency"},
		{"nothing listening", []string{"record", "-f", manifests, "--address", dead, "--rate", "0"}, "no probe got a response"},
		{"candidate unreachable", []string{"live", "-f", manifests, "--baseline", oldAddr, "--candidate", dead, "--rate", "0"}, "no probe got a response from candidate"},
		{"diff with one snapshot", []string{"diff", "only.json"}, "accepts 2 arg"},
		{"diff of a missing file", []string{"diff", "missing-a.json", "missing-b.json"}, "missing-a.json"},
		{"diff of something that is not a snapshot", []string{"diff", manifests, manifests}, "decode snapshot"},
		{"unknown output format", []string{"diff", "a.json", "b.json", "-o", "yaml"}, "unknown output format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := run(t, "", tt.args...)
			if got.code != ExitError {
				t.Errorf("exit code = %d, want %d\nstdout: %s", got.code, ExitError, got.stdout)
			}
			if !strings.Contains(got.stderr, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", got.stderr, tt.want)
			}
		})
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"lb.example.net:80", "lb.example.net:80", true},
		{"[::1]:8443", "[::1]:8443", true},
		{"http://lb.example.net", "lb.example.net:80", true},
		{"https://lb.example.net/", "lb.example.net:443", true},
		{"http://lb.example.net:8080", "lb.example.net:8080", true},
		{"lb.example.net", "", false},
		{":8080", "", false},
		{"http://lb.example.net/path", "", false},
		{"http://", "", false},
	}
	for _, tt := range tests {
		got, err := parseAddress("address", tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("parseAddress(%q) = %q, %v; want %q, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

// TestProbesExampleGolden pins the probes generated for the example
// manifests. A change here changes what every run sends.
func TestProbesExampleGolden(t *testing.T) {
	got := run(t, "", "probes", "-f", exampleDir, "--mutations")
	if got.code != ExitOK {
		t.Fatalf("exit code = %d: %s", got.code, got.stderr)
	}
	golden := filepath.Join("testdata", "probes-five-behaviours.golden.txt")
	if *update {
		if err := os.WriteFile(golden, []byte(got.stdout), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/cli -update` to create it)", err)
	}
	if diff := cmp.Diff(string(want), got.stdout); diff != "" {
		t.Errorf("probes differ from %s (-want +got):\n%s", golden, diff)
	}
}

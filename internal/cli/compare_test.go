package cli

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
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

// recordOld records the old proxy with mutations and returns the snapshot path.
func recordOld(t *testing.T, oldAddr, manifests string) string {
	t.Helper()
	snap := filepath.Join(t.TempDir(), "old.json")
	rec := run(t, "", "record", "-f", manifests, "--address", oldAddr, "--name", "old", "--kind", "ingress-nginx",
		"--mutations", "--rate", "0", "-o", snap)
	if rec.code != ExitOK {
		t.Fatalf("record: exit %d: %s", rec.code, rec.stderr)
	}
	return snap
}

func TestVerify(t *testing.T) {
	oldAddr, newAddr, manifests := startProxies(t)
	baseline := recordOld(t, oldAddr, manifests)

	t.Run("the endpoint the snapshot came from still matches", func(t *testing.T) {
		got := run(t, "", "verify", baseline, "--address", oldAddr, "--rate", "0", "--strict")
		if got.code != ExitOK {
			t.Fatalf("exit code = %d, want %d\n%s%s", got.code, ExitOK, got.stdout, got.stderr)
		}
		if !strings.Contains(got.stdout, "No differences on the") {
			t.Errorf("report does not state the result:\n%s", got.stdout)
		}
	})

	t.Run("a different endpoint is caught, with no manifests involved", func(t *testing.T) {
		dir := t.TempDir()
		junit, markdown, saved := filepath.Join(dir, "report.xml"), filepath.Join(dir, "summary.md"), filepath.Join(dir, "new.json")
		got := run(t, "", "verify", baseline, "--address", newAddr, "--name", "new", "--kind", "gateway-api", "--rate", "0",
			"--junit", junit, "--markdown", markdown, "--save", saved, "-o", "json")
		if got.code != ExitFindings {
			t.Fatalf("exit code = %d, want %d: %s", got.code, ExitFindings, got.stderr)
		}
		res := decodeDiff(t, got.stdout)
		if res.Summary.Diff == 0 || res.Baseline.Name != "old" || res.Candidate.Name != "new" {
			t.Errorf("summary = %+v, %q → %q", res.Summary, res.Baseline.Name, res.Candidate.Name)
		}
		// The mutations stored in the snapshot were replayed.
		if v := verdicts(res)["regex.example.com /aaa"]; v != model.VerdictDiff {
			t.Errorf("/aaa: verdict = %q, want DIFF", v)
		}

		var suites struct {
			Suites []struct {
				Tests    int `xml:"tests,attr"`
				Failures int `xml:"failures,attr"`
			} `xml:"testsuite"`
		}
		data, err := os.ReadFile(junit)
		if err != nil {
			t.Fatal(err)
		}
		if err := xml.Unmarshal(data, &suites); err != nil || len(suites.Suites) != 1 {
			t.Fatalf("--junit file is not a JUnit report: %v\n%s", err, data)
		}
		if suites.Suites[0].Failures != res.Summary.Diff || suites.Suites[0].Tests != res.Summary.Probes {
			t.Errorf("JUnit has %d failures in %d tests; the comparison has %d differences in %d probes",
				suites.Suites[0].Failures, suites.Suites[0].Tests, res.Summary.Diff, res.Summary.Probes)
		}

		md, err := os.ReadFile(markdown)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(md), "### Differences") || !strings.Contains(string(md), "NGX-REGEX-001 case-flip") {
			t.Errorf("--markdown file lacks the differences table:\n%s", md)
		}

		// The saved candidate snapshot is usable as an input in its own right.
		if again := run(t, "", "diff", baseline, saved, "-o", "json"); again.code != ExitFindings ||
			decodeDiff(t, again.stdout).Summary != res.Summary {
			t.Errorf("diff of the baseline against the --save file gives a different result")
		}
	})

	t.Run("other output formats on stdout", func(t *testing.T) {
		if got := run(t, "", "verify", baseline, "--address", newAddr, "--rate", "0", "-o", "junit"); !strings.HasPrefix(got.stdout, "<?xml") {
			t.Errorf("-o junit does not print XML:\n%.200s", got.stdout)
		}
		if got := run(t, "", "verify", baseline, "--address", newAddr, "--rate", "0", "-o", "markdown"); !strings.HasPrefix(got.stdout, "## routeparity") {
			t.Errorf("-o markdown does not print Markdown:\n%.200s", got.stdout)
		}
	})

	t.Run("bad input", func(t *testing.T) {
		tests := []struct {
			name string
			args []string
			want string
		}{
			{"no address", []string{"verify", baseline}, "--address or --https-address is required"},
			{"no snapshot", []string{"verify", "--address", newAddr}, "accepts 1 arg"},
			{"not a snapshot", []string{"verify", manifests, "--address", newAddr}, "decode snapshot"},
			{"zero samples", []string{"verify", baseline, "--address", newAddr, "--samples", "0"}, "--samples must be at least 1"},
			{"a header both ignored and compared", []string{"verify", baseline, "--address", newAddr,
				"--ignore-header", "Location", "--compare-header", "location"}, "both --ignore-header and --compare-header"},
			{"unwritable report", []string{"verify", baseline, "--address", newAddr, "--rate", "0",
				"--junit", filepath.Join(t.TempDir(), "no-such-dir", "r.xml")}, "no-such-dir"},
		}
		for _, tt := range tests {
			got := run(t, "", tt.args...)
			if got.code != ExitError || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("%s: exit %d, stderr %q; want exit %d mentioning %q", tt.name, got.code, got.stderr, ExitError, tt.want)
			}
		}
	})
}

func TestIgnoreHeaderDemotesADifference(t *testing.T) {
	oldAddr, newAddr, manifests := startProxies(t)
	baseline := recordOld(t, oldAddr, manifests)

	// /app differs in three ways: routed or not, status, and the redirect.
	// Ignoring Location removes only the third, and the report still shows it.
	got := run(t, "", "verify", baseline, "--address", newAddr, "--rate", "0", "--ignore-header", "Location", "-o", "json")
	res := decodeDiff(t, got.stdout)
	for _, c := range res.Comparisons {
		if c.Probe.Host+" "+c.Probe.RawTarget != "slash.example.com /app" {
			continue
		}
		if diff := cmp.Diff([]string{"routed", "client.status"}, deltaFields(c)); diff != "" {
			t.Errorf("/app deltas with Location ignored (-want +got):\n%s", diff)
		}
		if len(c.Suppressed) == 0 {
			t.Error("the ignored redirect is not recorded as suppressed")
		}
		return
	}
	t.Fatal("no comparison for slash.example.com /app")
}

func TestSamplesDetectsAnEndpointThatAnswersInconsistently(t *testing.T) {
	oldAddr, _, manifests := startProxies(t)
	baseline := recordOld(t, oldAddr, manifests)

	// A candidate that behaves like the old proxy, except that every other
	// request for /app/ fails. One sample can land on either answer.
	var requests, appRequests atomic.Int64
	old := oldProxy()
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host == "slash.example.com" && r.RequestURI == "/app/" && appRequests.Add(1)%2 == 0 {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		old.ServeHTTP(w, r)
	}))
	defer flaky.Close()
	addr := flaky.Listener.Addr().String()

	// One at a time, so the alternation is deterministic.
	got := run(t, "", "verify", baseline, "--address", addr, "--rate", "0", "--concurrency", "1", "--samples", "4", "-o", "json")
	res := decodeDiff(t, got.stdout)
	if got.code != ExitOK {
		t.Errorf("exit code = %d, want %d: an unstable endpoint is unverified, not different", got.code, ExitOK)
	}
	if want := int64(4 * res.Summary.Probes); requests.Load() != want {
		t.Errorf("the endpoint received %d requests, want %d (4 samples of %d probes)", requests.Load(), want, res.Summary.Probes)
	}
	var app model.Comparison
	for _, c := range res.Comparisons {
		if c.Probe.Host == "slash.example.com" && c.Probe.RawTarget == "/app/" {
			app = c
		}
	}
	if app.Verdict != model.VerdictUnverified || !strings.Contains(app.Reason, "the candidate gave 2 different answers to 4 identical requests") {
		t.Errorf("/app/: verdict %q, reason %q; want UNVERIFIED naming the inconsistency", app.Verdict, app.Reason)
	}
	if res.Summary.Diff != 0 || res.Summary.Unverified != 1 {
		t.Errorf("summary = %+v, want no differences and one unverified probe", res.Summary)
	}

	if strict := run(t, "", "verify", baseline, "--address", addr, "--rate", "0", "--concurrency", "1", "--samples", "4", "--strict"); strict.code != ExitUnverified {
		t.Errorf("exit code with --strict = %d, want %d", strict.code, ExitUnverified)
	}
}

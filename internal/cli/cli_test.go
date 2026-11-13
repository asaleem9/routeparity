package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite golden files with current output")

const exampleDir = "../../examples/five-behaviours"

type result struct {
	code   int
	stdout string
	stderr string
}

func run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, Streams{In: strings.NewReader(stdin), Out: &out, Err: &errOut})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// TestAuditExampleGolden pins the full text report for the example manifests.
// It runs the real converter, so it also changes when an ingress2gateway
// upgrade changes what the converter warns about; that is worth noticing.
func TestAuditExampleGolden(t *testing.T) {
	got := run(t, "", "audit", "-f", exampleDir)
	if got.code != ExitFindings {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, ExitFindings, got.stderr)
	}
	if got.stderr != "" {
		t.Errorf("unexpected stderr: %s", got.stderr)
	}
	if strings.Contains(got.stdout, "\x1b[") {
		t.Error("output written to a buffer contains ANSI escape codes")
	}

	golden := filepath.Join("testdata", "audit-five-behaviours.golden.txt")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
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
		t.Errorf("audit output differs from %s (-want +got):\n%s", golden, diff)
	}
}

func TestAuditReportsEachDocumentedBehaviour(t *testing.T) {
	got := run(t, "", "audit", "-f", exampleDir, "-o", "json")
	if got.code != ExitFindings {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, ExitFindings, got.stderr)
	}
	var rep struct {
		Ingresses int `json:"ingresses"`
		Findings  []struct {
			RuleID     string `json:"ruleID"`
			Confidence string `json:"confidence"`
			Message    string `json:"message"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &rep); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got.stdout)
	}
	if rep.Ingresses != 6 {
		t.Errorf("ingresses = %d, want 6", rep.Ingresses)
	}

	byRule := map[string]int{}
	var messages []string
	for _, f := range rep.Findings {
		byRule[f.RuleID]++
		messages = append(messages, f.Message)
		if f.Confidence != "predicted" {
			t.Errorf("%s: confidence = %q; an audit sends no traffic, so it can only predict", f.RuleID, f.Confidence)
		}
	}
	// One Ingress in the example per behaviour in the upstream post.
	for _, id := range []string{"NGX-REGEX-001", "NGX-REGEX-002", "NGX-REGEX-003", "NGX-SLASH-001", "NGX-NORM-001", "NGX-TLS-001"} {
		if byRule[id] != 1 {
			t.Errorf("%s reported %d time(s), want 1", id, byRule[id])
		}
	}
	all := strings.Join(messages, "\n")
	for _, annotation := range []string{"auth-url", "limit-rps"} {
		if !strings.Contains(all, annotation) {
			t.Errorf("no finding mentions the untranslated annotation %s", annotation)
		}
	}
}

func TestAuditExitCodes(t *testing.T) {
	// A single plain Ingress: the only finding is the low-severity note that
	// path normalisation cannot be judged from manifests.
	plain := `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
  annotations:
    kubernetes.io/ingress.class: nginx
spec:
  tls:
  - hosts: [example.com]
    secretName: cert
  rules:
  - host: example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web
            port:
              number: 80
`
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  int
	}{
		{"medium finding fails by default", plain, []string{"audit", "-f", "-"}, ExitFindings},
		{"threshold above the worst finding", plain, []string{"audit", "-f", "-", "--fail-on", "high"}, ExitOK},
		{"findings never fail with none", plain, []string{"audit", "-f", "-", "--fail-on", "none"}, ExitOK},
		{"example passes with none", "", []string{"audit", "-f", exampleDir, "--fail-on", "none"}, ExitOK},
		{"unknown threshold", plain, []string{"audit", "-f", "-", "--fail-on", "severe"}, ExitError},
		{"unknown output format", plain, []string{"audit", "-f", "-", "-o", "xml"}, ExitError},
		{"missing file", "", []string{"audit", "-f", "does-not-exist.yaml"}, ExitError},
		{"no filename", "", []string{"audit"}, ExitError},
		{"no Ingress of the class", plain, []string{"audit", "-f", "-", "--ingress-class", "traefik"}, ExitError},
		{"unknown command", "", []string{"frobnicate"}, ExitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := run(t, tt.stdin, tt.args...)
			if got.code != tt.want {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", got.code, tt.want, got.stdout, got.stderr)
			}
			if tt.want == ExitError && !strings.HasPrefix(got.stderr, "routeparity: ") {
				t.Errorf("stderr = %q, want an error message prefixed with the program name", got.stderr)
			}
		})
	}
}

func TestAuditNamesTheClassesItSkipped(t *testing.T) {
	got := run(t, "", "audit", "-f", exampleDir, "--ingress-class", "traefik")
	if got.code != ExitError {
		t.Fatalf("exit code = %d, want %d", got.code, ExitError)
	}
	if !strings.Contains(got.stderr, `"nginx"`) || !strings.Contains(got.stderr, "6 Ingresses were skipped") {
		t.Errorf("stderr = %q, want it to say which class the skipped Ingresses use", got.stderr)
	}
}

func TestAuditWithoutConverter(t *testing.T) {
	got := run(t, "", "audit", "-f", exampleDir, "--no-convert", "-o", "json")
	if got.code != ExitFindings {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, ExitFindings, got.stderr)
	}
	if strings.Contains(got.stdout, "NGX-CONVERT-001") {
		t.Error("--no-convert output still contains converter findings")
	}
	if strings.Contains(got.stdout, `"converter"`) {
		t.Error("--no-convert output names a converter")
	}
}

func TestExplain(t *testing.T) {
	list := run(t, "", "explain")
	if list.code != ExitOK || !strings.Contains(list.stdout, "NGX-REGEX-002") {
		t.Errorf("explain: code %d, stdout %q", list.code, list.stdout)
	}

	one := run(t, "", "explain", "ngx-regex-002") // IDs are accepted in any case
	if one.code != ExitOK {
		t.Fatalf("explain ngx-regex-002: code %d, stderr %s", one.code, one.stderr)
	}
	for _, want := range []string{"NGX-REGEX-002", "severity: high", "References:", "https://"} {
		if !strings.Contains(one.stdout, want) {
			t.Errorf("explain output lacks %q:\n%s", want, one.stdout)
		}
	}

	unknown := run(t, "", "explain", "NGX-NOPE-999")
	if unknown.code != ExitError || !strings.Contains(unknown.stderr, "NGX-NOPE-999") {
		t.Errorf("explain of an unknown rule: code %d, stderr %q", unknown.code, unknown.stderr)
	}
}

func TestVersionFlag(t *testing.T) {
	got := run(t, "", "--version")
	if got.code != ExitOK || !strings.HasPrefix(got.stdout, "routeparity ") {
		t.Errorf("--version: code %d, stdout %q", got.code, got.stdout)
	}
}

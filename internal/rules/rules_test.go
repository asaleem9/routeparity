package rules

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/source"
)

var update = flag.Bool("update", false, "rewrite golden files with current output")

// convertOnly lists rules whose findings come from a conversion run instead
// of the routing model, so they have no manifest fixture.
var convertOnly = map[string]bool{"NGX-CONVERT-001": true}

func loadWorld(t *testing.T, path string) *model.World {
	t.Helper()
	res, err := source.LoadPaths([]string{path}, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if len(res.Warnings) > 0 {
		t.Fatalf("load %s: unexpected warnings: %v", path, res.Warnings)
	}
	return model.NewWorld(res.Objects, model.WorldOptions{})
}

// TestAuditGolden runs each rule over its own fixture. The fixture's header
// comment states what the rule should and should not report; the golden file
// is the exact output.
func TestAuditGolden(t *testing.T) {
	for _, rule := range All() {
		id := rule.Meta().ID
		if convertOnly[id] {
			continue
		}
		t.Run(id, func(t *testing.T) {
			dir := filepath.Join("testdata", id)
			w := loadWorld(t, filepath.Join(dir, "ingress.yaml"))

			findings := rule.Audit(w)
			if len(findings) == 0 {
				t.Fatal("rule reported nothing on its own fixture")
			}
			for _, f := range findings {
				if f.RuleID != id {
					t.Errorf("finding carries rule ID %q, want %q", f.RuleID, id)
				}
				if f.Confidence != model.ConfidencePredicted {
					t.Errorf("audit finding has confidence %q, want %q", f.Confidence, model.ConfidencePredicted)
				}
				if f.Message == "" {
					t.Error("finding has no message")
				}
			}

			got, err := json.MarshalIndent(findings, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')

			golden := filepath.Join(dir, "audit.golden.json")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run `go test ./internal/rules -update` to create it)", err)
			}
			if diff := cmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("audit output differs from %s (-want +got):\n%s", golden, diff)
			}
		})
	}
}

// TestRulesAreQuietOnPlainIngresses guards against false positives. The
// NGX-NORM-001 fixture contains only Exact paths on hosts with no TLS and no
// annotations, the simplest Ingresses there are, so no other rule applies.
func TestRulesAreQuietOnPlainIngresses(t *testing.T) {
	w := loadWorld(t, filepath.Join("testdata", "NGX-NORM-001", "ingress.yaml"))
	for _, rule := range All() {
		id := rule.Meta().ID
		if id == "NGX-NORM-001" {
			continue // applies to every setup by design
		}
		if findings := rule.Audit(w); len(findings) != 0 {
			t.Errorf("%s reported %d finding(s) on plain Ingresses: %+v", id, len(findings), findings)
		}
	}
}

func TestMetaIsComplete(t *testing.T) {
	idFormat := regexp.MustCompile(`^NGX-[A-Z]+-\d{3}$`)
	seen := map[string]bool{}
	for _, rule := range All() {
		m := rule.Meta()
		if !idFormat.MatchString(m.ID) {
			t.Errorf("rule ID %q does not match %s", m.ID, idFormat)
		}
		if seen[m.ID] {
			t.Errorf("rule ID %q is used twice", m.ID)
		}
		seen[m.ID] = true
		if m.Title == "" || m.Summary == "" {
			t.Errorf("%s: title and summary are required", m.ID)
		}
		if len(m.References) == 0 {
			t.Errorf("%s: at least one reference is required", m.ID)
		}
		if m.Severity.Rank() == 0 && m.Severity != model.SeverityInfo {
			t.Errorf("%s: unknown severity %q", m.ID, m.Severity)
		}
		if got, ok := ByID(m.ID); !ok || got.Meta().ID != m.ID {
			t.Errorf("ByID(%q) did not return the rule", m.ID)
		}
		if !convertOnly[m.ID] {
			if _, err := os.Stat(filepath.Join("testdata", m.ID, "ingress.yaml")); err != nil {
				t.Errorf("%s has no fixture: %v", m.ID, err)
			}
		}
	}
	if _, ok := ByID("NGX-NOPE-999"); ok {
		t.Error("ByID returned a rule for an unknown ID")
	}
}

func TestConverterFindings(t *testing.T) {
	ingress := model.ObjectRef{Kind: "Ingress", Namespace: "demo", Name: "admin"}
	tests := []struct {
		name         string
		notice       convert.Notice
		wantSeverity model.Severity // empty means the notice is dropped
	}{
		{
			name:         "external auth is access control",
			notice:       convert.Notice{Level: "WARNING", Message: "Unsupported annotation nginx.ingress.kubernetes.io/auth-url", Objects: []model.ObjectRef{ingress}},
			wantSeverity: model.SeverityHigh,
		},
		{
			name:         "source range without an annotation name",
			notice:       convert.Notice{Level: "WARNING", Message: "IP-based authorization is not supported"},
			wantSeverity: model.SeverityHigh,
		},
		{
			name:         "snippets",
			notice:       convert.Notice{Level: "WARNING", Message: "Unsupported annotation nginx.ingress.kubernetes.io/configuration-snippet"},
			wantSeverity: model.SeverityHigh,
		},
		{
			name:         "rate limit",
			notice:       convert.Notice{Level: "WARNING", Message: "Unsupported annotation nginx.ingress.kubernetes.io/limit-rps"},
			wantSeverity: model.SeverityMedium,
		},
		{
			name:         "session affinity",
			notice:       convert.Notice{Level: "WARNING", Message: "Session affinity is not supported"},
			wantSeverity: model.SeverityMedium,
		},
		{
			name:         "unclassified warning keeps the rule's severity",
			notice:       convert.Notice{Level: "WARNING", Message: "Unsupported annotation nginx.ingress.kubernetes.io/proxy-buffering"},
			wantSeverity: model.SeverityMedium,
		},
		{
			name:         "errors are reported",
			notice:       convert.Notice{Level: "ERROR", Message: "something failed"},
			wantSeverity: model.SeverityMedium,
		},
		{
			name:   "informational notices are dropped",
			notice: convert.Notice{Level: "INFO", Message: "generated Gateway demo/nginx"},
		},
		{
			name:   "a notice that restates a dedicated rule is dropped",
			notice: convert.Notice{Level: "WARNING", Message: "Gateway API does not support configuring URL normalization (RFC 3986, Section 6)."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConverterFindings([]convert.Notice{tt.notice})
			if tt.wantSeverity == "" {
				if len(got) != 0 {
					t.Fatalf("got %d finding(s), want the notice dropped: %+v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d finding(s), want 1", len(got))
			}
			f := got[0]
			if f.Severity != tt.wantSeverity {
				t.Errorf("severity = %q, want %q", f.Severity, tt.wantSeverity)
			}
			if f.RuleID != "NGX-CONVERT-001" {
				t.Errorf("rule ID = %q, want NGX-CONVERT-001", f.RuleID)
			}
			if len(tt.notice.Objects) > 0 && (f.Object == nil || *f.Object != tt.notice.Objects[0]) {
				t.Errorf("object = %v, want %v", f.Object, tt.notice.Objects[0])
			}
		})
	}
}

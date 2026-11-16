package rules

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/pathgen"
)

func TestExplainRegexPrintsTheConfiguredPattern(t *testing.T) {
	w := loadWorld(t, filepath.Join("testdata", "NGX-REGEX-001", "ingress.yaml"))
	h := hostByProbe(w, "regex.example.com")
	if h == nil {
		t.Fatal("fixture host not found")
	}
	var path model.PathRule
	for _, p := range h.Paths {
		if !p.Canary && p.EffectivePath() == "/[A-Z]{3}" {
			path = p
			break
		}
	}
	base, err := BaseTarget(h, path)
	if err != nil {
		t.Fatal(err)
	}
	flipped, ok := pathgen.CaseFlip(base)
	if !ok {
		t.Fatal("the sample path has no letter to flip")
	}
	c := model.Comparison{
		Probe: model.Probe{
			Method: "GET", Scheme: "http", Host: "regex.example.com", RawTarget: flipped,
			Origin: model.Origin{
				Source: model.OriginMutation, RuleID: "NGX-REGEX-001", Mutation: "case-flip",
				Object: &path.Ingress,
			},
		},
		Verdict: model.VerdictDiff,
		Deltas:  []model.Delta{{Field: "routed", Baseline: "routed", Candidate: "not routed"}},
	}

	f := regexSemantics{}.Explain(c, Scope{World: w, Family: familyEnvoy})
	if f == nil || f.Fix == nil {
		t.Fatalf("finding = %+v", f)
	}
	if f.Path != "/[A-Z]{3}" {
		t.Errorf("path = %q, want the configured pattern and not the request", f.Path)
	}
	if !strings.Contains(f.Fix.Snippet, `(?i)/[A-Z]{3}.*`) || f.Fix.Implementation != familyEnvoy {
		t.Errorf("fix = %+v", f.Fix)
	}
	if !strings.Contains(f.Message, "letter case") || !strings.Contains(f.Message, "baseline routed") {
		t.Errorf("message = %s", f.Message)
	}

	// A family this snippet is not written for must not be told to paste it.
	other := regexSemantics{}.Explain(c, Scope{World: w, Family: "nginx-gateway-fabric"})
	if other == nil || other.Fix == nil || other.Fix.Snippet != "" || other.Fix.Implementation != "nginx-gateway-fabric" {
		t.Fatalf("fix for another family = %+v", other)
	}
}

func TestExplainAttributesHSTSRatherThanTheProbeRule(t *testing.T) {
	// The regex matched on both sides. The only difference is the header
	// ingress-nginx adds on its own, so the regex rule does not own it.
	res := &diff.Result{
		Candidate: model.TargetMeta{Implementation: "envoy-gateway"},
		Comparisons: []model.Comparison{{
			Probe: model.Probe{
				Method: "GET", Scheme: "https", Host: "regex.example.com", RawTarget: "/aaa",
				Origin: model.Origin{Source: model.OriginMutation, RuleID: "NGX-REGEX-001", Mutation: "case-flip"},
			},
			Verdict: model.VerdictDiff,
			Deltas: []model.Delta{{
				Field: "client.header.strict-transport-security", Baseline: "max-age=31536000; includeSubDomains",
			}},
		}},
	}
	Annotate(res, nil)
	f := res.Comparisons[0].Finding
	if f == nil || f.RuleID != "NGX-TLS-002" || f.Confidence != model.ConfidenceConfirmed || f.Fix == nil {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(f.Fix.Snippet, "Strict-Transport-Security") || !strings.Contains(f.Fix.Snippet, "max-age=31536000; includeSubDomains") {
		t.Errorf("fix snippet = %s", f.Fix.Snippet)
	}
}

func TestExplainNormalisationNamesEnvoyWhenThatIsTheCandidate(t *testing.T) {
	c := model.Comparison{
		Probe: model.Probe{
			Method: "GET", Scheme: "http", Host: "regex.example.com", RawTarget: "//AAA",
			Origin: model.Origin{Source: model.OriginMutation, RuleID: "NGX-NORM-001", Mutation: "double-slash"},
		},
		Verdict: model.VerdictDiff,
		Deltas:  []model.Delta{{Field: "backend.rawTarget", Baseline: "//AAA", Candidate: "/AAA"}},
	}
	f := urlNormalisation{}.Explain(c, Scope{Family: familyEnvoy})
	if f == nil || f.Fix == nil || f.Fix.Implementation != familyEnvoy {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(f.Message, "//AAA") || !strings.Contains(f.Message, "/AAA") {
		t.Errorf("message = %s", f.Message)
	}
	if !strings.Contains(f.Fix.Summary, "HTTPRoute cannot restore") {
		t.Errorf("envoy fix = %s", f.Fix.Summary)
	}

	generic := urlNormalisation{}.Explain(c, Scope{})
	if generic == nil || generic.Fix == nil || generic.Fix.Implementation != "" {
		t.Fatalf("generic fix = %+v", generic)
	}
}

func TestExplainSlashRedirectIncludesThePath(t *testing.T) {
	w := loadWorld(t, filepath.Join("testdata", "NGX-SLASH-001", "ingress.yaml"))
	c := model.Comparison{
		Probe: model.Probe{
			Method: "GET", Scheme: "http", Host: "slash.example.com", RawTarget: "/my-path",
			Origin: model.Origin{Source: model.OriginMutation, RuleID: "NGX-SLASH-001", Mutation: "strip-slash",
				Object: &model.ObjectRef{Kind: "Ingress", Namespace: "demo", Name: "slash"}},
		},
		Verdict: model.VerdictDiff,
		Deltas: []model.Delta{
			{Field: "client.status", Baseline: "301", Candidate: "404"},
			{Field: "client.location", Baseline: "http://slash.example.com/my-path/", Candidate: ""},
		},
	}
	f := trailingSlashRedirect{}.Explain(c, Scope{World: w})
	if f == nil || f.Fix == nil || !strings.Contains(f.Fix.Snippet, `"/my-path"`) || !strings.Contains(f.Fix.Snippet, `"/my-path/"`) {
		t.Fatalf("finding = %+v", f)
	}
}

func TestExplainConflictNamesBothIngresses(t *testing.T) {
	w := loadWorld(t, filepath.Join("testdata", "NGX-CONFLICT-001", "ingress.yaml"))
	c := model.Comparison{
		Probe: model.Probe{
			Method: "GET", Scheme: "http", Host: "conflict.example.com", RawTarget: "/api",
			Origin: model.Origin{Source: model.OriginDerived},
		},
		Verdict: model.VerdictDiff,
		Deltas:  []model.Delta{{Field: "backend.reached", Baseline: "demo/api-v1:80", Candidate: "demo/api-v2:80"}},
	}
	f := pathConflict{}.Explain(c, Scope{World: w})
	if f == nil || f.RuleID != "NGX-CONFLICT-001" || f.Fix == nil {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(f.Message, "api-v1") || !strings.Contains(f.Message, "api-v2") {
		t.Errorf("message = %s", f.Message)
	}
}

func TestExplainUnclassifiedWhenNothingClaimsTheDifference(t *testing.T) {
	res := &diff.Result{Comparisons: []model.Comparison{{
		Probe:   model.Probe{Method: "GET", Scheme: "http", Host: "shop.example.com", RawTarget: "/sale"},
		Verdict: model.VerdictDiff,
		Deltas:  []model.Delta{{Field: "client.header.x-request-id", Baseline: "1", Candidate: "2"}},
	}}}
	Annotate(res, nil)
	f := res.Comparisons[0].Finding
	if f == nil || f.RuleID != "" || f.Confidence != model.ConfidenceUnclassified {
		t.Fatalf("finding = %+v", f)
	}
}

func TestAnnotateRecordsAPredictedRuleThatWasNotChecked(t *testing.T) {
	w := loadWorld(t, filepath.Join("testdata", "NGX-REGEX-001", "ingress.yaml"))
	res := &diff.Result{Comparisons: []model.Comparison{{
		Probe: model.Probe{
			Method: "GET", Scheme: "http", Host: "regex.example.com", RawTarget: "/AAA",
			Origin: model.Origin{Source: model.OriginDerived},
		},
		Verdict: model.VerdictMatch,
	}}}
	Annotate(res, w)
	Annotate(res, w) // a second pass must not duplicate the gap

	var regex model.Coverage
	var gaps int
	for _, c := range res.Coverage {
		if c.RuleID == "NGX-REGEX-001" {
			regex = c
		}
	}
	for _, u := range res.Unverified {
		if u.Reason == model.ReasonNotExercised && strings.HasPrefix(u.Feature, "NGX-REGEX-001") {
			gaps++
		}
	}
	if regex.Status != model.CoverageNotExercised || gaps != 1 {
		t.Errorf("coverage = %+v, gaps = %d, unverified = %+v", regex, gaps, res.Unverified)
	}
}

func TestAnnotateDoesNotFlagARuleTheBaselineRouted(t *testing.T) {
	res := &diff.Result{Comparisons: []model.Comparison{{
		Probe: model.Probe{
			Method: "GET", Scheme: "http", Host: "regex.example.com", RawTarget: "/aaa",
			Origin: model.Origin{Source: model.OriginMutation, RuleID: "NGX-REGEX-001", Mutation: "case-flip"},
		},
		Verdict: model.VerdictMatch,
	}}}
	Annotate(res, nil)
	for _, c := range res.Coverage {
		if c.RuleID == "NGX-REGEX-001" && c.Status == model.CoverageMatches && len(res.Unverified) == 0 {
			return
		}
	}
	t.Errorf("coverage = %+v, unverified = %+v", res.Coverage, res.Unverified)
}

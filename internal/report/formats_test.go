package report

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
)

func comparison(target string, verdict model.Verdict, origin model.Origin, reason string, deltas ...model.Delta) model.Comparison {
	return model.Comparison{
		Probe:   model.Probe{Method: "GET", Scheme: "http", Host: "shop.example.com", RawTarget: target, Origin: origin},
		Verdict: verdict,
		Reason:  reason,
		Deltas:  deltas,
	}
}

// sampleResult has one comparison of each verdict, one feature that was not
// exercised, and some suppressed differences.
func sampleResult() *diff.Result {
	derived := model.Origin{Source: model.OriginDerived}
	regex := model.Origin{Source: model.OriginMutation, RuleID: "NGX-REGEX-001", Mutation: "case-flip"}
	return &diff.Result{
		Baseline:  model.TargetMeta{Name: "old", Kind: model.TargetIngressNGINX, Mode: model.ModeLab},
		Candidate: model.TargetMeta{Name: "new", Kind: model.TargetGatewayAPI, Mode: model.ModeLab},
		Summary:   diff.Summary{Probes: 5, Match: 1, Diff: 1, Unrouted: 1, Unverified: 1, Error: 1},
		Comparisons: []model.Comparison{
			comparison("/ok", model.VerdictMatch, derived, ""),
			comparison("/aaa", model.VerdictDiff, regex, "",
				model.Delta{Field: "client.status", Baseline: "200", Candidate: "404"},
				model.Delta{Field: "backend.reached", Baseline: "demo/web:80", Candidate: "not reached"}),
			comparison("/configured", model.VerdictUnrouted, derived, "neither target routes this request"),
			comparison("/down", model.VerdictError, derived, "baseline: status 200; candidate: timeout"),
			comparison("/split", model.VerdictUnverified, derived, "the baseline gave 2 different answers to 10 identical requests"),
		},
		Unverified: []model.Unverified{{
			Reason: model.ReasonNoProbe, Feature: "shop.example.com /foo(?=bar)",
			Object: &model.ObjectRef{Kind: "Ingress", Namespace: "demo", Name: "pcre"}, Detail: "cannot generate a path",
		}},
		Suppressed: map[string]int{"client.header.server": 4, "backend.header.x-real-ip": 2},
	}
}

func parseJUnit(t *testing.T, data []byte) junitSuite {
	t.Helper()
	var suites junitSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		t.Fatalf("output is not well-formed JUnit XML: %v\n%s", err, data)
	}
	if len(suites.Suites) != 1 {
		t.Fatalf("got %d test suites, want 1", len(suites.Suites))
	}
	return suites.Suites[0]
}

func TestDiffJUnit(t *testing.T) {
	tests := []struct {
		name                                  string
		strict                                bool
		wantFailures, wantErrors, wantSkipped int
	}{
		// Without --strict the command exits 1 for the one difference only,
		// and the file says the same: everything unchecked is skipped.
		{"default", false, 1, 0, 4},
		// With --strict everything unchecked fails the run.
		{"strict", true, 4, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := DiffJUnit(&buf, sampleResult(), tt.strict); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(buf.String(), "<?xml") {
				t.Error("output does not start with an XML declaration")
			}
			suite := parseJUnit(t, buf.Bytes())
			if suite.Tests != 6 || len(suite.Cases) != 6 {
				t.Errorf("tests = %d with %d cases, want 6: five probes and one unexercised feature", suite.Tests, len(suite.Cases))
			}
			if suite.Failures != tt.wantFailures || suite.Errors != tt.wantErrors || suite.Skipped != tt.wantSkipped {
				t.Errorf("failures/errors/skipped = %d/%d/%d, want %d/%d/%d",
					suite.Failures, suite.Errors, suite.Skipped, tt.wantFailures, tt.wantErrors, tt.wantSkipped)
			}

			byName := map[string]junitCase{}
			for _, c := range suite.Cases {
				byName[c.Name] = c
			}
			if c := byName["GET http://shop.example.com/ok"]; c.Failure != nil || c.Error != nil || c.Skipped != nil {
				t.Errorf("a matching probe is not a plain pass: %+v", c)
			}
			d := byName["GET http://shop.example.com/aaa"]
			if d.Failure == nil {
				t.Fatal("the differing probe is not a failure")
			}
			if d.Failure.Message != "differs in client.status, backend.reached" || d.Failure.Type != "DIFF" {
				t.Errorf("failure = %+v", d.Failure)
			}
			for _, want := range []string{"NGX-REGEX-001, case-flip", "client.status: 200 -> 404"} {
				if !strings.Contains(d.Failure.Body, want) {
					t.Errorf("failure body lacks %q:\n%s", want, d.Failure.Body)
				}
			}
			if d.Classname != "routeparity.shop.example.com" {
				t.Errorf("classname = %q, want probes grouped by host", d.Classname)
			}
			if _, ok := byName["shop.example.com /foo(?=bar)"]; !ok {
				t.Error("the feature that was not exercised has no test case")
			}
		})
	}
}

func TestDiffJUnitEscapesRequestTargets(t *testing.T) {
	target := `/search?q=<script>&x="1"`
	res := &diff.Result{Comparisons: []model.Comparison{
		comparison(target, model.VerdictDiff, model.Origin{}, "", model.Delta{Field: "client.status", Baseline: "200", Candidate: "<404>"}),
	}}
	var buf bytes.Buffer
	if err := DiffJUnit(&buf, res, false); err != nil {
		t.Fatal(err)
	}
	suite := parseJUnit(t, buf.Bytes())
	if got, want := suite.Cases[0].Name, "GET http://shop.example.com"+target; got != want {
		t.Errorf("name after a round trip = %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), "<script>") {
		t.Error("the request target appears unescaped in the XML")
	}
}

func TestDiffMarkdown(t *testing.T) {
	var buf bytes.Buffer
	if err := DiffMarkdown(&buf, sampleResult()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"## routeparity: old (ingress-nginx) → new (gateway-api)",
		"**Result:** The candidate behaves differently from the baseline on 1 probe.",
		"This is not a statement of parity",
		"| 5 | 1 | 1 | 1 | 1 | 1 |",
		"### Differences",
		"| `GET http://shop.example.com/aaa` | NGX-REGEX-001 case-flip | `client.status` | `200` | `404` |",
		// The request is named once; further fields of the same probe leave the cell empty.
		"|  |  | `backend.reached` | `demo/web:80` | `not reached` |",
		"### Not compared",
		"| `GET http://shop.example.com/configured` | UNROUTED | neither target routes this request |",
		"<summary>Not verified (1)</summary>",
		"| no-probe | `shop.example.com /foo(?=bar) (Ingress/demo/pcre)` | cannot generate a path |",
		"<summary>Differences recorded and not counted (2 fields)</summary>",
		"| `client.header.server` | 4 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/ok") {
		t.Error("a matching probe is listed; only the count belongs in the report")
	}
	// Every table row has the same number of cells as its header, or GitHub
	// renders the table as text.
	for _, block := range strings.Split(out, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		var header int
		for _, line := range lines {
			if !strings.HasPrefix(line, "|") {
				continue
			}
			cells := strings.Count(strings.ReplaceAll(line, `\|`, ""), "|")
			if header == 0 {
				header = cells
			} else if cells != header {
				t.Errorf("row has %d cell separators, header has %d: %s", cells, header, line)
			}
		}
	}
}

func TestDiffMarkdownCleanResult(t *testing.T) {
	res := &diff.Result{
		Baseline:  model.TargetMeta{Name: "old", Mode: model.ModeLab},
		Candidate: model.TargetMeta{Name: "new", Mode: model.ModeLab},
		Summary:   diff.Summary{Probes: 1, Match: 1},
		Comparisons: []model.Comparison{comparison("/a", model.VerdictMatch,
			model.Origin{Source: model.OriginMutation, Mutation: "case-flip"}, "")},
	}
	var buf bytes.Buffer
	if err := DiffMarkdown(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "**Result:** No differences on the 1 probe that could be compared.") {
		t.Errorf("result line missing:\n%s", out)
	}
	for _, absent := range []string{"### Differences", "### Not compared", "<details>", "not a statement of parity"} {
		if strings.Contains(out, absent) {
			t.Errorf("a clean report contains %q:\n%s", absent, out)
		}
	}
}

func TestMarkdownEscaping(t *testing.T) {
	codeTests := []struct{ in, want string }{
		{"", ""},
		{"/plain", "`/plain`"},
		{"/a|b", "`/a\\|b`"},
		{"/a`b", "``/a`b``"},
		{"`leading", "`` `leading ``"},
		{"/a``b`c", "```/a``b`c```"},
		{"line\nbreak", "`line break`"},
		{"<img src=x>", "`<img src=x>`"},
	}
	for _, tt := range codeTests {
		if got := mdCode(tt.in); got != tt.want {
			t.Errorf("mdCode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	textTests := []struct{ in, want string }{
		{"plain words", "plain words"},
		{"a|b", `a\|b`},
		{"<b>bold</b>", "&lt;b&gt;bold&lt;/b&gt;"},
		{"*x* _y_ `z`", "\\*x\\* \\_y\\_ \\`z\\`"},
		{"two\nlines", "two lines"},
	}
	for _, tt := range textTests {
		if got := mdText(tt.in); got != tt.want {
			t.Errorf("mdText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

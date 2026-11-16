//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/lab"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/rules"
	"github.com/asaleem9/routeparity/internal/source"
)

func exampleWorld(t *testing.T) *model.World {
	t.Helper()
	data, err := os.ReadFile("../examples/five-behaviours/ingress.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return loadWorld(t, string(data))
}

// compare runs both sides in the lab and returns each probe's comparison,
// keyed by request.
func compare(t *testing.T, w *model.World, candidate *lab.Candidate) map[string]model.Comparison {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	base, cand, err := lab.Run(ctx, w, candidate, lab.EnvoyGateway, labOptions(t))
	if err != nil {
		t.Fatalf("lab.Run: %v", err)
	}
	res := diff.Compare(base, cand, diff.Options{})
	rules.Annotate(res, w)
	if res.Summary.Error != 0 || res.Summary.Unrouted != 0 || res.Summary.Unverified != 0 {
		t.Errorf("summary = %+v; every probe should have been answered and compared", res.Summary)
	}
	for _, u := range res.Unverified {
		if u.Reason == lab.ReasonNotLoaded {
			t.Errorf("part of the configuration never took effect: %+v", u)
		}
	}
	if cand.Meta.Mode != model.ModeLab || cand.Meta.Versions["envoy-gateway"] != lab.EnvoyGateway.Version {
		t.Errorf("candidate metadata = %+v", cand.Meta)
	}
	out := map[string]model.Comparison{}
	for _, c := range res.Comparisons {
		out[c.Probe.Scheme+"://"+c.Probe.Host+c.Probe.RawTarget] = c
	}
	return out
}

type expectation struct {
	request string
	verdict model.Verdict
	// deltas are the fields that must differ, exactly, for a DIFF.
	deltas []string
}

func check(t *testing.T, got map[string]model.Comparison, want []expectation) {
	t.Helper()
	for _, w := range want {
		c, ok := got[w.request]
		if !ok {
			t.Errorf("%s: no comparison", w.request)
			continue
		}
		var fields []string
		for _, d := range c.Deltas {
			fields = append(fields, d.Field)
		}
		if c.Verdict != w.verdict {
			t.Errorf("%s: verdict %s (differs in %v), want %s", w.request, c.Verdict, fields, w.verdict)
			continue
		}
		if w.verdict == model.VerdictDiff && strings.Join(fields, ",") != strings.Join(w.deltas, ",") {
			t.Errorf("%s: differs in %v, want %v", w.request, fields, w.deltas)
		}
	}
}

// What the two tests below share: Envoy Gateway normalises a path before
// forwarding it and ingress-nginx forwards it as sent, and ingress-nginx adds
// an HSTS header that a Gateway does not. Neither depends on how the routes
// were written.
var alwaysDifferent = []expectation{
	{"http://regex-match.example.com//AAA", model.VerdictDiff, []string{"backend.rawTarget"}},
	{"http://trailing-slash.example.com/rp-x/../my-path/", model.VerdictDiff, []string{"backend.rawTarget"}},
	{"https://path-normalization.example.com/uuid", model.VerdictDiff, []string{"client.header.strict-transport-security"}},
}

// TestConvertedRoutes runs ingress2gateway's output for the example Ingresses
// on Envoy Gateway. The converter's own behaviour tests run on Istio; this is
// the same translation measured on a different implementation.
func TestConvertedRoutes(t *testing.T) {
	w := exampleWorld(t)
	manifests, err := convert.Manifests(w)
	if err != nil {
		t.Fatal(err)
	}
	out, err := convert.InProcess{}.Convert(context.Background(), bytes.NewReader(manifests), convert.Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := compare(t, w, &lab.Candidate{Gateways: out.Gateways, HTTPRoutes: out.HTTPRoutes, Other: out.Other, Source: out.Converter})

	check(t, got, []expectation{
		// The converter carries over the regex semantics, on every path of the host.
		{"http://regex-match.example.com/AAA", model.VerdictMatch, nil},
		{"http://regex-match.example.com/aaa", model.VerdictMatch, nil},
		{"http://regex-match.example.com/AAA/rp-extra", model.VerdictMatch, nil},
		{"http://regex-match.example.com/hEADER", model.VerdictMatch, nil},
		{"http://regex-match.example.com/Header/rp-extra", model.VerdictMatch, nil},
		// rewrite-target, including the regex matching it implies.
		{"http://rewrite-target.example.com/IP", model.VerdictMatch, nil},
		{"http://rewrite-target.example.com/ip", model.VerdictMatch, nil},
		{"http://rewrite-target.example.com/IP/rp-extra", model.VerdictMatch, nil},
		// The trailing-slash redirect and the redirect to HTTPS.
		{"http://trailing-slash.example.com/my-path", model.VerdictMatch, nil},
		{"http://path-normalization.example.com/uuid", model.VerdictMatch, nil},
		{"http://unrouted.routeparity.invalid/rp-unrouted-control", model.VerdictMatch, nil},
	})
	check(t, got, alwaysDifferent)
}

// TestLiteralRoutes runs a by-hand translation that copies each Ingress path
// into an HTTPRoute match and adds nothing else. Each behaviour ingress-nginx
// supplies on its own shows up as a measured difference, on a probe that
// carries the rule describing it.
func TestLiteralRoutes(t *testing.T) {
	loaded, err := source.LoadPaths([]string{"../examples/five-behaviours-gateway"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := &lab.Candidate{
		Gateways: loaded.Objects.Gateways, HTTPRoutes: loaded.Objects.HTTPRoutes, Other: loaded.Objects.GatewayExtras,
		Source: "the literal example",
	}
	got := compare(t, exampleWorld(t), candidate)

	unrouted := []string{"routed", "client.status", "backend.reached"}
	check(t, got, []expectation{
		// The paths as written work, which is why this translation looks right.
		{"http://regex-match.example.com/AAA", model.VerdictMatch, nil},
		{"http://regex-match.example.com/Header", model.VerdictMatch, nil},
		{"http://rewrite-target.example.com/IP", model.VerdictMatch, nil},
		{"http://trailing-slash.example.com/my-path/", model.VerdictMatch, nil},
		// Requests ingress-nginx serves and the literal routes do not.
		{"http://regex-match.example.com/aaa", model.VerdictDiff, unrouted},
		{"http://regex-match.example.com/AAA/rp-extra", model.VerdictDiff, unrouted},
		{"http://regex-match.example.com/hEADER", model.VerdictDiff, unrouted},
		{"http://rewrite-target.example.com/ip", model.VerdictDiff, unrouted},
		{"http://trailing-slash.example.com/my-path", model.VerdictDiff, []string{"routed", "client.status", "client.location"}},
		// No redirect to HTTPS: the request is served over plain HTTP.
		{"http://path-normalization.example.com/uuid", model.VerdictDiff, []string{"client.status", "client.location", "backend.reached"}},
	})
	check(t, got, alwaysDifferent)

	// Every difference on a rule's probe names that rule.
	rules := map[string]string{
		"http://regex-match.example.com/aaa":        "NGX-REGEX-001",
		"http://regex-match.example.com/hEADER":     "NGX-REGEX-002",
		"http://rewrite-target.example.com/ip":      "NGX-REGEX-003",
		"http://trailing-slash.example.com/my-path": "NGX-SLASH-001",
		"http://regex-match.example.com//AAA":       "NGX-NORM-001",
	}
	for request, rule := range rules {
		c := got[request]
		if c.Probe.Origin.RuleID != rule {
			t.Errorf("%s is attributed to %q, want %s", request, c.Probe.Origin.RuleID, rule)
		}
		if c.Finding == nil || c.Finding.RuleID != rule || c.Finding.Fix == nil || c.Finding.Fix.Summary == "" {
			t.Errorf("%s finding = %+v, want a confirmed %s fix", request, c.Finding, rule)
		}
	}
	// These probes are taken from the configuration, so the rule is not on
	// the probe. The difference itself is what identifies it.
	for request, rule := range map[string]string{
		"http://path-normalization.example.com/uuid":  "NGX-TLS-001",
		"https://path-normalization.example.com/uuid": "NGX-TLS-002",
	} {
		c := got[request]
		if c.Finding == nil || c.Finding.RuleID != rule || c.Finding.Fix == nil {
			t.Errorf("%s finding = %+v, want a %s fix", request, c.Finding, rule)
		}
	}
}

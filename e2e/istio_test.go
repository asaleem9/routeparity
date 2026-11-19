//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/asaleem9/routeparity/internal/lab"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/source"
)

func TestIstioConvertedRoutes(t *testing.T) {
	w := exampleWorld(t)
	res, got := compareImplementation(t, w, convertedCandidate(t, w), lab.Istio)
	check(t, got, []expectation{
		{"http://regex-match.example.com/aaa", model.VerdictMatch, nil},
		{"http://regex-match.example.com/hEADER", model.VerdictMatch, nil},
		{"http://rewrite-target.example.com/ip", model.VerdictMatch, nil},
		{"http://trailing-slash.example.com/my-path", model.VerdictMatch, nil},
		{"http://path-normalization.example.com/uuid", model.VerdictMatch, nil},
		{"https://path-normalization.example.com/uuid", model.VerdictDiff, []string{"client.header.strict-transport-security"}},
		{"http://trailing-slash.example.com/./my-path/", model.VerdictDiff, []string{"backend.rawTarget"}},
		{"http://trailing-slash.example.com//my-path/", model.VerdictDiff, []string{"routed", "client.status", "backend.reached"}},
	})
	for _, u := range res.Unverified {
		if u.Reason == lab.ReasonNotLoaded {
			t.Errorf("Istio rejected configuration: %+v", u)
		}
	}
}

func TestIstioLiteralRoutes(t *testing.T) {
	loaded, err := source.LoadPaths([]string{"../examples/five-behaviours-gateway"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, got := compareImplementation(t, exampleWorld(t), &lab.Candidate{Gateways: loaded.Objects.Gateways, HTTPRoutes: loaded.Objects.HTTPRoutes, Other: loaded.Objects.GatewayExtras, Source: "the literal example"}, lab.Istio)
	check(t, got, []expectation{
		{"http://regex-match.example.com/AAA", model.VerdictMatch, nil},
		{"http://regex-match.example.com/aaa", model.VerdictDiff, []string{"routed", "client.status", "backend.reached"}},
		{"http://trailing-slash.example.com/my-path", model.VerdictDiff, []string{"routed", "client.status", "client.location"}},
	})
	c := got["http://regex-match.example.com/aaa"]
	if c.Finding == nil || c.Finding.Fix == nil || c.Finding.Fix.Implementation != "istio" || !strings.Contains(c.Finding.Fix.Snippet, "(?i)") {
		t.Errorf("missing Istio regex fix: %+v", c.Finding)
	}
}

// Current ingress2gateway warns and omits unsupported capture rewrites.
// Keep the unverified accounting explicit, rather than silently claiming parity.
func TestIstioCaptureRewriteIsUnverified(t *testing.T) {
	loaded, err := source.LoadPaths([]string{"../examples/capture-rewrite"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := model.NewWorld(loaded.Objects, model.WorldOptions{})
	res, got := compareImplementation(t, w, convertedCandidate(t, w), lab.Istio)
	found := false
	for _, c := range got {
		if c.Probe.Origin.Source != model.OriginDerived {
			continue
		}
		for _, d := range c.Deltas {
			if d.Field == "backend.rawTarget" && d.Candidate == "/items/x" && d.Baseline == "/x" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("unsupported capture rewrite behavior changed; update the reproduction")
	}
	warned := false
	for _, u := range res.Unverified {
		if u.Reason == model.ReasonNotConverted && strings.Contains(u.Feature, "capture group references are not supported") {
			warned = true
		}
	}
	if !warned || res.Clean() {
		t.Fatal("unsupported capture rewrite was not reported as unverified")
	}
}

func TestIstioIndependentGateways(t *testing.T) {
	w := independentGatewayWorld(t)
	res, got := compareImplementation(t, w, convertedCandidate(t, w), lab.Istio)
	check(t, got, []expectation{{"http://one.example.com/one", model.VerdictMatch, nil}, {"http://two.example.com/two", model.VerdictMatch, nil}})
	for _, u := range res.Unverified {
		if u.Reason == lab.ReasonNotLoaded {
			t.Errorf("Gateway configuration did not load: %+v", u)
		}
	}
}

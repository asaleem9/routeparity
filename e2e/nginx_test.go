//go:build e2e

package e2e

import (
	"bytes"
	"context"
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

func convertedCandidate(t *testing.T, w *model.World) *lab.Candidate {
	t.Helper()
	manifests, err := convert.Manifests(w)
	if err != nil {
		t.Fatal(err)
	}
	out, err := convert.InProcess{}.Convert(context.Background(), bytes.NewReader(manifests), convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := &lab.Candidate{Gateways: out.Gateways, HTTPRoutes: out.HTTPRoutes, Other: out.Other, Source: out.Converter}
	for _, notice := range out.Notices {
		if notice.Level != "WARNING" && notice.Level != "ERROR" {
			continue
		}
		u := model.Unverified{Reason: model.ReasonNotConverted, Feature: notice.Message, Detail: "reported by ingress2gateway"}
		if len(notice.Objects) > 0 {
			ref := notice.Objects[0]
			u.Object = &ref
		}
		candidate.Unverified = append(candidate.Unverified, u)
	}
	return candidate
}

func compareImplementation(t *testing.T, w *model.World, c *lab.Candidate, impl lab.Implementation) (*diff.Result, map[string]model.Comparison) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	opts := labOptions(t)
	opts.ClusterName = clusterName() + "-" + impl.Name
	base, cand, err := lab.Run(ctx, w, c, impl, opts)
	if err != nil {
		t.Fatal(err)
	}
	if cand.Meta.Versions[impl.Name] != impl.Version {
		t.Fatalf("wrong implementation metadata: %+v", cand.Meta)
	}
	res := diff.Compare(base, cand, diff.Options{})
	rules.Annotate(res, w)
	if res.Summary.Error != 0 || res.Summary.Unrouted != 0 || res.Summary.Unverified != 0 {
		t.Errorf("unanswered or unverified probes: %+v", res.Summary)
	}
	got := map[string]model.Comparison{}
	for _, c := range res.Comparisons {
		got[c.Probe.Scheme+"://"+c.Probe.Host+c.Probe.RawTarget] = c
	}
	return res, got
}

func compareNGINX(t *testing.T, w *model.World, c *lab.Candidate) (*diff.Result, map[string]model.Comparison) {
	t.Helper()
	return compareImplementation(t, w, c, lab.NGINXGatewayFabric)
}

func TestNGINXGatewayFabricConvertedRoutes(t *testing.T) {
	w := exampleWorld(t)
	res, got := compareNGINX(t, w, convertedCandidate(t, w))
	check(t, got, []expectation{
		{"http://trailing-slash.example.com/my-path", model.VerdictMatch, nil},
		{"http://trailing-slash.example.com//my-path/", model.VerdictMatch, nil},
		{"http://path-normalization.example.com/uuid", model.VerdictMatch, nil},
		{"https://path-normalization.example.com/uuid", model.VerdictDiff, []string{"client.header.strict-transport-security"}},
		{"http://regex-match.example.com/AAA", model.VerdictDiff, []string{"routed", "client.status", "backend.reached"}},
	})
	// The standard emitter's (?i) paths are rejected by this pinned NGF. A
	// rejected route must remain visible even if another route serves the host.
	rejected := map[string]bool{}
	for _, u := range res.Unverified {
		if u.Reason == lab.ReasonNotLoaded && u.Object != nil && strings.Contains(u.Detail, "UnsupportedValue") {
			rejected[u.Object.Name] = true
		}
	}
	for _, name := range []string{"regex-match-regex-match-example-com", "rewrite-target-rewrite-target-example-com"} {
		if !rejected[name] {
			t.Errorf("rejected route %s missing from not-verified: %+v", name, res.Unverified)
		}
	}
	if res.Clean() {
		t.Fatal("rejected routes were reported as parity")
	}
}

func TestNGINXGatewayFabricLiteralRoutes(t *testing.T) {
	loaded, err := source.LoadPaths([]string{"../examples/five-behaviours-gateway"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, got := compareNGINX(t, exampleWorld(t), &lab.Candidate{Gateways: loaded.Objects.Gateways, HTTPRoutes: loaded.Objects.HTTPRoutes, Other: loaded.Objects.GatewayExtras, Source: "the literal example"})
	check(t, got, []expectation{
		{"http://rewrite-target.example.com/IP", model.VerdictMatch, nil},
		{"http://rewrite-target.example.com/ip", model.VerdictDiff, []string{"routed", "client.status", "backend.reached"}},
		{"http://trailing-slash.example.com/my-path/", model.VerdictMatch, nil},
		{"http://trailing-slash.example.com//my-path/", model.VerdictMatch, nil},
		{"http://trailing-slash.example.com/my-path", model.VerdictMatch, nil},
	})
	c := got["http://rewrite-target.example.com/ip"]
	if c.Finding == nil || c.Finding.Fix == nil || c.Finding.Fix.Implementation != "nginx" || c.Finding.Fix.Snippet != "" {
		t.Errorf("NGF received an Envoy-specific fix: %+v", c.Finding)
	}
}

func independentGatewayWorld(t *testing.T) *model.World {
	t.Helper()
	const manifests = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: one, namespace: one}
spec:
  ingressClassName: nginx
  rules:
  - host: one.example.com
    http:
      paths:
      - path: /one
        pathType: Exact
        backend: {service: {name: one, port: {number: 80}}}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: two, namespace: two}
spec:
  ingressClassName: nginx
  rules:
  - host: two.example.com
    http:
      paths:
      - path: /two
        pathType: Exact
        backend: {service: {name: two, port: {number: 80}}}
`
	return loadWorld(t, manifests)
}

func TestNGINXGatewayFabricIndependentGateways(t *testing.T) {
	w := independentGatewayWorld(t)
	c := convertedCandidate(t, w)
	if len(c.Gateways) != 2 {
		t.Fatalf("need two independent Gateways, got %d", len(c.Gateways))
	}
	res, got := compareNGINX(t, w, c)
	check(t, got, []expectation{{"http://one.example.com/one", model.VerdictMatch, nil}, {"http://two.example.com/two", model.VerdictMatch, nil}})
	for _, u := range res.Unverified {
		if u.Reason == lab.ReasonNotLoaded {
			t.Errorf("Gateway configuration did not load: %+v", u)
		}
	}
}

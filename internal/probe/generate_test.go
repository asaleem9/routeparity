package probe

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/source"
)

// ingress renders one Ingress. annotations are "name: value" pairs under the
// ingress-nginx prefix; paths are "path|PathType".
func ingress(name, host string, annotations []string, tls bool, paths ...string) string {
	var b strings.Builder
	b.WriteString("---\napiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: " + name + "\n  namespace: demo\n")
	if len(annotations) > 0 {
		b.WriteString("  annotations:\n")
		for _, a := range annotations {
			b.WriteString("    nginx.ingress.kubernetes.io/" + a + "\n")
		}
	}
	b.WriteString("spec:\n  ingressClassName: nginx\n")
	if tls {
		b.WriteString("  tls:\n  - hosts: [\"" + host + "\"]\n    secretName: cert\n")
	}
	b.WriteString("  rules:\n  - ")
	if host != "" {
		b.WriteString("host: \"" + host + "\"\n    ")
	}
	b.WriteString("http:\n      paths:\n")
	for _, p := range paths {
		path, pathType, _ := strings.Cut(p, "|")
		b.WriteString("      - path: \"" + path + "\"\n        pathType: " + pathType +
			"\n        backend:\n          service: {name: web, port: {number: 80}}\n")
	}
	return b.String()
}

func world(t *testing.T, manifests ...string) *model.World {
	t.Helper()
	res, err := source.Load(strings.NewReader(strings.Join(manifests, "")), "test")
	if err != nil {
		t.Fatal(err)
	}
	return model.NewWorld(res.Objects, model.WorldOptions{})
}

// requests lists probes as "scheme://host/target [source rule mutation]".
func requests(probes []model.Probe) []string {
	var out []string
	for _, p := range probes {
		label := string(p.Origin.Source)
		if p.Origin.RuleID != "" {
			label += " " + p.Origin.RuleID
		}
		if p.Origin.Mutation != "" {
			label += " " + p.Origin.Mutation
		}
		out = append(out, p.Scheme+"://"+p.Host+p.RawTarget+" ["+label+"]")
	}
	return out
}

func TestGenerateDerivedProbes(t *testing.T) {
	w := world(t,
		ingress("plain", "shop.example.com", nil, false, "/exact|Exact", "/prefix|Prefix", "/impl|ImplementationSpecific"),
		ingress("secure", "secure.example.com", nil, true, "/|Prefix"),
		ingress("wild", "*.apps.example.com", nil, false, "/w|Exact"),
	)
	got := requests(Generate(w, Options{}).Probes)
	want := []string{
		// A wildcard host is probed with a concrete name under it.
		"http://rp-wildcard.apps.example.com/w [derived]",
		// A TLS host is probed over both schemes; a Prefix path also below the path.
		"http://secure.example.com/ [derived]",
		"https://secure.example.com/ [derived]",
		"http://secure.example.com/rp-sub [derived]",
		"https://secure.example.com/rp-sub [derived]",
		"http://shop.example.com/exact [derived]",
		"http://shop.example.com/impl [derived]",
		"http://shop.example.com/prefix [derived]",
		"http://shop.example.com/prefix/rp-sub [derived]",
		"http://unrouted.routeparity.invalid/rp-unrouted-control [control unrouted]",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("probes without mutations (-want +got):\n%s", diff)
	}
}

func TestGenerateMutationsAreOptIn(t *testing.T) {
	w := world(t, ingress("regex", "r.example.com", []string{`use-regex: "true"`}, false, "/api/v[0-9]+|ImplementationSpecific"))

	for _, p := range Generate(w, Options{}).Probes {
		if p.Origin.Source == model.OriginMutation {
			t.Errorf("mutation %s generated without Mutations", p.RawTarget)
		}
		if p.Safety != model.SafetySafe {
			t.Errorf("%s has safety %q; default probes must be safe", p.RawTarget, p.Safety)
		}
	}

	got := requests(Generate(w, Options{Mutations: true}).Probes)
	want := []string{
		"http://r.example.com/%61pi/v0 [mutation NGX-NORM-001 percent-encoded]",
		"http://r.example.com/./api/v0 [mutation NGX-NORM-001 current-dir]",
		"http://r.example.com//api/v0 [mutation NGX-NORM-001 double-slash]",
		"http://r.example.com/API/V0 [mutation NGX-REGEX-001 case-flip]",
		// On a regex host the configured pattern is probed with a path it matches.
		"http://r.example.com/api/v0 [derived]",
		"http://r.example.com/api/v0/rp-extra [mutation NGX-REGEX-001 longer-path]",
		"http://r.example.com/rp-x/../api/v0 [mutation NGX-NORM-001 dot-segment]",
		"http://unrouted.routeparity.invalid/rp-unrouted-control [control unrouted]",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("probes with mutations (-want +got):\n%s", diff)
	}
}

func TestGenerateReportsPatternsItCannotProbe(t *testing.T) {
	// A lookahead is valid in nginx (PCRE) and not in Go's regexp.
	w := world(t, ingress("pcre", "r.example.com", []string{`use-regex: "true"`}, false,
		"/ok/[a-z]+|ImplementationSpecific", "/foo(?=bar)|ImplementationSpecific"))
	set := Generate(w, Options{Mutations: true})

	for _, p := range set.Probes {
		if strings.Contains(p.RawTarget, "foo") {
			t.Errorf("probe %s generated for a pattern that cannot be sampled", p.RawTarget)
		}
	}
	if len(set.Unverified) != 1 {
		t.Fatalf("unverified = %+v, want exactly one entry", set.Unverified)
	}
	u := set.Unverified[0]
	if u.Reason != model.ReasonNoProbe || !strings.Contains(u.Feature, "/foo(?=bar)") || u.Object == nil || u.Object.Name != "pcre" {
		t.Errorf("unverified entry = %+v, want a no-probe entry naming the pattern and its Ingress", u)
	}
}

func TestGenerateSkipsCanaries(t *testing.T) {
	w := world(t,
		ingress("main", "c.example.com", nil, false, "/app|Exact"),
		ingress("canary", "c.example.com", []string{`canary: "true"`, `canary-weight: "20"`}, false, "/app|Exact", "/canary-only|Exact"),
	)
	for _, p := range Generate(w, Options{Mutations: true}).Probes {
		if strings.Contains(p.RawTarget, "canary-only") {
			t.Errorf("probe %s generated from a canary Ingress, which defines no paths of its own", p.RawTarget)
		}
	}
}

func TestGenerateHasNoUnroutedControlWhenEverythingIsRouted(t *testing.T) {
	w := world(t, ingress("catch-all", "", nil, false, "/|Prefix"))
	set := Generate(w, Options{})
	for _, p := range set.Probes {
		if p.Origin.Source == model.OriginControl {
			t.Error("a control probe was generated although a rule without a host routes every request")
		}
		if p.Host == "" {
			t.Error("a probe has no Host")
		}
	}
	if len(set.Probes) == 0 {
		t.Error("no probes for a rule without a host")
	}
}

func TestGenerateIsDeterministicAndUnique(t *testing.T) {
	manifests := []string{
		ingress("a", "one.example.com", []string{`use-regex: "true"`}, false, "/x/[0-9]+|ImplementationSpecific", "/y|Exact"),
		ingress("b", "one.example.com", nil, true, "/y|Exact", "/z/|Prefix"),
		ingress("c", "two.example.com", nil, false, "/z/|Prefix", "/z|Exact"),
	}
	first := Generate(world(t, manifests...), Options{Mutations: true})
	second := Generate(world(t, manifests...), Options{Mutations: true})
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("two runs over the same input differ (-first +second):\n%s", diff)
	}

	seen := map[string]bool{}
	for _, p := range first.Probes {
		if p.ID == "" {
			t.Errorf("probe %s has no ID", p.RawTarget)
		}
		if seen[p.ID] {
			t.Errorf("probe %s://%s%s appears twice", p.Scheme, p.Host, p.RawTarget)
		}
		seen[p.ID] = true
	}
}

func TestDerivedWinsOverAnEqualMutation(t *testing.T) {
	// /app/ with its slash stripped is /app, which the configuration also
	// lists. The request is sent once and recorded as a configured path.
	w := world(t, ingress("both", "s.example.com", nil, false, "/app/|Prefix", "/app|Exact"))
	for _, p := range Generate(w, Options{Mutations: true}).Probes {
		if p.RawTarget == "/app" && p.Origin.Source != model.OriginDerived {
			t.Errorf("/app is recorded as %q, want derived", p.Origin.Source)
		}
	}
}

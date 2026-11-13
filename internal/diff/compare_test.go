package diff

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/asaleem9/routeparity/internal/model"
)

// Fixed response bodies, named for what they stand for.
const (
	nginx404 = "hash-of-nginx-default-backend-404"
	envoy404 = "hash-of-envoy-empty-404"
	appBody  = "hash-of-an-application-response"
)

func derived(t *testing.T, target string) model.Probe {
	t.Helper()
	p := model.Probe{Host: "shop.example.com", RawTarget: target, Origin: model.Origin{Source: model.OriginDerived}}
	if err := p.Seal(); err != nil {
		t.Fatal(err)
	}
	return p
}

func mutated(t *testing.T, target string) model.Probe {
	t.Helper()
	p := model.Probe{Host: "shop.example.com", RawTarget: target,
		Origin: model.Origin{Source: model.OriginMutation, RuleID: "NGX-REGEX-001", Mutation: "case-flip"}}
	if err := p.Seal(); err != nil {
		t.Fatal(err)
	}
	return p
}

func control(t *testing.T) model.Probe {
	t.Helper()
	p := model.Probe{Host: "unrouted.routeparity.invalid", RawTarget: "/rp-unrouted-control",
		Origin: model.Origin{Source: model.OriginControl}}
	if err := p.Seal(); err != nil {
		t.Fatal(err)
	}
	return p
}

type obsOpt func(*model.Observation)

func status(code int) obsOpt { return func(o *model.Observation) { o.Client.Status = code } }
func body(hash string) obsOpt {
	return func(o *model.Observation) { o.Client.BodySHA256 = hash }
}
func header(name, value string) obsOpt {
	return func(o *model.Observation) {
		o.Client.Headers = append(o.Client.Headers, model.Header{Name: name, Value: value})
	}
}
func failed(class string) obsOpt {
	return func(o *model.Observation) { o.Client.Error, o.Client.Status = class, 0 }
}
func backend(service, target string, headers ...model.Header) obsOpt {
	return func(o *model.Observation) {
		o.Backend = &model.BackendView{Namespace: "demo", Service: service, Port: "80", Method: "GET",
			RawTarget: target, Host: "shop.example.com", Headers: headers}
	}
}
func backendHost(host string) obsOpt { return func(o *model.Observation) { o.Backend.Host = host } }

func observe(p model.Probe, opts ...obsOpt) model.Observation {
	o := model.Observation{ProbeID: p.ID, Attempt: 1, Client: model.ClientView{Status: 200}}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// pair builds a baseline and a candidate snapshot for one probe, each with a
// control probe answered by that proxy's own 404.
func pair(t *testing.T, p model.Probe, base, cand []obsOpt) (*model.Snapshot, *model.Snapshot) {
	t.Helper()
	ctl := control(t)
	b := &model.Snapshot{
		Meta:         model.TargetMeta{Name: "old"},
		Probes:       []model.Probe{p, ctl},
		Observations: []model.Observation{observe(p, base...), observe(ctl, status(404), body(nginx404))},
	}
	c := &model.Snapshot{
		Meta:         model.TargetMeta{Name: "new"},
		Probes:       []model.Probe{p, ctl},
		Observations: []model.Observation{observe(p, cand...), observe(ctl, status(404), body(envoy404))},
	}
	return b, c
}

func fields(deltas []model.Delta) []string {
	var out []string
	for _, d := range deltas {
		out = append(out, d.Field)
	}
	return out
}

func TestCompareVerdicts(t *testing.T) {
	web := func(target string) obsOpt { return backend("web", target) }

	tests := []struct {
		name           string
		probe          model.Probe
		base, cand     []obsOpt
		opts           Options
		want           model.Verdict
		wantDeltas     []string
		wantSuppressed []string
	}{
		{
			name:  "same backend, same request",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a")}, cand: []obsOpt{web("/a")},
			want: model.VerdictMatch,
		},
		{
			name:  "one proxy routes it, the other does not",
			probe: mutated(t, "/UUID"),
			base:  []obsOpt{web("/UUID")}, cand: []obsOpt{status(404), body(envoy404)},
			want:       model.VerdictDiff,
			wantDeltas: []string{"routed", "client.status", "backend.reached"},
		},
		{
			name:  "same status, but the backend received a different path",
			probe: mutated(t, "/a/../b"),
			base:  []obsOpt{web("/a/../b")}, cand: []obsOpt{web("/b")},
			want:       model.VerdictDiff,
			wantDeltas: []string{"backend.rawTarget"},
		},
		{
			name:  "routed to a different service",
			probe: derived(t, "/api"),
			base:  []obsOpt{backend("api-v1", "/api")}, cand: []obsOpt{backend("api-v2", "/api")},
			want:       model.VerdictDiff,
			wantDeltas: []string{"backend.service"},
		},
		{
			name:  "the backend sees a different Host",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), backendHost("internal.svc")}, cand: []obsOpt{web("/a")},
			want:       model.VerdictDiff,
			wantDeltas: []string{"backend.host"},
		},
		{
			name:  "a mutation neither proxy routes is agreement",
			probe: mutated(t, "/nope"),
			base:  []obsOpt{status(404), body(nginx404)}, cand: []obsOpt{status(404), body(envoy404)},
			want: model.VerdictMatch,
		},
		{
			name:  "a configured path neither proxy routes is not a match",
			probe: derived(t, "/configured"),
			base:  []obsOpt{status(404), body(nginx404)}, cand: []obsOpt{status(404), body(envoy404)},
			want: model.VerdictUnrouted,
		},
		{
			name:  "an application 404 on one side and a proxy 404 on the other",
			probe: derived(t, "/missing-page"),
			base:  []obsOpt{status(404), body(appBody)}, cand: []obsOpt{status(404), body(envoy404)},
			want:       model.VerdictDiff,
			wantDeltas: []string{"routed"},
		},
		{
			name:  "redirect on one side only",
			probe: mutated(t, "/app"),
			base:  []obsOpt{status(301), header("Location", "http://shop.example.com/app/")}, cand: []obsOpt{status(404), body(envoy404)},
			want:       model.VerdictDiff,
			wantDeltas: []string{"routed", "client.status", "client.location"},
		},
		{
			name:  "the same redirect written three ways",
			probe: mutated(t, "/app"),
			base:  []obsOpt{status(301), header("Location", "http://shop.example.com/app/")},
			cand:  []obsOpt{status(301), header("Location", "/app/")},
			want:  model.VerdictMatch, wantSuppressed: []string{"client.header.location"},
		},
		{
			name:  "a redirect that only differs by the port connected to",
			probe: mutated(t, "/app"),
			base:  []obsOpt{status(301), header("Location", "http://shop.example.com:18080/app/")},
			cand:  []obsOpt{status(301), header("Location", "http://shop.example.com:18081/app/")},
			want:  model.VerdictMatch, wantSuppressed: []string{"client.header.location"},
		},
		{
			name:  "a redirect to a different path",
			probe: mutated(t, "/app"),
			base:  []obsOpt{status(301), header("Location", "/app/")},
			cand:  []obsOpt{status(301), header("Location", "/application/")},
			want:  model.VerdictDiff, wantDeltas: []string{"client.location"},
		},
		{
			name:  "a redirect that changes scheme",
			probe: derived(t, "/secure"),
			base:  []obsOpt{status(308), header("Location", "https://shop.example.com/secure")},
			cand:  []obsOpt{status(308), header("Location", "http://shop.example.com/secure")},
			want:  model.VerdictDiff, wantDeltas: []string{"client.location"},
		},
		{
			name:  "proxy noise in response headers",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Server", "nginx"), header("Date", "Mon")},
			cand:  []obsOpt{web("/a"), header("Server", "envoy"), header("Date", "Tue"), header("X-Envoy-Upstream-Service-Time", "3")},
			want:  model.VerdictMatch, wantSuppressed: []string{"client.header.date", "client.header.server", "client.header.x-envoy-upstream-service-time"},
		},
		{
			name:  "echo bodies of different length",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Content-Length", "310")}, cand: []obsOpt{web("/a"), header("Content-Length", "275")},
			want: model.VerdictMatch,
		},
		{
			name:  "real responses of different length",
			probe: derived(t, "/a"),
			base:  []obsOpt{header("Content-Length", "310"), body("same")}, cand: []obsOpt{header("Content-Length", "275"), body("same")},
			want: model.VerdictMatch, wantSuppressed: []string{"client.header.content-length"},
		},
		{
			name:  "HSTS present on one side only",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Strict-Transport-Security", "max-age=31536000")}, cand: []obsOpt{web("/a")},
			want: model.VerdictDiff, wantDeltas: []string{"client.header.strict-transport-security"},
		},
		{
			name:  "CORS answer differs",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Access-Control-Allow-Origin", "https://app.example.com")},
			cand:  []obsOpt{web("/a"), header("Access-Control-Allow-Origin", "*")},
			want:  model.VerdictDiff, wantDeltas: []string{"client.header.access-control-allow-origin"},
		},
		{
			name:  "the same cookie with a different value",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Set-Cookie", "route=abc123; Path=/; HttpOnly")},
			cand:  []obsOpt{web("/a"), header("Set-Cookie", "route=zzz999; HttpOnly; Path=/")},
			want:  model.VerdictMatch,
		},
		{
			name:  "an affinity cookie that is no longer set",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Set-Cookie", "INGRESSCOOKIE=abc; Path=/; HttpOnly")}, cand: []obsOpt{web("/a")},
			want: model.VerdictDiff, wantDeltas: []string{"client.header.set-cookie"},
		},
		{
			name:  "a cookie that lost an attribute",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a"), header("Set-Cookie", "s=1; Secure; HttpOnly")},
			cand:  []obsOpt{web("/a"), header("Set-Cookie", "s=1; HttpOnly")},
			want:  model.VerdictDiff, wantDeltas: []string{"client.header.set-cookie"},
		},
		{
			name:  "forwarded headers differ between proxies",
			probe: derived(t, "/a"),
			base: []obsOpt{backend("web", "/a",
				model.Header{Name: "X-Forwarded-Host", Value: "shop.example.com"},
				model.Header{Name: "X-Forwarded-For", Value: "10.0.0.1"},
				model.Header{Name: "X-Real-Ip", Value: "10.0.0.1"})},
			cand: []obsOpt{backend("web", "/a",
				model.Header{Name: "X-Forwarded-For", Value: "10.0.9.9"})},
			want: model.VerdictMatch, wantSuppressed: []string{"backend.header.x-forwarded-host", "backend.header.x-real-ip"},
		},
		{
			name:  "real backends with different bodies, by default",
			probe: derived(t, "/uuid"),
			base:  []obsOpt{body("hash-a")}, cand: []obsOpt{body("hash-b")},
			want: model.VerdictMatch, wantSuppressed: []string{"client.body"},
		},
		{
			name:  "real backends with different bodies, when asked to compare them",
			probe: derived(t, "/static.css"),
			base:  []obsOpt{body("hash-a")}, cand: []obsOpt{body("hash-b")},
			opts: Options{CompareBody: true},
			want: model.VerdictDiff, wantDeltas: []string{"client.body"},
		},
		{
			name:  "one target could not be reached",
			probe: derived(t, "/a"),
			base:  []obsOpt{web("/a")}, cand: []obsOpt{failed("connection-refused")},
			want: model.VerdictError,
		},
		{
			name:  "both targets fail the same way",
			probe: derived(t, "/a"),
			base:  []obsOpt{failed("timeout")}, cand: []obsOpt{failed("timeout")},
			want: model.VerdictError, // identical failures are not parity
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, c := pair(t, tt.probe, tt.base, tt.cand)
			res := Compare(b, c, tt.opts)
			got := res.Comparisons[0]
			if got.Verdict != tt.want {
				t.Errorf("verdict = %s, want %s (reason %q, deltas %+v)", got.Verdict, tt.want, got.Reason, got.Deltas)
			}
			if diff := cmp.Diff(tt.wantDeltas, fields(got.Deltas)); diff != "" {
				t.Errorf("deltas (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantSuppressed, fields(got.Suppressed)); diff != "" {
				t.Errorf("suppressed (-want +got):\n%s", diff)
			}
			if got.Verdict != model.VerdictMatch && got.Verdict != model.VerdictDiff && got.Reason == "" {
				t.Errorf("%s verdict has no reason", got.Verdict)
			}
		})
	}
}

func TestCompareWithoutAControlFallsBackToStatus(t *testing.T) {
	p := derived(t, "/configured")
	b := &model.Snapshot{Probes: []model.Probe{p}, Observations: []model.Observation{observe(p, status(404), body(nginx404))}}
	c := &model.Snapshot{Probes: []model.Probe{p}, Observations: []model.Observation{observe(p, status(503), body(envoy404))}}
	got := Compare(b, c, Options{}).Comparisons[0]
	// Without a control there is no telling a proxy's 404 from an
	// application's, so the cautious reading is that neither side routed it.
	if got.Verdict != model.VerdictUnrouted {
		t.Errorf("verdict = %s, want UNROUTED", got.Verdict)
	}
}

func TestCompareIgnoresAControlThatWasRouted(t *testing.T) {
	// A catch-all route answers the control probe from a backend. That shows
	// nothing about what "unrouted" looks like, so it must not be used.
	p, ctl := derived(t, "/a"), control(t)
	snap := func() *model.Snapshot {
		return &model.Snapshot{
			Probes:       []model.Probe{p, ctl},
			Observations: []model.Observation{observe(p, backend("web", "/a")), observe(ctl, backend("web", "/rp-unrouted-control"))},
		}
	}
	res := Compare(snap(), snap(), Options{})
	for _, c := range res.Comparisons {
		if c.Verdict != model.VerdictMatch {
			t.Errorf("%s %s: verdict = %s, want MATCH", c.Probe.Origin.Source, c.Probe.RawTarget, c.Verdict)
		}
	}
}

func TestCompareAccountsForEveryProbe(t *testing.T) {
	sent, onlyBase, repeated := derived(t, "/sent"), derived(t, "/only-in-baseline"), derived(t, "/not-sent-anywhere")
	b := &model.Snapshot{
		Meta:         model.TargetMeta{Name: "old"},
		Probes:       []model.Probe{sent, onlyBase, repeated},
		Observations: []model.Observation{observe(sent, backend("web", "/sent")), observe(onlyBase, backend("web", "/only-in-baseline"))},
		Unverified:   []model.Unverified{{Reason: model.ReasonNoProbe, Feature: "shop.example.com /x(?=y)"}},
	}
	c := &model.Snapshot{
		Meta:         model.TargetMeta{Name: "new"},
		Probes:       []model.Probe{sent, repeated},
		Observations: []model.Observation{observe(sent, backend("web", "/sent"))},
		Unverified: []model.Unverified{
			{Reason: model.ReasonNoProbe, Feature: "shop.example.com /x(?=y)"}, // same as the baseline's
			{Reason: model.ReasonUnsupported, Feature: "https"},
		},
	}
	res := Compare(b, c, Options{})

	want := Summary{Probes: 3, Match: 1, Unverified: 2}
	if diff := cmp.Diff(want, res.Summary); diff != "" {
		t.Errorf("summary (-want +got):\n%s", diff)
	}
	if len(res.Unverified) != 2 {
		t.Errorf("unverified = %+v, want the two distinct entries with the shared one listed once", res.Unverified)
	}
	if res.Clean() {
		t.Error("Clean is true although probes went unverified")
	}
	if !res.Incomplete() {
		t.Error("Incomplete is false although probes went unverified")
	}
	if res.Baseline.Name != "old" || res.Candidate.Name != "new" {
		t.Errorf("metadata = %q, %q", res.Baseline.Name, res.Candidate.Name)
	}
}

func TestCleanOnlyWhenEverythingMatchedAndWasChecked(t *testing.T) {
	p := derived(t, "/a")
	b, c := pair(t, p, []obsOpt{backend("web", "/a")}, []obsOpt{backend("web", "/a")})
	res := Compare(b, c, Options{})
	if !res.Clean() || res.Incomplete() {
		t.Errorf("Clean = %v, Incomplete = %v for two identical snapshots; summary %+v", res.Clean(), res.Incomplete(), res.Summary)
	}

	c.Unverified = []model.Unverified{{Reason: model.ReasonBackendDependent, Feature: "auth-url"}}
	if res := Compare(b, c, Options{}); res.Clean() {
		t.Error("Clean is true although the candidate lists an unverified feature")
	}
}

func TestSuppressedTotalsAreCounted(t *testing.T) {
	p1, p2 := derived(t, "/a"), derived(t, "/b")
	ctl := control(t)
	mk := func(server string) *model.Snapshot {
		return &model.Snapshot{
			Probes: []model.Probe{p1, p2, ctl},
			Observations: []model.Observation{
				observe(p1, backend("web", "/a"), header("Server", server)),
				observe(p2, backend("web", "/b"), header("Server", server)),
				observe(ctl, status(404), body(server)),
			},
		}
	}
	res := Compare(mk("nginx"), mk("envoy"), Options{})
	if res.Suppressed["client.header.server"] != 2 {
		t.Errorf("suppressed totals = %v, want client.header.server counted on both routed probes", res.Suppressed)
	}
}

// attempts builds the observations for one probe sent several times.
func attempts(p model.Probe, each ...[]obsOpt) []model.Observation {
	var out []model.Observation
	for i, opts := range each {
		o := observe(p, opts...)
		o.Attempt = i + 1
		out = append(out, o)
	}
	return out
}

func TestCompareRepeatedProbes(t *testing.T) {
	main, canary := []obsOpt{backend("web", "/app")}, []obsOpt{backend("web-canary", "/app")}
	failing := []obsOpt{status(503), body("upstream-error")}

	tests := []struct {
		name       string
		base, cand [][]obsOpt
		want       model.Verdict
		wantReason string
	}{
		{
			name: "both sides answer the same way every time",
			base: [][]obsOpt{main, main, main}, cand: [][]obsOpt{main, main, main},
			want: model.VerdictMatch,
		},
		{
			name: "both sides are stable and differ",
			base: [][]obsOpt{main, main}, cand: [][]obsOpt{canary, canary},
			want: model.VerdictDiff,
		},
		{
			name: "a weighted canary on the baseline",
			base: [][]obsOpt{main, canary, main, main}, cand: [][]obsOpt{main, main, main, main},
			want: model.VerdictUnverified, wantReason: "the baseline gave 2 different answers to 4 identical requests",
		},
		{
			name: "a flaky candidate that happens to match on the first attempt",
			base: [][]obsOpt{main, main, main}, cand: [][]obsOpt{main, failing, main},
			want: model.VerdictUnverified, wantReason: "the candidate gave 2 different answers to 3 identical requests",
		},
		{
			name: "both sides unstable",
			base: [][]obsOpt{main, canary}, cand: [][]obsOpt{canary, main},
			want: model.VerdictUnverified, wantReason: "the baseline gave 2 different answers to 2 identical requests and the candidate gave 2",
		},
		{
			name: "a connection that drops on one attempt",
			base: [][]obsOpt{main, main}, cand: [][]obsOpt{main, {failed("connection-closed")}},
			want: model.VerdictUnverified, wantReason: "the candidate gave 2 different answers",
		},
		{
			name: "differences that are only noise do not make a side unstable",
			base: [][]obsOpt{{backend("web", "/app"), header("Date", "Mon")}, {backend("web", "/app"), header("Date", "Tue")}},
			cand: [][]obsOpt{main, main},
			want: model.VerdictMatch,
		},
		{
			name: "one side sampled once, the other several times",
			base: [][]obsOpt{main}, cand: [][]obsOpt{main, main, main},
			want: model.VerdictMatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := derived(t, "/app")
			p.Repeat = len(tt.base)
			ctl := control(t)
			b := &model.Snapshot{Probes: []model.Probe{p, ctl},
				Observations: append(attempts(p, tt.base...), observe(ctl, status(404), body(nginx404)))}
			c := &model.Snapshot{Probes: []model.Probe{p, ctl},
				Observations: append(attempts(p, tt.cand...), observe(ctl, status(404), body(envoy404)))}
			got := Compare(b, c, Options{}).Comparisons[0]
			if got.Verdict != tt.want {
				t.Errorf("verdict = %s, want %s (reason %q)", got.Verdict, tt.want, got.Reason)
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestHeaderPolicy(t *testing.T) {
	p := derived(t, "/a")
	hsts := []obsOpt{backend("web", "/a"), header("Strict-Transport-Security", "max-age=1"), header("X-Frame-Options", "DENY")}
	bare := []obsOpt{backend("web", "/a")}

	t.Run("default", func(t *testing.T) {
		b, c := pair(t, p, hsts, bare)
		got := Compare(b, c, Options{}).Comparisons[0]
		if diff := cmp.Diff([]string{"client.header.strict-transport-security"}, fields(got.Deltas)); diff != "" {
			t.Errorf("deltas (-want +got):\n%s", diff)
		}
	})
	t.Run("an ignored header is suppressed, and still recorded", func(t *testing.T) {
		b, c := pair(t, p, hsts, bare)
		res := Compare(b, c, Options{IgnoreHeaders: []string{"Strict-Transport-Security"}})
		got := res.Comparisons[0]
		if got.Verdict != model.VerdictMatch {
			t.Errorf("verdict = %s, want MATCH", got.Verdict)
		}
		if res.Suppressed["client.header.strict-transport-security"] != 1 {
			t.Errorf("suppressed = %v, want the ignored header counted", res.Suppressed)
		}
	})
	t.Run("an extra header can be made significant", func(t *testing.T) {
		b, c := pair(t, p, hsts, bare)
		got := Compare(b, c, Options{CompareHeaders: []string{" x-frame-options "}}).Comparisons[0]
		want := []string{"client.header.strict-transport-security", "client.header.x-frame-options"}
		if diff := cmp.Diff(want, fields(got.Deltas)); diff != "" {
			t.Errorf("deltas (-want +got):\n%s", diff)
		}
	})
	t.Run("ignoring Location", func(t *testing.T) {
		b, c := pair(t, mutated(t, "/app"),
			[]obsOpt{status(301), header("Location", "/app/")}, []obsOpt{status(301), header("Location", "/other/")})
		if got := Compare(b, c, Options{}).Comparisons[0]; got.Verdict != model.VerdictDiff {
			t.Fatalf("verdict = %s without the option, want DIFF", got.Verdict)
		}
		got := Compare(b, c, Options{IgnoreHeaders: []string{"location"}}).Comparisons[0]
		if got.Verdict != model.VerdictMatch || len(got.Suppressed) != 1 {
			t.Errorf("verdict = %s, suppressed = %+v; want MATCH with the redirect recorded as suppressed", got.Verdict, got.Suppressed)
		}
	})
}

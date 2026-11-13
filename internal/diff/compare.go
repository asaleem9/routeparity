// Package diff compares two snapshots probe by probe.
//
// The worst outcome for this tool is a false MATCH, so the rules here lean
// one way: a difference that might matter is reported, a difference that is
// treated as noise is still recorded and counted, and agreement that proves
// nothing about routing is not called a match.
package diff

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/asaleem9/routeparity/internal/model"
)

// Options controls a comparison.
type Options struct {
	// CompareBody reports differing response bodies as differences. It is off
	// by default because real backends put request-specific data in bodies;
	// with it off, a differing body is recorded as suppressed.
	CompareBody bool
	// IgnoreHeaders are response headers whose differences are never counted,
	// including ones that are significant by default. They are still recorded
	// as suppressed, so ignoring a header is visible in every report.
	IgnoreHeaders []string
	// CompareHeaders are extra response headers to treat as significant.
	CompareHeaders []string
}

// headerPolicy decides which response headers count.
type headerPolicy struct {
	ignore  map[string]bool
	compare map[string]bool
}

func (o Options) headers() headerPolicy {
	p := headerPolicy{ignore: map[string]bool{}, compare: map[string]bool{}}
	for _, name := range o.IgnoreHeaders {
		p.ignore[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range o.CompareHeaders {
		p.compare[strings.ToLower(strings.TrimSpace(name))] = true
	}
	return p
}

// significant reports whether a difference in the named header is a
// divergence. name is lower case.
func (p headerPolicy) significant(name string) bool {
	if p.ignore[name] {
		return false
	}
	return name == "location" || significantHeaders[name] || p.compare[name]
}

// Summary counts verdicts.
type Summary struct {
	Probes     int `json:"probes"`
	Match      int `json:"match"`
	Diff       int `json:"diff"`
	Unrouted   int `json:"unrouted"`
	Unverified int `json:"unverified"`
	Error      int `json:"error"`
}

// Result is the outcome of comparing a candidate snapshot against a baseline.
type Result struct {
	Baseline    model.TargetMeta   `json:"baseline"`
	Candidate   model.TargetMeta   `json:"candidate"`
	Summary     Summary            `json:"summary"`
	Comparisons []model.Comparison `json:"comparisons"`
	// Unverified lists what neither snapshot could check, from both.
	Unverified []model.Unverified `json:"unverified,omitempty"`
	// Suppressed counts, per field, the differences that were recorded and
	// not treated as divergences.
	Suppressed map[string]int `json:"suppressed,omitempty"`
}

// Clean reports whether the comparison found nothing wrong and left nothing
// unchecked. Only a clean result supports a statement of parity.
func (r *Result) Clean() bool {
	return r.Summary.Diff == 0 && !r.Incomplete()
}

// Incomplete reports whether anything could not be verified.
func (r *Result) Incomplete() bool {
	return r.Summary.Unrouted > 0 || r.Summary.Unverified > 0 || r.Summary.Error > 0 || len(r.Unverified) > 0
}

// significantHeaders are response headers whose difference is always a
// divergence: they change what a client does next or what it is allowed to do.
// Location is handled separately because it needs resolving first.
var significantHeaders = map[string]bool{
	"set-cookie":                       true,
	"strict-transport-security":        true,
	"www-authenticate":                 true,
	"access-control-allow-origin":      true,
	"access-control-allow-methods":     true,
	"access-control-allow-headers":     true,
	"access-control-allow-credentials": true,
	"access-control-expose-headers":    true,
	"access-control-max-age":           true,
}

// forwardedHeaders are the request headers a proxy adds for the backend. Two
// different proxies never agree on all of them, so a difference is recorded
// as suppressed and surfaced as a total, not reported on every probe.
var forwardedHeaders = []string{
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port", "X-Forwarded-Proto",
	"X-Forwarded-Prefix", "X-Forwarded-Scheme", "X-Real-Ip", "X-Scheme", "X-Original-Uri",
}

// Compare compares the candidate against the baseline. The baseline's probes
// define what is compared, in their order.
func Compare(base, cand *model.Snapshot, opts Options) *Result {
	res := &Result{
		Baseline:   base.Meta,
		Candidate:  cand.Meta,
		Unverified: mergeUnverified(base.Unverified, cand.Unverified),
		Suppressed: map[string]int{},
	}

	baseObs, candObs := byProbe(base.Observations), byProbe(cand.Observations)
	baseSig, candSig := unroutedSignature(base), unroutedSignature(cand)

	for _, probe := range base.Probes {
		c := model.Comparison{Probe: probe}
		b, haveBase := baseObs[probe.ID]
		k, haveCand := candObs[probe.ID]
		switch {
		case !haveBase && !haveCand:
			c.Verdict, c.Reason = model.VerdictUnverified, "the probe was not sent to either target"
		case !haveBase:
			c.Verdict, c.Reason = model.VerdictUnverified, "the probe was not sent to the baseline"
		case !haveCand:
			c.Verdict, c.Reason = model.VerdictUnverified, "the probe was not sent to the candidate"
		default:
			// A target that answers the same request in more than one way
			// has no single behaviour to compare. That happens with weighted
			// canaries, rate limits and unhealthy backends, and comparing one
			// sample of each side would give a verdict that changes from run
			// to run.
			baseAnswers, candAnswers := distinctAnswers(probe, b, baseSig, opts), distinctAnswers(probe, k, candSig, opts)
			if baseAnswers > 1 || candAnswers > 1 {
				c.Verdict = model.VerdictUnverified
				c.Reason = unstableReason(baseAnswers, len(b), candAnswers, len(k))
			} else {
				compareOne(&c, b[0], k[0], baseSig, candSig, opts)
			}
		}
		for _, s := range c.Suppressed {
			res.Suppressed[s.Field]++
		}
		res.Comparisons = append(res.Comparisons, c)

		res.Summary.Probes++
		switch c.Verdict {
		case model.VerdictMatch:
			res.Summary.Match++
		case model.VerdictDiff:
			res.Summary.Diff++
		case model.VerdictUnrouted:
			res.Summary.Unrouted++
		case model.VerdictUnverified:
			res.Summary.Unverified++
		case model.VerdictError:
			res.Summary.Error++
		}
	}
	if len(res.Suppressed) == 0 {
		res.Suppressed = nil
	}
	return res
}

// distinctAnswers counts how many different answers one target gave to the
// attempts of one probe. Two attempts are the same answer when comparing them
// with each other finds no difference, so stability is judged by exactly the
// rules that judge parity.
func distinctAnswers(probe model.Probe, attempts []model.Observation, sig *signature, opts Options) int {
	var distinct []model.Observation
	for _, obs := range attempts {
		known := false
		for _, d := range distinct {
			if sameAnswer(probe, d, obs, sig, opts) {
				known = true
				break
			}
		}
		if !known {
			distinct = append(distinct, obs)
		}
	}
	return len(distinct)
}

func sameAnswer(probe model.Probe, a, b model.Observation, sig *signature, opts Options) bool {
	if a.Client.Error != "" || b.Client.Error != "" {
		return a.Client.Error == b.Client.Error
	}
	c := model.Comparison{Probe: probe}
	compareOne(&c, a, b, sig, sig, opts)
	return c.Verdict != model.VerdictDiff
}

func unstableReason(baseAnswers, baseAttempts, candAnswers, candAttempts int) string {
	var parts []string
	if baseAnswers > 1 {
		parts = append(parts, fmt.Sprintf("the baseline gave %d different answers to %d identical requests", baseAnswers, baseAttempts))
	}
	if candAnswers > 1 {
		parts = append(parts, fmt.Sprintf("the candidate gave %d different answers to %d identical requests", candAnswers, candAttempts))
	}
	return strings.Join(parts, " and ") + ", so there is no single behaviour to compare"
}

func compareOne(c *model.Comparison, b, k model.Observation, baseSig, candSig *signature, opts Options) {
	if b.Client.Error != "" || k.Client.Error != "" {
		c.Verdict = model.VerdictError
		c.Reason = fmt.Sprintf("baseline: %s; candidate: %s", outcome(b), outcome(k))
		return
	}

	baseUnrouted, candUnrouted := baseSig.matches(b), candSig.matches(k)
	if baseUnrouted && candUnrouted {
		// Both targets gave their "nothing matches" answer. For a request
		// invented to test a boundary that is real agreement. For a path
		// taken from the configuration it means the configuration is not in
		// effect on either side, and agreeing on that proves nothing.
		switch c.Probe.Origin.Source {
		case model.OriginMutation, model.OriginControl:
			c.Verdict, c.Reason = model.VerdictMatch, "neither target routes this request"
		default:
			c.Verdict = model.VerdictUnrouted
			c.Reason = "neither target routes this request, although it comes from the configuration; check that the routes are loaded and the Host is right"
		}
		return
	}
	if baseUnrouted != candUnrouted {
		c.Deltas = append(c.Deltas, model.Delta{Field: "routed", Baseline: routedWord(!baseUnrouted), Candidate: routedWord(!candUnrouted)})
	}

	if b.Client.Status != k.Client.Status {
		c.Deltas = append(c.Deltas, model.Delta{
			Field: "client.status", Baseline: fmt.Sprint(b.Client.Status), Candidate: fmt.Sprint(k.Client.Status),
		})
	}
	policy := opts.headers()
	compareLocation(c, b, k, policy)
	compareResponseHeaders(c, b, k, policy)
	compareBackend(c, b, k)

	// Bodies are only comparable when a backend produced both. An echo body
	// describes the request the backend saw, which compareBackend covers.
	if b.Backend == nil && k.Backend == nil && b.Client.Status == k.Client.Status &&
		b.Client.Status/100 == 2 && b.Client.BodySHA256 != k.Client.BodySHA256 {
		d := model.Delta{Field: "client.body", Baseline: bodyWord(b), Candidate: bodyWord(k)}
		if opts.CompareBody {
			c.Deltas = append(c.Deltas, d)
		} else {
			c.Suppressed = append(c.Suppressed, d)
		}
	}

	if len(c.Deltas) > 0 {
		c.Verdict = model.VerdictDiff
	} else {
		c.Verdict = model.VerdictMatch
	}
}

func compareLocation(c *model.Comparison, b, k model.Observation, policy headerPolicy) {
	bl, kl := first(b.Client.Headers, "Location"), first(k.Client.Headers, "Location")
	if bl == "" && kl == "" {
		return
	}
	br, kr := resolveLocation(c.Probe, bl), resolveLocation(c.Probe, kl)
	switch {
	case br != kr && policy.significant("location"):
		c.Deltas = append(c.Deltas, model.Delta{Field: "client.location", Baseline: bl, Candidate: kl})
	case bl != kl:
		// Same destination, written differently: relative against absolute,
		// or with the port the test happened to connect to.
		c.Suppressed = append(c.Suppressed, model.Delta{Field: "client.header.location", Baseline: bl, Candidate: kl})
	}
}

// resolveLocation turns a Location value into the absolute URL a client would
// request next, without the port. The port is dropped because the two targets
// of a comparison are usually reached on different ports, and a proxy that
// echoes its port back says nothing about routing.
func resolveLocation(p model.Probe, location string) string {
	if location == "" {
		return ""
	}
	base, err := url.Parse(p.Scheme + "://" + p.Host + "/")
	if err != nil {
		return location
	}
	ref, err := url.Parse(location)
	if err != nil {
		return location
	}
	abs := base.ResolveReference(ref)
	return abs.Scheme + "://" + strings.ToLower(abs.Hostname()) + abs.EscapedPath() + query(abs)
}

func query(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

func compareResponseHeaders(c *model.Comparison, b, k model.Observation, policy headerPolicy) {
	names := map[string]bool{}
	for _, h := range b.Client.Headers {
		names[strings.ToLower(h.Name)] = true
	}
	for _, h := range k.Client.Headers {
		names[strings.ToLower(h.Name)] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		if name == "location" {
			continue // compared after resolving, above
		}
		if name == "content-length" && b.Backend != nil && k.Backend != nil {
			// Both bodies are the echo's description of the request it got,
			// whose length follows from the headers each proxy added. Those
			// are compared directly, so the length says nothing new.
			continue
		}
		bv, kv := joined(b.Client.Headers, name), joined(k.Client.Headers, name)
		if name == "set-cookie" {
			// Cookie values are usually per-session. Which cookies are set,
			// and with which attributes, is what a migration can change.
			bv, kv = cookieShapes(b.Client.Headers), cookieShapes(k.Client.Headers)
		}
		if bv == kv {
			continue
		}
		d := model.Delta{Field: "client.header." + name, Baseline: bv, Candidate: kv}
		if policy.significant(name) {
			c.Deltas = append(c.Deltas, d)
		} else {
			c.Suppressed = append(c.Suppressed, d)
		}
	}
}

// cookieShapes lists each Set-Cookie as its name and attribute names, sorted,
// leaving out values.
func cookieShapes(headers []model.Header) string {
	var shapes []string
	for _, value := range model.HeaderValues(headers, "Set-Cookie") {
		parts := strings.Split(value, ";")
		name, _, _ := strings.Cut(strings.TrimSpace(parts[0]), "=")
		var attrs []string
		for _, attr := range parts[1:] {
			key, _, _ := strings.Cut(strings.TrimSpace(attr), "=")
			if key != "" {
				attrs = append(attrs, strings.ToLower(key))
			}
		}
		sort.Strings(attrs)
		shapes = append(shapes, strings.TrimSpace(name+" "+strings.Join(attrs, " ")))
	}
	sort.Strings(shapes)
	return strings.Join(shapes, ", ")
}

func compareBackend(c *model.Comparison, b, k model.Observation) {
	switch {
	case b.Backend == nil && k.Backend == nil:
		return
	case b.Backend == nil || k.Backend == nil:
		c.Deltas = append(c.Deltas, model.Delta{Field: "backend.reached", Baseline: backendName(b.Backend), Candidate: backendName(k.Backend)})
		return
	}
	add := func(field, base, cand string) {
		if base != cand {
			c.Deltas = append(c.Deltas, model.Delta{Field: field, Baseline: base, Candidate: cand})
		}
	}
	add("backend.service", backendName(b.Backend), backendName(k.Backend))
	add("backend.method", b.Backend.Method, k.Backend.Method)
	add("backend.rawTarget", b.Backend.RawTarget, k.Backend.RawTarget)
	add("backend.host", b.Backend.Host, k.Backend.Host)
	if b.Backend.BodyLen != k.Backend.BodyLen {
		add("backend.bodyLen", fmt.Sprint(b.Backend.BodyLen), fmt.Sprint(k.Backend.BodyLen))
	}

	for _, name := range forwardedHeaders {
		bv, kv := joined(b.Backend.Headers, name), joined(k.Backend.Headers, name)
		if name == "X-Forwarded-For" || name == "X-Real-Ip" {
			// The address is that of whatever sits in front of each target,
			// so only whether the header is set at all is comparable.
			bv, kv = presence(bv), presence(kv)
		}
		if bv != kv {
			c.Suppressed = append(c.Suppressed, model.Delta{
				Field: "backend.header." + strings.ToLower(name), Baseline: orAbsent(bv), Candidate: orAbsent(kv),
			})
		}
	}
}

// signature is what a target answers when no route matches.
type signature struct {
	status int
	body   string
}

// unroutedSignature reads the control probe's observation from a snapshot.
// It returns nil when the snapshot has no usable control.
func unroutedSignature(s *model.Snapshot) *signature {
	for _, p := range s.Probes {
		if p.Origin.Source != model.OriginControl {
			continue
		}
		for _, obs := range s.Observations {
			if obs.ProbeID != p.ID {
				continue
			}
			if obs.Client.Error != "" || obs.Backend != nil {
				return nil // the control was routed or failed; it shows nothing
			}
			return &signature{status: obs.Client.Status, body: obs.Client.BodySHA256}
		}
	}
	return nil
}

// matches reports whether an observation is the target's no-route answer.
// Without a control it falls back to the statuses proxies use for that, which
// cannot tell a proxy's 404 from an application's.
func (s *signature) matches(obs model.Observation) bool {
	if obs.Backend != nil || obs.Client.Error != "" {
		return false
	}
	if s == nil {
		return obs.Client.Status == 404 || obs.Client.Status == 503
	}
	return obs.Client.Status == s.status && obs.Client.BodySHA256 == s.body
}

func byProbe(observations []model.Observation) map[string][]model.Observation {
	out := map[string][]model.Observation{}
	for _, obs := range observations {
		out[obs.ProbeID] = append(out[obs.ProbeID], obs)
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Attempt < list[j].Attempt })
	}
	return out
}

func mergeUnverified(lists ...[]model.Unverified) []model.Unverified {
	type key struct {
		reason  model.UnverifiedReason
		feature string
		object  string
		detail  string
	}
	seen := map[key]bool{}
	var out []model.Unverified
	for _, list := range lists {
		for _, u := range list {
			k := key{reason: u.Reason, feature: u.Feature, detail: u.Detail}
			if u.Object != nil {
				k.object = u.Object.String()
			}
			if !seen[k] {
				seen[k] = true
				out = append(out, u)
			}
		}
	}
	return out
}

func outcome(obs model.Observation) string {
	if obs.Client.Error != "" {
		return obs.Client.Error
	}
	return fmt.Sprintf("status %d", obs.Client.Status)
}

func routedWord(routed bool) string {
	if routed {
		return "routed"
	}
	return "not routed"
}

func bodyWord(obs model.Observation) string {
	hash := obs.Client.BodySHA256
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return fmt.Sprintf("%d bytes, sha256 %s", obs.Client.BodyLen, hash)
}

func backendName(b *model.BackendView) string {
	if b == nil {
		return "not reached"
	}
	name := b.Namespace + "/" + b.Service
	if b.Port != "" {
		name += ":" + b.Port
	}
	return name
}

func first(headers []model.Header, name string) string {
	if values := model.HeaderValues(headers, name); len(values) > 0 {
		return values[0]
	}
	return ""
}

func joined(headers []model.Header, name string) string {
	return strings.Join(model.HeaderValues(headers, name), ", ")
}

func presence(value string) string {
	if value == "" {
		return ""
	}
	return "set"
}

func orAbsent(value string) string {
	if value == "" {
		return "absent"
	}
	return value
}

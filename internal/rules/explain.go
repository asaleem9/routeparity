package rules

import (
	"fmt"
	"sort"
	"strings"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/pathgen"
)

// familyEnvoy is the implementation family of Envoy-based gateways, including
// Envoy Gateway. Regular expressions match the whole path case-sensitively,
// and the path is normalised before it is forwarded.
const familyEnvoy = "envoy"

// familyOf maps an implementation name to the family its fixes are written
// for. An unknown implementation gets no family, and the fixes stay the ones
// that apply to any conformant Gateway, or say which family they assume.
func familyOf(implementation string) string {
	switch strings.ToLower(strings.TrimSpace(implementation)) {
	case "envoy-gateway", familyEnvoy:
		return familyEnvoy
	case "nginx-gateway-fabric":
		return "nginx"
	case "istio":
		return "istio"
	default:
		return ""
	}
}

// Annotate explains each difference and records which rules the run checked.
// w may be nil when the manifests are not available; explanations then omit
// the path-specific parts that need them. Rules the configuration predicts
// but the run never checked are added to the result's unverified list, so a
// run with gaps cannot be reported as equivalent.
func Annotate(res *diff.Result, w *model.World) {
	if res == nil {
		return
	}
	s := Scope{World: w, Family: familyOf(res.Candidate.Implementation)}
	for i := range res.Comparisons {
		c := &res.Comparisons[i]
		if c.Verdict != model.VerdictDiff {
			continue
		}
		c.Finding = explainOne(*c, s)
	}
	res.Coverage = coverageOf(res, w)
	for _, gap := range notExercised(res.Coverage) {
		if !alreadyUnverified(res.Unverified, gap) {
			res.Unverified = append(res.Unverified, gap)
		}
	}
}

// explainOne asks the probe's own rule first, then the rules that explain
// differences on probes they did not generate. A difference nobody claims is
// reported as unclassified rather than left looking unexplained.
func explainOne(c model.Comparison, s Scope) *model.Finding {
	if c.Probe.Origin.RuleID != "" {
		if rule, ok := ByID(c.Probe.Origin.RuleID); ok {
			if f := rule.Explain(c, s); f != nil {
				return f
			}
		}
	}
	for _, rule := range []Rule{hsts{}, sslRedirect{}, pathConflict{}} {
		if f := rule.Explain(c, s); f != nil {
			return f
		}
	}
	return &model.Finding{
		Severity:   model.SeverityMedium,
		Confidence: model.ConfidenceUnclassified,
		Object:     c.Probe.Origin.Object,
		Host:       c.Probe.Host,
		Path:       c.Probe.RawTarget,
		Message:    "No catalogued behaviour explains this difference.",
	}
}

func (r regexSemantics) Explain(c model.Comparison, s Scope) *model.Finding {
	return explainRegex(r.Meta(), c, s, ownsUseRegex,
		"ingress-nginx matches a use-regex path case-insensitively and anchors it only at the start. A path copied literally into an HTTPRoute does neither.")
}

func (r regexLeak) Explain(c model.Comparison, s Scope) *model.Finding {
	return explainRegex(r.Meta(), c, s, func(p model.PathRule) bool {
		return !ownsUseRegex(p) && !ownsRewriteTarget(p)
	}, "This Ingress did not ask for regex matching. Another Ingress on the host did, and ingress-nginx then matches every path on the host that way. A literal Exact or PathPrefix match does not.")
}

func (r rewriteImpliesRegex) Explain(c model.Comparison, s Scope) *model.Finding {
	return explainRegex(r.Meta(), c, s, func(p model.PathRule) bool {
		return !ownsUseRegex(p) && ownsRewriteTarget(p)
	}, "rewrite-target makes ingress-nginx match the path as a case-insensitive regex prefix. A URLRewrite filter rewrites the path without changing how it matches.")
}

func explainRegex(m Meta, c model.Comparison, s Scope, owns func(model.PathRule) bool, behaviour string) *model.Finding {
	if c.Verdict != model.VerdictDiff || !regexDiverged(c) {
		return nil
	}
	var how string
	switch c.Probe.Origin.Mutation {
	case "case-flip":
		how = "The request differs from the configured path only in letter case. " + behaviour
	case "longer-path":
		how = "The request continues past the configured path. " + behaviour
	default:
		return nil
	}
	f := confirmed(m, c, routeSentence(c, how))
	path := configuredPath(s, c, owns)
	if path != "" {
		f.Path = path
	}
	f.Fix = regexFixFor(path, s.Family)
	return &f
}

// regexFixFor is the observed-difference form of regexFix. The snippet
// assumes an engine that matches the whole path case-sensitively. For any
// other family the snippet is withheld, because pasting it there would be wrong.
func regexFixFor(path, family string) *model.Fix {
	var fix *model.Fix
	if path != "" {
		fix = regexFix(path)
	} else {
		fix = &model.Fix{
			Summary: "Match the path as a case-insensitive regular expression that also accepts longer paths. " +
				"routeparity audit prints the match for each path.",
		}
	}
	switch family {
	case familyEnvoy, "":
		fix.Implementation = familyEnvoy
	case "istio":
		fix.Implementation = "istio"
	default:
		fix.Snippet = ""
		fix.Implementation = family
		fix.Summary = "Match the path case-insensitively and accept paths longer than the pattern, in the form this implementation documents. " +
			"The Envoy match is not given, because this implementation does not use Envoy's whole-path regular expressions."
	}
	return fix
}

func (r trailingSlashRedirect) Explain(c model.Comparison, s Scope) *model.Finding {
	if c.Verdict != model.VerdictDiff || c.Probe.Origin.Mutation != "strip-slash" {
		return nil
	}
	if !hasField(c, "routed", "client.status", "client.location") {
		return nil
	}
	f := confirmed(r.Meta(), c, "The baseline redirected the slashless path to the path with the slash, and the candidate did not. "+
		"ingress-nginx adds that redirect for an Exact or Prefix path that ends in a slash. Gateway API does not, so the redirect has to be written as a filter.")
	source := c.Probe.RawTarget
	if h := hostByProbe(s.World, c.Probe.Host); h != nil {
		for _, cand := range slashCandidates(h) {
			if cand.source != source {
				continue
			}
			f.Path = cand.rule.EffectivePath()
			if cand.covering != "" {
				f.Message = fmt.Sprintf(
					"The slashless path %q is also matched by the Prefix path %q. ingress-nginx may redirect it to %q or route it to the broader path, and the candidate did something else. Confirm which behaviour you rely on before adding a redirect.",
					source, cand.covering, cand.rule.EffectivePath())
				f.Fix = nil
				return &f
			}
			f.Fix = slashFix(source, cand.rule.EffectivePath())
			return &f
		}
	}
	f.Fix = slashFix(source, source+"/")
	return &f
}

func slashFix(source, dest string) *model.Fix {
	return &model.Fix{
		Summary: "Add a rule that redirects the slashless path.",
		Snippet: fmt.Sprintf("- matches:\n  - path:\n      type: Exact\n      value: %q\n  filters:\n  - type: RequestRedirect\n    requestRedirect:\n      statusCode: 301\n      path:\n        type: ReplaceFullPath\n        replaceFullPath: %q\n", source, dest),
	}
}

func (r urlNormalisation) Explain(c model.Comparison, s Scope) *model.Finding {
	if c.Verdict != model.VerdictDiff {
		return nil
	}
	switch c.Probe.Origin.Mutation {
	case "dot-segment", "current-dir", "double-slash", "percent-encoded":
	default:
		return nil
	}
	if onlyForwarding(c) {
		d, _ := deltaByField(c, "backend.rawTarget")
		f := confirmed(r.Meta(), c, fmt.Sprintf(
			"Both sides routed this request. The baseline forwarded the path as the client sent it (%s) and the candidate forwarded %s. "+
				"ingress-nginx matches on a normalised path and forwards the original.",
			shown(d.Baseline), shown(d.Candidate)))
		f.Fix = normForwardingFix(s.Family)
		return &f
	}
	if !hasField(c, "routed", "client.status", "backend.reached") {
		return nil
	}
	f := confirmed(r.Meta(), c, "The baseline treated this request as the configured path and the candidate did not. "+
		"ingress-nginx removes dot segments, resolves .. and collapses repeated slashes before matching. The candidate compared the path as it arrived.")
	f.Fix = &model.Fix{
		Summary: "The path the client sent is not a path to copy into a route: it exists to show normalisation. " +
			"Decide whether the backend should see the path as sent or the normalised path, and make the candidate do that.",
	}
	return &f
}

// onlyForwarding reports a difference where both sides routed the request and
// the path the backend received is what changed.
func onlyForwarding(c model.Comparison) bool {
	saw := false
	for _, d := range c.Deltas {
		switch d.Field {
		case "backend.rawTarget":
			saw = true
		case "routed", "client.status", "client.location", "backend.reached":
			return false
		}
	}
	return saw
}

func normForwardingFix(family string) *model.Fix {
	if family == familyEnvoy {
		return &model.Fix{
			Summary: "Envoy Gateway merges slashes, resolves dot segments and percent-decodes the path before forwarding, " +
				"and an HTTPRoute cannot restore the original. A backend that depends on the path as the client sent it has to accept the normalised path.",
			Implementation: familyEnvoy,
		}
	}
	return &model.Fix{
		Summary: "The candidate changed the path before forwarding it. Gateway API has no filter that restores the path the client sent, " +
			"so a backend that depends on that path has to accept the path the candidate forwarded.",
	}
}

func (r hsts) Explain(c model.Comparison, _ Scope) *model.Finding {
	if c.Verdict != model.VerdictDiff || c.Probe.Scheme != "https" {
		return nil
	}
	d, ok := deltaByField(c, "client.header.strict-transport-security")
	if !ok {
		return nil
	}
	f := confirmed(r.Meta(), c, fmt.Sprintf(
		"On this HTTPS response %s and %s. ingress-nginx adds Strict-Transport-Security by default for a host with TLS. "+
			"The Ingress does not mention the header, so a translation does not add it either.",
		sent(d.Baseline, "the baseline"), sent(d.Candidate, "the candidate")))
	if d.Baseline != "" {
		f.Fix = &model.Fix{
			Summary: "Add the header on the routes attached to the HTTPS listener.",
			Snippet: fmt.Sprintf("filters:\n- type: ResponseHeaderModifier\n  responseHeaderModifier:\n    set:\n    - name: Strict-Transport-Security\n      value: %q\n", d.Baseline),
		}
	}
	return &f
}

func (r sslRedirect) Explain(c model.Comparison, _ Scope) *model.Finding {
	if c.Verdict != model.VerdictDiff || c.Probe.Scheme != "http" || !baselineRedirectedToHTTPS(c) {
		return nil
	}
	f := confirmed(r.Meta(), c, "The baseline redirected this plain HTTP request to HTTPS and the candidate did not. "+
		"ingress-nginx does that for a host with TLS unless ssl-redirect is false. A Gateway redirects only where an HTTPRoute on the HTTP listener says so.")
	f.Fix = &model.Fix{
		Summary: "Attach a redirect-only HTTPRoute to the HTTP listener for this host.",
		Snippet: "rules:\n- filters:\n  - type: RequestRedirect\n    requestRedirect:\n      scheme: https\n      statusCode: 308\n",
	}
	return &f
}

func baselineRedirectedToHTTPS(c model.Comparison) bool {
	if d, ok := deltaByField(c, "client.status"); ok && d.Baseline == "308" && d.Candidate != "308" {
		return true
	}
	d, ok := deltaByField(c, "client.location")
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.ToLower(d.Baseline), "https://") && !strings.HasPrefix(strings.ToLower(d.Candidate), "https://")
}

func (r pathConflict) Explain(c model.Comparison, s Scope) *model.Finding {
	if c.Verdict != model.VerdictDiff || s.World == nil || c.Probe.Origin.Source != model.OriginDerived {
		return nil
	}
	d, ok := deltaByField(c, "backend.reached")
	if !ok || d.Baseline == "" || d.Candidate == "" || d.Baseline == d.Candidate {
		return nil
	}
	if strings.Contains(d.Baseline, "not reached") || strings.Contains(d.Candidate, "not reached") {
		return nil
	}
	h := hostByProbe(s.World, c.Probe.Host)
	if h == nil {
		return nil
	}
	var names []string
	backends := map[string]bool{}
	for _, p := range h.Paths {
		if p.Canary || p.EffectivePath() != c.Probe.RawTarget {
			continue
		}
		b := p.Backend.String()
		if backends[b] {
			continue
		}
		backends[b] = true
		names = append(names, fmt.Sprintf("%s (%s)", p.Ingress, b))
	}
	if len(backends) < 2 {
		return nil
	}
	f := confirmed(r.Meta(), c, fmt.Sprintf(
		"The baseline sent this request to %s and the candidate sent it to %s. %s claim this host and path, and ingress-nginx serves the older Ingress. After conversion the winner depends on how the routes were merged.",
		d.Baseline, d.Candidate, strings.Join(names, " and ")))
	f.Fix = &model.Fix{
		Summary: "Keep one of the Ingresses and delete the other, then convert again. ingress-nginx serves the older of the two.",
	}
	return &f
}

func confirmed(m Meta, c model.Comparison, message string) model.Finding {
	return model.Finding{
		RuleID:     m.ID,
		Severity:   m.Severity,
		Confidence: model.ConfidenceConfirmed,
		Object:     c.Probe.Origin.Object,
		Host:       c.Probe.Host,
		Path:       c.Probe.RawTarget,
		Message:    message,
	}
}

func routeSentence(c model.Comparison, rest string) string {
	switch {
	case lostRoute(c):
		return "The baseline routed this request and the candidate did not. " + rest
	case gainedRoute(c):
		return "The candidate routed this request and the baseline did not. " + rest
	default:
		return "The two sides answered differently. " + rest
	}
}

func lostRoute(c model.Comparison) bool {
	d, ok := deltaByField(c, "routed")
	return ok && d.Baseline == "routed" && d.Candidate == "not routed"
}

func gainedRoute(c model.Comparison) bool {
	d, ok := deltaByField(c, "routed")
	return ok && d.Baseline == "not routed" && d.Candidate == "routed"
}

func regexDiverged(c model.Comparison) bool {
	return hasField(c, "routed", "client.status", "backend.reached")
}

func hasField(c model.Comparison, fields ...string) bool {
	want := map[string]bool{}
	for _, field := range fields {
		want[field] = true
	}
	for _, d := range c.Deltas {
		if want[d.Field] {
			return true
		}
	}
	return false
}

func deltaByField(c model.Comparison, field string) (model.Delta, bool) {
	for _, d := range c.Deltas {
		if d.Field == field {
			return d, true
		}
	}
	return model.Delta{}, false
}

func shown(v string) string {
	if v == "" {
		return "nothing"
	}
	return v
}

func sent(value, who string) string {
	if value == "" {
		return who + " sent none"
	}
	return who + " sent " + value
}

// configuredPath returns the Ingress path a probe was built from, when the
// manifests are available and the probe can be traced to one path.
func configuredPath(s Scope, c model.Comparison, owns func(model.PathRule) bool) string {
	p, ok := matchPath(s.World, c, owns)
	if !ok {
		return ""
	}
	return p.EffectivePath()
}

func matchPath(w *model.World, c model.Comparison, owns func(model.PathRule) bool) (model.PathRule, bool) {
	if w == nil || c.Probe.Origin.Object == nil {
		return model.PathRule{}, false
	}
	h := hostByProbe(w, c.Probe.Host)
	if h == nil {
		return model.PathRule{}, false
	}
	ref := *c.Probe.Origin.Object
	var candidates []model.PathRule
	for _, p := range h.Paths {
		if p.Canary || p.Ingress != ref || (owns != nil && !owns(p)) {
			continue
		}
		candidates = append(candidates, p)
	}
	switch len(candidates) {
	case 0:
		return model.PathRule{}, false
	case 1:
		return candidates[0], true
	}
	for _, p := range candidates {
		base, err := BaseTarget(h, p)
		if err != nil {
			continue
		}
		if variationTarget(c.Probe.Origin.Mutation, base) == c.Probe.RawTarget {
			return p, true
		}
	}
	return model.PathRule{}, false
}

func variationTarget(mutation, base string) string {
	switch mutation {
	case "case-flip":
		target, ok := pathgen.CaseFlip(base)
		if !ok {
			return ""
		}
		return target
	case "longer-path":
		return pathgen.Extend(base, "rp-extra")
	case "strip-slash":
		target, ok := pathgen.StripSlash(base)
		if !ok {
			return ""
		}
		return target
	case "dot-segment":
		return pathgen.DotSegment(base)
	case "current-dir":
		return pathgen.CurrentDir(base)
	case "double-slash":
		return pathgen.DoubleSlash(base)
	case "percent-encoded":
		target, ok := pathgen.EncodeFirstLetter(base)
		if !ok {
			return ""
		}
		return target
	default:
		return ""
	}
}

func hostByProbe(w *model.World, probeHost string) *model.Host {
	if w == nil {
		return nil
	}
	for _, h := range w.Hosts {
		if ProbeHost(h) == probeHost {
			return h
		}
	}
	return nil
}

type tally struct {
	probes int
	routed int
	diffs  int
}

func coverageOf(res *diff.Result, w *model.World) []model.Coverage {
	tallies := map[string]*tally{}
	touch := func(id string) *tally {
		if tallies[id] == nil {
			tallies[id] = &tally{}
		}
		return tallies[id]
	}
	tlsHosts := hostsWithTLS(w)

	for _, c := range res.Comparisons {
		if c.Verdict != model.VerdictMatch && c.Verdict != model.VerdictDiff {
			continue
		}
		ids := map[string]bool{}
		if c.Probe.Origin.RuleID != "" {
			ids[c.Probe.Origin.RuleID] = true
		}
		if c.Finding != nil && c.Finding.RuleID != "" {
			ids[c.Finding.RuleID] = true
		}
		if len(tlsHosts) > 0 && c.Probe.Scheme == "https" {
			ids["NGX-TLS-002"] = true
		}
		if c.Probe.Scheme == "http" && c.Probe.Origin.Source == model.OriginDerived && tlsHosts[c.Probe.Host] {
			ids["NGX-TLS-001"] = true
		}
		routed := baselineRouted(c)
		for id := range ids {
			t := touch(id)
			t.probes++
			if routed {
				t.routed++
			}
			if c.Verdict == model.VerdictDiff && explainedBy(c, id) {
				t.diffs++
			}
		}
	}

	var out []model.Coverage
	for _, rule := range All() {
		m := rule.Meta()
		t := tallies[m.ID]
		if t == nil {
			t = &tally{}
		}
		predicted := 0
		if w != nil {
			predicted = len(rule.Audit(w))
		}
		if t.probes == 0 && predicted == 0 {
			continue
		}
		var status model.CoverageStatus
		switch {
		case t.diffs > 0:
			status = model.CoverageDiffers
		case t.routed > 0:
			status = model.CoverageMatches
		default:
			status = model.CoverageNotExercised
		}
		out = append(out, model.Coverage{
			RuleID: m.ID,
			Title:  m.Title,
			Probes: t.probes,
			Routed: t.routed,
			Diffs:  t.diffs,
			Status: status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

func explainedBy(c model.Comparison, id string) bool {
	if c.Finding != nil && c.Finding.RuleID != "" {
		return c.Finding.RuleID == id
	}
	return c.Probe.Origin.RuleID == id
}

func baselineRouted(c model.Comparison) bool {
	if c.Verdict == model.VerdictMatch {
		return true
	}
	if c.Verdict != model.VerdictDiff {
		return false
	}
	if d, ok := deltaByField(c, "routed"); ok && d.Baseline == "not routed" {
		return false
	}
	return true
}

func hostsWithTLS(w *model.World) map[string]bool {
	out := map[string]bool{}
	if w == nil {
		return out
	}
	for _, h := range w.Hosts {
		if h.TLS {
			out[ProbeHost(h)] = true
		}
	}
	return out
}

func notExercised(coverage []model.Coverage) []model.Unverified {
	var out []model.Unverified
	for _, c := range coverage {
		if c.Status != model.CoverageNotExercised {
			continue
		}
		detail := "requests were sent, and the baseline routed none of them"
		if c.Probes == 0 {
			detail = "predicted from the manifests, and no request checked it"
		}
		out = append(out, model.Unverified{
			Reason:  model.ReasonNotExercised,
			Feature: c.RuleID + ", " + c.Title,
			Detail:  detail,
		})
	}
	return out
}

func alreadyUnverified(list []model.Unverified, u model.Unverified) bool {
	for _, have := range list {
		if have.Reason == u.Reason && have.Feature == u.Feature {
			return true
		}
	}
	return false
}

package rules

import (
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

// A host is in regex mode when any non-canary Ingress on it sets use-regex or
// rewrite-target. Every path on the host then falls under exactly one of the
// three rules below, depending on what its own Ingress asked for:
//
//	its Ingress sets use-regex                     regexSemantics
//	its Ingress sets rewrite-target only           rewriteImpliesRegex
//	its Ingress sets neither                       regexLeak

func ownsUseRegex(p model.PathRule) bool { return p.AnnotationBool("use-regex") }

func ownsRewriteTarget(p model.PathRule) bool {
	v, _ := p.Annotation("rewrite-target")
	return v != ""
}

// triggersFrom lists the Ingresses, other than self, that put the host into
// regex mode.
func triggersFrom(h *model.Host, self model.ObjectRef) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range h.RegexTriggers {
		if t.Ingress == self {
			continue
		}
		s := fmt.Sprintf("%s sets %s", t.Ingress, t.Annotation)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// regexFix is the path match that reproduces ingress-nginx's regex semantics
// on engines that match the whole path, case-sensitively, by default.
func regexFix(path string) *model.Fix {
	return &model.Fix{
		Summary: "Match the path as a case-insensitive regular expression that also accepts longer paths. " +
			"RegularExpression support and syntax are implementation-specific, so confirm your Gateway accepts the (?i) flag.",
		Snippet: fmt.Sprintf("matches:\n- path:\n    type: RegularExpression\n    value: %q\n", "(?i)"+path+".*"),
	}
}

// regexSemantics covers paths whose own Ingress opted into regex matching.
type regexSemantics struct{ noExplain }

func (regexSemantics) Meta() Meta {
	return Meta{
		ID:       "NGX-REGEX-001",
		Title:    "Regex paths match case-insensitively and as a prefix",
		Severity: model.SeverityHigh,
		Summary: "With use-regex, ingress-nginx matches a path pattern case-insensitively and only anchors it at the start, " +
			"so /[A-Z]{3} also matches /uuid and /abc/anything. Gateway API leaves RegularExpression semantics to the " +
			"implementation; Envoy-based gateways match the whole path case-sensitively, so the same pattern stops matching.",
		References: []string{refFiveBehaviours, refI2GWProvider},
	}
}

func (r regexSemantics) Audit(w *model.World) []model.Finding {
	var out []model.Finding
	for _, h := range w.Hosts {
		if !h.RegexMode() {
			continue
		}
		for _, p := range h.Paths {
			if p.Canary || !ownsUseRegex(p) {
				continue
			}
			f := predicted(r.Meta(), h, p, fmt.Sprintf(
				"ingress-nginx matches %q as a case-insensitive regular expression anchored only at the start. "+
					"A RegularExpression match with the same value matches fewer requests on gateways that match the whole path case-sensitively.",
				p.EffectivePath()))
			f.Fix = regexFix(p.EffectivePath())
			out = append(out, f)
		}
	}
	return out
}

// regexLeak covers paths that are matched as regexes only because a different
// Ingress on the same host turned regex mode on.
type regexLeak struct{ noExplain }

func (regexLeak) Meta() Meta {
	return Meta{
		ID:       "NGX-REGEX-002",
		Title:    "Regex mode applies to every path on the host",
		Severity: model.SeverityHigh,
		Summary: "use-regex and rewrite-target take effect per host, not per Ingress. Once any Ingress on a host sets one of them, " +
			"ingress-nginx treats every path on that host, including Exact and Prefix paths from other Ingresses, as a " +
			"case-insensitive regex prefix. An Exact path /Header then also serves /headers. Gateway API never reinterprets " +
			"an Exact or PathPrefix match, so requests that used to be routed get a 404.",
		References: []string{refFiveBehaviours},
	}
}

func (r regexLeak) Audit(w *model.World) []model.Finding {
	var out []model.Finding
	for _, h := range w.Hosts {
		if !h.RegexMode() {
			continue
		}
		for _, p := range h.Paths {
			if p.Canary || ownsUseRegex(p) || ownsRewriteTarget(p) {
				continue
			}
			f := predicted(r.Meta(), h, p, fmt.Sprintf(
				"%s path %q is matched as a case-insensitive regex prefix, because another Ingress on this host enables regex mode. "+
					"Translated literally it will stop matching requests that differ in case or have a longer path.",
				pathTypeName(p.PathType), p.EffectivePath()))
			f.Evidence = triggersFrom(h, p.Ingress)
			f.Fix = regexFix(p.EffectivePath())
			out = append(out, f)
		}
	}
	return out
}

// rewriteImpliesRegex covers paths whose Ingress sets rewrite-target without
// use-regex.
type rewriteImpliesRegex struct{ noExplain }

func (rewriteImpliesRegex) Meta() Meta {
	return Meta{
		ID:       "NGX-REGEX-003",
		Title:    "rewrite-target turns on regex matching",
		Severity: model.SeverityHigh,
		Summary: "Setting rewrite-target makes ingress-nginx match the Ingress's paths as case-insensitive regex prefixes, exactly as " +
			"use-regex does, and with the same host-wide effect. A Gateway API URLRewrite filter rewrites the path without " +
			"changing how the route matches, so a literal translation matches fewer requests.",
		References: []string{refFiveBehaviours},
	}
}

func (r rewriteImpliesRegex) Audit(w *model.World) []model.Finding {
	var out []model.Finding
	for _, h := range w.Hosts {
		if !h.RegexMode() {
			continue
		}
		for _, p := range h.Paths {
			if p.Canary || ownsUseRegex(p) || !ownsRewriteTarget(p) {
				continue
			}
			target, _ := p.Annotation("rewrite-target")
			f := predicted(r.Meta(), h, p, fmt.Sprintf(
				"%s path %q is matched as a case-insensitive regex prefix because its Ingress sets rewrite-target (%q). "+
					"A URLRewrite filter on a literal Exact or PathPrefix match will rewrite fewer requests.",
				pathTypeName(p.PathType), p.EffectivePath(), target))
			f.Fix = regexFix(p.EffectivePath())
			out = append(out, f)
		}
	}
	return out
}

func pathTypeName(t networkingv1.PathType) string {
	if t == "" {
		return string(networkingv1.PathTypeImplementationSpecific)
	}
	return string(t)
}

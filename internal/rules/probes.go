package rules

import (
	"strconv"
	"strings"

	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/pathgen"
)

// Probe hosts for rules that name no concrete host. The .invalid top-level
// domain is reserved, so these can never collide with a real name.
const (
	// anyHost is sent for Ingress rules that match every host.
	anyHost = "any-host.routeparity.invalid"
	// wildcardLabel replaces the "*" of a wildcard host.
	wildcardLabel = "rp-wildcard"
)

// ProbeHost returns the Host header that exercises a host's rules.
func ProbeHost(h *model.Host) string {
	switch {
	case h.Name == "":
		return anyHost
	case strings.HasPrefix(h.Name, "*."):
		return wildcardLabel + h.Name[1:]
	}
	return h.Name
}

// BaseTarget returns a request-target that the path rule matches on
// ingress-nginx. For a host in regex mode every path is a pattern, so a
// matching path is generated; that can fail for patterns outside Go's regexp
// syntax, in which case the rule cannot be probed.
func BaseTarget(h *model.Host, p model.PathRule) (string, error) {
	if h.RegexMode() {
		return pathgen.Sample(p.EffectivePath())
	}
	return p.EffectivePath(), nil
}

// CanaryRepeat is how many times a probe is sent when a weighted canary
// splits its path's traffic at random. One sample of a random split says
// nothing; several show whether the target is stable enough to compare.
const CanaryRepeat = 10

// Repeat returns how many times a probe for the path should be sent: more
// than once only when a canary Ingress on the same host and path sends a
// share of requests elsewhere by weight.
func Repeat(h *model.Host, p model.PathRule) int {
	for _, c := range h.Paths {
		if !c.Canary || c.EffectivePath() != p.EffectivePath() {
			continue
		}
		weight, _ := c.Annotation("canary-weight")
		if n, err := strconv.Atoi(strings.TrimSpace(weight)); err == nil && n > 0 {
			return CanaryRepeat
		}
	}
	return 1
}

// mutation builds a probe that a rule owns. It returns false when the target
// cannot be sent as a well-formed request.
func mutation(ruleID string, h *model.Host, p model.PathRule, name, target string) (model.Probe, bool) {
	ref := p.Ingress
	probe := model.Probe{
		Host:      ProbeHost(h),
		RawTarget: target,
		Origin:    model.Origin{Source: model.OriginMutation, RuleID: ruleID, Object: &ref, Mutation: name},
		Repeat:    Repeat(h, p),
	}
	if err := probe.Seal(); err != nil {
		return model.Probe{}, false
	}
	return probe, true
}

// regexProbes exercises the two ways ingress-nginx's regex matching is wider
// than a whole-path, case-sensitive match: it ignores case, and it accepts
// anything after the pattern.
func regexProbes(ruleID string, w *model.World, owns func(model.PathRule) bool) []model.Probe {
	var out []model.Probe
	for _, h := range w.Hosts {
		if !h.RegexMode() {
			continue
		}
		for _, p := range h.Paths {
			if p.Canary || !owns(p) {
				continue
			}
			base, err := BaseTarget(h, p)
			if err != nil {
				continue // reported as unverified by the probe generator
			}
			if target, ok := pathgen.CaseFlip(base); ok {
				if probe, ok := mutation(ruleID, h, p, "case-flip", target); ok {
					out = append(out, probe)
				}
			}
			if probe, ok := mutation(ruleID, h, p, "longer-path", pathgen.Extend(base, "rp-extra")); ok {
				out = append(out, probe)
			}
		}
	}
	return out
}

func (r regexSemantics) Probes(w *model.World) []model.Probe {
	return regexProbes(r.Meta().ID, w, ownsUseRegex)
}

func (r regexLeak) Probes(w *model.World) []model.Probe {
	return regexProbes(r.Meta().ID, w, func(p model.PathRule) bool {
		return !ownsUseRegex(p) && !ownsRewriteTarget(p)
	})
}

func (r rewriteImpliesRegex) Probes(w *model.World) []model.Probe {
	return regexProbes(r.Meta().ID, w, func(p model.PathRule) bool {
		return !ownsUseRegex(p) && ownsRewriteTarget(p)
	})
}

// Probes requests each slash-terminated path without its slash, which is the
// request ingress-nginx answers with a redirect.
func (r trailingSlashRedirect) Probes(w *model.World) []model.Probe {
	var out []model.Probe
	for _, h := range w.Hosts {
		for _, c := range slashCandidates(h) {
			if probe, ok := mutation(r.Meta().ID, h, c.rule, "strip-slash", c.source); ok {
				out = append(out, probe)
			}
		}
	}
	return out
}

// normalisedPathsPerHost bounds the normalisation probes. Normalisation is a
// property of the proxy, not of a path, so a couple of paths per host show it.
const normalisedPathsPerHost = 2

// Probes sends paths that only match their rule after normalisation: dot
// segments, a repeated slash, and a percent-encoded letter.
func (r urlNormalisation) Probes(w *model.World) []model.Probe {
	id := r.Meta().ID
	var out []model.Probe
	for _, h := range w.Hosts {
		used := 0
		for _, p := range h.Paths {
			if used == normalisedPathsPerHost {
				break
			}
			if p.Canary {
				continue
			}
			base, err := BaseTarget(h, p)
			if err != nil || base == "/" {
				continue // "/" matches before and after normalisation alike
			}
			used++
			variants := []struct{ name, target string }{
				{"dot-segment", pathgen.DotSegment(base)},
				{"current-dir", pathgen.CurrentDir(base)},
				{"double-slash", pathgen.DoubleSlash(base)},
			}
			if target, ok := pathgen.EncodeFirstLetter(base); ok {
				variants = append(variants, struct{ name, target string }{"percent-encoded", target})
			}
			for _, v := range variants {
				if probe, ok := mutation(id, h, p, v.name, v.target); ok {
					out = append(out, probe)
				}
			}
		}
	}
	return out
}

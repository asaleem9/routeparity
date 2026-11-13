// Package probe decides which requests to send. Probes come from the routing
// rules themselves, from the catalogued behaviours in package rules, and from
// a few generic variations that can expose a difference no rule describes.
package probe

import (
	"fmt"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/pathgen"
	"github.com/asaleem9/routeparity/internal/rules"
)

// UnroutedHost is the Host header of the control probe. No rule names it, so
// each target's answer to it shows what that target says when nothing
// matches. The comparison uses that to tell "both rejected the request" from
// "both served it".
const UnroutedHost = "unrouted.routeparity.invalid"

// UnroutedTarget is the request-target of the control probe.
const UnroutedTarget = "/rp-unrouted-control"

// Options selects which probes are generated.
type Options struct {
	// Mutations adds requests that are not taken from the configuration:
	// rule-specific variations and the generic ones. They use safe methods,
	// but a WAF or IDS in front of a live endpoint may object to them.
	Mutations bool
	// Samples sends every probe at least this many times, so that a target
	// which answers inconsistently is detected instead of compared on one
	// lucky or unlucky sample. Values below one mean once.
	Samples int
}

// Set is the result of generation.
type Set struct {
	Probes []model.Probe
	// Unverified lists rules for which no probe could be built.
	Unverified []model.Unverified
}

// Generate builds the probes for a World. The result is deterministic: the
// same World and Options always give the same probes in the same order.
func Generate(w *model.World, opts Options) Set {
	var set Set
	seen := map[string]bool{}
	add := func(p model.Probe) {
		if seen[p.ID] {
			return // an earlier generator already asks for this exact request
		}
		seen[p.ID] = true
		set.Probes = append(set.Probes, p)
	}

	// Derived probes go first so that, when a mutation happens to equal a
	// path from the configuration, the request is recorded as derived.
	for _, h := range w.Hosts {
		for _, p := range h.Paths {
			if p.Canary {
				continue
			}
			ref := p.Ingress
			base, err := rules.BaseTarget(h, p)
			if err != nil {
				set.Unverified = append(set.Unverified, model.Unverified{
					Reason:  model.ReasonNoProbe,
					Feature: fmt.Sprintf("%s %s", h.DisplayName(), p.EffectivePath()),
					Object:  &ref,
					Detail:  err.Error(),
				})
				continue
			}
			targets := []string{base}
			if p.PathType == networkingv1.PathTypePrefix && !h.RegexMode() {
				// A Prefix rule also has to match below the path.
				targets = append(targets, pathgen.Extend(base, "rp-sub"))
			}
			schemes := []string{"http"}
			if h.TLS {
				schemes = append(schemes, "https")
			}
			for _, target := range targets {
				for _, scheme := range schemes {
					probe := model.Probe{
						Scheme:    scheme,
						Host:      rules.ProbeHost(h),
						RawTarget: target,
						Origin:    model.Origin{Source: model.OriginDerived, Object: &ref},
						Repeat:    rules.Repeat(h, p),
					}
					if err := probe.Seal(); err != nil {
						set.Unverified = append(set.Unverified, model.Unverified{
							Reason:  model.ReasonNoProbe,
							Feature: fmt.Sprintf("%s %s", h.DisplayName(), p.EffectivePath()),
							Object:  &ref,
							Detail:  err.Error(),
						})
						continue
					}
					add(probe)
				}
			}
		}
	}

	if opts.Mutations {
		for _, rule := range rules.All() {
			for _, p := range rule.Probes(w) {
				add(p)
			}
		}
		for _, p := range generic(w) {
			add(p)
		}
	}

	if control, ok := unroutedControl(w); ok {
		add(control)
	}

	for i := range set.Probes {
		if set.Probes[i].Repeat < opts.Samples {
			set.Probes[i].Repeat = opts.Samples
		}
	}

	sort.SliceStable(set.Probes, func(i, j int) bool {
		a, b := set.Probes[i], set.Probes[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.RawTarget != b.RawTarget {
			return a.RawTarget < b.RawTarget
		}
		return a.Scheme < b.Scheme
	})
	return set
}

// generic returns variations that belong to no catalogued rule. On a host
// that is not in regex mode both setups are expected to treat them alike, so
// a difference here is something the catalogue does not know about yet.
func generic(w *model.World) []model.Probe {
	var out []model.Probe
	for _, h := range w.Hosts {
		if h.RegexMode() {
			continue // the regex rules own every variation on these hosts
		}
		for _, p := range h.Paths {
			if p.Canary {
				continue
			}
			base := p.EffectivePath()
			var variants []struct{ name, target string }
			if target, ok := pathgen.CaseFlip(base); ok {
				variants = append(variants, struct{ name, target string }{"case-flip", target})
			}
			if target, ok := pathgen.Append(base, "rp"); ok {
				// Whether /foo also serves /foorp differs between path types
				// and between proxies.
				variants = append(variants, struct{ name, target string }{"segment-boundary", target})
			}
			if target, ok := pathgen.AddSlash(base); ok {
				variants = append(variants, struct{ name, target string }{"add-slash", target})
			}
			ref := p.Ingress
			for _, v := range variants {
				probe := model.Probe{
					Scheme:    rules.MutationScheme(h),
					Host:      rules.ProbeHost(h),
					RawTarget: v.target,
					Origin:    model.Origin{Source: model.OriginMutation, Object: &ref, Mutation: v.name},
					Repeat:    rules.Repeat(h, p),
				}
				if err := probe.Seal(); err == nil {
					out = append(out, probe)
				}
			}
		}
	}
	return out
}

// unroutedControl builds the control probe. It returns false when the
// configuration routes every host, since then no request is unrouted.
func unroutedControl(w *model.World) (model.Probe, bool) {
	for _, h := range w.Hosts {
		if h.Name == "" {
			return model.Probe{}, false
		}
	}
	probe := model.Probe{
		Host:      UnroutedHost,
		RawTarget: UnroutedTarget,
		Origin:    model.Origin{Source: model.OriginControl, Mutation: "unrouted"},
	}
	if err := probe.Seal(); err != nil {
		return model.Probe{}, false
	}
	return probe, true
}

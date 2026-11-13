// Package rules is the catalogue of ingress-nginx behaviours that a Gateway
// API setup may not reproduce. Each rule can predict a divergence from
// configuration alone, generate probes that exercise it, and explain an
// observed difference.
package rules

import (
	"sort"

	"github.com/asaleem9/routeparity/internal/model"
)

// Meta describes a rule. IDs are stable: they appear in reports, snapshots
// and documentation links, so an ID is never reused for a different behaviour.
type Meta struct {
	ID       string
	Title    string
	Severity model.Severity
	// Summary says what ingress-nginx does and why a Gateway may differ.
	Summary string
	// References are the sources the rule is based on.
	References []string
}

// Rule is one catalogued behaviour.
type Rule interface {
	Meta() Meta
	// Audit predicts divergences from configuration alone. Findings it
	// returns have ConfidencePredicted.
	Audit(w *model.World) []model.Finding
	// Probes returns requests that exercise the behaviour. Each carries the
	// rule's ID in its origin.
	Probes(w *model.World) []model.Probe
	// Explain turns a comparison on one of the rule's probes into a finding,
	// or returns nil when the comparison shows no divergence the rule covers.
	Explain(c model.Comparison, w *model.World) *model.Finding
}

// References shared by several rules.
const (
	refFiveBehaviours = "https://kubernetes.io/blog/2026/02/27/ingress-nginx-before-you-migrate/"
	refI2GWProvider   = "https://github.com/kubernetes-sigs/ingress2gateway/blob/main/pkg/i2gw/providers/ingressnginx/README.md"
)

// auditOnly is embedded by rules whose probes and explanations are not
// written yet, so that each rule file only contains what it implements.
type auditOnly struct{}

func (auditOnly) Probes(*model.World) []model.Probe                     { return nil }
func (auditOnly) Explain(model.Comparison, *model.World) *model.Finding { return nil }

// All returns every rule, ordered by ID.
func All() []Rule {
	all := []Rule{
		regexSemantics{},
		regexLeak{},
		rewriteImpliesRegex{},
		trailingSlashRedirect{},
		urlNormalisation{},
		sslRedirect{},
		pathConflict{},
		converterNotice{},
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Meta().ID < all[j].Meta().ID })
	return all
}

// ByID looks a rule up by its ID.
func ByID(id string) (Rule, bool) {
	for _, r := range All() {
		if r.Meta().ID == id {
			return r, true
		}
	}
	return nil, false
}

// predicted builds a finding for a path from static analysis.
func predicted(m Meta, h *model.Host, p model.PathRule, message string) model.Finding {
	ref := p.Ingress
	return model.Finding{
		RuleID:     m.ID,
		Severity:   m.Severity,
		Confidence: model.ConfidencePredicted,
		Object:     &ref,
		Host:       h.DisplayName(),
		Path:       p.EffectivePath(),
		Message:    message,
	}
}

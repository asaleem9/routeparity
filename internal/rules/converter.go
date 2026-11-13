package rules

import (
	"regexp"
	"strings"

	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/model"
)

// converterNotice turns the converter's own warnings into findings. The
// converter is the authority on what it did not translate, so this rule
// reports what it says rather than keeping a second list that could drift
// from the converter version in use.
type converterNotice struct {
	noProbes
	noExplain
}

func (converterNotice) Meta() Meta {
	return Meta{
		ID:       "NGX-CONVERT-001",
		Title:    "The converter did not translate a feature",
		Severity: model.SeverityMedium,
		Summary: "ingress2gateway reports every annotation and feature it recognises but cannot express in Gateway API, and every " +
			"annotation it does not recognise at all. Each one is behaviour that exists on ingress-nginx and will be missing " +
			"after migration unless it is rebuilt by hand, usually with an implementation-specific policy.",
		References: []string{refI2GWProvider},
	}
}

// Audit returns nothing: this rule's findings come from ConverterFindings,
// because they need a conversion run rather than the routing model.
func (converterNotice) Audit(*model.World) []model.Finding { return nil }

var annotationInMessage = regexp.MustCompile(`nginx\.ingress\.kubernetes\.io/([a-z0-9-]+)`)

// coveredElsewhere lists phrases from converter notices that restate a
// behaviour a dedicated rule already reports, with a fuller explanation. If
// the converter rewords a notice the only effect is that it shows up twice.
var coveredElsewhere = []string{
	"URL normalization", // NGX-NORM-001
}

func restatesAnotherRule(message string) bool {
	for _, phrase := range coveredElsewhere {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// ConverterFindings maps the converter's warnings and errors to findings.
// Informational notices are dropped.
func ConverterFindings(notices []convert.Notice) []model.Finding {
	meta := converterNotice{}.Meta()
	var out []model.Finding
	for _, n := range notices {
		if n.Level != "WARNING" && n.Level != "ERROR" {
			continue
		}
		if restatesAnotherRule(n.Message) {
			continue
		}
		f := model.Finding{
			RuleID:     meta.ID,
			Severity:   noticeSeverity(n.Message),
			Confidence: model.ConfidencePredicted,
			Message:    "ingress2gateway: " + n.Message,
		}
		if len(n.Objects) > 0 {
			ref := n.Objects[0]
			f.Object = &ref
		}
		if why := noticeImpact(n.Message); why != "" {
			f.Evidence = append(f.Evidence, why)
		}
		out = append(out, f)
	}
	return out
}

// impact describes what is lost when a class of feature is not translated.
type impact struct {
	severity model.Severity
	why      string
}

var (
	impactAccess  = impact{model.SeverityHigh, "this controls who can reach the backend; without it requests that were rejected are served"}
	impactSnippet = impact{model.SeverityHigh, "snippets inject arbitrary nginx configuration, which has no Gateway API equivalent and must be reviewed by hand"}
	impactLimit   = impact{model.SeverityMedium, "rate and connection limits stop being enforced"}
	impactSession = impact{model.SeverityMedium, "requests from one client may be spread across backends"}
	impactCanary  = impact{model.SeverityMedium, "traffic that was sent to the canary backend goes to the main backend, or the reverse"}
)

// annotationImpact classifies an annotation by its short name.
func annotationImpact(name string) (impact, bool) {
	switch {
	case strings.HasPrefix(name, "auth-"),
		name == "whitelist-source-range", name == "denylist-source-range",
		name == "satisfy":
		return impactAccess, true
	case strings.HasSuffix(name, "-snippet"):
		return impactSnippet, true
	case strings.HasPrefix(name, "limit-"):
		return impactLimit, true
	case name == "affinity", name == "affinity-mode", strings.HasPrefix(name, "session-cookie-"):
		return impactSession, true
	case strings.HasPrefix(name, "canary"):
		return impactCanary, true
	}
	return impact{}, false
}

// messageImpact classifies a notice that names no annotation.
func messageImpact(message string) (impact, bool) {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "authorization"), strings.Contains(lower, "authentication"):
		return impactAccess, true
	case strings.Contains(lower, "snippet"):
		return impactSnippet, true
	case strings.Contains(lower, "affinity"):
		return impactSession, true
	case strings.Contains(lower, "rate limit"):
		return impactLimit, true
	}
	return impact{}, false
}

func classify(message string) (impact, bool) {
	if m := annotationInMessage.FindStringSubmatch(message); m != nil {
		if imp, ok := annotationImpact(m[1]); ok {
			return imp, true
		}
	}
	return messageImpact(message)
}

func noticeSeverity(message string) model.Severity {
	if imp, ok := classify(message); ok {
		return imp.severity
	}
	return converterNotice{}.Meta().Severity
}

func noticeImpact(message string) string {
	imp, _ := classify(message)
	return imp.why
}

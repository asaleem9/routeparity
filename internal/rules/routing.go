package rules

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

// trailingSlashRedirect covers the redirect nginx adds for a location that
// ends in a slash.
type trailingSlashRedirect struct{ auditOnly }

func (trailingSlashRedirect) Meta() Meta {
	return Meta{
		ID:       "NGX-SLASH-001",
		Title:    "A path ending in a slash redirects the slashless request",
		Severity: model.SeverityMedium,
		Summary: "For an Exact or Prefix path such as /app/, ingress-nginx answers a request for /app with a 301 to /app/. " +
			"The redirect comes from nginx, not from anything in the Ingress, and it does not happen for regex paths. " +
			"Gateway API implementations add no implicit redirects, so /app returns 404 unless a RequestRedirect filter is configured.",
		References: []string{refFiveBehaviours},
	}
}

func (r trailingSlashRedirect) Audit(w *model.World) []model.Finding {
	var out []model.Finding
	for _, h := range w.Hosts {
		if h.RegexMode() {
			continue // regex locations get no implicit redirect
		}
		literal := map[string]bool{}
		var prefixes []string
		for _, p := range h.Paths {
			if p.Canary || !isLiteral(p.PathType) {
				continue
			}
			literal[p.EffectivePath()] = true
			if p.PathType == networkingv1.PathTypePrefix {
				prefixes = append(prefixes, p.EffectivePath())
			}
		}

		reported := map[string]bool{}
		for _, p := range h.Paths {
			path := p.EffectivePath()
			if p.Canary || !isLiteral(p.PathType) || path == "/" || !strings.HasSuffix(path, "/") {
				continue
			}
			source := strings.TrimSuffix(path, "/")
			if source == "" || literal[source] || reported[source] {
				continue // the slashless path has its own rule, or was already reported
			}
			reported[source] = true

			f := predicted(r.Meta(), h, p, fmt.Sprintf(
				"ingress-nginx answers %q with a 301 redirect to %q. Without an explicit redirect the Gateway returns 404 for %q.",
				source, path, source))
			f.Fix = &model.Fix{
				Summary: "Add a rule that redirects the slashless path.",
				Snippet: fmt.Sprintf("- matches:\n  - path:\n      type: Exact\n      value: %q\n  filters:\n  - type: RequestRedirect\n    requestRedirect:\n      statusCode: 301\n      path:\n        type: ReplaceFullPath\n        replaceFullPath: %q\n", source, path),
			}
			if covering := coveringPrefix(prefixes, source); covering != "" {
				// The documented behaviour is for a path with no other match.
				// Whether nginx still redirects when a broader prefix also
				// matches is not documented, so this is reported as uncertain.
				f.Severity = model.SeverityLow
				f.Message = fmt.Sprintf(
					"%q is also matched by the Prefix path %q on this host. ingress-nginx may answer %q with a 301 redirect to %q "+
						"or route it to the broader path; a Gateway will route it to the broader path. Confirm which you rely on.",
					source, covering, source, path)
				f.Fix = nil
			}
			out = append(out, f)
		}
	}
	return out
}

func isLiteral(t networkingv1.PathType) bool {
	return t == networkingv1.PathTypeExact || t == networkingv1.PathTypePrefix
}

// coveringPrefix returns a Prefix path that matches path on a segment
// boundary, or "" when none does.
func coveringPrefix(prefixes []string, path string) string {
	for _, prefix := range prefixes {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		if len(path) == len(prefix) || strings.HasSuffix(prefix, "/") || path[len(prefix)] == '/' {
			return prefix
		}
	}
	return ""
}

// urlNormalisation covers path normalisation before matching.
type urlNormalisation struct{ auditOnly }

func (urlNormalisation) Meta() Meta {
	return Meta{
		ID:       "NGX-NORM-001",
		Title:    "Request paths are normalised before matching",
		Severity: model.SeverityLow,
		Summary: "ingress-nginx removes . segments, resolves .. segments and collapses repeated slashes before it matches a request " +
			"against Ingress rules, so /a/../uuid and //uuid both match an Exact path /uuid. Gateway implementations normalise " +
			"differently and to different degrees, which changes both which route matches and what path the backend receives.",
		References: []string{refFiveBehaviours},
	}
}

func (r urlNormalisation) Audit(w *model.World) []model.Finding {
	if len(w.Hosts) == 0 {
		return nil
	}
	// This is a property of the controllers rather than of any one Ingress,
	// so it is reported once.
	m := r.Meta()
	return []model.Finding{{
		RuleID:     m.ID,
		Severity:   m.Severity,
		Confidence: model.ConfidencePredicted,
		Message: "ingress-nginx normalises dot segments and repeated slashes before matching. How your Gateway implementation " +
			"normalises paths cannot be determined from manifests; it has to be measured with requests.",
	}}
}

// sslRedirect covers the HTTP to HTTPS redirect ingress-nginx adds for hosts
// with TLS.
type sslRedirect struct{ auditOnly }

func (sslRedirect) Meta() Meta {
	return Meta{
		ID:       "NGX-TLS-001",
		Title:    "Hosts with TLS redirect HTTP to HTTPS",
		Severity: model.SeverityMedium,
		Summary: "When any Ingress configures TLS for a host, ingress-nginx redirects plain HTTP requests for that host to HTTPS " +
			"with a 308, for every path on the host, unless the path's Ingress sets ssl-redirect to false. A Gateway only " +
			"redirects where an HTTPRoute on the HTTP listener says so.",
		References: []string{refI2GWProvider},
	}
}

func (r sslRedirect) Audit(w *model.World) []model.Finding {
	var out []model.Finding
	for _, h := range w.Hosts {
		if !h.TLS {
			continue
		}
		redirected := 0
		var optedOut []string
		for _, p := range h.Paths {
			if p.Canary {
				continue
			}
			if v, ok := p.Annotation("ssl-redirect"); ok {
				// Only an explicit false disables the redirect.
				if enabled, err := strconv.ParseBool(v); err == nil && !enabled {
					optedOut = append(optedOut, fmt.Sprintf("%s %s", p.Ingress, p.EffectivePath()))
					continue
				}
			}
			redirected++
		}
		if redirected == 0 {
			continue
		}
		m := r.Meta()
		f := model.Finding{
			RuleID:     m.ID,
			Severity:   m.Severity,
			Confidence: model.ConfidencePredicted,
			Host:       h.DisplayName(),
			Message: fmt.Sprintf(
				"ingress-nginx answers plain HTTP requests for %d path(s) on this host with a 308 redirect to HTTPS. "+
					"The Gateway needs an HTTP listener with a redirecting HTTPRoute to do the same.", redirected),
			Fix: &model.Fix{
				Summary: "Attach a redirect-only HTTPRoute to the HTTP listener for this host.",
				Snippet: "rules:\n- filters:\n  - type: RequestRedirect\n    requestRedirect:\n      scheme: https\n      statusCode: 308\n",
			},
		}
		if len(optedOut) > 0 {
			sort.Strings(optedOut)
			f.Evidence = append(f.Evidence, "ssl-redirect is disabled for: "+strings.Join(optedOut, ", "))
		}
		out = append(out, f)
	}
	return out
}

// pathConflict covers the same host and path being claimed by two Ingresses.
type pathConflict struct{ auditOnly }

func (pathConflict) Meta() Meta {
	return Meta{
		ID:       "NGX-CONFLICT-001",
		Title:    "Two Ingresses route the same host and path to different backends",
		Severity: model.SeverityMedium,
		Summary: "When two Ingresses define the same path on the same host, ingress-nginx serves the backend of the older Ingress and " +
			"ignores the other. Which backend wins after conversion depends on how the routes are merged and on the creation " +
			"order of the new objects, so it can flip.",
		References: []string{"https://github.com/kubernetes-sigs/ingress2gateway#processing-order-and-conflicts"},
	}
}

func (r pathConflict) Audit(w *model.World) []model.Finding {
	var out []model.Finding
	for _, h := range w.Hosts {
		type key struct {
			path string
			kind networkingv1.PathType
		}
		first := map[key]model.PathRule{}
		reported := map[key]bool{}
		for _, p := range h.Paths {
			if p.Canary {
				continue
			}
			k := key{p.EffectivePath(), p.PathType}
			winner, seen := first[k]
			if !seen {
				first[k] = p
				continue
			}
			if winner.Ingress == p.Ingress || winner.Backend == p.Backend || reported[k] {
				continue
			}
			reported[k] = true
			f := predicted(r.Meta(), h, p, fmt.Sprintf(
				"%s path %q is defined by %s (backend %s) and by %s (backend %s). ingress-nginx serves the older Ingress.",
				pathTypeName(p.PathType), p.EffectivePath(), winner.Ingress, winner.Backend, p.Ingress, p.Backend))
			if w.UndatedIngresses > 0 {
				f.Evidence = append(f.Evidence,
					"the manifests carry no creationTimestamp, so which Ingress is older cannot be determined from them")
			} else {
				f.Evidence = append(f.Evidence, fmt.Sprintf("%s is older and wins on ingress-nginx", winner.Ingress))
			}
			out = append(out, f)
		}
	}
	return out
}

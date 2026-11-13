// Package audit predicts where a Gateway API setup will behave differently
// from ingress-nginx, using manifests alone. It sends no traffic, so every
// finding is a prediction.
package audit

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/rules"
)

// Options controls an audit.
type Options struct {
	// Converter, when set, is run over the Ingresses so that its warnings
	// about untranslated features become findings.
	Converter convert.Converter
	// Emitter is passed to the converter. Empty means its default.
	Emitter string
}

// Report is the result of an audit.
type Report struct {
	IngressClass string                 `json:"ingressClass"`
	Ingresses    int                    `json:"ingresses"`
	Hosts        int                    `json:"hosts"`
	Paths        int                    `json:"paths"`
	Skipped      []model.SkippedIngress `json:"skipped,omitempty"`
	// Converter identifies the converter whose warnings are included, or is
	// empty when none was run.
	Converter string `json:"converter,omitempty"`
	Emitter   string `json:"emitter,omitempty"`
	// Notes are caveats about the audit itself.
	Notes    []string        `json:"notes,omitempty"`
	Findings []model.Finding `json:"findings"`
}

// Count returns the number of findings at each severity.
func (r *Report) Count() map[model.Severity]int {
	counts := map[model.Severity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
	}
	return counts
}

// Worst returns the highest severity among the findings, and false when there
// are none.
func (r *Report) Worst() (model.Severity, bool) {
	if len(r.Findings) == 0 {
		return "", false
	}
	worst := r.Findings[0].Severity
	for _, f := range r.Findings[1:] {
		if f.Severity.Rank() > worst.Rank() {
			worst = f.Severity
		}
	}
	return worst, true
}

// Run audits a World.
func Run(ctx context.Context, w *model.World, opts Options) (*Report, error) {
	rep := &Report{
		IngressClass: w.IngressClass,
		Ingresses:    len(w.Ingresses),
		Hosts:        len(w.Hosts),
		Skipped:      w.Skipped,
		Findings:     []model.Finding{},
	}
	for _, h := range w.Hosts {
		rep.Paths += len(h.Paths)
	}
	if w.UndatedIngresses > 0 && len(w.Ingresses) > 1 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d of %d Ingresses have no creationTimestamp. ingress-nginx resolves conflicting rules in favour of the oldest Ingress, "+
				"so precedence between them is assumed, not known. Export manifests from the cluster to keep timestamps.",
			w.UndatedIngresses, len(w.Ingresses)))
	}

	for _, rule := range rules.All() {
		rep.Findings = append(rep.Findings, rule.Audit(w)...)
	}

	if opts.Converter != nil && len(w.Ingresses) > 0 {
		manifests, err := marshalForConverter(w)
		if err != nil {
			return nil, err
		}
		out, err := opts.Converter.Convert(ctx, bytes.NewReader(manifests), convert.Options{
			IngressClass: w.IngressClass,
			Emitter:      opts.Emitter,
		})
		if out != nil {
			rep.Converter = out.Converter
			rep.Emitter = opts.Emitter
			rep.Findings = append(rep.Findings, rules.ConverterFindings(out.Notices)...)
		}
		if err != nil {
			// A failed conversion is itself the most useful thing to report:
			// these manifests cannot be migrated mechanically as they stand.
			rep.Findings = append(rep.Findings, model.Finding{
				RuleID:     "NGX-CONVERT-001",
				Severity:   model.SeverityHigh,
				Confidence: model.ConfidencePredicted,
				Message:    "the converter could not convert these manifests: " + err.Error(),
			})
		}
	}

	sortFindings(rep.Findings)
	return rep, nil
}

// sortFindings orders findings most severe first, then by where they apply,
// so that output is stable between runs.
func sortFindings(findings []model.Finding) {
	object := func(f model.Finding) string {
		if f.Object == nil {
			return ""
		}
		return f.Object.String()
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if object(a) != object(b) {
			return object(a) < object(b)
		}
		return a.Message < b.Message
	})
}

// marshalForConverter renders the selected Ingresses, and the Services they
// may reference by port name, as a multi-document YAML stream.
func marshalForConverter(w *model.World) ([]byte, error) {
	var buf bytes.Buffer
	write := func(obj any) error {
		doc, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		buf.WriteString("---\n")
		buf.Write(doc)
		return nil
	}
	for i := range w.Ingresses {
		ing := w.Ingresses[i]
		ing.APIVersion = networkingv1.SchemeGroupVersion.String()
		ing.Kind = "Ingress"
		if err := write(&ing); err != nil {
			return nil, fmt.Errorf("marshal %s: %w", model.IngressRef(&ing), err)
		}
	}

	names := make([]string, 0, len(w.Services))
	byName := map[string]corev1.Service{}
	for key, svc := range w.Services {
		names = append(names, key.String())
		byName[key.String()] = svc
	}
	sort.Strings(names)
	for _, name := range names {
		svc := byName[name]
		svc.APIVersion = "v1"
		svc.Kind = "Service"
		if err := write(&svc); err != nil {
			return nil, fmt.Errorf("marshal Service %s: %w", name, err)
		}
	}
	return buf.Bytes(), nil
}

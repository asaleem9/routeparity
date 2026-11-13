package audit

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/asaleem9/routeparity/internal/convert"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/source"
)

const manifests = `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: regex
  namespace: demo
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
spec:
  ingressClassName: nginx
  rules:
  - host: a.example.com
    http:
      paths:
      - path: /x/[0-9]+
        pathType: ImplementationSpecific
        backend:
          service:
            name: web
            port:
              name: http
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: other-class
  namespace: demo
spec:
  ingressClassName: traefik
  rules:
  - host: b.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web
            port:
              name: http
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: demo
spec:
  ports:
  - name: http
    port: 80
`

func world(t *testing.T) *model.World {
	t.Helper()
	res, err := source.Load(strings.NewReader(manifests), "test")
	if err != nil {
		t.Fatal(err)
	}
	return model.NewWorld(res.Objects, model.WorldOptions{})
}

// fakeConverter records what it was given and returns canned output.
type fakeConverter struct {
	gotManifests string
	gotOptions   convert.Options
	out          *convert.Output
	err          error
}

func (f *fakeConverter) Convert(_ context.Context, r io.Reader, opts convert.Options) (*convert.Output, error) {
	data, _ := io.ReadAll(r)
	f.gotManifests = string(data)
	f.gotOptions = opts
	return f.out, f.err
}

func ruleIDs(rep *Report) map[string]int {
	out := map[string]int{}
	for _, f := range rep.Findings {
		out[f.RuleID]++
	}
	return out
}

func TestRunWithoutConverter(t *testing.T) {
	rep, err := Run(context.Background(), world(t), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Ingresses != 1 || rep.Hosts != 1 || rep.Paths != 1 || len(rep.Skipped) != 1 {
		t.Errorf("counts = %d Ingresses, %d hosts, %d paths, %d skipped; want 1, 1, 1, 1",
			rep.Ingresses, rep.Hosts, rep.Paths, len(rep.Skipped))
	}
	if rep.Converter != "" {
		t.Errorf("Converter = %q, want empty when none ran", rep.Converter)
	}
	ids := ruleIDs(rep)
	if ids["NGX-REGEX-001"] != 1 || ids["NGX-NORM-001"] != 1 || len(ids) != 2 {
		t.Errorf("findings by rule = %v, want one NGX-REGEX-001 and one NGX-NORM-001", ids)
	}
	if rep.Findings[0].Severity != model.SeverityHigh {
		t.Errorf("first finding has severity %q; findings should be ordered most severe first", rep.Findings[0].Severity)
	}
	if worst, ok := rep.Worst(); !ok || worst != model.SeverityHigh {
		t.Errorf("Worst = %q, %v; want high, true", worst, ok)
	}
	if len(rep.Notes) != 0 {
		t.Errorf("unexpected notes for a single Ingress: %v", rep.Notes)
	}
}

func TestRunPassesSelectedIngressesToTheConverter(t *testing.T) {
	conv := &fakeConverter{out: &convert.Output{
		Converter: "fake@v1",
		Notices: []convert.Notice{
			{Level: "WARNING", Message: "Unsupported annotation nginx.ingress.kubernetes.io/auth-url"},
			{Level: "INFO", Message: "generated a Gateway"},
		},
	}}
	rep, err := Run(context.Background(), world(t), Options{Converter: conv, Emitter: "envoy-gateway"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(conv.gotManifests, "name: regex") {
		t.Error("the selected Ingress was not passed to the converter")
	}
	if strings.Contains(conv.gotManifests, "other-class") {
		t.Error("an Ingress of another class was passed to the converter")
	}
	if !strings.Contains(conv.gotManifests, "kind: Service") {
		t.Error("Services were not passed to the converter; it needs them to resolve named ports")
	}
	if !strings.Contains(conv.gotManifests, "kind: Ingress") || !strings.Contains(conv.gotManifests, "apiVersion: networking.k8s.io/v1") {
		t.Error("Ingress passed to the converter lacks its apiVersion or kind")
	}
	if conv.gotOptions.IngressClass != "nginx" || conv.gotOptions.Emitter != "envoy-gateway" {
		t.Errorf("converter options = %+v, want class nginx and emitter envoy-gateway", conv.gotOptions)
	}

	if rep.Converter != "fake@v1" || rep.Emitter != "envoy-gateway" {
		t.Errorf("report names converter %q with emitter %q", rep.Converter, rep.Emitter)
	}
	if got := ruleIDs(rep)["NGX-CONVERT-001"]; got != 1 {
		t.Errorf("NGX-CONVERT-001 reported %d time(s), want 1 (the warning, not the info notice)", got)
	}
}

func TestRunReportsAFailedConversion(t *testing.T) {
	conv := &fakeConverter{
		out: &convert.Output{Converter: "fake@v1", Notices: []convert.Notice{{Level: "WARNING", Message: "partial"}}},
		err: errors.New("conflicting rules"),
	}
	rep, err := Run(context.Background(), world(t), Options{Converter: conv})
	if err != nil {
		t.Fatalf("Run returned an error for a failed conversion; it should be a finding: %v", err)
	}
	var failure *model.Finding
	for i, f := range rep.Findings {
		if strings.Contains(f.Message, "conflicting rules") {
			failure = &rep.Findings[i]
		}
	}
	if failure == nil {
		t.Fatalf("no finding reports the conversion failure: %+v", rep.Findings)
	}
	if failure.Severity != model.SeverityHigh {
		t.Errorf("conversion failure has severity %q, want high", failure.Severity)
	}
	if got := ruleIDs(rep)["NGX-CONVERT-001"]; got != 2 {
		t.Errorf("NGX-CONVERT-001 reported %d time(s), want 2 (the failure and the notice collected before it)", got)
	}
}

func TestRunNotesUnknownPrecedence(t *testing.T) {
	two := manifests + `
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: second
  namespace: demo
spec:
  ingressClassName: nginx
  rules:
  - host: c.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web
            port:
              name: http
`
	res, err := source.Load(strings.NewReader(two), "test")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), model.NewWorld(res.Objects, model.WorldOptions{}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "creationTimestamp") {
		t.Errorf("notes = %v, want one about missing creationTimestamps", rep.Notes)
	}
}

func TestEmptyReport(t *testing.T) {
	rep := &Report{}
	if _, ok := rep.Worst(); ok {
		t.Error("Worst reports a severity for a report with no findings")
	}
}

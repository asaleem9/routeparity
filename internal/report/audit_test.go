package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/asaleem9/routeparity/internal/audit"
	"github.com/asaleem9/routeparity/internal/model"
)

func TestWrap(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		prefix string
		width  int
		want   string
	}{
		{"fits on one line", "one two", "", 20, "one two\n"},
		{"breaks between words", "one two three", "", 8, "one two\nthree\n"},
		{"prefix counts towards the width and repeats", "one two three", "> ", 9, "> one two\n> three\n"},
		{"a word longer than the width is not split", "a extraordinarily b", "", 5, "a\nextraordinarily\nb\n"},
		{"runs of whitespace collapse", "one \n  two", "", 20, "one two\n"},
		{"empty text produces nothing", "   ", "> ", 20, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrap(tt.text, tt.prefix, tt.width); got != tt.want {
				t.Errorf("wrap = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuditTextWithNoFindings(t *testing.T) {
	var buf bytes.Buffer
	rep := &audit.Report{IngressClass: "nginx", Ingresses: 1, Hosts: 1, Paths: 1, Findings: []model.Finding{}}
	if err := AuditText(&buf, rep, Plain); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 Ingress · 1 host · 1 path") {
		t.Errorf("singular counts are not rendered as singular:\n%s", out)
	}
	if !strings.Contains(out, "No findings.") {
		t.Errorf("output does not say there are no findings:\n%s", out)
	}
	// Finding nothing statically is not evidence of parity, and the report
	// has to keep saying so.
	if !strings.Contains(out, "No requests were sent") {
		t.Errorf("output does not state that nothing was verified:\n%s", out)
	}
}

func TestStyle(t *testing.T) {
	if got := Plain.Severity(model.SeverityHigh, "HIGH"); got != "HIGH" {
		t.Errorf("Plain style changed the text: %q", got)
	}
	coloured := Style{colour: true}
	for _, sev := range []model.Severity{model.SeverityHigh, model.SeverityMedium, model.SeverityLow, model.SeverityInfo} {
		got := coloured.Severity(sev, "X")
		if !strings.HasPrefix(got, "\x1b[") || !strings.HasSuffix(got, "X\x1b[0m") {
			t.Errorf("Severity(%s) = %q, want the text wrapped in an escape sequence and a reset", sev, got)
		}
	}
	// A buffer is not a terminal, so it never gets colour.
	if StyleFor(&bytes.Buffer{}) != Plain {
		t.Error("StyleFor a non-terminal writer is not Plain")
	}
}

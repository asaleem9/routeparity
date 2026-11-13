// Package report renders results for people and for machines.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/asaleem9/routeparity/internal/audit"
	"github.com/asaleem9/routeparity/internal/model"
	"github.com/asaleem9/routeparity/internal/rules"
)

// AuditJSON writes an audit report as indented JSON.
func AuditJSON(w io.Writer, rep *audit.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(rep)
}

const (
	textWidth = 100
	indent    = "        " // aligns under the rule ID column
)

// AuditText writes an audit report for a terminal. Colour is used only when
// style allows it.
func AuditText(w io.Writer, rep *audit.Report, style Style) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  ingress class %q\n", style.Bold("routeparity audit"), rep.IngressClass)
	fmt.Fprintf(&b, "  %s · %s · %s",
		plural(rep.Ingresses, "Ingress", "Ingresses"), plural(rep.Hosts, "host", "hosts"), plural(rep.Paths, "path", "paths"))
	if n := len(rep.Skipped); n > 0 {
		fmt.Fprintf(&b, " · %d skipped", n)
	}
	b.WriteByte('\n')
	if rep.Converter != "" {
		emitter := rep.Emitter
		if emitter == "" {
			emitter = "standard"
		}
		fmt.Fprintf(&b, "  converter warnings from %s (%s emitter)\n", rep.Converter, emitter)
	}
	for _, s := range rep.Skipped {
		fmt.Fprintf(&b, "  skipped %s: %s\n", s.Ref, s.Reason)
	}
	for _, note := range rep.Notes {
		b.WriteString(wrap("note: "+note, "  ", textWidth))
	}
	b.WriteByte('\n')

	titles := map[string]string{}
	for _, r := range rules.All() {
		titles[r.Meta().ID] = r.Meta().Title
	}

	for _, f := range rep.Findings {
		label := fmt.Sprintf("%-6s", strings.ToUpper(string(f.Severity)))
		fmt.Fprintf(&b, "%s  %s  %s\n", style.Severity(f.Severity, label), style.Bold(f.RuleID), titles[f.RuleID])

		var where []string
		if f.Host != "" {
			where = append(where, strings.TrimSpace(f.Host+" "+f.Path))
		}
		if f.Object != nil {
			where = append(where, f.Object.String())
		}
		if len(where) > 0 {
			b.WriteString(indent + style.Dim(strings.Join(where, "  ·  ")) + "\n")
		}
		b.WriteString(wrap(f.Message, indent, textWidth))
		for _, e := range f.Evidence {
			b.WriteString(wrap("because: "+e, indent, textWidth))
		}
		if f.Fix != nil {
			b.WriteString(wrap("fix: "+f.Fix.Summary, indent, textWidth))
			for _, line := range strings.Split(strings.TrimRight(f.Fix.Snippet, "\n"), "\n") {
				if line != "" {
					b.WriteString(indent + "    " + style.Dim(line) + "\n")
				}
			}
		}
		b.WriteByte('\n')
	}

	counts := rep.Count()
	if len(rep.Findings) == 0 {
		b.WriteString("No findings.\n")
	} else {
		fmt.Fprintf(&b, "%s %d high · %d medium · %d low · %d info\n", style.Bold("Summary:"),
			counts[model.SeverityHigh], counts[model.SeverityMedium], counts[model.SeverityLow], counts[model.SeverityInfo])
	}
	b.WriteString(wrap("These are predictions from the manifests. No requests were sent, so none of them is confirmed, "+
		"and behaviour that depends on controller settings or on the Gateway implementation is not covered.", "", textWidth))

	_, err := io.WriteString(w, b.String())
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// wrap breaks text into lines of at most width columns, each starting with
// prefix, and ends with a newline.
func wrap(text, prefix string, width int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := prefix
	lineLen := len(prefix)
	empty := true
	for _, word := range words {
		if !empty && lineLen+1+len(word) > width {
			b.WriteString(line + "\n")
			line, lineLen, empty = prefix, len(prefix), true
		}
		if !empty {
			line += " "
			lineLen++
		}
		line += word
		lineLen += len(word)
		empty = false
	}
	b.WriteString(line + "\n")
	return b.String()
}

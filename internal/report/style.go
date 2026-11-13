package report

import (
	"io"
	"os"

	"golang.org/x/term"

	"github.com/asaleem9/routeparity/internal/model"
)

// Style decides whether output carries ANSI colour.
type Style struct{ colour bool }

// Plain never colours output.
var Plain = Style{}

// StyleFor colours output when w is a terminal and the NO_COLOR convention
// (https://no-color.org) does not forbid it.
func StyleFor(w io.Writer) Style {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return Plain
	}
	f, ok := w.(*os.File)
	if !ok {
		return Plain
	}
	return Style{colour: term.IsTerminal(int(f.Fd()))}
}

func (s Style) paint(code, text string) string {
	if !s.colour {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Bold emphasises text.
func (s Style) Bold(text string) string { return s.paint("1", text) }

// Dim de-emphasises text.
func (s Style) Dim(text string) string { return s.paint("2", text) }

// Severity colours a severity label. The label text always carries the
// meaning; colour only reinforces it.
func (s Style) Severity(sev model.Severity, text string) string {
	switch sev {
	case model.SeverityHigh:
		return s.paint("1;31", text)
	case model.SeverityMedium:
		return s.paint("33", text)
	case model.SeverityLow:
		return s.paint("36", text)
	default:
		return s.paint("2", text)
	}
}

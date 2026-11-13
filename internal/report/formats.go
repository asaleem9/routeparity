package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
)

// JUnit XML, in the dialect CI systems agree on: one testsuite, one testcase
// per probe.
type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Properties []junitProperty `xml:"properties>property"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitOutcome `xml:"failure,omitempty"`
	Error     *junitOutcome `xml:"error,omitempty"`
	Skipped   *junitOutcome `xml:"skipped,omitempty"`
}

type junitOutcome struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Body    string `xml:",chardata"`
}

// DiffJUnit writes a comparison as JUnit XML. A difference is a failure.
// What could not be verified is skipped, or a failure when strict is set, so
// that the file agrees with the command's exit status.
func DiffJUnit(w io.Writer, res *diff.Result, strict bool) error {
	suite := junitSuite{
		Name: "routeparity",
		Properties: []junitProperty{
			{Name: "baseline", Value: describeTarget(res.Baseline)},
			{Name: "candidate", Value: describeTarget(res.Candidate)},
			{Name: "result", Value: Conclusion(res)[0]},
		},
	}
	unchecked := func(c *junitCase, kind, message, detail string) {
		outcome := &junitOutcome{Message: message, Type: kind, Body: detail}
		switch {
		case !strict:
			c.Skipped = outcome
			suite.Skipped++
		case kind == string(model.VerdictError):
			c.Error = outcome
			suite.Errors++
		default:
			c.Failure = outcome
			suite.Failures++
		}
	}

	for _, c := range res.Comparisons {
		tc := junitCase{Name: requestLine(c.Probe), Classname: "routeparity." + c.Probe.Host}
		origin := describeOrigin(c.Probe.Origin)
		switch c.Verdict {
		case model.VerdictMatch:
		case model.VerdictDiff:
			var lines, summary []string
			for _, d := range c.Deltas {
				lines = append(lines, fmt.Sprintf("%s: %s -> %s", d.Field, d.Baseline, d.Candidate))
				summary = append(summary, d.Field)
			}
			if origin != "" {
				lines = append([]string{origin}, lines...)
			}
			tc.Failure = &junitOutcome{
				Message: "differs in " + strings.Join(summary, ", "),
				Type:    string(model.VerdictDiff),
				Body:    strings.Join(lines, "\n"),
			}
			suite.Failures++
		default:
			unchecked(&tc, string(c.Verdict), c.Reason, origin)
		}
		suite.Cases = append(suite.Cases, tc)
	}
	for _, u := range res.Unverified {
		tc := junitCase{Name: u.Feature, Classname: "routeparity.not-verified"}
		detail := u.Detail
		if u.Object != nil {
			detail = strings.TrimSpace(u.Object.String() + " " + detail)
		}
		unchecked(&tc, string(model.VerdictUnverified), string(u.Reason), detail)
		suite.Cases = append(suite.Cases, tc)
	}
	suite.Tests = len(suite.Cases)

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(junitSuites{Suites: []junitSuite{suite}}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// DiffMarkdown writes a comparison as GitHub-flavoured Markdown, sized for a
// pull request comment or a CI job summary.
func DiffMarkdown(w io.Writer, res *diff.Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "## routeparity: %s → %s\n\n", mdText(describeTarget(res.Baseline)), mdText(describeTarget(res.Candidate)))

	for i, paragraph := range Conclusion(res) {
		paragraph = mdText(paragraph)
		if i == 0 {
			paragraph = "**Result:** " + paragraph
		}
		b.WriteString(paragraph + "\n\n")
	}

	s := res.Summary
	b.WriteString("| Probes | Match | Differ | Unrouted | Unverified | Failed |\n|---:|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d |\n\n", s.Probes, s.Match, s.Diff, s.Unrouted, s.Unverified, s.Error)

	var differing, unchecked []model.Comparison
	for _, c := range res.Comparisons {
		switch c.Verdict {
		case model.VerdictMatch:
		case model.VerdictDiff:
			differing = append(differing, c)
		default:
			unchecked = append(unchecked, c)
		}
	}

	if len(differing) > 0 {
		b.WriteString("### Differences\n\n| Request | From | Field | Baseline | Candidate |\n|---|---|---|---|---|\n")
		for _, c := range differing {
			for i, d := range c.Deltas {
				request, origin := "", ""
				if i == 0 {
					request, origin = mdCode(requestLine(c.Probe)), mdText(originLabel(c.Probe.Origin))
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", request, origin, mdCode(d.Field), mdCode(d.Baseline), mdCode(d.Candidate))
			}
		}
		b.WriteByte('\n')
	}

	if len(unchecked) > 0 {
		b.WriteString("### Not compared\n\n| Request | Verdict | Why |\n|---|---|---|\n")
		for _, c := range unchecked {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", mdCode(requestLine(c.Probe)), c.Verdict, mdText(c.Reason))
		}
		b.WriteByte('\n')
	}

	if len(res.Unverified) > 0 {
		fmt.Fprintf(&b, "<details>\n<summary>Not verified (%d)</summary>\n\n| Reason | What | Detail |\n|---|---|---|\n", len(res.Unverified))
		for _, u := range res.Unverified {
			what := u.Feature
			if u.Object != nil {
				what += " (" + u.Object.String() + ")"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", u.Reason, mdCode(what), mdText(u.Detail))
		}
		b.WriteString("\n</details>\n\n")
	}

	if len(res.Suppressed) > 0 {
		names := make([]string, 0, len(res.Suppressed))
		for name := range res.Suppressed {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if res.Suppressed[names[i]] != res.Suppressed[names[j]] {
				return res.Suppressed[names[i]] > res.Suppressed[names[j]]
			}
			return names[i] < names[j]
		})
		fmt.Fprintf(&b, "<details>\n<summary>Differences recorded and not counted (%d fields)</summary>\n\n| Field | Probes |\n|---|---:|\n", len(names))
		for _, name := range names {
			fmt.Fprintf(&b, "| %s | %d |\n", mdCode(name), res.Suppressed[name])
		}
		b.WriteString("\n</details>\n")
	}

	_, err := io.WriteString(w, strings.TrimRight(b.String(), "\n")+"\n")
	return err
}

// originLabel is the short form of a probe's origin for a table cell: the
// rule and variation when there is one.
func originLabel(o model.Origin) string {
	switch {
	case o.RuleID != "" && o.Mutation != "":
		return o.RuleID + " " + o.Mutation
	case o.Mutation != "":
		return o.Mutation
	case o.Source == model.OriginDerived:
		return "configuration"
	}
	return string(o.Source)
}

// mdText escapes text for a Markdown table cell or paragraph.
func mdText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;", "*", `\*`, "_", `\_`, "`", "\\`").Replace(s)
}

// mdCode renders a value as inline code. Values here are request targets
// and header values, which are untrusted input: a target can contain
// backticks, pipes and HTML.
func mdCode(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	// A code span is delimited by a backtick run longer than any inside it.
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	// Inside a table a pipe ends the cell even within a code span.
	return fence + pad + strings.ReplaceAll(s, "|", `\|`) + pad + fence
}

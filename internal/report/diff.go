package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/asaleem9/routeparity/internal/diff"
	"github.com/asaleem9/routeparity/internal/model"
)

// DiffJSON writes a comparison as indented JSON.
func DiffJSON(w io.Writer, res *diff.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(res)
}

// DiffText writes a comparison for a terminal. Matching probes are counted,
// not listed; everything else is listed, because everything else needs
// someone to look at it.
func DiffText(w io.Writer, res *diff.Result, style Style) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s  →  %s\n", style.Bold("routeparity diff"), describeTarget(res.Baseline), describeTarget(res.Candidate))
	s := res.Summary
	fmt.Fprintf(&b, "  %s · %d match · %d differ · %d unrouted · %d unverified · %d failed\n\n",
		plural(s.Probes, "probe", "probes"), s.Match, s.Diff, s.Unrouted, s.Unverified, s.Error)

	order := map[model.Verdict]int{
		model.VerdictDiff: 0, model.VerdictUnrouted: 1, model.VerdictError: 2, model.VerdictUnverified: 3,
	}
	var shown []model.Comparison
	for _, c := range res.Comparisons {
		if c.Verdict != model.VerdictMatch {
			shown = append(shown, c)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool { return order[shown[i].Verdict] < order[shown[j].Verdict] })

	for _, c := range shown {
		label := fmt.Sprintf("%-10s", string(c.Verdict))
		fmt.Fprintf(&b, "%s  %s\n", style.Verdict(c.Verdict, label), style.Bold(requestLine(c.Probe)))
		if origin := describeOrigin(c.Probe.Origin); origin != "" {
			b.WriteString(diffIndent + style.Dim(origin) + "\n")
		}
		if c.Reason != "" {
			b.WriteString(wrap(c.Reason, diffIndent, textWidth))
		}
		width := 0
		for _, d := range c.Deltas {
			if len(d.Field) > width {
				width = len(d.Field)
			}
		}
		for _, d := range c.Deltas {
			fmt.Fprintf(&b, "%s%-*s  %s  →  %s\n", diffIndent, width, d.Field, d.Baseline, d.Candidate)
		}
		b.WriteByte('\n')
	}

	if len(res.Unverified) > 0 {
		b.WriteString(style.Bold("Not verified") + "\n")
		for _, u := range res.Unverified {
			line := fmt.Sprintf("%s: %s", u.Reason, u.Feature)
			if u.Object != nil {
				line += " (" + u.Object.String() + ")"
			}
			if u.Detail != "" {
				line += ". " + u.Detail
			}
			b.WriteString(wrap(line, "  ", textWidth))
		}
		b.WriteByte('\n')
	}

	if len(res.Suppressed) > 0 {
		b.WriteString(style.Bold("Differences recorded and not counted") + "\n")
		fieldNames := make([]string, 0, len(res.Suppressed))
		for field := range res.Suppressed {
			fieldNames = append(fieldNames, field)
		}
		sort.Slice(fieldNames, func(i, j int) bool {
			if res.Suppressed[fieldNames[i]] != res.Suppressed[fieldNames[j]] {
				return res.Suppressed[fieldNames[i]] > res.Suppressed[fieldNames[j]]
			}
			return fieldNames[i] < fieldNames[j]
		})
		for _, field := range fieldNames {
			fmt.Fprintf(&b, "  %-44s %s\n", field, plural(res.Suppressed[field], "probe", "probes"))
		}
		b.WriteString(wrap("These are response headers that vary between proxies or between requests, such as Date, and the "+
			"forwarding headers each proxy adds for the backend. Use -o json to see the values.", "  ", textWidth))
		b.WriteByte('\n')
	}

	b.WriteString(conclusion(res, style))
	_, err := io.WriteString(w, b.String())
	return err
}

const diffIndent = "            " // aligns under the request, past the verdict column

// conclusion states what the comparison does and does not show, wrapped for
// a terminal.
func conclusion(res *diff.Result, style Style) string {
	var b strings.Builder
	for i, sentence := range Conclusion(res) {
		if i == 0 {
			sentence = style.Bold("Result:") + " " + sentence
		}
		b.WriteString(wrap(sentence, "", textWidth))
	}
	return b.String()
}

// Conclusion states, as separate paragraphs, what the comparison found and
// what it could not check. The first paragraph is the result. It never claims
// the two setups are equivalent while anything went unchecked.
func Conclusion(res *diff.Result) []string {
	s := res.Summary
	var out []string
	switch {
	case s.Diff > 0:
		out = append(out, fmt.Sprintf("The candidate behaves differently from the baseline on %s.", plural(s.Diff, "probe", "probes")))
	case s.Match == 0:
		out = append(out, "Nothing could be compared.")
	default:
		out = append(out, fmt.Sprintf("No differences on the %s that could be compared.", plural(s.Match, "probe", "probes")))
	}
	if res.Incomplete() {
		var gaps []string
		if s.Unrouted > 0 {
			gaps = append(gaps, plural(s.Unrouted, "configured path was", "configured paths were")+" routed by neither target")
		}
		if s.Error > 0 {
			gaps = append(gaps, plural(s.Error, "probe", "probes")+" got no response")
		}
		if s.Unverified > 0 {
			gaps = append(gaps, plural(s.Unverified, "probe", "probes")+" could not be compared")
		}
		if n := len(res.Unverified); n > 0 {
			gaps = append(gaps, plural(n, "feature was", "features were")+" not exercised at all")
		}
		out = append(out, "This is not a statement of parity: "+strings.Join(gaps, ", ")+".")
	}
	if !sentMutations(res) {
		out = append(out, "Only paths written in the manifests were requested. The behaviours that most often differ "+
			"between proxies show up on other requests, such as a path in different case or with a trailing segment. "+
			"Record with --mutations to send those.")
	}
	if res.Baseline.Mode != model.ModeLab || res.Candidate.Mode != model.ModeLab {
		out = append(out, "Only what the client received was compared. Whether both proxies sent the backend the same "+
			"request is not visible from outside.")
	}
	return out
}

// sentMutations reports whether the comparison includes any request that
// was not taken directly from the configuration.
func sentMutations(res *diff.Result) bool {
	for _, c := range res.Comparisons {
		if c.Probe.Origin.Source == model.OriginMutation {
			return true
		}
	}
	return false
}

func describeTarget(meta model.TargetMeta) string {
	name := meta.Name
	if name == "" {
		name = "unnamed"
	}
	var details []string
	if meta.Kind != "" && meta.Kind != model.TargetUnknown {
		details = append(details, string(meta.Kind))
	}
	if meta.Address != "" {
		details = append(details, meta.Address)
	}
	if len(details) == 0 {
		return name
	}
	return fmt.Sprintf("%s (%s)", name, strings.Join(details, ", "))
}

func requestLine(p model.Probe) string {
	return fmt.Sprintf("%s %s://%s%s", p.Method, p.Scheme, p.Host, p.RawTarget)
}

// describeOrigin says where a probe came from, so that a difference on it can
// be traced to a rule or to an object in the configuration.
func describeOrigin(o model.Origin) string {
	var parts []string
	switch o.Source {
	case model.OriginDerived:
		parts = append(parts, "from the configuration")
	case model.OriginMutation:
		switch {
		case o.RuleID != "" && o.Mutation != "":
			parts = append(parts, fmt.Sprintf("%s, %s", o.RuleID, o.Mutation))
		case o.Mutation != "":
			parts = append(parts, "variation: "+o.Mutation)
		}
	case model.OriginControl:
		parts = append(parts, "control")
	case model.OriginCorpus:
		parts = append(parts, "from recorded traffic")
	}
	if o.Object != nil {
		parts = append(parts, o.Object.String())
	}
	return strings.Join(parts, "  ·  ")
}

// ProbesText lists probes, one per line.
func ProbesText(w io.Writer, probes []model.Probe, unverified []model.Unverified) error {
	var b strings.Builder
	for _, p := range probes {
		fmt.Fprintf(&b, "%-7s %-64s %s\n", p.Method, p.Scheme+"://"+p.Host+p.RawTarget, describeOrigin(p.Origin))
	}
	fmt.Fprintf(&b, "\n%s\n", plural(len(probes), "probe", "probes"))
	for _, u := range unverified {
		line := fmt.Sprintf("no probe for %s", u.Feature)
		if u.Object != nil {
			line += " (" + u.Object.String() + ")"
		}
		if u.Detail != "" {
			line += ": " + u.Detail
		}
		b.WriteString(wrap(line, "", textWidth))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

package model

// Verdict is the outcome of comparing one probe across two targets.
type Verdict string

const (
	// VerdictMatch means both targets answered and nothing that matters differed.
	VerdictMatch Verdict = "MATCH"
	// VerdictDiff means the targets behaved differently.
	VerdictDiff Verdict = "DIFF"
	// VerdictUnrouted means both targets gave the answer of a proxy with no
	// matching route. The responses agree, but agreement here proves nothing
	// about the routing rules, so it is never counted as a match.
	VerdictUnrouted Verdict = "UNROUTED"
	// VerdictUnverified means the comparison could not be made meaningfully.
	VerdictUnverified Verdict = "UNVERIFIED"
	// VerdictError means at least one target could not be observed.
	VerdictError Verdict = "ERROR"
)

// Delta is one field that differed between baseline and candidate.
type Delta struct {
	// Field is a dotted path such as "client.status", "client.header.location"
	// or "backend.rawTarget".
	Field     string `json:"field"`
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
}

// Comparison is the verdict for one probe, with the evidence behind it.
type Comparison struct {
	Probe   Probe   `json:"probe"`
	Verdict Verdict `json:"verdict"`
	Deltas  []Delta `json:"deltas,omitempty"`
	// Suppressed lists differences that were ignored as noise. They are kept
	// and counted so that suppression is always visible.
	Suppressed []Delta `json:"suppressed,omitempty"`
	// Reason explains UNROUTED, UNVERIFIED and ERROR verdicts.
	Reason string `json:"reason,omitempty"`
}

// Severity ranks how much a finding matters.
type Severity string

// Severities, from least to most important.
const (
	SeverityInfo   Severity = "info"
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

// Rank orders severities, higher being more severe. Unknown values rank lowest.
func (s Severity) Rank() int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// Confidence says how a finding was established.
type Confidence string

const (
	// ConfidenceConfirmed means a probe observed the divergence.
	ConfidenceConfirmed Confidence = "confirmed"
	// ConfidencePredicted means static analysis expects a divergence that has
	// not been observed.
	ConfidencePredicted Confidence = "predicted"
	// ConfidenceUnclassified means a divergence was observed that no rule explains.
	ConfidenceUnclassified Confidence = "unclassified"
)

// Fix is a suggested change that removes a divergence.
type Fix struct {
	Summary string `json:"summary"`
	// Snippet is a YAML fragment to apply, when one can be given.
	Snippet string `json:"snippet,omitempty"`
	// Implementation names the Gateway implementation family the snippet is
	// written for. Empty means it applies to any conformant implementation.
	Implementation string `json:"implementation,omitempty"`
}

// Finding is one thing a user should know about, attributed to a rule.
type Finding struct {
	RuleID     string     `json:"ruleID"`
	Severity   Severity   `json:"severity"`
	Confidence Confidence `json:"confidence"`
	Object     *ObjectRef `json:"object,omitempty"`
	Host       string     `json:"host,omitempty"`
	Path       string     `json:"path,omitempty"`
	Message    string     `json:"message"`
	Evidence   []string   `json:"evidence,omitempty"`
	Fix        *Fix       `json:"fix,omitempty"`
}

// Package engine plans units (one per file), asks the validator, localises
// findings by drill-down and assembles the report data (spec §7–§8).
package engine

import "time"

type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Finding struct {
	ID          string      `json:"id"`
	Rule        string      `json:"rule"`
	RuleFile    string      `json:"ruleFile"`
	File        string      `json:"file"`
	Confidence  string      `json:"confidence"` // likely | possible
	Probability float64     `json:"probability"`
	Lines       []LineRange `json:"lines,omitempty"`
	Evidence    string      `json:"evidence,omitempty"`
	Preexisting bool        `json:"preexisting,omitempty"`
	Message     string      `json:"message"`
	// DismissedReason is set on findings in Report.Dismissed: the file and
	// rule are unchanged since someone ran `jevrules dismiss`.
	DismissedReason string `json:"dismissedReason,omitempty"`
	// EarlierDismissal is the reason of a dismissal that no longer applies
	// because the file or the rule changed since.
	EarlierDismissal string `json:"earlierDismissal,omitempty"`
}

type MatrixRow struct {
	File   string   `json:"file"`
	Rules  []string `json:"rules"`
	Result string   `json:"result"` // findings | dismissed | clean | error | unchecked | planned
	// Probabilities from the whole-file pass, per rule (JSON only).
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type SkippedFile struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

type SkippedRule struct {
	Rule     string `json:"rule"`
	RuleFile string `json:"ruleFile"`
	Reason   string `json:"reason"`
}

type ErrorEntry struct {
	File    string `json:"file,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Message string `json:"message"`
}

type Summary struct {
	FilesConsidered int   `json:"filesConsidered"`
	FilesChecked    int   `json:"filesChecked"`
	FilesSkipped    int   `json:"filesSkipped"`
	RulesLoaded     int   `json:"rulesLoaded"`
	RulesApplied    int   `json:"rulesApplied"` // (file, rule) applications
	ValidatorCalls  int   `json:"validatorCalls"`
	CacheHits       int   `json:"cacheHits"`
	InputTokens     int   `json:"inputTokens"`
	Findings        int   `json:"findings"`
	Dismissed       int   `json:"dismissed"`
	DurationMs      int64 `json:"durationMs"` // wall time of the run
	// --plan --estimate only
	EstimatedRequests    int `json:"estimatedRequests,omitempty"`
	EstimatedInputTokens int `json:"estimatedInputTokens,omitempty"`
}

type Report struct {
	Version     int          `json:"version"`
	GeneratedAt time.Time    `json:"generatedAt"`
	RepoRoot    string       `json:"repoRoot"`
	Mode        string       `json:"mode"`
	Plan        bool         `json:"plan,omitempty"`
	Validator   string       `json:"validator"`
	Model       string       `json:"model,omitempty"`
	Summary     Summary      `json:"summary"`
	Findings    []Finding    `json:"findings"`
	Dismissed   []Finding    `json:"dismissed"`
	RuleMatrix  []MatrixRow  `json:"ruleMatrix"`
	Skipped     Skipped      `json:"skipped"`
	Errors      []ErrorEntry `json:"errors"`
	Incomplete  bool         `json:"incomplete,omitempty"`
	Unchecked   []string     `json:"unchecked,omitempty"`
	// Fatal is set when the run aborted (auth failure). Not part of JSON
	// schema guarantees beyond being a string.
	Fatal string `json:"fatal,omitempty"`
}

type Skipped struct {
	Files []SkippedFile `json:"files"`
	Rules []SkippedRule `json:"rules"`
}

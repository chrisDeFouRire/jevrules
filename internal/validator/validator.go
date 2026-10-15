// Package validator is the only JEV-aware part of jevrules (spec §7.1).
// Everything else talks to the Validator interface, so tests and --plan need
// no network.
package validator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/chrisDeFouRire/jevRules/internal/rules"
)

// State is what JEV judges: one file, or one window of it.
type State struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Content  string `json:"content"` // every line prefixed "NNN| "
}

type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
	// Raw: return the choice distribution as-is (Result.Choices) instead
	// of a violation probability. Used to pinpoint lines.
	Raw bool `json:"-"`
}

type Request struct {
	State     State
	Questions map[string]Question // keyed by rule id
}

type Result struct {
	// Violation probability per rule id, 0..1.
	Probabilities map[string]float64
	// Choice distributions for Raw questions.
	Choices     map[string]map[string]float64
	InputTokens int
	Model       string // versioned model that answered
}

type Validator interface {
	Name() string
	Model() string
	Validate(ctx context.Context, req Request) (Result, error)
}

var (
	// ErrAuth: key missing or rejected. Aborts the whole run (exit 3).
	ErrAuth = errors.New("JEV API key missing or invalid")
	// ErrUnavailable: JEV kept failing; the run aborts (exit 3).
	ErrUnavailable = errors.New("JEV unavailable")
	// ErrTooLarge: request exceeds the model context; split and retry.
	ErrTooLarge = errors.New("request exceeds model context")
)

const instructionsTemplate = "Does `content` violate the rule below? Judge only `content` against the rule.\n\nRULE:\n"

var noulCriteria = map[string]string{
	"true":  "At least one construct in `content` violates the rule (see its BAD examples)",
	"false": "`content` complies with the rule, or the rule does not concern it",
}

// BuildQuestion turns a rule into a JEV question (spec §7.2). The rule body
// is sent verbatim, never translated.
func BuildQuestion(r *rules.Rule) Question {
	ins := instructionsTemplate + r.Body
	if len(r.Choice) > 0 {
		return Question{
			Type:         "choice",
			Instructions: "Which option describes `content` with respect to the rule below? Judge only `content` against the rule.\n\nRULE:\n" + r.Body,
			Criteria:     r.Choice,
		}
	}
	return Question{Type: "noul", Instructions: ins, Criteria: noulCriteria}
}

// PinpointQuestion asks which numbered line in `content` violates the rule.
// Options are the line numbers (with the line text as description) plus
// "none", listed last because Jev leans toward the first options.
func PinpointQuestion(r *rules.Rule, lines []string, first int) Question {
	crit := map[string]any{}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		crit[fmt.Sprint(first+i)] = "line " + fmt.Sprint(first+i) + ": " + strings.TrimSpace(l)
	}
	crit["none"] = "No single line violates the rule on its own"
	return Question{
		Type:         "choice",
		Instructions: "Which numbered line of `content` violates the rule below?\n\nRULE:\n" + r.Body,
		Criteria:     crit,
		Raw:          true,
	}
}

// QuestionDigest identifies a question's exact form for cache keys.
func QuestionDigest(q Question) string {
	b, _ := json.Marshal(struct {
		Question
		Raw bool `json:"raw"`
	}{q, q.Raw})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// violation extracts P(violation) from one raw answer.
func violation(q Question, raw json.RawMessage) (float64, error) {
	var a struct {
		Type          string             `json:"type"`
		Noul          *float64           `json:"noul"`
		Probabilities map[string]float64 `json:"probabilities"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return 0, err
	}
	switch q.Type {
	case "noul":
		if a.Noul == nil {
			return 0, fmt.Errorf("noul answer without noul field: %s", raw)
		}
		return *a.Noul, nil
	case "choice":
		p, ok := a.Probabilities["compliant"]
		if !ok {
			return 0, fmt.Errorf("choice answer without compliant probability: %s", raw)
		}
		return 1 - p, nil
	}
	return 0, fmt.Errorf("unsupported question type %q", q.Type)
}

// EstimateTokens is a conservative input-token estimate (≈3 bytes/token for
// code) used for windowing, --estimate and --budget-tokens.
func EstimateTokens(s State, qs map[string]Question) int {
	n := len(s.Content) + len(s.Path) + 40
	for _, q := range qs {
		n += len(q.Instructions) + 200
	}
	return n/3 + 1
}

// LargestQuestionTokens is the estimate for the biggest single question,
// which must fit the per-question context together with the state.
func LargestQuestionTokens(qs map[string]Question) int {
	m := 0
	for _, q := range qs {
		m = max(m, (len(q.Instructions)+200)/3+1)
	}
	return m
}

func sortedKeys(m map[string]Question) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Language guesses a language name from a file extension.
func Language(file string) string {
	i := strings.LastIndexByte(file, '.')
	if i < 0 {
		return "text"
	}
	switch ext := strings.ToLower(file[i+1:]); ext {
	case "java":
		return "java"
	case "ts":
		return "typescript"
	case "js", "mjs", "cjs":
		return "javascript"
	case "html":
		return "html"
	case "scss", "css":
		return ext
	case "sql":
		return "sql"
	case "xml":
		return "xml"
	case "gradle":
		return "groovy"
	case "properties":
		return "properties"
	case "md", "mdc":
		return "markdown"
	case "json":
		return "json"
	case "yml", "yaml":
		return "yaml"
	default:
		return ext
	}
}

// NumberLines prefixes each line with its 1-based number so windows keep the
// original numbering (spec §7.2).
func NumberLines(lines []string, first int) string {
	var b strings.Builder
	w := len(fmt.Sprint(first + len(lines) - 1))
	w = max(w, 3)
	for i, l := range lines {
		fmt.Fprintf(&b, "%*d| %s\n", w, first+i, l)
	}
	return b.String()
}

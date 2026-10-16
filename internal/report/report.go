// Package report renders an engine.Report as JSON or agent-oriented text
// (spec §6). It is a pure function of the report.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/chrisDeFouRire/jevRules/internal/engine"
)

func JSON(w io.Writer, r *engine.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// howToAct tells the reading agent what the findings are for. It states the
// goal and leaves the method to the agent: a fixed workflow (always drill
// down, always grep) is wrong for some rules.
const howToAct = `
The findings are possible errors only. For each finding, the validator examined
one file against one rule. It did not examine other files.
For each finding:
1. Read the rule.
2. Select the best method to examine the code for this rule.
3. If the code does not obey the rule, correct the code.
4. If the code obeys the rule, record what you examined and the result:
   jevrules dismiss <rule> <file> --reason "<text>"
   Later runs do not report it again until the file or the rule changes.
Use --precision only if no other method is possible. It uses many validator calls.
`

// Text renders findings grouped by rule. explainRef maps a rule file path to
// the argument for `jevrules explain` (scope-qualified when ids collide).
func Text(w io.Writer, r *engine.Report, explainRef func(ruleFile string) string) {
	if r.Plan {
		plan(w, r, explainRef)
		return
	}
	s := r.Summary
	fmt.Fprintf(w, "jevrules: %d files checked, %d rule applications, %d possible violations\n",
		s.FilesChecked, s.RulesApplied, s.Findings)
	if r.Fatal != "" {
		fmt.Fprintf(w, "ABORTED: %s\n", r.Fatal)
	}
	if len(r.Findings) > 0 {
		fmt.Fprint(w, howToAct)
	}

	type group struct {
		rule, file, msg string
		items           []engine.Finding
	}
	var groups []*group
	byFile := map[string]*group{}
	for _, f := range r.Findings {
		g, ok := byFile[f.RuleFile]
		if !ok {
			g = &group{rule: f.Rule, file: f.RuleFile, msg: f.Message}
			byFile[f.RuleFile] = g
			groups = append(groups, g)
		}
		g.items = append(g.items, f)
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\nRULE %s  (%s)\n", g.rule, g.file)
		if g.msg != "" {
			fmt.Fprintf(w, "  %q\n", g.msg)
		}
		for _, f := range g.items {
			tags := []string{f.Confidence, fmt.Sprintf("p=%.2f", f.Probability)}
			if f.Preexisting {
				tags = append(tags, "preexisting")
			}
			if len(f.Lines) == 0 {
				tags = append(tags, "file-level")
			}
			fmt.Fprintf(w, "  - %s%s  [%s]\n", f.File, lineSuffix(f.Lines), strings.Join(tags, ", "))
			if f.Evidence != "" {
				ev, _, _ := strings.Cut(f.Evidence, "\n")
				fmt.Fprintf(w, "      %s\n", ev)
			}
			if f.EarlierDismissal != "" {
				fmt.Fprintf(w, "      dismissed before, but the file or rule changed since: %s\n", f.EarlierDismissal)
			}
		}
		fmt.Fprintf(w, "  next: jevrules explain %s\n", explainRef(g.file))
	}

	clean := 0
	for _, m := range r.RuleMatrix {
		if m.Result == "clean" {
			clean++
		}
	}
	fmt.Fprintf(w, "\nCLEAN (checked, no findings): %d files\n", clean)
	if len(r.Dismissed) > 0 {
		fmt.Fprintf(w, "DISMISSED (file and rule unchanged since): %d\n", len(r.Dismissed))
		for _, f := range r.Dismissed {
			fmt.Fprintf(w, "  - %s  %s: %s\n", f.Rule, f.File, f.DismissedReason)
		}
	}
	if line := skippedLine(r); line != "" {
		fmt.Fprintf(w, "SKIPPED: %s\n", line)
	}
	if len(r.Errors) > 0 {
		fmt.Fprintf(w, "ERRORS: %d\n", len(r.Errors))
		for _, e := range r.Errors {
			fmt.Fprintf(w, "  - %s: %s\n", or(e.File, "-"), e.Message)
		}
	}
	if r.Incomplete {
		fmt.Fprintf(w, "INCOMPLETE: %d files not checked (budget or abort)\n", len(r.Unchecked))
		for _, f := range r.Unchecked {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	}
	fmt.Fprintf(w, "validator: %s, %d calls, %d cache hits, %d input tokens",
		or(r.Model, r.Validator), s.ValidatorCalls, s.CacheHits, s.InputTokens)
	if s.InputTokens > 0 && r.Validator == "jev" {
		fmt.Fprintf(w, " (~$%.4f)", costUSD(s.InputTokens))
	}
	fmt.Fprintf(w, ", %s\n", elapsed(s.DurationMs))
}

func plan(w io.Writer, r *engine.Report, explainRef func(string) string) {
	s := r.Summary
	fmt.Fprintf(w, "jevrules --plan: %d files, %d rule applications (no validator call)\n",
		len(r.RuleMatrix), s.RulesApplied)
	for _, m := range r.RuleMatrix {
		fmt.Fprintf(w, "\n%s\n", m.File)
		for _, id := range m.Rules {
			fmt.Fprintf(w, "  - %s\n", id)
		}
	}
	if line := skippedLine(r); line != "" {
		fmt.Fprintf(w, "\nSKIPPED: %s\n", line)
	}
	if s.EstimatedRequests > 0 {
		fmt.Fprintf(w, "ESTIMATE: %d requests, ~%d input tokens (~$%.4f), first pass only; drill-down adds calls for flagged files\n",
			s.EstimatedRequests, s.EstimatedInputTokens, costUSD(s.EstimatedInputTokens))
	}
	if len(r.RuleMatrix) > 0 {
		fmt.Fprintf(w, "next: jevrules explain <rule-id>   (prints the rule text)\n")
	}
}

// elapsed formats a run duration: "850ms", "4.2s", "1m12s".
func elapsed(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return d.String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// costUSD estimates the JEV price of n input tokens ($42 per billion, docs/spec.md).
func costUSD(n int) float64 { return float64(n) * 42 / 1e9 }

func skippedLine(r *engine.Report) string {
	reasons := map[string]int{}
	for _, f := range r.Skipped.Files {
		reasons[f.Reason]++
	}
	var keys []string
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d files (%s)", reasons[k], k))
	}
	wf, man := 0, 0
	for _, s := range r.Skipped.Rules {
		if strings.HasPrefix(s.Reason, "workflow") {
			wf++
		} else {
			man++
		}
	}
	if wf > 0 {
		parts = append(parts, fmt.Sprintf("%d workflow rules", wf))
	}
	if man > 0 {
		parts = append(parts, fmt.Sprintf("%d manual rules", man))
	}
	return strings.Join(parts, ", ")
}

func lineSuffix(ls []engine.LineRange) string {
	if len(ls) == 0 {
		return ""
	}
	var parts []string
	for _, l := range ls {
		if l.Start == l.End {
			parts = append(parts, fmt.Sprint(l.Start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", l.Start, l.End))
		}
	}
	return ":" + strings.Join(parts, ",")
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

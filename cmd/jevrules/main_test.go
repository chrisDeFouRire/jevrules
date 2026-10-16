package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/chrisDeFouRire/jevRules/internal/engine"
)

// workspace: <root>/front is a git repo with one rule and a 200-line file
// whose line 30 carries the mock validator's violation marker.
func workspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	w := func(rel, s string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("front/.cursor/rules/no-tooltip.mdc", "---\ndescription: No native tooltips\nglobs: **/*.{html,ts}\nalwaysApply: true\n---\nDo not use title.")
	w("front/.cursor/rules/code-map.mdc", "---\ndescription: graph\nalwaysApply: true\n---\nworkflow")
	w("back/.cursor/rules/java.mdc", "---\nglobs: **/*.java\n---\njava rule")
	var lines []string
	for i := 1; i <= 200; i++ {
		l := fmt.Sprintf("<div>line %d</div>", i)
		if i == 30 {
			l = "<i title=\"x\">JEVRULES-VIOLATES:no-tooltip</i>"
		}
		lines = append(lines, l)
	}
	w("front/src/a.component.html", strings.Join(lines, "\n")+"\n")
	w("front/src/clean.ts", "export const x = 1;\n")
	w("front/package-lock.json", "{}\n")
	git(t, filepath.Join(root, "front"), "init", "-q")
	git(t, filepath.Join(root, "front"), "add", ".")
	git(t, filepath.Join(root, "front"), "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	return root
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func replaceLine(t *testing.T, path string, n int, s string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	ls := strings.Split(string(data), "\n")
	ls[n-1] = s
	os.WriteFile(path, []byte(strings.Join(ls, "\n")), 0o644)
}

func runJSON(t *testing.T, root string, args ...string) (*engine.Report, []byte, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"--root", root, "--validator", "mock", "--format", "json", "--quiet"}, args...), &out, &errb)
	if code == exitUsage {
		t.Fatalf("usage error: %s", errb.String())
	}
	var rep engine.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s\nstderr: %s", err, out.String(), errb.String())
	}
	return &rep, out.Bytes(), code
}

func TestChangedPreexistingAndCache(t *testing.T) {
	root := workspace(t)
	file := filepath.Join(root, "front/src/a.component.html")

	// Change far from line 30: the violation is preexisting.
	replaceLine(t, file, 150, "<div>edited</div>")
	rep, raw, code := runJSON(t, root, "--fail-on", "likely", "--precision", "high")
	if code != exitFindings {
		t.Errorf("exit = %d, want 1", code)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("findings = %+v", rep.Findings)
	}
	f := rep.Findings[0]
	if f.Rule != "no-tooltip" || f.File != "front/src/a.component.html" || f.Confidence != "likely" || !f.Preexisting {
		t.Errorf("finding = %+v", f)
	}
	if len(f.Lines) == 0 || f.Lines[0].Start > 30 || f.Lines[0].End < 30 || f.Lines[0].End-f.Lines[0].Start >= 20 {
		t.Errorf("lines = %+v, want a ≤20-line range around 30", f.Lines)
	}
	if !strings.Contains(f.Evidence, "JEVRULES-VIOLATES") {
		t.Errorf("evidence = %q", f.Evidence)
	}
	if got := rep.Skipped.Rules; len(got) != 1 || got[0].Rule != "code-map" {
		t.Errorf("skipped rules = %+v", got)
	}
	validateSchema(t, raw)

	// acceptance #5: nothing changed → 0 validator calls
	rep2, _, _ := runJSON(t, root, "--precision", "high")
	if rep2.Summary.ValidatorCalls != 0 || len(rep2.Findings) != 1 {
		t.Errorf("rerun: calls=%d findings=%d", rep2.Summary.ValidatorCalls, len(rep2.Findings))
	}

	// A violation inside the changed hunk is not preexisting.
	replaceLine(t, file, 150, "<b title=\"y\">JEVRULES-VIOLATES:no-tooltip</b>")
	rep3, _, _ := runJSON(t, root, "--precision", "high")
	if len(rep3.Findings) != 1 || rep3.Findings[0].Preexisting || rep3.Findings[0].Lines[0].Start > 150 || rep3.Findings[0].Lines[0].End < 150 {
		t.Errorf("in-hunk finding = %+v", rep3.Findings)
	}

	// --whole-file: drill-down covers the whole file, no preexisting flag
	rep4, _, _ := runJSON(t, root, "--whole-file", "--precision", "high")
	if len(rep4.Findings) != 1 || len(rep4.Findings[0].Lines) != 2 || rep4.Findings[0].Preexisting {
		t.Errorf("whole-file finding = %+v", rep4.Findings)
	}
}

func TestDefaultIsFileLevel(t *testing.T) {
	root := workspace(t)
	replaceLine(t, filepath.Join(root, "front/src/a.component.html"), 150, "<div>edited</div>")
	rep, raw, _ := runJSON(t, root)
	if len(rep.Findings) != 1 || len(rep.Findings[0].Lines) != 0 || rep.Findings[0].Evidence != "" {
		t.Errorf("default should be file-level: %+v", rep.Findings)
	}
	if rep.Summary.ValidatorCalls != 1 {
		t.Errorf("default should make one call per file, made %d", rep.Summary.ValidatorCalls)
	}
	validateSchema(t, raw)
}

func TestPlanAndSelection(t *testing.T) {
	root := workspace(t)
	// clean tree: --changed selects nothing
	rep, raw, code := runJSON(t, root, "--plan")
	if code != exitOK || len(rep.RuleMatrix) != 0 {
		t.Errorf("clean plan: %d %+v", code, rep.RuleMatrix)
	}
	validateSchema(t, raw)

	os.WriteFile(filepath.Join(root, "front/src/new.ts"), []byte("x\n"), 0o644)          // untracked
	os.WriteFile(filepath.Join(root, "front/package-lock.json"), []byte("{1}\n"), 0o644) // generated
	rep, _, _ = runJSON(t, root, "--plan")
	if len(rep.RuleMatrix) != 1 || rep.RuleMatrix[0].File != "front/src/new.ts" {
		t.Errorf("plan matrix = %+v", rep.RuleMatrix)
	}
	if len(rep.Skipped.Files) != 1 || rep.Skipped.Files[0].Reason != "generated" {
		t.Errorf("skipped = %+v", rep.Skipped.Files)
	}

	rep, _, _ = runJSON(t, root, "--plan", "--all")
	var files []string
	for _, m := range rep.RuleMatrix {
		files = append(files, m.File)
	}
	if strings.Join(files, ",") != "front/src/a.component.html,front/src/clean.ts" {
		t.Errorf("--all (tracked only) = %v", files)
	}

	rep, _, _ = runJSON(t, root, "--plan", "--estimate", filepath.Join(root, "front/src"))
	if rep.Summary.EstimatedRequests != 3 { // three files, one request each
		t.Errorf("estimate = %+v", rep.Summary)
	}

	// explicit paths, --rule filter
	rep, _, _ = runJSON(t, root, "--rule", "java", filepath.Join(root, "front/src/clean.ts"))
	if len(rep.RuleMatrix) != 0 || len(rep.Skipped.Files) != 1 {
		t.Errorf("--rule filter: %+v %+v", rep.RuleMatrix, rep.Skipped.Files)
	}
}

func TestBudgetAndExitCodes(t *testing.T) {
	root := workspace(t)
	rep, _, code := runJSON(t, root, "--budget-tokens", "1", filepath.Join(root, "front/src"))
	if !rep.Incomplete || len(rep.Unchecked) != 2 || code != exitOK {
		t.Errorf("budget: incomplete=%v unchecked=%v code=%d", rep.Incomplete, rep.Unchecked, code)
	}

	var out, errb bytes.Buffer
	if c := run([]string{"--root", root, "--format", "yaml"}, &out, &errb); c != exitUsage {
		t.Errorf("bad format exit %d", c)
	}

	// JEV unreachable: exit 3; --plan still works
	t.Setenv("TYPESAFE_API_KEY", "k")
	t.Setenv("JEVRULES_JEV_ENDPOINT", "http://127.0.0.1:1/v1/systemone")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	out.Reset()
	os.WriteFile(filepath.Join(root, "front/src/new.ts"), []byte("x\n"), 0o644)
	if c := run([]string{"--root", root, "--quiet", "--no-cache", "--rule", "no-tooltip"}, &out, &errb); c != exitJEV {
		t.Errorf("unreachable exit %d\n%s", c, out.String())
	}
	if c := run([]string{"--root", root, "--plan"}, &out, &errb); c != exitOK {
		t.Errorf("plan with JEV down: exit %d", c)
	}
}

func TestRulesAndExplain(t *testing.T) {
	root := workspace(t)
	var out, errb bytes.Buffer
	if c := run([]string{"rules", "--root", root, "--for", filepath.Join(root, "front/src/a.component.html")}, &out, &errb); c != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "APPLIES  no-tooltip") || strings.Contains(out.String(), "APPLIES  code-map") {
		t.Errorf("rules --for:\n%s", out.String())
	}
	out.Reset()
	if c := run([]string{"explain", "--root", root, "no-tooltip"}, &out, &errb); c != 0 || !strings.Contains(out.String(), "Do not use title.") {
		t.Errorf("explain: %d %s", c, out.String())
	}
	if c := run([]string{"explain", "--root", root, "nope"}, &out, &errb); c != exitUsage {
		t.Errorf("explain unknown: %d", c)
	}
}

func validateSchema(t *testing.T, raw []byte) {
	t.Helper()
	c := jsonschema.NewCompiler()
	sch, err := c.Compile("../../jevrules.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("schema: %v", err)
	}
}

func TestDismiss(t *testing.T) {
	root := workspace(t)
	file := filepath.Join(root, "front/src/a.component.html")
	target := "front/src/a.component.html"
	var out, errb bytes.Buffer

	if c := run([]string{"dismiss", "--root", root, "no-tooltip", target}, &out, &errb); c != exitUsage {
		t.Errorf("dismiss without --reason: exit %d", c)
	}
	if c := run([]string{"dismiss", "--root", root, "java", target, "--reason", "x"}, &out, &errb); c != exitUsage {
		t.Errorf("dismiss with a rule that does not apply: exit %d", c)
	}
	if c := run([]string{"dismiss", "--root", root, "no-tooltip", target, "--reason", "title is an SVG attribute"}, &out, &errb); c != exitOK {
		t.Fatalf("dismiss: exit %d: %s", c, errb.String())
	}

	rep, raw, code := runJSON(t, root, "--fail-on", "possible", "--precision", "high", file)
	if code != exitOK || len(rep.Findings) != 0 || len(rep.Dismissed) != 1 || rep.Summary.Dismissed != 1 {
		t.Fatalf("after dismiss: exit %d, findings %+v, dismissed %+v", code, rep.Findings, rep.Dismissed)
	}
	if d := rep.Dismissed[0]; d.ID != "D1" || d.DismissedReason != "title is an SVG attribute" || len(d.Lines) != 0 {
		t.Errorf("dismissed = %+v (want no drill-down)", d)
	}
	if rep.RuleMatrix[0].Result != "dismissed" {
		t.Errorf("matrix result = %q", rep.RuleMatrix[0].Result)
	}
	validateSchema(t, raw)

	// The file changes: the dismissal no longer applies, but its reason is shown.
	replaceLine(t, file, 150, "<div>edited</div>")
	rep, raw, _ = runJSON(t, root, file)
	if len(rep.Findings) != 1 || len(rep.Dismissed) != 0 || rep.Findings[0].EarlierDismissal != "title is an SVG attribute" {
		t.Fatalf("after edit: findings %+v, dismissed %+v", rep.Findings, rep.Dismissed)
	}
	validateSchema(t, raw)

	// Dismissing again replaces the entry instead of adding one.
	if c := run([]string{"dismiss", "--root", root, "no-tooltip", target, "--reason", "still fine"}, &out, &errb); c != exitOK {
		t.Fatalf("dismiss again: exit %d: %s", c, errb.String())
	}
	b, _ := os.ReadFile(filepath.Join(root, ".jevrules-dismissed.json"))
	if n := strings.Count(string(b), `"ruleFile"`); n != 1 {
		t.Errorf("entries = %d, want 1:\n%s", n, b)
	}
}

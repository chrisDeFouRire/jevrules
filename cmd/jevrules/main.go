// Command jevrules checks files against `.cursor/rules` with JEV.
// See docs/spec.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"

	flag "github.com/spf13/pflag"

	"github.com/chrisDeFouRire/jevRules/internal/cache"
	"github.com/chrisDeFouRire/jevRules/internal/config"
	"github.com/chrisDeFouRire/jevRules/internal/engine"
	"github.com/chrisDeFouRire/jevRules/internal/files"
	"github.com/chrisDeFouRire/jevRules/internal/report"
	"github.com/chrisDeFouRire/jevRules/internal/rules"
	"github.com/chrisDeFouRire/jevRules/internal/validator"
)

const (
	exitOK       = 0
	exitFindings = 1
	exitUsage    = 2
	exitJEV      = 3
)

// version is set at build time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "rules":
			return cmdRules(args[1:], stdout, stderr)
		case "explain":
			return cmdExplain(args[1:], stdout, stderr)
		case "version", "--version":
			fmt.Fprintln(stdout, "jevrules", version)
			return exitOK
		case "help", "-h", "--help":
			usage(stdout)
			return exitOK
		}
	}
	return cmdCheck(args, stdout, stderr)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `jevrules checks your files against Cursor's .cursor/rules, using JEV.

Usage:
  jevrules [paths…] [flags]          check files (by default, your uncommitted changes)
  jevrules --plan [--estimate]       show which rules apply, without calling JEV
  jevrules rules [--for <file>]      list discovered rules
  jevrules explain <rule-id>         print a rule's text (use scope/id if ids collide)
  jevrules version                   print the version

Targets (union):  --changed (default) --staged --since <ref> --all <paths…>
Rules:            --rule <id> --exclude-rule <id> --scope <dir>
Output:           --format text|json --out <file> --fail-on none|possible|likely
Validator:        --validator jev|mock|none --model <id> --no-cache --jobs N
                  --precision none|low|normal|high --whole-file --budget-tokens N
Other:            --root <dir> (default: current directory) --config <file> --include-unscoped --quiet

API key: put it in ~/.config/jevrules/credentials ($XDG_CONFIG_HOME/jevrules/ if set),
         or set TYPESAFE_API_KEY. The environment variable wins.
Exit:    0 ok, 1 findings at or above --fail-on, 2 usage or config error,
         3 JEV unreachable, or key missing or invalid
`)
}

type common struct {
	root            string
	configFile      string
	includeUnscoped bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", "", "workspace root (default: current directory)")
	fs.StringVar(&c.configFile, "config", "", "config file (default: <root>/.jevrules.yml)")
	fs.BoolVar(&c.includeUnscoped, "include-unscoped", false, "treat manual rules (no globs, no alwaysApply) as code rules")
}

func (c *common) load() (string, *config.Config, *rules.Index, error) {
	root := c.root
	if root == "" {
		root = "."
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", nil, nil, err
	}
	cfg, err := config.Load(root, c.configFile)
	if err != nil {
		return "", nil, nil, err
	}
	idx, err := rules.Load(root, cfg, c.includeUnscoped)
	if err != nil {
		return "", nil, nil, err
	}
	return root, cfg, idx, nil
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jevrules", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	var c common
	c.register(fs)
	var (
		o                       files.Options
		ruleIDs, excludeIDs     []string
		scopes                  []string
		format, out, failOn     string
		plan, estimate          bool
		jobs                    int
		wholeFile               bool
		precision, vname, model string
		budget                  int
		noCache, quiet          bool
	)
	fs.BoolVar(&o.Changed, "changed", false, "uncommitted files: staged + unstaged + untracked (default)")
	fs.BoolVar(&o.Staged, "staged", false, "staged files only")
	fs.StringVar(&o.Since, "since", "", "files changed vs <ref>, plus uncommitted")
	fs.BoolVar(&o.All, "all", false, "every tracked file matching at least one rule")
	fs.StringArrayVar(&ruleIDs, "rule", nil, "only this rule (repeatable)")
	fs.StringArrayVar(&excludeIDs, "exclude-rule", nil, "skip this rule (repeatable)")
	fs.StringArrayVar(&scopes, "scope", nil, "only rules from this scope root, e.g. frontend (repeatable)")
	fs.StringVar(&format, "format", "text", "text | json")
	fs.StringVar(&out, "out", "", "write the report to a file")
	fs.StringVar(&failOn, "fail-on", "none", "none | possible | likely")
	fs.BoolVar(&plan, "plan", false, "list applicable rules per file, no validator call")
	fs.BoolVar(&estimate, "estimate", false, "with --plan: estimate requests and input tokens")
	fs.IntVar(&jobs, "jobs", 4, "concurrent files")
	fs.StringVar(&precision, "precision", "none", "line localisation: none (file-level, 1 call per file) | low (120-line windows) | normal (≤20 lines) | high (exact lines)")
	fs.BoolVar(&wholeFile, "whole-file", false, "do not limit drill-down to changed hunks")
	fs.IntVar(&budget, "budget-tokens", 0, "stop after this many input tokens (0 = no limit)")
	fs.StringVar(&vname, "validator", "", "jev | mock | none (default from config: jev)")
	fs.StringVar(&model, "model", "", "JEV model id (default from config: "+config.DefaultModel+")")
	fs.BoolVar(&noCache, "no-cache", false, "do not read or write .jevrules-cache")
	fs.BoolVar(&quiet, "quiet", false, "no progress on stderr")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	o.Paths = fs.Args()
	if !o.Staged && o.Since == "" && !o.All && len(o.Paths) == 0 {
		o.Changed = true
	}
	if format != "text" && format != "json" {
		fmt.Fprintf(stderr, "jevrules: --format must be text or json\n")
		return exitUsage
	}
	if failOn != "none" && failOn != "possible" && failOn != "likely" {
		fmt.Fprintf(stderr, "jevrules: --fail-on must be none, possible or likely\n")
		return exitUsage
	}
	if precision != "none" && precision != "low" && precision != "normal" && precision != "high" {
		fmt.Fprintf(stderr, "jevrules: --precision must be none, low, normal or high\n")
		return exitUsage
	}
	if estimate {
		plan = true
	}

	root, cfg, idx, err := c.load()
	if err != nil {
		fmt.Fprintf(stderr, "jevrules: %v\n", err)
		return exitUsage
	}
	if vname == "" {
		vname = cfg.Validator
	}
	if model != "" {
		cfg.Jev.Model = model
	}
	for _, id := range append(append([]string{}, ruleIDs...), excludeIDs...) {
		if len(idx.Find(id)) == 0 {
			fmt.Fprintf(stderr, "jevrules: unknown rule %q (see `jevrules rules`)\n", id)
			return exitUsage
		}
	}
	for i, s := range scopes {
		scopes[i] = strings.Trim(filepath.ToSlash(s), "/")
	}

	var v validator.Validator
	switch vname {
	case "jev":
		v = validator.NewJEV(cfg.Jev.Model)
	case "mock":
		m := &validator.Mock{Patterns: map[string][]*regexp.Regexp{}}
		for _, r := range idx.Rules {
			m.Patterns[r.ID] = append(m.Patterns[r.ID], r.Patterns...)
		}
		v = m
	case "none":
		plan = true
	default:
		fmt.Fprintf(stderr, "jevrules: unknown validator %q\n", vname)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sel := files.NewSelector(ctx, root, idx.ScopeRoots())
	e := &engine.Engine{
		Root: root, Cfg: cfg, Index: idx, Sel: sel, V: v,
		Cache: cache.Open(root, noCache || plan),
		Opt: engine.Options{
			Files: o, Plan: plan, Estimate: estimate, Jobs: jobs,
			Drilldown: precision != "none", Precision: precision, WholeFile: wholeFile,
			BudgetTokens: budget,
			RuleFilter:   ruleFilter(idx, ruleIDs, excludeIDs, scopes),
		},
	}
	if !quiet && !plan && isTTY(os.Stderr) {
		e.Opt.Progress = func(done, total int, file string) {
			fmt.Fprintf(stderr, "\r\033[K[%d/%d] %s", done, total, file)
			if done == total {
				fmt.Fprint(stderr, "\r\033[K")
			}
		}
	}

	rep, runErr := e.Run(ctx)
	if runErr != nil && rep == nil {
		fmt.Fprintf(stderr, "jevrules: %v\n", runErr)
		return exitUsage
	}
	fatal := errors.Is(runErr, validator.ErrAuth) || errors.Is(runErr, validator.ErrUnavailable)
	if runErr != nil && !fatal {
		fmt.Fprintf(stderr, "jevrules: %v\n", runErr)
		return exitUsage
	}

	w := stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			fmt.Fprintf(stderr, "jevrules: %v\n", err)
			return exitUsage
		}
		defer f.Close()
		w = f
	}
	if format == "json" {
		if err := report.JSON(w, rep); err != nil {
			fmt.Fprintf(stderr, "jevrules: %v\n", err)
			return exitUsage
		}
	} else {
		report.Text(w, rep, explainRef(idx))
	}

	if runErr != nil {
		fmt.Fprintf(stderr, "jevrules: %v\n", runErr)
		return exitJEV
	}
	if allErrored(rep) {
		return exitJEV
	}
	for _, f := range rep.Findings {
		if failOn == "possible" || failOn == "likely" && f.Confidence == "likely" {
			return exitFindings
		}
	}
	return exitOK
}

// allErrored: every unit that needed the validator failed (spec §5.5).
func allErrored(rep *engine.Report) bool {
	n, errs := 0, 0
	for _, m := range rep.RuleMatrix {
		if m.Result == "planned" {
			continue
		}
		n++
		if m.Result == "error" {
			errs++
		}
	}
	return n > 0 && errs == n
}

func ruleFilter(idx *rules.Index, only, exclude, scopes []string) func(*rules.Rule) bool {
	set := func(ids []string) map[*rules.Rule]bool {
		m := map[*rules.Rule]bool{}
		for _, id := range ids {
			for _, r := range idx.Find(id) {
				m[r] = true
			}
		}
		return m
	}
	onlySet, exSet := set(only), set(exclude)
	return func(r *rules.Rule) bool {
		if len(onlySet) > 0 && !onlySet[r] || exSet[r] {
			return false
		}
		if len(scopes) > 0 {
			for _, s := range scopes {
				if r.ScopeRoot == s || s == "." && r.ScopeRoot == "" {
					return true
				}
			}
			return false
		}
		return true
	}
}

// explainRef returns the shortest unambiguous `explain` argument for a rule.
func explainRef(idx *rules.Index) func(string) string {
	return func(ruleFile string) string {
		for _, r := range idx.Rules {
			if r.Path == ruleFile {
				if len(idx.Find(r.ID)) > 1 {
					return r.Scope() + "/" + r.ID
				}
				return r.ID
			}
		}
		return ruleFile
	}
}

func cmdRules(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs)
	var forFile, format string
	fs.StringVar(&forFile, "for", "", "show whether each rule applies to this file")
	fs.StringVar(&format, "format", "text", "text | json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	root, _, idx, err := c.load()
	if err != nil {
		fmt.Fprintf(stderr, "jevrules: %v\n", err)
		return exitUsage
	}
	rel := ""
	if forFile != "" {
		abs, _ := filepath.Abs(forFile)
		r, err := filepath.Rel(root, abs)
		if err != nil || strings.HasPrefix(r, "..") {
			fmt.Fprintf(stderr, "jevrules: %s is outside the root %s\n", forFile, root)
			return exitUsage
		}
		rel = filepath.ToSlash(r)
	}
	if format == "json" {
		type row struct {
			*rules.Rule
			Applies *bool `json:"applies,omitempty"`
		}
		var rows []row
		for _, r := range idx.Rules {
			x := row{Rule: r}
			if rel != "" {
				a := idx.Applies(r, rel)
				x.Applies = &a
			}
			rows = append(rows, x)
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{"root": root, "rules": rows})
		return exitOK
	}
	fmt.Fprintf(stdout, "%d rules under %s\n", len(idx.Rules), root)
	scope := "\x00"
	for _, r := range idx.Rules {
		if r.ScopeRoot != scope {
			scope = r.ScopeRoot
			fmt.Fprintf(stdout, "\n[%s]\n", r.Scope())
		}
		mark := ""
		if rel != "" {
			if idx.Applies(r, rel) {
				mark = "APPLIES  "
			} else {
				mark = "-        "
			}
		}
		globs := strings.Join(r.Globs, ", ")
		if globs == "" {
			globs = "(no globs)"
		}
		always := ""
		if r.AlwaysApply {
			always = " alwaysApply"
		}
		fmt.Fprintf(stdout, "  %s%-38s %-8s %s%s\n", mark, r.ID, r.Kind, globs, always)
	}
	return exitOK
}

func cmdExplain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: jevrules explain <rule-id | scope/rule-id>\n")
		return exitUsage
	}
	_, _, idx, err := c.load()
	if err != nil {
		fmt.Fprintf(stderr, "jevrules: %v\n", err)
		return exitUsage
	}
	rs := idx.Find(fs.Arg(0))
	if len(rs) == 0 {
		fmt.Fprintf(stderr, "jevrules: unknown rule %q (see `jevrules rules`)\n", fs.Arg(0))
		return exitUsage
	}
	for i, r := range rs {
		if i > 0 {
			fmt.Fprintln(stdout, "\n---")
		}
		fmt.Fprintf(stdout, "RULE %s  (%s)\n", r.ID, r.Path)
		if r.Description != "" {
			fmt.Fprintf(stdout, "description: %s\n", r.Description)
		}
		fmt.Fprintf(stdout, "scope: %s   globs: %s   kind: %s\n\n", r.Scope(), strings.Join(r.Globs, ", "), r.Kind)
		fmt.Fprintln(stdout, r.Body)
	}
	return exitOK
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

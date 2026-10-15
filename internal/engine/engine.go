package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chrisDeFouRire/jevRules/internal/cache"
	"github.com/chrisDeFouRire/jevRules/internal/config"
	"github.com/chrisDeFouRire/jevRules/internal/files"
	"github.com/chrisDeFouRire/jevRules/internal/rules"
	"github.com/chrisDeFouRire/jevRules/internal/validator"
)

// JEV context limits (jev-1.13/1.14): 32k tokens for state + one question,
// 64k for state + all questions. We stay under them with a margin because
// token counts are estimated.
const (
	perQuestionTokens = 32000 * 8 / 10
	perRequestTokens  = 64000 * 8 / 10
	hunkContext       = 10
)

var errBudget = errors.New("token budget exhausted")

type Options struct {
	Files        files.Options
	Plan         bool
	Estimate     bool
	Jobs         int
	Drilldown    bool
	Precision    string // none | low | normal | high (Drilldown is false for none)
	WholeFile    bool
	BudgetTokens int
	RuleFilter   func(*rules.Rule) bool
	// Progress, if set, receives one line per finished file (stderr).
	Progress func(done, total int, file string)
}

type Engine struct {
	Root  string
	Cfg   *config.Config
	Index *rules.Index
	Sel   *files.Selector
	V     validator.Validator
	Cache *cache.Cache
	Opt   Options

	calls, hits, tokens atomic.Int64
}

type outcome struct {
	file     string
	skip     string // reason; "" when processed
	silent   bool   // dropped without being counted (--all, no rules)
	row      *MatrixRow
	findings []Finding
	errs     []ErrorEntry
	estReq   int
	estTok   int
	budget   bool  // stopped by the budget guard
	fatal    error // aborts the run (authentication)
}

// Run executes the whole pipeline and returns the report. A non-nil error
// means the run aborted (e.g. authentication failure); the partial report is
// still returned.
func (e *Engine) Run(ctx context.Context) (*Report, error) {
	rep := &Report{
		Version:     1,
		GeneratedAt: time.Now().UTC().Truncate(time.Second),
		RepoRoot:    e.Root,
		Mode:        e.Opt.Files.Mode(),
		Plan:        e.Opt.Plan,
		Findings:    []Finding{},
		RuleMatrix:  []MatrixRow{},
		Skipped:     Skipped{Files: []SkippedFile{}, Rules: []SkippedRule{}},
		Errors:      []ErrorEntry{},
	}
	start := time.Now()
	defer func() { rep.Summary.DurationMs = time.Since(start).Milliseconds() }()
	if !e.Opt.Plan {
		rep.Validator = e.V.Name()
		rep.Model = e.V.Model()
	}
	rep.Summary.RulesLoaded = len(e.Index.Rules)
	for _, r := range e.Index.Rules {
		if e.Opt.RuleFilter != nil && !e.Opt.RuleFilter(r) {
			continue
		}
		switch r.Kind {
		case rules.KindWorkflow:
			rep.Skipped.Rules = append(rep.Skipped.Rules, SkippedRule{r.ID, r.Path, "workflow rule (" + r.KindReason + ")"})
		case rules.KindManual:
			rep.Skipped.Rules = append(rep.Skipped.Rules, SkippedRule{r.ID, r.Path, "manual rule (" + r.KindReason + ")"})
		}
	}

	cands, err := e.Sel.Select(ctx, e.Opt.Files, e.Index.ScopeRoots())
	if err != nil {
		return rep, err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	jobs := max(e.Opt.Jobs, 1)
	in := make(chan files.Candidate)
	out := make(chan outcome)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range in {
				o := e.process(ctx, c)
				if o.fatal != nil {
					cancel(o.fatal)
				}
				out <- o
			}
		}()
	}
	go func() {
		defer close(in)
		for _, c := range cands {
			select {
			case in <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(out) }()

	var outs []outcome
	done := 0
	for o := range out {
		outs = append(outs, o)
		done++
		if e.Opt.Progress != nil && !o.silent {
			e.Opt.Progress(done, len(cands), o.file)
		}
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].file < outs[j].file })

	for _, o := range outs {
		if o.silent {
			continue
		}
		rep.Summary.FilesConsidered++
		if o.skip != "" {
			rep.Summary.FilesSkipped++
			rep.Skipped.Files = append(rep.Skipped.Files, SkippedFile{o.file, o.skip})
			continue
		}
		rep.RuleMatrix = append(rep.RuleMatrix, *o.row)
		rep.Summary.RulesApplied += len(o.row.Rules)
		rep.Summary.EstimatedRequests += o.estReq
		rep.Summary.EstimatedInputTokens += o.estTok
		switch o.row.Result {
		case "findings", "clean":
			rep.Summary.FilesChecked++
		case "unchecked":
			rep.Incomplete = true
			rep.Unchecked = append(rep.Unchecked, o.file)
		}
		rep.Findings = append(rep.Findings, o.findings...)
		rep.Errors = append(rep.Errors, o.errs...)
	}
	if ctx.Err() != nil && len(outs) < len(cands) {
		rep.Incomplete = true
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return firstLine(a) < firstLine(b)
	})
	for i := range rep.Findings {
		rep.Findings[i].ID = fmt.Sprintf("F%d", i+1)
	}
	rep.Summary.Findings = len(rep.Findings)
	rep.Summary.ValidatorCalls = int(e.calls.Load())
	rep.Summary.CacheHits = int(e.hits.Load())
	rep.Summary.InputTokens = int(e.tokens.Load())
	if !e.Opt.Estimate {
		rep.Summary.EstimatedRequests, rep.Summary.EstimatedInputTokens = 0, 0
	}

	if cause := context.Cause(ctx); errors.Is(cause, validator.ErrAuth) || errors.Is(cause, validator.ErrUnavailable) {
		rep.Fatal = cause.Error()
		return rep, cause
	}
	return rep, nil
}

func firstLine(f Finding) int {
	if len(f.Lines) == 0 {
		return 0
	}
	return f.Lines[0].Start
}

// unit is one file being checked.
type unit struct {
	file    string
	lang    string
	lines   []string
	rules   []*rules.Rule
	byID    map[string]*rules.Rule
	changed []files.Range // nil: whole file counts as changed
	scoped  bool
}

func (e *Engine) process(ctx context.Context, c files.Candidate) outcome {
	o := outcome{file: c.File}
	if reason := files.Excluded(c.File, e.Cfg.Exclude); reason != "" {
		o.skip = reason
		return o
	}
	rs := e.Index.For(c.File, e.Opt.RuleFilter)
	if len(rs) == 0 {
		o.skip = "no applicable rules"
		o.silent = e.Opt.Files.All && len(e.Opt.Files.Paths) == 0
		return o
	}
	abs := filepath.Join(e.Root, filepath.FromSlash(c.File))
	st, err := os.Stat(abs)
	if err != nil {
		o.skip = "unreadable"
		return o
	}
	if e.Cfg.Defaults.MaxBytes > 0 && st.Size() > e.Cfg.Defaults.MaxBytes {
		o.skip = "too large"
		return o
	}
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	o.row = &MatrixRow{File: c.File, Rules: ids}

	if e.Opt.Plan && !e.Opt.Estimate {
		o.row.Result = "planned"
		return o
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		o.skip = "unreadable"
		o.row = nil
		return o
	}
	if files.IsBinary(content) {
		o.skip = "binary"
		o.row = nil
		return o
	}
	u := &unit{file: c.File, lang: validator.Language(c.File), lines: splitLines(string(content)), rules: rs, byID: map[string]*rules.Rule{}}
	for _, r := range rs {
		u.byID[r.ID] = r
	}

	// Deterministic prefilter (spec §8.8): rules whose patterns all miss are
	// answered 0 without asking.
	var ask []*rules.Rule
	probs := map[string]float64{}
	for _, r := range rs {
		if len(r.Patterns) > 0 && !anyMatch(r, string(content)) {
			probs[r.ID] = 0
			continue
		}
		ask = append(ask, r)
	}

	if e.Opt.Plan { // --estimate: first pass only, no network
		o.row.Result = "planned"
		if len(ask) > 0 {
			o.estReq, o.estTok = e.estimate(u, ask)
		}
		return o
	}

	if len(ask) > 0 {
		p, err := e.askRange(ctx, u, files.Range{Start: 1, End: len(u.lines)}, ask, 0)
		if err != nil {
			return e.failed(o, err)
		}
		for k, v := range p {
			probs[k] = v
		}
	}
	o.row.Probabilities = probs

	th := e.Cfg.Jev.Thresholds
	var flagged []*rules.Rule
	for _, r := range rs {
		if probs[r.ID] >= th.Possible {
			flagged = append(flagged, r)
		}
	}
	if len(flagged) == 0 {
		o.row.Result = "clean"
		return o
	}

	locs := map[string]location{}
	if e.Opt.Drilldown {
		if e.Opt.Files.DiffScoped() && !e.Opt.WholeFile {
			ch, err := e.Sel.ChangedLines(ctx, c, e.Opt.Files)
			if err != nil {
				o.errs = append(o.errs, ErrorEntry{File: c.File, Message: "diff: " + err.Error()})
			} else {
				u.changed, u.scoped = ch, ch != nil
			}
		}
		var drill []*rules.Rule
		for _, r := range flagged {
			if probs[r.ID] >= th.Drilldown {
				drill = append(drill, r)
			}
		}
		if len(drill) > 0 {
			locs, err = e.drilldown(ctx, u, drill)
			if err != nil {
				if errors.Is(err, errBudget) || errors.Is(err, validator.ErrAuth) || errors.Is(err, validator.ErrUnavailable) {
					return e.failed(o, err)
				}
				o.errs = append(o.errs, ErrorEntry{File: c.File, Message: "drill-down: " + err.Error()})
			}
		}
	}

	for _, r := range flagged {
		p := probs[r.ID]
		f := Finding{
			Rule:        r.ID,
			RuleFile:    r.Path,
			File:        c.File,
			Confidence:  "possible",
			Probability: round3(p),
			Message:     r.Description,
		}
		if p >= th.Likely {
			f.Confidence = "likely"
		}
		if f.Message == "" {
			f.Message = firstHeading(r.Body)
		}
		if l, ok := locs[r.ID]; ok {
			f.Lines = l.ranges
			f.Preexisting = l.preexisting
			f.Evidence = evidence(u, r, l.ranges)
		}
		o.findings = append(o.findings, f)
	}
	o.row.Result = "findings"
	return o
}

func (e *Engine) failed(o outcome, err error) outcome {
	o.row.Result = "error"
	if errors.Is(err, errBudget) {
		o.row.Result = "unchecked"
		o.budget = true
		return o
	}
	if errors.Is(err, context.Canceled) {
		o.row.Result = "unchecked"
		return o
	}
	if errors.Is(err, validator.ErrAuth) || errors.Is(err, validator.ErrUnavailable) {
		o.fatal = err
	}
	o.errs = append(o.errs, ErrorEntry{File: o.file, Message: err.Error()})
	return o
}

// askRange asks the given rules about lines [rng.Start, rng.End], splitting the
// range when it exceeds the context, and returns the max probability per rule
// over the pieces.
func (e *Engine) askRange(ctx context.Context, u *unit, rng files.Range, rs []*rules.Rule, depth int) (map[string]float64, error) {
	st := e.state(u, rng)
	qs := questions(rs)
	stTok := validator.EstimateTokens(st, nil)
	if stTok+validator.LargestQuestionTokens(qs) > perQuestionTokens && rng.End > rng.Start && depth < 12 {
		return e.askSplit(ctx, u, rng, rs, depth)
	}
	out := map[string]float64{}
	for _, group := range groupRules(rs, stTok, perRequestTokens) {
		p, err := e.ask(ctx, st, group)
		if errors.Is(err, validator.ErrTooLarge) && rng.End > rng.Start && depth < 12 {
			sp, err := e.askSplit(ctx, u, rng, group, depth)
			if err != nil {
				return nil, err
			}
			p = sp
		} else if err != nil {
			return nil, err
		}
		for k, v := range p {
			out[k] = v
		}
	}
	return out, nil
}

func (e *Engine) askSplit(ctx context.Context, u *unit, rng files.Range, rs []*rules.Rule, depth int) (map[string]float64, error) {
	a, b := halves(rng, 5)
	pa, err := e.askRange(ctx, u, a, rs, depth+1)
	if err != nil {
		return nil, err
	}
	pb, err := e.askRange(ctx, u, b, rs, depth+1)
	if err != nil {
		return nil, err
	}
	for k, v := range pb {
		pa[k] = max(pa[k], v)
	}
	return pa, nil
}

// groupRules packs questions so state + questions fit the per-request context.
func groupRules(rs []*rules.Rule, stateTok, limit int) [][]*rules.Rule {
	var groups [][]*rules.Rule
	var cur []*rules.Rule
	size := stateTok
	for _, r := range rs {
		q := validator.LargestQuestionTokens(questions([]*rules.Rule{r}))
		if len(cur) > 0 && size+q > limit {
			groups = append(groups, cur)
			cur, size = nil, stateTok
		}
		cur = append(cur, r)
		size += q
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// ask sends one request (one state, several rules), using the cache.
func (e *Engine) ask(ctx context.Context, st validator.State, rs []*rules.Rule) (map[string]float64, error) {
	stJSON, _ := json.Marshal(st)
	stDigest := cache.Digest(stJSON)
	out := map[string]float64{}
	missing := map[string]validator.Question{}
	keys := map[string]string{}
	for _, r := range rs {
		q := validator.BuildQuestion(r)
		k := cache.Key(stDigest, validator.QuestionDigest(q), e.V.Model())
		if p, _, ok := e.Cache.Get(k); ok {
			out[r.ID] = p
			continue
		}
		missing[r.ID] = q
		keys[r.ID] = k
	}
	if len(missing) == 0 {
		e.hits.Add(1)
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	est := validator.EstimateTokens(st, missing)
	if e.Opt.BudgetTokens > 0 && int(e.tokens.Load())+est > e.Opt.BudgetTokens {
		return nil, errBudget
	}
	e.calls.Add(1)
	res, err := e.V.Validate(ctx, validator.Request{State: st, Questions: missing})
	if err != nil {
		return nil, err
	}
	e.tokens.Add(int64(res.InputTokens))
	for id, p := range res.Probabilities {
		out[id] = p
		e.Cache.Put(keys[id], p, res.Model)
	}
	return out, nil
}

// askChoice sends one raw choice question, using the cache.
func (e *Engine) askChoice(ctx context.Context, st validator.State, id string, q validator.Question) (map[string]float64, error) {
	stJSON, _ := json.Marshal(st)
	k := cache.Key(cache.Digest(stJSON), validator.QuestionDigest(q), e.V.Model())
	if probs, ok := e.Cache.GetChoices(k); ok {
		e.hits.Add(1)
		return probs, nil
	}
	qs := map[string]validator.Question{id: q}
	if e.Opt.BudgetTokens > 0 && int(e.tokens.Load())+validator.EstimateTokens(st, qs) > e.Opt.BudgetTokens {
		return nil, errBudget
	}
	e.calls.Add(1)
	res, err := e.V.Validate(ctx, validator.Request{State: st, Questions: qs})
	if err != nil {
		return nil, err
	}
	e.tokens.Add(int64(res.InputTokens))
	e.Cache.PutChoices(k, res.Choices[id], res.Model)
	return res.Choices[id], nil
}

func (e *Engine) state(u *unit, rng files.Range) validator.State {
	var lines []string
	if len(u.lines) > 0 {
		lines = u.lines[rng.Start-1 : rng.End]
	}
	return validator.State{Path: u.file, Language: u.lang, Content: validator.NumberLines(lines, rng.Start)}
}

func (e *Engine) estimate(u *unit, rs []*rules.Rule) (int, int) {
	var reqs, tok int
	var walk func(rng files.Range)
	walk = func(rng files.Range) {
		st := e.state(u, rng)
		qs := questions(rs)
		stTok := validator.EstimateTokens(st, nil)
		if stTok+validator.LargestQuestionTokens(qs) > perQuestionTokens && rng.End > rng.Start {
			a, b := halves(rng, 5)
			walk(a)
			walk(b)
			return
		}
		for _, g := range groupRules(rs, stTok, perRequestTokens) {
			reqs++
			tok += validator.EstimateTokens(st, questions(g))
		}
	}
	walk(files.Range{Start: 1, End: len(u.lines)})
	return reqs, tok
}

func questions(rs []*rules.Rule) map[string]validator.Question {
	qs := make(map[string]validator.Question, len(rs))
	for _, r := range rs {
		qs[r.ID] = validator.BuildQuestion(r)
	}
	return qs
}

func anyMatch(r *rules.Rule, s string) bool {
	for _, re := range r.Patterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		ls[i] = strings.TrimSuffix(l, "\r")
	}
	return ls
}

// halves splits a range in two with a small overlap.
func halves(r files.Range, overlap int) (files.Range, files.Range) {
	mid := (r.Start + r.End) / 2
	a := files.Range{Start: r.Start, End: min(mid+overlap/2, r.End)}
	b := files.Range{Start: max(mid+1-overlap/2, r.Start), End: r.End}
	if b.Start <= a.Start { // tiny range
		b.Start = a.Start + 1
	}
	return a, b
}

func round3(p float64) float64 { return float64(int(p*1000+0.5)) / 1000 }

func firstHeading(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(l, "# "))
		if l != "" {
			return l
		}
	}
	return ""
}

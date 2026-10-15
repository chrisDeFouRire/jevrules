package engine

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/chrisDeFouRire/jevRules/internal/files"
	"github.com/chrisDeFouRire/jevRules/internal/rules"
	"github.com/chrisDeFouRire/jevRules/internal/validator"
)

// location is where drill-down localised a (file, rule) pair.
type location struct {
	ranges      []LineRange
	preexisting bool
}

// drilldown localises flagged rules (spec §8.3): re-ask the rules on
// overlapping windows, then bisect flagged windows down to the precision's
// minimum size. JEV only gives probabilities; the line ranges are ours.
func (e *Engine) drilldown(ctx context.Context, u *unit, rs []*rules.Rule) (map[string]location, error) {
	n := len(u.lines)
	full := []files.Range{{Start: 1, End: n}}
	regions := full
	if u.scoped {
		regions = expandHunks(u.changed, hunkContext, n)
	}

	flaggedIn := func(regs []files.Range, ask []*rules.Rule) (map[string][]files.Range, error) {
		hits := map[string][]files.Range{}
		for _, w := range windows(u.lines, regs, e.Cfg.Drilldown.Window, e.Cfg.Drilldown.Overlap) {
			p, err := e.askRange(ctx, u, w, ask, 0)
			if err != nil {
				return nil, err
			}
			for _, r := range ask {
				if p[r.ID] >= e.Cfg.Jev.Thresholds.Drilldown {
					hits[r.ID] = append(hits[r.ID], w)
				}
			}
		}
		return hits, nil
	}

	hits, err := flaggedIn(regions, rs)
	if err != nil {
		return nil, err
	}
	out := map[string]location{}
	var outside []*rules.Rule
	for _, r := range rs {
		if len(hits[r.ID]) > 0 {
			out[r.ID] = location{}
		} else if u.scoped {
			outside = append(outside, r)
		}
	}
	// Not near the change: look at the rest of the file (preexisting).
	if len(outside) > 0 {
		more, err := flaggedIn(full, outside)
		if err != nil {
			return nil, err
		}
		for _, r := range outside {
			if len(more[r.ID]) > 0 {
				hits[r.ID] = more[r.ID]
				out[r.ID] = location{preexisting: true}
			}
		}
	}

	minLines := 0 // low: flagged windows as-is
	if e.Opt.Precision != "low" {
		minLines = 20
	}
	for id, loc := range out {
		var rngs []files.Range
		for _, w := range hits[id] {
			if minLines == 0 {
				rngs = append(rngs, w)
				continue
			}
			b, err := e.bisect(ctx, u, u.byID[id], w, minLines)
			if err != nil {
				return nil, err
			}
			if e.Opt.Precision == "high" {
				b, err = e.pinpointAll(ctx, u, u.byID[id], b)
				if err != nil {
					return nil, err
				}
			}
			rngs = append(rngs, b...)
		}
		for _, m := range mergeRanges(rngs) {
			loc.ranges = append(loc.ranges, LineRange{m.Start, m.End})
		}
		out[id] = loc
	}
	return out, nil
}

// bisect narrows a flagged range: keep halves that still flag; if neither
// half flags on its own (the violation needs both), keep the parent.
func (e *Engine) bisect(ctx context.Context, u *unit, r *rules.Rule, rng files.Range, minLines int) ([]files.Range, error) {
	if rng.End-rng.Start+1 <= minLines {
		return []files.Range{rng}, nil
	}
	a, b := halves(rng, 2)
	var out []files.Range
	for _, h := range []files.Range{a, b} {
		p, err := e.askRange(ctx, u, h, []*rules.Rule{r}, 0)
		if err != nil {
			return nil, err
		}
		if p[r.ID] >= e.Cfg.Jev.Thresholds.Drilldown {
			sub, err := e.bisect(ctx, u, r, h, minLines)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
	}
	if len(out) == 0 {
		return []files.Range{rng}, nil
	}
	return out, nil
}

// pinpointAll replaces each range by the line(s) a choice question singles
// out, keeping the range when Jev picks "none" or is undecided.
func (e *Engine) pinpointAll(ctx context.Context, u *unit, r *rules.Rule, rs []files.Range) ([]files.Range, error) {
	var out []files.Range
	for _, rng := range rs {
		lines, err := e.pinpoint(ctx, u, r, rng)
		if err != nil {
			return nil, err
		}
		if len(lines) == 0 {
			out = append(out, rng)
			continue
		}
		for _, l := range lines {
			out = append(out, files.Range{Start: l, End: l})
		}
	}
	return out, nil
}

// pinpoint asks one choice question over the range's line numbers. It
// returns up to 3 lines with probability ≥ 0.25 and ≥ half the top one.
func (e *Engine) pinpoint(ctx context.Context, u *unit, r *rules.Rule, rng files.Range) ([]int, error) {
	st := e.state(u, rng)
	q := validator.PinpointQuestion(r, u.lines[rng.Start-1:rng.End], rng.Start)
	probs, err := e.askChoice(ctx, st, r.ID, q)
	if err != nil {
		return nil, err
	}
	type cand struct {
		line int
		p    float64
	}
	var cs []cand
	top := 0.0
	for k, p := range probs {
		top = max(top, p)
		if n, err := strconv.Atoi(k); err == nil {
			cs = append(cs, cand{n, p})
		}
	}
	if probs["none"] >= top {
		return nil, nil
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].p > cs[j].p })
	var lines []int
	for _, c := range cs {
		if len(lines) == 3 || c.p < 0.25 || c.p < top/2 {
			break
		}
		lines = append(lines, c.line)
	}
	sort.Ints(lines)
	return lines, nil
}

// windows cuts regions into overlapping windows of ~size lines, ending each
// window on a blank line when one is close to the cut.
func windows(lines []string, regions []files.Range, size, overlap int) []files.Range {
	size = max(size, 10)
	overlap = min(max(overlap, 0), size/2)
	var out []files.Range
	for _, reg := range regions {
		start := reg.Start
		for {
			end := min(start+size-1, reg.End)
			if end < reg.End {
				for i := end; i > end-overlap && i > start; i-- {
					if strings.TrimSpace(lines[i-1]) == "" {
						end = i
						break
					}
				}
			}
			out = append(out, files.Range{Start: start, End: end})
			if end >= reg.End {
				break
			}
			start = max(end-overlap+1, start+1)
		}
	}
	return out
}

func expandHunks(hs []files.Range, ctx, n int) []files.Range {
	var rs []files.Range
	for _, h := range hs {
		rs = append(rs, files.Range{Start: max(1, h.Start-ctx), End: min(n, h.End+ctx)})
	}
	return mergeRanges(rs)
}

func mergeRanges(rs []files.Range) []files.Range {
	if len(rs) == 0 {
		return nil
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Start < rs[j].Start })
	out := []files.Range{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+1 {
			last.End = max(last.End, r.End)
		} else {
			out = append(out, r)
		}
	}
	return out
}

// evidence picks up to 3 lines from the first range: changed lines first,
// then lines matching the rule's patterns, then the first non-blank lines.
func evidence(u *unit, r *rules.Rule, rs []LineRange) string {
	if len(rs) == 0 {
		return ""
	}
	rng := rs[0]
	var pick []int
	add := func(i int) {
		for _, p := range pick {
			if p == i {
				return
			}
		}
		if len(pick) < 3 && strings.TrimSpace(u.lines[i-1]) != "" {
			pick = append(pick, i)
		}
	}
	for _, re := range r.Patterns {
		for i := rng.Start; i <= rng.End; i++ {
			if re.MatchString(u.lines[i-1]) {
				add(i)
			}
		}
	}
	if u.scoped {
		for _, h := range u.changed {
			for i := max(h.Start, rng.Start); i <= min(h.End, rng.End); i++ {
				add(i)
			}
		}
	}
	for i := rng.Start; i <= rng.End && len(pick) < 3; i++ {
		add(i)
	}
	sort.Ints(pick)
	var b []string
	for _, i := range pick {
		l := strings.TrimSpace(u.lines[i-1])
		if len(l) > 200 {
			l = l[:200] + "…"
		}
		b = append(b, l)
	}
	return strings.Join(b, "\n")
}

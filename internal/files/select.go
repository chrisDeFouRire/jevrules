// Package files selects candidate files (spec §5.1) across the git
// repositories found under the workspace root. The root itself need not be a
// repository: each rule scope (e.g. backend/, frontend/) may be its
// own repo, or not a repo at all.
package files

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type Repo struct {
	Top string // absolute toplevel
}

type Options struct {
	Changed bool
	Staged  bool
	Since   string
	All     bool
	Paths   []string // as given on the command line (relative to cwd)
}

// Mode names the selection for the report.
func (o Options) Mode() string {
	var m []string
	if o.Changed {
		m = append(m, "changed")
	}
	if o.Staged {
		m = append(m, "staged")
	}
	if o.Since != "" {
		m = append(m, "since")
	}
	if o.All {
		m = append(m, "all")
	}
	if len(o.Paths) > 0 {
		m = append(m, "paths")
	}
	return strings.Join(m, "+")
}

// DiffScoped reports whether line-level diff scoping applies (spec §8.4).
func (o Options) DiffScoped() bool {
	return (o.Changed || o.Staged || o.Since != "") && !o.All && len(o.Paths) == 0
}

type Candidate struct {
	File string // root-relative, slash separated
	Repo *Repo  // nil when not in a git repo
	// Untracked files count as entirely changed.
	Untracked bool
}

type Selector struct {
	Root  string
	repos map[string]*Repo // keyed by scope root
	tops  []*Repo
}

// NewSelector detects the git repository (if any) containing each scope root.
func NewSelector(ctx context.Context, root string, scopeRoots []string) *Selector {
	s := &Selector{Root: root, repos: map[string]*Repo{}}
	byTop := map[string]*Repo{}
	for _, sc := range append([]string{""}, scopeRoots...) {
		t, err := repoTop(ctx, filepath.Join(root, sc))
		if err != nil {
			continue
		}
		r, ok := byTop[t]
		if !ok {
			r = &Repo{Top: t}
			byTop[t] = r
			s.tops = append(s.tops, r)
		}
		s.repos[sc] = r
	}
	return s
}

func (s *Selector) Repos() []*Repo { return s.tops }

// RepoFor returns the repo containing a root-relative file, if any.
func (s *Selector) RepoFor(file string) *Repo {
	abs := filepath.Join(s.Root, filepath.FromSlash(file))
	var best *Repo
	for _, r := range s.tops {
		if strings.HasPrefix(abs, r.Top+string(filepath.Separator)) && (best == nil || len(r.Top) > len(best.Top)) {
			best = r
		}
	}
	return best
}

// Select returns the union of the requested targets, deduplicated and sorted.
func (s *Selector) Select(ctx context.Context, o Options, scopeRoots []string) ([]Candidate, error) {
	set := map[string]Candidate{}
	add := func(c Candidate) {
		if prev, ok := set[c.File]; ok {
			c.Untracked = c.Untracked || prev.Untracked
		}
		set[c.File] = c
	}
	if o.Changed || o.Since != "" {
		for _, r := range s.tops {
			cs, err := s.statusChanged(ctx, r)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
				add(c)
			}
		}
	}
	if o.Staged {
		for _, r := range s.tops {
			out, err := git(ctx, r.Top, "diff", "--cached", "--name-only", "-z", "--diff-filter=d")
			if err != nil {
				return nil, err
			}
			for _, p := range splitZ(out) {
				if f, ok := s.fromRepo(r, p); ok {
					add(Candidate{File: f, Repo: r})
				}
			}
		}
	}
	if o.Since != "" {
		for _, r := range s.tops {
			out, err := git(ctx, r.Top, "diff", "--name-only", "-z", "--diff-filter=d", o.Since)
			if err != nil {
				return nil, fmt.Errorf("--since %s in %s: %w", o.Since, r.Top, err)
			}
			for _, p := range splitZ(out) {
				if f, ok := s.fromRepo(r, p); ok {
					add(Candidate{File: f, Repo: r})
				}
			}
		}
	}
	if o.All {
		for _, sc := range scopeRoots {
			fs, err := s.listDir(ctx, filepath.Join(s.Root, sc), false)
			if err != nil {
				return nil, err
			}
			for _, f := range fs {
				add(Candidate{File: f, Repo: s.RepoFor(f)})
			}
		}
	}
	for _, p := range o.Paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", p, err)
		}
		if st.IsDir() {
			fs, err := s.listDir(ctx, abs, true)
			if err != nil {
				return nil, err
			}
			for _, f := range fs {
				add(Candidate{File: f, Repo: s.RepoFor(f)})
			}
			continue
		}
		f, ok := s.rel(abs)
		if !ok {
			return nil, fmt.Errorf("target %s is outside the root %s", p, s.Root)
		}
		add(Candidate{File: f, Repo: s.RepoFor(f)})
	}
	out := make([]Candidate, 0, len(set))
	for _, c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// statusChanged returns staged + unstaged + untracked files, minus deletions.
func (s *Selector) statusChanged(ctx context.Context, r *Repo) ([]Candidate, error) {
	out, err := git(ctx, r.Top, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var cs []Candidate
	entries := splitZ(out)
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		xy, p := e[:2], e[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the next entry is the rename source
		}
		if strings.Contains(xy, "D") {
			continue
		}
		f, ok := s.fromRepo(r, p)
		if !ok {
			continue
		}
		if st, err := os.Stat(filepath.Join(s.Root, f)); err != nil || st.IsDir() {
			continue
		}
		cs = append(cs, Candidate{File: f, Repo: r, Untracked: xy == "??"})
	}
	return cs, nil
}

// listDir lists files under dir: git-tracked (plus untracked non-ignored when
// others is true) inside a repo, else a directory walk skipping generated dirs.
func (s *Selector) listDir(ctx context.Context, dir string, others bool) ([]string, error) {
	if _, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree"); err == nil {
		args := []string{"ls-files", "-z", "--full-name"}
		if others {
			args = append(args, "-co", "--exclude-standard")
		}
		out, err := git(ctx, dir, append(args, ".")...)
		if err != nil {
			return nil, err
		}
		top, err := repoTop(ctx, dir)
		if err != nil {
			return nil, err
		}
		r := &Repo{Top: top}
		var fs []string
		for _, p := range splitZ(out) {
			if f, ok := s.fromRepo(r, p); ok {
				fs = append(fs, f)
			}
		}
		return fs, nil
	}
	var res []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || generatedDir[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if f, ok := s.rel(p); ok {
				res = append(res, f)
			}
		}
		return nil
	})
	return res, err
}

func (s *Selector) fromRepo(r *Repo, p string) (string, bool) {
	return s.rel(filepath.Join(r.Top, filepath.FromSlash(p)))
}

// repoTop returns the toplevel of the repo containing dir, spelled from dir
// itself rather than from `--show-toplevel`: on case-insensitive filesystems
// git reports the on-disk case (…/Projects) while our paths keep the user's
// (…/projects), and symlinked roots differ the same way.
func repoTop(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	prefix := strings.Trim(strings.TrimSpace(string(out)), "/")
	if prefix == "" {
		return abs, nil
	}
	return filepath.Clean(filepath.Join(abs, strings.Repeat("../", len(strings.Split(prefix, "/"))))), nil
}

func (s *Selector) rel(abs string) (string, bool) {
	rel, err := filepath.Rel(s.Root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// --- skip rules (spec §5.1 last paragraph) ---

var generatedDir = map[string]bool{"build": true, "dist": true, "node_modules": true, "graphify-out": true, ".jevrules-cache": true}

// Excluded returns a skip reason for generated or configured-excluded paths.
func Excluded(file string, exclude []string) string {
	segs := strings.Split(file, "/")
	for _, s := range segs[:len(segs)-1] {
		if generatedDir[s] {
			return "generated"
		}
	}
	base := path.Base(file)
	if strings.HasSuffix(base, ".lock") || base == "package-lock.json" {
		return "generated"
	}
	for _, g := range exclude {
		if m, _ := doublestar.Match(g, file); m {
			return "excluded"
		}
	}
	return ""
}

// IsBinary sniffs for a NUL byte in the first 8 KiB.
func IsBinary(content []byte) bool {
	n := min(len(content), 8192)
	return bytes.IndexByte(content[:n], 0) >= 0
}

// --- diff hunks (spec §8.4) ---

type Range struct{ Start, End int }

// ChangedLines returns the changed line ranges (new-file numbering) of a
// candidate, or nil when the whole file counts as changed.
func (s *Selector) ChangedLines(ctx context.Context, c Candidate, o Options) ([]Range, error) {
	if c.Repo == nil || c.Untracked {
		return nil, nil
	}
	abs := filepath.Join(s.Root, filepath.FromSlash(c.File))
	rp, err := filepath.Rel(c.Repo.Top, abs)
	if err != nil {
		return nil, err
	}
	var base []string
	switch {
	case o.Since != "":
		ref := o.Since
		if a, b, ok := strings.Cut(ref, "..."); ok {
			if b == "" {
				b = "HEAD"
			}
			mb, err := git(ctx, c.Repo.Top, "merge-base", a, b)
			if err != nil {
				return nil, err
			}
			ref = strings.TrimSpace(string(mb))
		} else if a, _, ok := strings.Cut(ref, ".."); ok {
			ref = a
		}
		base = []string{ref}
	case o.Staged && !o.Changed:
		base = []string{"--cached"}
	default:
		if _, err := git(ctx, c.Repo.Top, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			return nil, nil // no commits yet: everything is new
		}
		base = []string{"HEAD"}
	}
	args := append([]string{"diff", "-U0", "--no-color", "--no-ext-diff"}, base...)
	out, err := git(ctx, c.Repo.Top, append(args, "--", filepath.ToSlash(rp))...)
	if err != nil {
		return nil, err
	}
	return parseHunks(out), nil
}

func parseHunks(diff []byte) []Range {
	rs := []Range{}
	for _, l := range strings.Split(string(diff), "\n") {
		if !strings.HasPrefix(l, "@@ ") {
			continue
		}
		// @@ -a,b +c,d @@
		f := strings.Fields(l)
		if len(f) < 3 || !strings.HasPrefix(f[2], "+") {
			continue
		}
		startS, countS, hasCount := strings.Cut(f[2][1:], ",")
		start, _ := strconv.Atoi(startS)
		count := 1
		if hasCount {
			count, _ = strconv.Atoi(countS)
		}
		if count == 0 { // pure deletion: mark the line after the gap
			rs = append(rs, Range{max(start, 1), max(start, 1)})
			continue
		}
		rs = append(rs, Range{start, start + count - 1})
	}
	return rs
}

func splitZ(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

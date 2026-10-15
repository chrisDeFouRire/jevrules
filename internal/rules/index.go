// Package rules discovers and parses `.cursor/rules` files and decides which
// rules apply to which files (spec §4).
package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/chrisDeFouRire/jevRules/internal/config"
)

type Kind string

const (
	KindCode     Kind = "code"     // checked against source files
	KindWorkflow Kind = "workflow" // instructs the agent's process; never sent to the validator
	KindManual   Kind = "manual"   // Cursor "apply manually / intelligently": never auto-selected
)

type Rule struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`      // root-relative, slash separated
	ScopeRoot   string   `json:"scopeRoot"` // root-relative dir containing `.cursor/`, "" for root
	Description string   `json:"description,omitempty"`
	Globs       []string `json:"globs"`
	AlwaysApply bool     `json:"alwaysApply"`
	Kind        Kind     `json:"kind"`
	KindReason  string   `json:"kindReason"`
	Body        string   `json:"-"`
	BodyHash    string   `json:"bodyHash"`

	Patterns []*regexp.Regexp  `json:"-"`
	Choice   map[string]string `json:"-"`
}

// Scope is the display name of the rule's scope root ("." for the root).
func (r *Rule) Scope() string {
	if r.ScopeRoot == "" {
		return "."
	}
	return r.ScopeRoot
}

type Index struct {
	Root       string
	Rules      []*Rule
	extensions map[string]bool
}

// Directories never descended into while looking for rules or files.
var SkipDirs = map[string]bool{
	"node_modules": true, "build": true, "dist": true, "target": true,
	"graphify-out": true, ".git": true, ".jevrules-cache": true,
}

// Load scans root for `**/.cursor/rules/**/*.{mdc,md}`.
// includeUnscoped turns manual rules into code rules (spec §4.2 last row).
func Load(root string, cfg *config.Config, includeUnscoped bool) (*Index, error) {
	idx := &Index{Root: root, extensions: map[string]bool{}}
	for _, e := range cfg.Defaults.Extensions {
		idx.extensions[strings.TrimPrefix(strings.ToLower(e), ".")] = true
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable subtree: ignore
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != root && (SkipDirs[name] || strings.HasPrefix(name, ".") && name != ".cursor") {
			return filepath.SkipDir
		}
		if name == "rules" && filepath.Base(filepath.Dir(p)) == ".cursor" {
			if err := idx.loadDir(p, cfg, includeUnscoped); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(idx.Rules, func(i, j int) bool { return idx.Rules[i].Path < idx.Rules[j].Path })
	return idx, nil
}

func (idx *Index) loadDir(dir string, cfg *config.Config, includeUnscoped bool) error {
	scopeAbs := filepath.Dir(filepath.Dir(dir))
	scope, err := filepath.Rel(idx.Root, scopeAbs)
	if err != nil {
		return err
	}
	scope = filepath.ToSlash(scope)
	if scope == "." {
		scope = ""
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := filepath.Ext(p)
		if ext != ".mdc" && ext != ".md" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(idx.Root, p)
		r, err := newRule(string(data), filepath.ToSlash(rel), scope, cfg, includeUnscoped)
		if err != nil {
			return err
		}
		idx.Rules = append(idx.Rules, r)
		return nil
	})
}

func newRule(src, relPath, scope string, cfg *config.Config, includeUnscoped bool) (*Rule, error) {
	fm, body := splitFrontmatter(src)
	r := &Rule{
		ID:          strings.TrimSuffix(path.Base(relPath), path.Ext(relPath)),
		Path:        relPath,
		ScopeRoot:   scope,
		Description: fm.description,
		AlwaysApply: fm.alwaysApply,
		Body:        strings.TrimSpace(body),
		Globs:       []string{},
	}
	if fm.hasGlobs {
		r.Globs = SplitGlobs(fm.globsRaw)
	}
	sum := sha256.Sum256([]byte(r.Body))
	r.BodyHash = hex.EncodeToString(sum[:])

	switch {
	case !fm.present:
		r.Kind, r.KindReason = KindManual, "no frontmatter"
	case len(r.Globs) > 0:
		r.Kind, r.KindReason = KindCode, "has globs"
	case r.AlwaysApply:
		r.Kind, r.KindReason = KindWorkflow, "alwaysApply without globs"
	default:
		r.Kind, r.KindReason = KindManual, "no globs and not alwaysApply"
	}
	if r.Kind == KindManual && includeUnscoped {
		r.Kind, r.KindReason = KindCode, "--include-unscoped"
	}

	if o, ok := cfg.Override(r.Scope(), r.ID); ok {
		if len(o.Globs) > 0 {
			r.Globs = o.Globs
			if r.Kind != KindCode {
				r.Kind, r.KindReason = KindCode, "globs set in "+config.FileName
			}
		}
		if o.Kind != "" {
			r.Kind, r.KindReason = Kind(o.Kind), "set in "+config.FileName
		}
		for _, pat := range o.Patterns {
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("%s: rules.%s.patterns: %w", config.FileName, r.ID, err)
			}
			r.Patterns = append(r.Patterns, re)
		}
		r.Choice = o.Choice
	}
	for _, g := range r.Globs {
		if !doublestar.ValidatePattern(g) {
			return nil, fmt.Errorf("%s: invalid glob %q", relPath, g)
		}
	}
	return r, nil
}

// InScope reports whether a root-relative file lies under the rule's scope
// root, returning the path relative to that scope.
func (r *Rule) InScope(file string) (string, bool) {
	if r.ScopeRoot == "" {
		return file, true
	}
	if rest, ok := strings.CutPrefix(file, r.ScopeRoot+"/"); ok {
		return rest, true
	}
	return "", false
}

// Applies implements spec §4.3. Globs are anchored at the scope root as in
// Cursor (`*.ts` is root-level only; use `**/*.ts` for any depth).
func (idx *Index) Applies(r *Rule, file string) bool {
	if r.Kind != KindCode {
		return false
	}
	rel, ok := r.InScope(file)
	if !ok {
		return false
	}
	if len(r.Globs) > 0 {
		for _, g := range r.Globs {
			if m, _ := doublestar.Match(g, rel); m {
				return true
			}
		}
		return false
	}
	// code rule without globs: every file with a default extension
	return idx.extensions[strings.TrimPrefix(strings.ToLower(path.Ext(rel)), ".")]
}

// For returns the code rules that apply to a root-relative file.
func (idx *Index) For(file string, filter func(*Rule) bool) []*Rule {
	var out []*Rule
	for _, r := range idx.Rules {
		if (filter == nil || filter(r)) && idx.Applies(r, file) {
			out = append(out, r)
		}
	}
	return out
}

// ScopeRoots lists the distinct scope roots, in order.
func (idx *Index) ScopeRoots() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range idx.Rules {
		if !seen[r.ScopeRoot] {
			seen[r.ScopeRoot] = true
			out = append(out, r.ScopeRoot)
		}
	}
	return out
}

// Find returns rules matching an id, or "<scope>/<id>".
func (idx *Index) Find(id string) []*Rule {
	var out []*Rule
	for _, r := range idx.Rules {
		if r.ID == id || r.Scope()+"/"+r.ID == id || r.Path == id {
			out = append(out, r)
		}
	}
	return out
}

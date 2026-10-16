// Package dismiss records findings that a person or agent examined and
// rejected, so later runs do not report them again (.jevrules-dismissed.json,
// meant to be committed). A dismissal is keyed by the content hashes of the
// file and of the rule body: it stops applying as soon as either changes.
package dismiss

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chrisDeFouRire/jevRules/internal/cache"
)

const FileName = ".jevrules-dismissed.json"

type Entry struct {
	RuleFile string `json:"ruleFile"` // root-relative rule path (ids can collide)
	File     string `json:"file"`     // root-relative, slash-separated
	FileSHA  string `json:"fileSha256"`
	RuleSHA  string `json:"ruleSha256"`
	Reason   string `json:"reason"`
	Date     string `json:"date"` // YYYY-MM-DD
}

type Store struct {
	path    string
	Entries []Entry
	byKey   map[[2]string]int
}

type fileFormat struct {
	Version    int     `json:"version"`
	Dismissals []Entry `json:"dismissals"`
}

// Load reads <root>/.jevrules-dismissed.json. A missing file is an empty store.
func Load(root string) (*Store, error) {
	s := &Store{path: filepath.Join(root, FileName)}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.index()
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	s.Entries = f.Dismissals
	s.index()
	return s, nil
}

func (s *Store) index() {
	s.byKey = map[[2]string]int{}
	for i, e := range s.Entries {
		s.byKey[[2]string{e.RuleFile, e.File}] = i
	}
}

// Lookup returns the dismissal for (rule, file) and whether it still applies
// to the given content. ok is false when there is no dismissal at all.
func (s *Store) Lookup(ruleFile, file string, content []byte, ruleBody string) (e Entry, current, ok bool) {
	if s == nil {
		return Entry{}, false, false
	}
	i, ok := s.byKey[[2]string{ruleFile, file}]
	if !ok {
		return Entry{}, false, false
	}
	e = s.Entries[i]
	return e, e.FileSHA == cache.Digest(content) && e.RuleSHA == cache.Digest([]byte(ruleBody)), true
}

// Add records a dismissal, replacing any earlier one for the same rule and file.
func (s *Store) Add(ruleFile, file string, content []byte, ruleBody, reason string, now time.Time) {
	e := Entry{
		RuleFile: ruleFile, File: file,
		FileSHA: cache.Digest(content), RuleSHA: cache.Digest([]byte(ruleBody)),
		Reason: reason, Date: now.Format("2006-01-02"),
	}
	if i, ok := s.byKey[[2]string{ruleFile, file}]; ok {
		s.Entries[i] = e
	} else {
		s.Entries = append(s.Entries, e)
	}
	sort.Slice(s.Entries, func(i, j int) bool {
		a, b := s.Entries[i], s.Entries[j]
		if a.RuleFile != b.RuleFile {
			return a.RuleFile < b.RuleFile
		}
		return a.File < b.File
	})
	s.index()
}

// Save writes the store, sorted, with stable formatting for clean diffs.
func (s *Store) Save() error {
	b, err := json.MarshalIndent(fileFormat{Version: 1, Dismissals: s.Entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(b, '\n'), 0o644)
}

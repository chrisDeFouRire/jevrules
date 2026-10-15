// Package cache stores validator answers by content hash (spec §8.5):
// an unchanged state + unchanged question + same model never calls JEV twice.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

const DirName = ".jevrules-cache"

type Cache struct {
	dir string // empty: disabled
}

type entry struct {
	Probability float64            `json:"p"`
	Choices     map[string]float64 `json:"choices,omitempty"`
	Model       string             `json:"model"`
}

// Open prepares <root>/.jevrules-cache, which ignores itself in git.
func Open(root string, disabled bool) *Cache {
	if disabled {
		return &Cache{}
	}
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Cache{}
	}
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); err != nil {
		_ = os.WriteFile(gi, []byte("*\n"), 0o644)
	}
	return &Cache{dir: dir}
}

// Key = sha256(state) + sha256(question incl. rule body) + model.
func Key(stateDigest, questionDigest, model string) string {
	sum := sha256.Sum256([]byte(stateDigest + "\x00" + questionDigest + "\x00" + model))
	return hex.EncodeToString(sum[:])
}

func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *Cache) path(key string) string {
	return filepath.Join(c.dir, key[:2], key[2:]+".json")
}

func (c *Cache) Get(key string) (float64, string, bool) {
	e, ok := c.get(key)
	return e.Probability, e.Model, ok
}

func (c *Cache) GetChoices(key string) (map[string]float64, bool) {
	e, ok := c.get(key)
	return e.Choices, ok && e.Choices != nil
}

func (c *Cache) get(key string) (entry, bool) {
	var e entry
	if c.dir == "" {
		return e, false
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return e, false
	}
	return e, json.Unmarshal(data, &e) == nil
}

func (c *Cache) Put(key string, p float64, model string) {
	c.put(key, entry{Probability: p, Model: model})
}

func (c *Cache) PutChoices(key string, probs map[string]float64, model string) {
	c.put(key, entry{Choices: probs, Model: model})
}

func (c *Cache) put(key string, e entry) {
	if c.dir == "" {
		return
	}
	data, _ := json.Marshal(e)
	fp := c.path(key)
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(fp), "tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil && cerr == nil {
		_ = os.Rename(tmp.Name(), fp)
	} else {
		_ = os.Remove(tmp.Name())
	}
}

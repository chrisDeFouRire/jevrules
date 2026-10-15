// Package config loads the optional .jevrules.yml sidecar file (spec §9).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const FileName = ".jevrules.yml"

// DefaultModel is pinned rather than "jev-latest": the alias moves between
// releases, which would silently shift probabilities and cache keys.
const DefaultModel = "jev-1.14.0"

type Thresholds struct {
	Likely    float64 `yaml:"likely"`
	Possible  float64 `yaml:"possible"`
	Drilldown float64 `yaml:"drilldown"`
}

type Jev struct {
	Model      string     `yaml:"model"`
	Thresholds Thresholds `yaml:"thresholds"`
}

type Defaults struct {
	Extensions []string `yaml:"extensions"`
	MaxBytes   int64    `yaml:"maxBytes"`
}

type Drilldown struct {
	Window  int `yaml:"window"`
	Overlap int `yaml:"overlap"`
}

// RuleOverride is a per-rule entry under `rules:`. Keys are a rule id
// (applies in every scope) or "<scope>/<id>" (one scope only).
type RuleOverride struct {
	Kind     string   `yaml:"kind"`     // code | workflow
	Patterns []string `yaml:"patterns"` // regex prefilter: JEV is asked only if one matches
	// Choice opts the rule into a `choice` question. Must contain a
	// "compliant" option; violation probability = 1 - P(compliant).
	Choice map[string]string `yaml:"choice"`
	Globs  []string          `yaml:"globs"` // replaces the rule's own globs
}

type Config struct {
	Validator string                  `yaml:"validator"`
	Jev       Jev                     `yaml:"jev"`
	Defaults  Defaults                `yaml:"defaults"`
	Drilldown Drilldown               `yaml:"drilldown"`
	Exclude   []string                `yaml:"exclude"`
	Rules     map[string]RuleOverride `yaml:"rules"`

	Path string `yaml:"-"` // file it was loaded from, empty if none
}

func Default() *Config {
	return &Config{
		Validator: "jev",
		Jev: Jev{
			Model:      DefaultModel,
			Thresholds: Thresholds{Likely: 0.85, Possible: 0.5, Drilldown: 0.5},
		},
		Defaults: Defaults{
			Extensions: []string{"java", "ts", "html", "scss", "properties", "sql", "xml", "gradle"},
			MaxBytes:   200000,
		},
		Drilldown: Drilldown{Window: 120, Overlap: 20},
		Rules:     map[string]RuleOverride{},
	}
}

// Load reads <root>/.jevrules.yml (or the explicit file) over the
// defaults. A missing default file is fine; a missing explicit one is not.
func Load(root, explicit string) (*Config, error) {
	cfg := Default()
	p := explicit
	if p == "" {
		p = filepath.Join(root, FileName)
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) && explicit == "" {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	cfg.Path = p
	if cfg.Rules == nil {
		cfg.Rules = map[string]RuleOverride{}
	}
	for k, r := range cfg.Rules {
		if r.Kind != "" && r.Kind != "code" && r.Kind != "workflow" {
			return nil, fmt.Errorf("%s: rules.%s.kind must be code or workflow", p, k)
		}
		if r.Choice != nil {
			if _, ok := r.Choice["compliant"]; !ok {
				return nil, fmt.Errorf("%s: rules.%s.choice needs a \"compliant\" option", p, k)
			}
		}
	}
	return cfg, nil
}

// Override returns the override for a rule, preferring "<scope>/<id>".
func (c *Config) Override(scope, id string) (RuleOverride, bool) {
	if o, ok := c.Rules[scope+"/"+id]; ok {
		return o, true
	}
	o, ok := c.Rules[id]
	return o, ok
}

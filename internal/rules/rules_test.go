package rules

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chrisDeFouRire/jevRules/internal/config"
)

func TestSplitGlobs(t *testing.T) {
	cases := map[string][]string{
		`**/*.java`:                                 {"**/*.java"},
		`"**/*.java"`:                               {"**/*.java"},
		`"**/*.ts,**/*.html,**/*.scss"`:             {"**/*.ts", "**/*.html", "**/*.scss"},
		`a.java,b.java`:                             {"a.java", "b.java"},
		`docs/**/*.md, docs/**/*.mdx`:               {"docs/**/*.md", "docs/**/*.mdx"},
		`"**/forms/**","**/form-fields/**"`:         {"**/forms/**", "**/form-fields/**"},
		`**/*.{html,ts}`:                            {"**/*.{html,ts}"},
		`"**/*.{bundle,properties,strings,po,pot}"`: {"**/*.{bundle,properties,strings,po,pot}"},
		`["a/**", "b/*.ts"]`:                        {"a/**", "b/*.ts"},
		``:                                          nil,
	}
	for in, want := range cases {
		if got := SplitGlobs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SplitGlobs(%q) = %q, want %q", in, got, want)
		}
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureIndex(t *testing.T, cfg *config.Config) *Index {
	root := t.TempDir()
	write(t, root, "back/.cursor/rules/java-fqn.mdc", "---\ndescription: No FQN\nglobs: **/*.java\nalwaysApply: false\n---\nbody")
	write(t, root, "back/.cursor/rules/sub/nested.mdc", "---\nglobs: \"module-shop/**/Shop.java\"\n---\nnested")
	write(t, root, "back/.cursor/rules/code-map.mdc", "---\ndescription: graph\nalwaysApply: true\n---\nworkflow")
	write(t, root, "front/.cursor/rules/no-tooltip.mdc", "---\ndescription: tooltips\nglobs: **/*.{html,ts}\nalwaysApply: true\n---\nno title")
	write(t, root, "front/.cursor/rules/style-guide.md", "---\napply: always\n---\nNever…")
	write(t, root, "front/.cursor/rules/plain.mdc", "just text, no frontmatter")
	write(t, root, "front/.cursor/rules/agent.mdc", "---\ndescription: pulled in when relevant\nalwaysApply: false\n---\nx")
	write(t, root, "front/node_modules/pkg/.cursor/rules/ignored.mdc", "---\nglobs: **\n---\nx")
	if cfg == nil {
		cfg = config.Default()
	}
	idx, err := Load(root, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestLoadKindsAndScopes(t *testing.T) {
	idx := fixtureIndex(t, nil)
	got := map[string]Kind{}
	for _, r := range idx.Rules {
		got[r.Scope()+"/"+r.ID] = r.Kind
	}
	want := map[string]Kind{
		"back/java-fqn":     KindCode,
		"back/nested":       KindCode,
		"back/code-map":     KindWorkflow,
		"front/no-tooltip":  KindCode,
		"front/style-guide": KindWorkflow, // apply: always, no globs
		"front/plain":       KindManual,
		"front/agent":       KindManual,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v\nwant %v", got, want)
	}
}

func TestAppliesScopeIsolation(t *testing.T) {
	idx := fixtureIndex(t, nil)
	ids := func(file string) []string {
		var out []string
		for _, r := range idx.For(file, nil) {
			out = append(out, r.ID)
		}
		return out
	}
	// acceptance #2: a .java file under front/ never matches a back/ rule
	if got := ids("front/src/Foo.java"); got != nil {
		t.Errorf("front java got %v", got)
	}
	if got := ids("back/src/Foo.java"); !reflect.DeepEqual(got, []string{"java-fqn"}) {
		t.Errorf("back java got %v", got)
	}
	// acceptance #3 shape: alwaysApply + glob applies, workflow does not
	if got := ids("front/src/app/x.component.html"); !reflect.DeepEqual(got, []string{"no-tooltip"}) {
		t.Errorf("html got %v", got)
	}
	// globs are anchored at the scope root (Cursor semantics)
	if got := ids("back/module-shop/src/Shop.java"); !reflect.DeepEqual(got, []string{"java-fqn", "nested"}) {
		t.Errorf("anchored got %v", got)
	}
	if got := ids("back/x/module-shop/src/Shop.java"); !reflect.DeepEqual(got, []string{"java-fqn"}) {
		t.Errorf("anchored mismatch got %v", got)
	}
}

func TestConfigOverrides(t *testing.T) {
	cfg := config.Default()
	cfg.Rules["code-map"] = config.RuleOverride{Kind: "code"}
	cfg.Rules["front/agent"] = config.RuleOverride{Globs: []string{"**/*.sql"}}
	cfg.Rules["java-fqn"] = config.RuleOverride{Kind: "workflow"}
	idx := fixtureIndex(t, cfg)
	k := map[string]Kind{}
	for _, r := range idx.Rules {
		k[r.Scope()+"/"+r.ID] = r.Kind
	}
	if k["back/code-map"] != KindCode || k["front/agent"] != KindCode || k["back/java-fqn"] != KindWorkflow {
		t.Fatalf("overrides not applied: %v", k)
	}
	// code rule without globs → default extensions
	if len(idx.For("back/a.sql", nil)) != 1 || len(idx.For("back/a.md", nil)) != 0 {
		t.Fatalf("default extension matching wrong")
	}
	if len(idx.For("front/q.sql", nil)) != 1 {
		t.Fatalf("override globs not used")
	}
}

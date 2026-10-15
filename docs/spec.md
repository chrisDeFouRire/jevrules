# `jevrules`: checking AI agents' code against Cursor rules

Status: draft spec · Owner: TBD · Date: 2026-10-15

## 1. Problem

`backend/.cursor/rules/*.mdc` and `frontend/.cursor/rules/*.mdc` (dozens of rules each, plus the odd legacy `.md` file) encode project conventions. Cursor applies them automatically; Claude Code and Codex do not read `.cursor/rules`. Agents using those tools therefore break conventions silently.

`jevrules` is a small CLI that:

1. selects a set of files (uncommitted, whole repo, or explicit paths),
2. determines which rules apply to each file (from the rule's `globs` / `alwaysApply`),
3. asks **JEV** to judge each (file, rule-set) pair efficiently,
4. prints a report an AI agent can act on directly ("rule X may be broken in file Y at lines A–B, because …, here is how to dig deeper").

It is advisory: it finds *candidate* violations for an agent or human to confirm. It is not a gate by default.

## 2. JEV (TypeSafe AI) and what it means for the design

JEV is TypeSafe AI's "System One" decision model (<https://typesafe.ai>, docs <https://docs.typesafe.ai>, keys at <https://console.typesafe.ai>). It is not a chat model: you send a `state` plus typed questions and get back calibrated probabilities. Reference facts used by this spec (from the public API reference, to re-verify at implementation time):

| Item | Value |
|---|---|
| Endpoint | `POST https://api.typesafe.ai/v1/systemone`, `Authorization: Bearer <key>` |
| Request | `{ model: "jev-latest", state: string\|object\|array, questions: { <name>: {type, instructions, criteria?} } }` |
| Question types | `noul` (yes/no → `noul` ∈ [0,1], **no confidence field**), `choice` (≤255 options → `choice`, `probabilities`, `confidence`), `score` (2–10 levels → `score`, `probabilities`, `confidence`) |
| Batching | Many questions per call, evaluated **in parallel and in isolation** against the **same single `state`**. No multi-state batching. |
| Limits | 32 MiB body, nesting ≤ 64, gzip/zstd accepted, `max_tokens_exceeded` (400) if over model context |
| Errors | 401 key, 413, 422 malformed question, 429 rate limit (back off), 529 overloaded (retry) |
| Pricing | per input token, about $42 per billion at the time of writing. That is cheap enough to check every applicable rule. |
| Access | Signups were reported paused in late Sept 2026; an API key must exist before M1 |

**Consequences for the design**

1. **One request per file**, with **one question per applicable rule** (rules are independent and isolated by construction).
2. JEV returns a **probability, not line numbers or explanations**. Localisation and explanation are the tool's job (§8.3 "drill-down"); the report must not pretend JEV pointed at a line.
3. Rules are prose with ❌/✅ examples. The rule body goes into `instructions` / `criteria` (§7.2); the file goes into `state`.
4. The tool needs a key: the `TYPESAFE_API_KEY` env var, or `~/.config/jevrules/credentials` (`$XDG_CONFIG_HOME/jevrules/credentials` when set). Never written to the report, cache or logs.

## 3. Goals / non-goals

**Goals**
- Zero dependency on Cursor; works from Claude Code, Codex, a git hook, or CI.
- Read the existing `.cursor/rules` files unchanged (single source of truth).
- Minimise validator calls: only changed files, only matching rules, cached results.
- Output stable and parseable (JSON) plus a compact human/agent text form.

**Non-goals**
- Auto-fixing code.
- Replacing linters (Checkstyle, ESLint) for rules that are mechanically checkable.
- Executing rules that instruct the *agent's workflow* rather than constrain code (see §4.3).
- Editing or generating `.cursor/rules` files.

## 4. Rule discovery and selection

### 4.1 Rule sources

Scan, relative to the repo root, every `**/.cursor/rules/*.{mdc,md}` (not inside `node_modules`, `build`, `dist`). Each directory's *scope root* is the directory that contains its `.cursor/`:

- `backend/.cursor/rules/*` → scope `backend/`
- `frontend/.cursor/rules/*` → scope `frontend/`

A rule only applies to files **under its scope root**. Globs are matched against the path *relative to the scope root* (so `**/*.java` in `backend` does not match anything in `frontend`).

### 4.2 Frontmatter parsing

Format observed in this repo:

```
---
description: Avoid fully-qualified class names in Java code
globs: **/*.java
alwaysApply: false
---
<markdown body = the rule text>
```

Parser must tolerate all variants seen today:

| Variant | Example |
|---|---|
| Bare glob | `globs: **/*.java` |
| Quoted glob | `globs: "**/*.java"` |
| Comma-separated | `globs: "**/*.ts,**/*.html,**/*.scss"` or unquoted `a.java,b.java` |
| Multiple quoted items | `globs: "**/forms/**","**/form-fields/**"` |
| Brace expansion | `**/*.{html,ts}` (commas inside braces are **not** separators) |
| Missing `globs` | rule is always-apply or manual (27 rules lack `globs`) |
| Different key | A legacy `.md` rule uses `apply: always`; treat it as `alwaysApply: true` |
| No frontmatter | whole file is body; treated as manual (never auto-selected) unless `--include-unscoped` |

A rule record: `{ id, path, scopeRoot, description, globs[], alwaysApply, body, bodyHash }`. `id` is the file name without extension (e.g. `java-no-fully-qualified-names`).

### 4.3 Applicability

For a candidate file `f`, rule `r` applies iff `f` is under `r.scopeRoot` **and**:

- `r.globs` non-empty and any glob matches (gitignore-style, `**` aware, brace expansion), **or**
- `r.alwaysApply` is true **and** the rule is a *code rule* (below).

Many `alwaysApply: true` rules are workflow instructions, not code constraints (e.g. `code-map`, `read-docs-first`, `consult-backend-api`, `build-before-done`, `use-gradle-wrapper`, `commit-messages`). Checking source files against them is noise. Therefore:

- Add an optional **sidecar override file** `.jevrules.yml` at repo root (§9) with `kind: workflow | code` per rule id (default `code` when the rule has globs, `workflow` when always-apply and no globs).
- `workflow` rules are never sent to the validator; they are listed under `skipped` in the report with the reason.
- An `alwaysApply` rule with no globs and `kind: code` applies to all files whose extension is in `defaults.extensions` (default: `java, ts, html, scss, properties, sql, xml, gradle`).

## 5. CLI

```
jevrules [targets] [options]
```

### 5.1 Target selection (mutually composable; union)

| Flag | Meaning |
|---|---|
| *(default)* / `--changed` | Uncommitted files: staged + unstaged + untracked (non-ignored), excluding deletions. |
| `--staged` | Staged only (pre-commit hook). |
| `--since <ref>` | Files changed vs. `<ref>` (e.g. `origin/main...HEAD`) plus uncommitted. |
| `--all` | Every tracked file under any rule scope that matches at least one rule. |
| `<path>…` | Explicit files or directories (directories expanded recursively, respecting `.gitignore`). |

Binary files, files > `--max-bytes` (default 200 000), generated paths (`build/`, `dist/`, `node_modules/`, `graphify-out/`, `*.lock`, `package-lock.json`) and files matched by `.jevrules.yml` `exclude` are skipped and counted in the summary.

### 5.2 Rule filters

`--rule <id>` (repeatable), `--scope backend|frontend`, `--exclude-rule <id>`.

### 5.3 Output

`--format text` (default, §6.2) | `json` (§6.1) | `sarif` (optional, v2).
`--out <file>` to write instead of stdout. `--fail-on <none|possible|likely>` sets exit code behaviour (§5.5), default `none`.

### 5.4 Modes

| Command | Behaviour |
|---|---|
| `jevrules` | Full run with validator. |
| `jevrules --plan` | No validator call. Prints for each file the rules that apply. Cheap; lets an agent read the relevant rules itself. |
| `jevrules rules [--for <file>]` | Lists discovered rules, their globs, kind, and (with `--for`) whether they apply. |
| `jevrules explain <rule-id>` | Prints the rule body, so an agent can load it on demand. |

### 5.5 Exit codes

`0` ran, nothing at/above `--fail-on`; `1` findings at/above threshold; `2` usage/config error; `3` JEV unavailable, key missing/invalid, or errored for all units. Partial validator failures yield `0`/`1` plus `errors[]` in the report.

## 6. Report format

The report must let an agent go from "what's wrong" to "what to read next" without extra discovery.

### 6.1 JSON (canonical)

```json
{
  "version": 1,
  "generatedAt": "2026-10-15T10:00:00Z",
  "repoRoot": "/abs/path",
  "mode": "changed",
  "summary": {
    "filesConsidered": 14, "filesChecked": 11, "filesSkipped": 3,
    "rulesLoaded": 59, "rulesApplied": 17, "validatorCalls": 11, "cacheHits": 4,
    "findings": 3
  },
  "findings": [
    {
      "id": "F1",
      "rule": "java-no-fully-qualified-names",
      "ruleFile": "backend/.cursor/rules/java-no-fully-qualified-names.mdc",
      "file": "backend/module-billing/src/main/java/com/example/billing/Foo.java",
      "confidence": "likely",
      "probability": 0.93,
      "lines": [{ "start": 42, "end": 42 }],
      "evidence": "public java.util.List<java.time.LocalDate> dates(...)",
      "message": "Avoid fully-qualified class names in Java code; add an import instead"
    }
  ],
  "ruleMatrix": [
    { "file": "…/Foo.java", "rules": ["java-no-fully-qualified-names", "java-coding-style"], "result": "findings" }
  ],
  "skipped": {
    "files": [{ "file": "…/package-lock.json", "reason": "excluded" }],
    "rules": [{ "rule": "code-map", "reason": "workflow rule" }]
  },
  "errors": []
}
```

`confidence` ∈ `likely | possible` derived from JEV's `noul` probability (§7.3); the raw value is in `probability`. `lines` and `evidence` come from drill-down (§8.3) and are omitted with `--no-drilldown`. JEV does not produce `message`/`suggestion`: `message` is the rule's `description` frontmatter, and `suggestion` is omitted (the agent reads the rule via `explain`).

### 6.2 Text (agent-oriented)

Grouped by **rule first** (an agent usually fixes one convention at a time), compact, with copy-pasteable follow-ups:

```
jevrules: 11 files checked, 17 rule applications, 3 possible violations

RULE java-no-fully-qualified-names  (backend/.cursor/rules/java-no-fully-qualified-names.mdc)
  "Avoid fully-qualified class names in Java code; add an import instead"
  - backend/module-billing/src/main/java/com/example/billing/Foo.java:42  [likely]
      public java.util.List<java.time.LocalDate> dates(...)
  next: jevrules explain java-no-fully-qualified-names

CLEAN (checked, no findings): 8 files
SKIPPED: 3 files (excluded), 9 workflow rules
```

Rules for the text output: no ANSI colour unless TTY; paths are repo-root-relative; one finding per line plus at most one evidence line; always include the rule file path and the `next:` hint.

## 7. Architecture

```
targets ─▶ FileSelector ─▶ RuleIndex ─▶ Planner ─▶ JevClient ─▶ Drilldown ─▶ Reporter
              (git)        (parse+glob)  (units+cache)  (HTTP)    (localise)   (json/text)
```

- **FileSelector**: shells out to `git status --porcelain=v1 -z`, `git diff --name-only`, `git ls-files`. This workspace root is currently *not* a git repo; each of `backend/` and `frontend/` may be, so the selector detects repos per scope and handles multiple roots; with no git, only explicit paths and `--all` (directory walk).
- **RuleIndex**: parses frontmatter (tolerant `globs` grammar, §4.2), compiles globs once.
- **Planner**: builds one *unit* per file (§8), consults the cache.
- **JevClient**: the only JEV-aware component, behind a `Validator` interface (`jev`, `none`, `mock`) so tests and `--plan` need no network.
- **Drilldown**: second pass on flagged (file, rule) pairs to find lines (§8.3).
- **Reporter**: pure function from results to §6 formats.

### 7.1 Validator interface

```
validate({ file, content, rules: [{id, text, examples?}], lineRange? })
   -> { results: [{ ruleId, violationProbability: 0..1, usage }] }
```

### 7.2 Request construction (JEV)

For file `f` with applicable rules `r1..rn`:

```json
{
  "model": "jev-latest",
  "state": {
    "path": "backend/module-billing/src/main/java/com/example/billing/Foo.java",
    "language": "java",
    "content": "<file text, each line prefixed `NNN| `>"
  },
  "questions": {
    "java-no-fully-qualified-names": {
      "type": "noul",
      "instructions": "Does `content` violate the rule below? Judge only `content` against the rule.\n\nRULE:\n<rule body verbatim>",
      "criteria": {
        "true": "At least one construct in `content` violates the rule (see its BAD examples)",
        "false": "`content` complies with the rule, or the rule does not concern it"
      }
    }
  }
}
```

- Question keys are rule ids (keys are not sent to the model; they only route answers).
- Rule bodies are sent verbatim (some are French: do not translate).
- Line prefixes let drill-down chunks keep original numbering.
- Use `noul` by default (one probability = violation likelihood). A rule may opt into `choice` in `.jevrules.yml` when it has distinct violation kinds (e.g. `{compliant, violates-A, violates-B}`), which also yields `confidence`.
- Compress the body (zstd/gzip) when ≥ 1 KiB.

### 7.3 Probability → confidence

Defaults (tunable in config; **to be calibrated on real repo data in M1**):

| `noul` | `confidence` in report |
|---|---|
| ≥ 0.85 | `likely` |
| 0.5 – 0.85 | `possible` |
| < 0.5 | not reported |

The raw probability is always included in JSON.

## 8. Efficiency

1. **Glob pre-filter.** A file is never sent with a rule that doesn't match it. A typical changed file has 2–6 rules, not 59.
2. **One call per file** with all its rules as questions (JEV evaluates them in parallel). Split into several calls only if the request would exceed the model context (`max_tokens_exceeded`), and then split the *file* into windows (§8.3), not the rules.
3. **Drill-down for localisation (§6 needs lines)**, run only for pairs with `noul ≥ drilldown.threshold` (default 0.5):
   - Split the file into overlapping windows (default 120 lines, 20 overlap, aligned to blank lines / method boundaries when cheap to detect).
   - Re-ask the *same* rule's question per window (one call per window, all flagged rules as questions).
   - Windows scoring ≥ threshold become `lines: [{start,end}]` and the window's lines become `evidence` (trimmed to ≤ 3 lines).
   - Optional bisect refinement to ≤ 20 lines (`--precision high`).
   - If drill-down is disabled (`--no-drilldown`) findings have file-level location only.
4. **Diff-scoped mode** for `--changed/--staged/--since`: drill-down windows are limited to hunks that touch changed lines (± 10); a finding found only outside them is marked `preexisting: true`. The whole-file first pass still runs (a change can break a rule elsewhere in the file, e.g. a missing import). `--whole-file` disables the scoping.
5. **Content-hash cache** `.jevrules-cache/` (gitignored): key = `sha256(file content) + sha256(rule body) + model + question-form digest`; value = probability. Unchanged file + unchanged rule ⇒ no call.
6. **Concurrency** `--jobs N` (default 4); on 429/529 exponential backoff with jitter (max 5 tries); 401 aborts with exit 3.
7. **Cost guard**: `--budget-tokens N` stops cleanly and reports `incomplete: true` with the unchecked files listed. A `--plan --estimate` prints expected request count and input tokens, no network.
8. **Deterministic pre-checks (v2)**: per-rule `patterns:` regex that must hit before JEV is asked (e.g. `\bjava\.\w+\.\w+` for fully-qualified names); cuts calls and false positives for syntactic rules.

## 9. Configuration: `.jevrules.yml` (repo root, optional)

```yaml
validator: jev            # jev | none
jev:
  model: jev-latest
  thresholds: { likely: 0.85, possible: 0.5, drilldown: 0.5 }
defaults:
  extensions: [java, ts, html, scss, properties, sql, xml, gradle]
  maxBytes: 200000
exclude:
  - "**/build/**"
  - "**/node_modules/**"
  - "graphify-out/**"
rules:
  code-map:            { kind: workflow }
  read-docs-first:     { kind: workflow }
  java-no-fully-qualified-names:
    patterns: ['\bjava\.\w+\.\w+']   # optional prefilter (v2)
```

All keys optional; the tool must work with no config file.

## 10. Acceptance criteria

1. `jevrules rules` lists every rule in both `.cursor/rules` folders with parsed globs; no rule is dropped for a quoting/comma/brace variant in §4.2.
2. A `.java` file under `frontend/` never matches a `backend` rule, and vice versa.
3. `jevrules --plan` on a changed `.component.html` lists `no-native-html-tooltip` (alwaysApply + glob) and not `code-map`.
4. A file with `title="x"` on an `<i>` is reported against `no-native-html-tooltip`; a file using `<ui-panel title="…">` is not (documented exception in that rule).
5. Re-running with no file/rule changes performs 0 validator calls.
6. `--format json` output validates against a published JSON schema (`jevrules.schema.json`).
7. With the validator unreachable: exit code `3`, `--plan` still works.
8. Handles ≥ 1 000 files in `--all` without loading them all in memory at once (stream units).
9. A fixtures suite (≥ 3 known-bad and 3 known-good snippets per sampled rule, e.g. `java-no-fully-qualified-names`, `no-native-html-tooltip`, `api-models-no-dto-suffix`) reaches ≥ 90 % agreement at the default thresholds; results recorded in `docs/jevrules-calibration.md`.

## 11. Agent integration

- Add a short section to root `AGENTS.md` (read by Codex) and a `CLAUDE.md` pointer: "Before finishing a task, run `jevrules --changed --format text`; for each finding run `jevrules explain <rule>` and fix or justify."
- Optional Claude Code Stop/PreToolUse hook and git `pre-commit` calling `jevrules --staged --fail-on likely`.

## 12. Risks

| Risk | Mitigation |
|---|---|
| Workflow-style rules produce noise | `kind: workflow` default heuristics + sidecar overrides (§4.3). |
| False positives erode trust | Confidence levels, evidence line, `preexisting` flag, advisory exit code by default. |
| Some rules are not written in English | Pass rule text verbatim; no translation. |
| Rule drift between Cursor and this tool | Tool reads the same files; no copies. |
| JEV returns probabilities only (no lines/explanations) | Drill-down pass (§8.3); evidence is the tool's own excerpt, not JEV's reasoning. |
| Probability calibration for rule-compliance is unproven | Calibrate thresholds on a labelled sample in M1; keep advisory by default. |
| API access/pricing changes (signups reported paused) | `Validator` interface, `--plan` works offline, cache limits spend. |
| Prose rules with ❌/✅ examples may confuse a decision model | Evaluate per rule on known-bad/known-good fixtures (acceptance #9). |

## 13. Milestones

1. **M0**: rule index, glob matching, `rules` and `--plan` (no validator). Useful standalone.
2. **M1**: JEV client (one request per file), probability → confidence, calibration on fixtures, text + JSON report, `--changed`/paths.
3. **M2**: drill-down for line numbers, cache, diff-scoped mode, `--all`, `--since`, concurrency, budget guard.
4. **M3**: sidecar config, regex prefilters, hooks/AGENTS.md integration, SARIF.

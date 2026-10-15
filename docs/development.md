# Development

## Build and test

```bash
go build -o jevrules ./cmd/jevrules
go test ./...      # offline: uses the mock validator and an httptest JEV server
```

`--validator mock` flags a rule when the file contains `JEVRULES-VIOLATES:<rule-id>` or matches the rule's `patterns`, so you can exercise the whole pipeline without a key. `JEVRULES_JEV_ENDPOINT` overrides the JEV API URL.

## Releasing

Releases are built by [GoReleaser](https://goreleaser.com) ([`.goreleaser.yaml`](../.goreleaser.yaml)) from the [release workflow](../.github/workflows/release.yml) when a `v*` tag is pushed:

```bash
git tag v0.2.0
git push origin v0.2.0
```

The workflow runs the tests, builds static binaries for `darwin/arm64` and `linux/amd64`, and publishes a GitHub Release with `jevrules_<os>_<arch>.tar.gz` archives, `checksums.txt` and a changelog generated from the commits. Tags with a suffix such as `v0.2.0-rc1` are published as pre-releases. The version is stamped into the binary (`jevrules version`); local builds report `dev`.

To try the release build locally without publishing:

```bash
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

## Deviations from the spec

Where the implementation departs from [spec.md](spec.md):

- The default model is `jev-1.14.0` rather than `jev-latest`, so probabilities and the cache stay stable across JEV releases.
- Slash-less globs follow Cursor (anchored to the scope root), not gitignore (match at any depth).
- For `alwaysApply: true` rules that also have globs, the globs still restrict which files are checked. Cursor ignores globs on always-apply rules, but sending, say, `no-native-html-tooltip` with every `.java` file would only add noise.
- Drill-down is opt-in (`--precision`, default `none`), so the spec's `--no-drilldown` (§8.3) is effectively the default. Findings are file-level, without `lines` or `evidence`. An agent can find the violation itself once it has read the rule, and a run costs exactly one JEV call per file.
- When a file fits but all its questions together exceed the per-request context, the rules are split across requests. The file itself is windowed only when it does not fit on its own.
- Added: the `version` command, `--config`, `--validator mock`, a circuit breaker for an unreachable JEV, per-rule `probabilities` in `ruleMatrix`, `summary.durationMs` in the report, and an estimated cost and run time in the text footer. The credentials file follows `$XDG_CONFIG_HOME`.
- Dropped: the fixtures calibration suite (acceptance #9). The default `possible` threshold is 0.5, the natural cut-off for a probability.
- Not done yet (v2 in the spec): SARIF output.

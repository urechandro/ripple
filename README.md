# ripple

ripple watches your Go project for file changes and runs only the tests affected by your edit — no more waiting for the full test suite on every save.

It uses a reverse dependency graph to find which packages import what you changed, and a call graph to narrow it down to the specific test functions that actually exercise the modified code.

## Install

```bash
brew tap urechandro/ripple
brew install ripple
```

## Usage

Run ripple from your project root:

```bash
ripple
```

Save a `.go` file and ripple will immediately run only the tests relevant to your change.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-method` | `cha` | Call graph algorithm: `cha` (fast, conservative) or `rta` (slower, precise) |
| `-depth` | `1` | How far up the reverse dep graph to walk: `0` = changed package only, `1` = +direct importers, `-1` = full transitive |
| `-skip` | `integration` | Comma-separated substrings — packages whose import path matches are skipped |
| `-json` | | Non-interactive mode: print affected tests as JSON and exit |
| `-run` | | With `-json`: also run the tests and include results |
| `-files` | | With `-json`: comma-separated list of changed files (default: detect from `git diff`) |
| `-base` | | With `-json`: git ref to diff against (e.g. `origin/main`). Default: `HEAD` |

### Keyboard shortcuts

| Key | Action |
|-----|--------|
| `r` | Rebuild the call graph |
| `d` | Toggle debug view |
| `v` | Toggle coverage |
| `c` | Cancel running tests |
| `q` / `ctrl+c` | Quit |

## How it works

1. On startup, ripple builds a reverse dependency graph (`go list`) and a call graph of your project.
2. When a `.go` file is saved, ripple finds the changed symbols (via `git diff` or an in-memory snapshot).
3. It walks the reverse dep graph to find affected packages and queries the call graph to find which test functions transitively call the changed code.
4. Only those tests are run — nothing more.

### Call graph methods

- **CHA** (Class Hierarchy Analysis) — fast, conservative. May run a few extra tests but never misses one.
- **RTA** (Rapid Type Analysis) — slower, more precise. Only includes types actually instantiated in the program.

CHA is the better default for most projects. Use RTA if you want tighter targeting and don't mind the slower startup.

## CI

ripple has a JSON mode designed for CI pipelines. Instead of running your full test suite on every PR, ripple finds which `.go` files changed, traces the blast radius through the dependency and call graphs, and runs only the affected tests.

### GitHub Action

The easiest way to use ripple in CI — add this to your workflow:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0

- uses: urechandro/ripple@v0
  with:
    method: cha     # conservative, never misses a test
    depth: "-1"     # full transitive dependency walk
    skip: integration
```

The action installs Go, builds ripple, diffs against the PR base branch, and runs only affected tests. It exposes `tests-passed`, `tests-failed`, and `json` outputs for downstream steps.

| Input | Default | Description |
|-------|---------|-------------|
| `method` | `cha` | Call graph algorithm |
| `depth` | `-1` | Reverse dep graph depth |
| `skip` | `integration` | Packages to skip |
| `go-version-file` | `go.mod` | Go version detection |
| `ripple-version` | `latest` | Ripple version to install |

### Manual setup

If you prefer not to use the action:

```yaml
- run: go install github.com/urechandro/ripple@latest

- run: |
    OUTPUT=$(ripple -json -run -method cha -depth -1 -base "origin/${{ github.base_ref }}" 2>/dev/null)
    echo "$OUTPUT" | jq .
    FAILED=$(echo "$OUTPUT" | jq -r '.summary.tests_failed // 0')
    if [ "$FAILED" -gt 0 ]; then exit 1; fi
```

The `-base` flag tells ripple which git ref to diff against, so it finds changed files automatically.

### Recommended CI settings

Use `-method cha -depth -1` for CI:

- **CHA** is conservative — it may run a few extra tests but will never miss one. A false "pass" in CI is worse than running 10 extra tests.
- **`-depth -1`** walks the full transitive dependency graph. At the default `-depth 1`, a test two hops away from your change could be missed.

### What ripple can't see

- **Reflection / `go:linkname` / assembly** — calls invisible to static analysis won't be traced.
- **Non-Go files** — changes to `testdata/`, config, or build-tag-gated code won't trigger tests.
- **Init-time side effects** — `init()` functions are caught by the dep graph but the call graph can't narrow to specific tests.

For maximum safety, pair ripple with a periodic full `go test ./...` run (e.g. nightly or on merge to main).

## License

MIT

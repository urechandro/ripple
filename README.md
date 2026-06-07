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
| `-stream` | | With `-json`: emit NDJSON (header + per-test events + result) for live progress |
| `-serve` | | Long-running stdio daemon for editor integration (see "Daemon mode" below) |
| `--` | | Everything after `--` is forwarded to `go test` (e.g. `ripple -- -race -timeout 30s`) |

### Config file

Create a `.ripple.yaml` in your project root to set defaults:

```yaml
skip: integration,e2e
depth: 2
method: rta
test_flags:
  - -race
  - -timeout
  - 30s
```

All fields are optional. CLI flags override config values. Test flags from the config and `--` on the CLI are merged (CLI flags appended after config flags).

### Streaming JSON output

`ripple -json -run -stream` emits one JSON object per line on stdout so long-lived consumers (editors, dashboards) can render progress as tests finish instead of waiting for the whole run. Order is always: one `header`, zero or more `event`, then exactly one terminal `result`.

```
{"type":"header","changed_files":[…],"affected_packages":[…]}
{"type":"event","action":"run","package":"example.com/foo","test":"TestX"}
{"type":"event","action":"pass","package":"example.com/foo","test":"TestX","elapsed":0.04}
…
{"type":"result","changed_files":[…],"affected_packages":[…with results…],"summary":{…}}
```

- `header` is the same shape as the legacy `-json` output minus `summary` — it's what ripple knows before any test runs.
- `event` is per-`go test -json` event. `action` is one of `start`, `run`, `pass`, `fail`, `skip`, `output`, `build-fail`, `build-output`. Package-level events have no `test` field.
- `result` is the full aggregated state, identical in shape to non-stream `-json` output. Treat it as the "run finished" signal.

Note: the call graph build still writes `warning: …` lines on stdout before any JSON appears. Consumers should ignore any line that does not start with `{`.

Without `-stream`, `-json` behavior is unchanged: a single indented document at end-of-run.

### Daemon mode (`-serve`)

`ripple -serve <root>` runs as a long-running stdio daemon. The call graph is built once at startup (the expensive part — typically 5–15 s on a large repo) and stays warm in memory; an internal fsnotify watcher marks modules dirty as `.go` files change, and the next request incrementally rebuilds only those modules. This is the mode editor integrations should use — spawning `ripple -json -run` on every save throws the warm graph away each time.

Protocol:

- **stdin** — one JSON request per line:
  - `{"id":"<opaque>","cmd":"run","files":["abs/path.go", …]}` — run affected tests for these files. `id` is echoed in every envelope of the response.
  - `{"cmd":"ping"}` — emits `{"type":"pong","id":"<id>"}` for liveness checks.
  - `{"cmd":"shutdown"}` — daemon exits 0.
  - EOF on stdin also shuts down cleanly.
- **stdout** — same NDJSON envelopes as `-stream` (`header` / `event` / `result`), each carrying the request `id` as an optional field. An `error` envelope (`{"type":"error","id":"<id>","message":"…"}`) ends a request without a `result` — the daemon itself keeps running.
- **stderr** — human-readable lifecycle messages: `ripple: serve ready`, `ripple: incremental rebuild N module(s) in Mms`, `ripple: serve shutting down`. Not part of the wire format.

Example session:

```
$ ripple -serve /path/to/repo
{"id":"1","cmd":"run","files":["/path/to/repo/foo/bar.go"]}
{"type":"header","id":"1","changed_files":[…],"affected_packages":[…]}
{"type":"event","id":"1","action":"pass","package":"example.com/foo/bar","test":"TestX","elapsed":0.04}
…
{"type":"result","id":"1","changed_files":[…],"affected_packages":[…],"summary":{…}}
{"cmd":"shutdown"}
```

Per-request semantics match `-json -run -stream`: same package selection, same skip patterns, same `-method` / `-depth` flags carried over from the daemon's command line. `MarkProcessed` is called after each request so subsequent runs see only the delta against the prior save, matching the TUI watcher's behavior.

**Rebuild policy:** the request path never blocks on a call-graph rebuild. The fsnotify watcher debounces `.go` writes by 200 ms and triggers a background `rebuildIncremental` for affected modules; the request handler always serves with the current cgs snapshot. This keeps save-to-events latency at ~50 ms even on single-module monorepos where `callgraph.Build` for one module is identical work to a full rebuild. The trade-off is that signature changes (new/removed/renamed exported symbols) may be invisible to `TestsCovering` for one save until the background rebuild lands; body-only edits are always correct.

### Keyboard shortcuts

| Key | Action |
|-----|--------|
| `r` | Rebuild the call graph |
| `a` | Run all tests |
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

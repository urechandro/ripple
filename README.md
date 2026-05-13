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

## License

MIT

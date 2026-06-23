package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"

	callgraph "github.com/urechandro/go-callgraph"
	"github.com/urechandro/ripple/internal/config"
	"github.com/urechandro/ripple/internal/depgraph"
	"github.com/urechandro/ripple/internal/runner"
	"github.com/urechandro/ripple/internal/symbols"
	"github.com/urechandro/ripple/internal/ui"
)

func main() {
	skipFlag := flag.String("skip", "integration", "comma-separated list of substrings — packages whose import path contains any of these are displayed but not run")
	depthFlag := flag.Int("depth", 1, "how far up the reverse dep graph to walk: 0=changed package only, 1=+direct importers, -1=full transitive")
	methodFlag := flag.String("method", "cha", "call graph algorithm: cha (fast, conservative) or rta (slower, precise)")
	jsonFlag := flag.Bool("json", false, "non-interactive mode: output affected tests as JSON and exit")
	runFlag := flag.Bool("run", false, "with -json: also run the affected tests and include results")
	filesFlag := flag.String("files", "", "with -json: comma-separated list of changed files (default: detect from git diff)")
	baseFlag := flag.String("base", "", "with -json: git ref to diff against (e.g. origin/main). Default: HEAD")
	streamFlag := flag.Bool("stream", false, "with -json: emit NDJSON (header + per-test events + result) instead of a single document; lets long-lived consumers render progress live")
	serveFlag := flag.Bool("serve", false, "long-running stdio daemon: read JSON requests on stdin, emit NDJSON envelopes on stdout, keep the call graph warm and rebuild incrementally on file changes — use this for editor integration")
	timeoutFlag := flag.Duration("test-timeout", 0, "with -json/-serve: max wall-clock time for a test run before ripple cancels it and kills the go test processes; 0 disables ripple's own timeout (go test's built-in -timeout still applies)")

	// Extract extra go test flags passed after "--".
	// We strip them before flag.Parse so they don't interfere with ripple's own flags.
	var testFlags []string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--" {
			testFlags = os.Args[i+1:]
			os.Args = os.Args[:i]
			break
		}
	}
	flag.Parse()

	root, err := filepath.Abs(".")
	if err != nil {
		fatalf("abs path: %v", err)
	}
	if flag.NArg() > 0 {
		root, err = filepath.Abs(flag.Arg(0))
		if err != nil {
			fatalf("abs path: %v", err)
		}
	}

	// Load .ripple.yaml — config values fill in for flags not explicitly set on the CLI.
	cfg, err := config.Load(root)
	if err != nil {
		fatalf("config: %v", err)
	}
	explicitFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	if cfg.Skip != nil && !explicitFlags["skip"] {
		*skipFlag = *cfg.Skip
	}
	if cfg.Depth != nil && !explicitFlags["depth"] {
		*depthFlag = *cfg.Depth
	}
	if cfg.Method != nil && !explicitFlags["method"] {
		*methodFlag = *cfg.Method
	}
	if cfg.TestTimeout != nil && !explicitFlags["test-timeout"] {
		d, err := time.ParseDuration(*cfg.TestTimeout)
		if err != nil {
			fatalf("config: invalid test_timeout %q: %v", *cfg.TestTimeout, err)
		}
		*timeoutFlag = d
	}
	testFlags = append(cfg.TestFlags, testFlags...)

	var cgMethod callgraph.Method
	switch strings.ToLower(*methodFlag) {
	case "cha":
		cgMethod = callgraph.CHA
	case "rta":
		cgMethod = callgraph.RTA
	default:
		fatalf("unknown -method %q (use cha or rta)", *methodFlag)
	}

	skipPatterns := strings.Split(*skipFlag, ",")

	// Progress messages go to stderr so stdout stays clean for JSON mode.
	fmt.Fprintln(os.Stderr, "Building dependency graph…")
	graph, err := depgraph.Build(root)
	if err != nil {
		fatalf("depgraph: %v", err)
	}

	modRoots := graph.ModuleRoots()
	fmt.Fprintf(os.Stderr, "Building call graph (%s)…\n", strings.ToUpper(string(cgMethod)))
	cg, err := callgraph.Build(modRoots, cgMethod)
	if err != nil {
		fatalf("callgraph: %v", err)
	}

	if *streamFlag && !*jsonFlag {
		fatalf("-stream requires -json")
	}
	if *serveFlag && *jsonFlag {
		fatalf("-serve and -json are mutually exclusive")
	}

	if *jsonFlag {
		runJSON(root, graph, cg, skipPatterns, *depthFlag, *runFlag, *streamFlag, *filesFlag, *baseFlag, testFlags, *timeoutFlag)
		return
	}

	// Build per-module call graph cache for scoped incremental rebuilds.
	cgs := make(map[string]*callgraph.Graph, len(modRoots))
	for _, mr := range modRoots {
		cgs[mr] = cg
	}

	if *serveFlag {
		runServe(root, graph, cgs, skipPatterns, *depthFlag, cgMethod, testFlags, *timeoutFlag)
		return
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fatalf("watcher: %v", err)
	}
	defer watcher.Close()

	if err := watchGoPackageDirs(watcher, root); err != nil {
		fatalf("watching: %v", err)
	}

	model := ui.New(root, graph, cgs, watcher, skipPatterns, *depthFlag, cgMethod, testFlags)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatalf("tui: %v", err)
	}
}

// ── JSON mode ────────────────────────────────────────────────────────────────

// jsonOutput is the top-level JSON structure.
type jsonOutput struct {
	ChangedFiles     []jsonChangedFile `json:"changed_files"`
	AffectedPackages []jsonPackage     `json:"affected_packages"`
	Summary          *jsonSummary      `json:"summary,omitempty"`
}

type jsonChangedFile struct {
	Path    string   `json:"path"`
	Symbols []string `json:"symbols"`
	Source  string   `json:"source"`
}

type jsonPackage struct {
	ImportPath string           `json:"import_path"`
	Tests      []string         `json:"tests,omitempty"`
	RunFilter  string           `json:"run_filter,omitempty"`
	Skipped    string           `json:"skipped,omitempty"`
	Status     string           `json:"status,omitempty"`
	Elapsed    float64          `json:"elapsed,omitempty"`
	Results    []jsonTestResult `json:"results,omitempty"`
}

type jsonTestResult struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Elapsed float64  `json:"elapsed,omitempty"`
	Output  []string `json:"output,omitempty"`
}

type jsonSummary struct {
	TestsPassed int `json:"tests_passed"`
	TestsFailed int `json:"tests_failed"`
	PkgsPassed  int `json:"packages_passed"`
	PkgsFailed  int `json:"packages_failed"`
	PkgsSkipped int `json:"packages_skipped"`
}

// ── Streaming envelopes (one JSON object per line on stdout when -stream is set).
//
// Order is always: header, zero-or-more event, terminal result. The `type`
// field discriminates so consumers can switch on it. A run with no tests still
// produces header + result. The legacy single-document -json output (no -stream)
// is byte-identical to the previous release; nothing else changes.

type streamHeader struct {
	Type             string            `json:"type"`         // "header"
	ID               string            `json:"id,omitempty"` // set in serve mode for request correlation
	ChangedFiles     []jsonChangedFile `json:"changed_files"`
	AffectedPackages []jsonPackage     `json:"affected_packages"`
}

type streamEvent struct {
	Type    string  `json:"type"`         // "event"
	ID      string  `json:"id,omitempty"` // set in serve mode for request correlation
	Action  string  `json:"action"`
	Package string  `json:"package"`
	Test    string  `json:"test,omitempty"`
	Elapsed float64 `json:"elapsed,omitempty"`
	Output  string  `json:"output,omitempty"`
}

type streamResult struct {
	Type             string            `json:"type"`         // "result"
	ID               string            `json:"id,omitempty"` // set in serve mode for request correlation
	ChangedFiles     []jsonChangedFile `json:"changed_files"`
	AffectedPackages []jsonPackage     `json:"affected_packages"`
	Summary          *jsonSummary      `json:"summary,omitempty"`
}

// streamError is a per-request failure emitted only in serve mode. It tells
// the client this request id is finished without a result envelope; the
// daemon itself keeps running.
type streamError struct {
	Type    string `json:"type"` // "error"
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
}

func runJSON(root string, graph *depgraph.Graph, cg *callgraph.Graph, skipPatterns []string, depth int, run bool, stream bool, filesArg, baseRef string, testFlags []string, timeout time.Duration) {
	changedFiles := resolveChangedFiles(root, filesArg, baseRef)
	cgs := map[string]*callgraph.Graph{}
	for _, mr := range graph.ModuleRoots() {
		cgs[mr] = cg
	}
	runJSONForFiles(root, graph, cgs, skipPatterns, depth, run, stream, "", changedFiles, testFlags, timeout)
}

// resolveChangedFiles translates the -files flag (comma-separated) or a git
// diff against baseRef into absolute file paths.
func resolveChangedFiles(root, filesArg, baseRef string) []string {
	if filesArg == "" {
		return gitChangedGoFiles(root, baseRef)
	}
	var out []string
	for _, f := range strings.Split(filesArg, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !filepath.IsAbs(f) {
			f = filepath.Join(root, f)
		}
		out = append(out, f)
	}
	return out
}

// runJSONForFiles is the shared kernel between `-json` one-shot mode and the
// long-running `-serve` daemon. The caller has already resolved which files
// changed and may pass a non-empty `id` so the envelopes carry a request
// correlation key (serve mode only — empty in one-shot mode).
//
// In serve mode, `cgs` is the per-module call graph cache that the daemon
// keeps warm and rebuilds incrementally; in one-shot mode every entry points
// at the same single graph.
func runJSONForFiles(root string, graph *depgraph.Graph, cgs map[string]*callgraph.Graph, skipPatterns []string, depth int, run bool, stream bool, id string, changedFiles []string, testFlags []string, timeout time.Duration) {
	if len(changedFiles) == 0 {
		out := jsonOutput{}
		if stream {
			emitStream(streamHeader{Type: "header", ID: id})
			emitStream(streamResult{Type: "result", ID: id})
			return
		}
		writeJSON(out)
		return
	}

	// Analyze changed symbols per file.
	var fileInfos []jsonChangedFile
	var allSymbols []string
	symbolsSeen := map[string]bool{}

	for _, path := range changedFiles {
		syms, source, _ := symbols.ChangedInFile(path)
		fileInfos = append(fileInfos, jsonChangedFile{
			Path:    path,
			Symbols: syms,
			Source:  source,
		})
		for _, s := range syms {
			if !symbolsSeen[s] {
				symbolsSeen[s] = true
				allSymbols = append(allSymbols, s)
			}
		}
	}

	// Find affected test packages.
	affectedSet := map[string]bool{}
	for _, path := range changedFiles {
		for _, pkg := range graph.AffectedTestPackages(path, depth) {
			affectedSet[pkg] = true
		}
	}

	affected := make([]string, 0, len(affectedSet))
	for pkg := range affectedSet {
		affected = append(affected, pkg)
	}
	sort.Strings(affected)

	// Build affected dirs and find covering tests via call graph.
	affectedDirSet := map[string]bool{}
	for _, pkg := range affected {
		if dir, ok := graph.DirFor(pkg); ok {
			affectedDirSet[dir] = true
		}
	}
	testsByDir := mergedTestsCovering(cgs, affectedDirSet, changedFiles, allSymbols)

	// Build package list with test filters.
	skipReason := func(importPath string) string {
		for _, p := range skipPatterns {
			if p != "" && strings.Contains(importPath, p) {
				return p
			}
		}
		return ""
	}

	var packages []jsonPackage
	var toRun []runner.PackageTest

	for _, pkg := range affected {
		if reason := skipReason(pkg); reason != "" {
			packages = append(packages, jsonPackage{
				ImportPath: pkg,
				Skipped:    reason,
			})
			continue
		}

		jp := jsonPackage{ImportPath: pkg}

		if len(allSymbols) > 0 {
			dir, dirOK := graph.DirFor(pkg)
			if dirOK {
				tests := testsByDir[dir]
				if len(tests) == 0 {
					continue // no blast radius — skip package entirely
				}
				sort.Strings(tests)
				jp.Tests = tests
				jp.RunFilter = symbols.RunFilter(tests)
			}
		}

		packages = append(packages, jp)
		toRun = append(toRun, runner.PackageTest{
			ImportPath: pkg,
			RunFilter:  jp.RunFilter,
		})
	}

	out := jsonOutput{
		ChangedFiles:     fileInfos,
		AffectedPackages: packages,
	}

	if stream {
		emitStream(streamHeader{
			Type:             "header",
			ID:               id,
			ChangedFiles:     fileInfos,
			AffectedPackages: packages,
		})
	}

	// Optionally run the tests.
	if run && len(toRun) > 0 {
		eventCh := make(chan runner.TestEvent, 256)
		pkgsByModule := make(map[string][]runner.PackageTest)
		for _, pt := range toRun {
			if modRoot, ok := graph.ModuleFor(pt.ImportPath); ok {
				pkgsByModule[modRoot] = append(pkgsByModule[modRoot], pt)
			}
		}

		// Bound the run when a timeout is configured so a hung test can't
		// pin the serve daemon (and its go test child processes) forever.
		// Cancelling ctx kills the go test processes; the producer goroutine
		// then closes eventCh and the consumer below drains to completion.
		ctx := context.Background()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		go func() {
			runner.Run(ctx, pkgsByModule, testFlags, eventCh)
			close(eventCh)
		}()

		// Collect results keyed by import path.
		type pkgState struct {
			status  string
			elapsed float64
			tests   map[string]*jsonTestResult
		}
		states := map[string]*pkgState{}
		for _, pt := range toRun {
			states[pt.ImportPath] = &pkgState{
				status: "pass",
				tests:  map[string]*jsonTestResult{},
			}
		}

		for ev := range eventCh {
			if stream {
				emitStream(streamEvent{
					Type:    "event",
					ID:      id,
					Action:  ev.Action,
					Package: ev.Package,
					Test:    ev.Test,
					Elapsed: ev.Elapsed,
					Output:  ev.Output,
				})
			}
			st, ok := states[ev.Package]
			if !ok {
				continue
			}
			switch ev.Action {
			case "pass":
				if ev.Test == "" {
					st.elapsed = ev.Elapsed
				} else {
					t := st.tests[ev.Test]
					if t == nil {
						t = &jsonTestResult{Name: ev.Test}
						st.tests[ev.Test] = t
					}
					t.Status = "pass"
					t.Elapsed = ev.Elapsed
				}
			case "fail":
				if ev.Test == "" {
					st.status = "fail"
					st.elapsed = ev.Elapsed
				} else {
					t := st.tests[ev.Test]
					if t == nil {
						t = &jsonTestResult{Name: ev.Test}
						st.tests[ev.Test] = t
					}
					t.Status = "fail"
					t.Elapsed = ev.Elapsed
					st.status = "fail"
				}
			case "skip":
				if ev.Test != "" {
					t := st.tests[ev.Test]
					if t == nil {
						t = &jsonTestResult{Name: ev.Test}
						st.tests[ev.Test] = t
					}
					t.Status = "skip"
				}
			case "output":
				if ev.Test != "" {
					t := st.tests[ev.Test]
					if t == nil {
						t = &jsonTestResult{Name: ev.Test}
						st.tests[ev.Test] = t
					}
					trimmed := strings.TrimRight(ev.Output, "\n")
					if trimmed != "" {
						t.Output = append(t.Output, trimmed)
					}
				}
			case "build-fail", "build-output":
				st.status = "build-fail"
			}
		}

		// If the deadline fired, the test processes were killed mid-run and
		// any package without a terminal event would otherwise be reported as
		// a spurious pass. Surface the timeout instead. In stream mode an
		// error envelope ends the request without a result, matching the
		// daemon's protocol.
		if ctx.Err() == context.DeadlineExceeded {
			if stream {
				emitStream(streamError{
					Type:    "error",
					ID:      id,
					Message: fmt.Sprintf("test run exceeded -test-timeout (%s); cancelled", timeout),
				})
				return
			}
			fmt.Fprintf(os.Stderr, "ripple: test run exceeded -test-timeout (%s); cancelled\n", timeout)
		}

		// Merge results back into packages.
		out.Summary = &jsonSummary{}
		for i := range out.AffectedPackages {
			pkg := &out.AffectedPackages[i]
			st, ok := states[pkg.ImportPath]
			if !ok {
				if pkg.Skipped != "" {
					out.Summary.PkgsSkipped++
				}
				continue
			}
			pkg.Status = st.status
			pkg.Elapsed = st.elapsed
			for _, t := range st.tests {
				// Only include output for failures to keep JSON compact.
				if t.Status != "fail" {
					t.Output = nil
				}
				pkg.Results = append(pkg.Results, *t)
			}
			sort.Slice(pkg.Results, func(a, b int) bool {
				return pkg.Results[a].Name < pkg.Results[b].Name
			})

			switch st.status {
			case "pass":
				out.Summary.PkgsPassed++
			case "fail", "build-fail":
				out.Summary.PkgsFailed++
			}
			for _, t := range pkg.Results {
				switch t.Status {
				case "pass":
					out.Summary.TestsPassed++
				case "fail":
					out.Summary.TestsFailed++
				}
			}
		}
	}

	if stream {
		emitStream(streamResult{
			Type:             "result",
			ID:               id,
			ChangedFiles:     out.ChangedFiles,
			AffectedPackages: out.AffectedPackages,
			Summary:          out.Summary,
		})
		return
	}
	writeJSON(out)
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatalf("json encode: %v", err)
	}
}

// emitStream writes one compact JSON line to stdout. Stdout is unbuffered on
// Go's side so the line reaches the consumer as soon as the kernel flushes.
// Encoding failures during -stream mode are fatal — there's no useful recovery
// when the output stream is corrupt mid-run.
func emitStream(v any) {
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(v); err != nil {
		fatalf("stream encode: %v", err)
	}
}

// mergedTestsCovering iterates the per-module call-graph cache and unions
// matching tests so packages in dirty modules (which got a fresh cg) and
// clean modules (still on the original cg) are both considered. Mirrors
// the TUI's `testsCovering` aggregation in internal/ui/model.go.
func mergedTestsCovering(cgs map[string]*callgraph.Graph, dirs map[string]bool, files, symbols []string) map[string][]string {
	merged := map[string][]string{}
	seen := map[string]map[string]bool{}
	// Multiple module roots can share one *Graph (callgraph.Build is called
	// with a slice of roots and returns a single graph covering all of them).
	// Dedup by pointer identity so we don't pay for redundant lookups.
	visited := map[*callgraph.Graph]bool{}
	for _, cg := range cgs {
		if cg == nil || visited[cg] {
			continue
		}
		visited[cg] = true
		for dir, tests := range cg.TestsCovering(dirs, files, symbols) {
			if seen[dir] == nil {
				seen[dir] = map[string]bool{}
			}
			for _, t := range tests {
				if !seen[dir][t] {
					seen[dir][t] = true
					merged[dir] = append(merged[dir], t)
				}
			}
		}
	}
	return merged
}

// gitChangedGoFiles returns .go files that have been modified according to git.
// When baseRef is set (e.g. "origin/main"), it uses a three-dot diff to find
// files changed on the current branch since it diverged from baseRef.
func gitChangedGoFiles(root, baseRef string) []string {
	var diffs [][]string
	if baseRef != "" {
		diffs = [][]string{
			{"diff", "--name-only", baseRef + "...HEAD", "--", "*.go"},
		}
	} else {
		diffs = [][]string{
			{"diff", "--name-only", "HEAD", "--", "*.go"},
			{"diff", "--name-only", "--", "*.go"},
		}
	}

	for _, args := range diffs {
		out, err := execGit(root, args...)
		if err != nil {
			continue
		}
		var files []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			files = append(files, filepath.Join(root, line))
		}
		if len(files) > 0 {
			return files
		}
	}
	return nil
}

func execGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func watchGoPackageDirs(w *fsnotify.Watcher, root string) error {
	skip := map[string]bool{
		".git": true, "vendor": true, "node_modules": true,
		"dist": true, "testdata": true, ".idea": true, ".vscode": true,
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return w.Add(path)
		}
		return nil
	})
}


func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ripple: "+format+"\n", args...)
	os.Exit(1)
}

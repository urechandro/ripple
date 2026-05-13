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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"

	callgraph "github.com/urechandro/go-callgraph"
	"github.com/andreasurech/ripple/internal/depgraph"
	"github.com/andreasurech/ripple/internal/runner"
	"github.com/andreasurech/ripple/internal/symbols"
	"github.com/andreasurech/ripple/internal/ui"
)

func main() {
	skipFlag := flag.String("skip", "integration", "comma-separated list of substrings — packages whose import path contains any of these are displayed but not run")
	depthFlag := flag.Int("depth", 1, "how far up the reverse dep graph to walk: 0=changed package only, 1=+direct importers, -1=full transitive")
	methodFlag := flag.String("method", "cha", "call graph algorithm: cha (fast, conservative) or rta (slower, precise)")
	jsonFlag := flag.Bool("json", false, "non-interactive mode: output affected tests as JSON and exit")
	runFlag := flag.Bool("run", false, "with -json: also run the affected tests and include results")
	filesFlag := flag.String("files", "", "with -json: comma-separated list of changed files (default: detect from git diff)")
	flag.Parse()

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

	// Progress messages go to stderr so stdout stays clean for JSON mode.
	fmt.Fprintln(os.Stderr, "Building dependency graph…")
	graph, err := depgraph.Build(root)
	if err != nil {
		fatalf("depgraph: %v", err)
	}

	fmt.Fprintf(os.Stderr, "Building call graph (%s)…\n", strings.ToUpper(string(cgMethod)))
	cg, err := callgraph.Build(graph.ModuleRoots(), cgMethod)
	if err != nil {
		fatalf("callgraph: %v", err)
	}

	if *jsonFlag {
		runJSON(root, graph, cg, skipPatterns, *depthFlag, *runFlag, *filesFlag)
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

	model := ui.New(root, graph, cg, watcher, skipPatterns, *depthFlag, cgMethod)
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

func runJSON(root string, graph *depgraph.Graph, cg *callgraph.Graph, skipPatterns []string, depth int, run bool, filesArg string) {
	// Determine changed files: from --files flag or git diff.
	var changedFiles []string
	if filesArg != "" {
		for _, f := range strings.Split(filesArg, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !filepath.IsAbs(f) {
				f = filepath.Join(root, f)
			}
			changedFiles = append(changedFiles, f)
		}
	} else {
		changedFiles = gitChangedGoFiles(root)
	}

	if len(changedFiles) == 0 {
		out := jsonOutput{}
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
	testsByDir := cg.TestsCovering(affectedDirSet, changedFiles, allSymbols)

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

	// Optionally run the tests.
	if run && len(toRun) > 0 {
		eventCh := make(chan runner.TestEvent, 256)
		pkgsByModule := make(map[string][]runner.PackageTest)
		for _, pt := range toRun {
			if modRoot, ok := graph.ModuleFor(pt.ImportPath); ok {
				pkgsByModule[modRoot] = append(pkgsByModule[modRoot], pt)
			}
		}

		go func() {
			runner.Run(context.Background(), pkgsByModule, eventCh)
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

	writeJSON(out)
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatalf("json encode: %v", err)
	}
}

// gitChangedGoFiles returns .go files that have been modified according to git.
func gitChangedGoFiles(root string) []string {
	// Try staged+unstaged vs HEAD first, then unstaged only (for new repos).
	for _, args := range [][]string{
		{"diff", "--name-only", "HEAD", "--", "*.go"},
		{"diff", "--name-only", "--", "*.go"},
	} {
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

// watchGoPackageDirs adds every directory under root (excluding .git/vendor)
// to the fsnotify watcher.
func watchGoPackageDirs(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
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

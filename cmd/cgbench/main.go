// Command cgbench measures the cost of a scoped incremental call-graph rebuild
// (load only the reverse-dependency closure of a changed package) against the
// current full-module rebuild, and checks that the scoped graph produces the
// same test blast radius as the full graph.
//
// Usage:
//
//	go run ./cmd/cgbench [-method cha|rta] [-depth N] [-iter N] <module-dir>
//
// It builds the dependency graph for the module, builds the full call graph
// once, then for a representative spread of changed files (smallest, median,
// and largest reverse-dependency closures) builds a scoped call graph and
// compares timing and blast radius.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	callgraph "github.com/urechandro/go-callgraph"
	"github.com/urechandro/ripple/internal/depgraph"
)

func main() {
	methodFlag := flag.String("method", "cha", "call graph algorithm: cha or rta")
	depthFlag := flag.Int("depth", 1, "reverse-dep walk depth (matches ripple -depth)")
	iterFlag := flag.Int("iter", 3, "scoped-build iterations per scenario (best time reported)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cgbench [-method] [-depth] [-iter] <module-dir>")
		os.Exit(2)
	}
	dir, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fatal(err)
	}

	var method callgraph.Method
	switch strings.ToLower(*methodFlag) {
	case "cha":
		method = callgraph.CHA
	case "rta":
		method = callgraph.RTA
	default:
		fatal(fmt.Errorf("unknown -method %q", *methodFlag))
	}

	fmt.Printf("module: %s\nmethod: %s  depth: %d\n\n", dir, strings.ToUpper(*methodFlag), *depthFlag)

	g, err := depgraph.Build(dir)
	if err != nil {
		fatal(err)
	}
	roots := g.ModuleRoots()
	fmt.Printf("dependency graph: %d files, %d module root(s)\n", g.FileCount(), len(roots))

	// --- Full rebuild: build the whole module's call graph (current behaviour). ---
	start := time.Now()
	fullCG, err := callgraph.Build(roots, method)
	if err != nil {
		fatal(err)
	}
	fullDur := time.Since(start)
	fmt.Printf("full-module build: %s\n\n", fullDur.Round(time.Millisecond))

	// --- Pick representative changed files by scope size (cheap BFS, no build). ---
	candidates := sourceFiles(dir)
	if len(candidates) == 0 {
		fatal(fmt.Errorf("no source files found under %s", dir))
	}
	type scoped struct {
		file      string
		pkgsByMod map[string][]string
		scopeN    int
	}
	var all []scoped
	for _, f := range candidates {
		s := g.ScopedRebuildSet([]string{f}, *depthFlag)
		n := 0
		for _, ps := range s {
			n += len(ps)
		}
		if n == 0 {
			continue // file not in graph (e.g. excluded build tag)
		}
		all = append(all, scoped{file: f, pkgsByMod: s, scopeN: n})
	}
	if len(all) == 0 {
		fatal(fmt.Errorf("no changed-file scenarios produced a scope"))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].scopeN < all[j].scopeN })
	total := totalPkgs(g)
	scenarios := pick(all)

	fmt.Printf("total packages in module: %d\n", total)
	fmt.Printf("%-44s %7s %9s %9s %8s   %s\n", "changed file", "scope", "scoped", "speedup", "blast", "match")
	fmt.Println(strings.Repeat("-", 100))

	for _, sc := range scenarios {
		syms := funcNames(sc.file)
		affectedDirs, affectedFile := blastInputs(g, sc.file, *depthFlag)

		// Scoped build: load only the reverse-dep closure, build from those packages.
		best := time.Duration(1<<62 - 1)
		var scopedCG *callgraph.Graph
		for i := 0; i < *iterFlag; i++ {
			fmt.Fprintf(os.Stderr, "  [scope=%d/%d mods=%d] building %s (iter %d/%d)…\n",
				sc.scopeN, total, len(sc.pkgsByMod), rel(dir, sc.file), i+1, *iterFlag)
			st := time.Now()
			cg := buildScoped(sc.pkgsByMod, method)
			d := time.Since(st)
			fmt.Fprintf(os.Stderr, "    -> %s\n", d.Round(time.Millisecond))
			if d < best {
				best = d
				scopedCG = cg
			}
		}

		fullTests := fullCG.TestsCovering(affectedDirs, affectedFile, syms)
		scopedTests := scopedCG.TestsCovering(affectedDirs, affectedFile, syms)
		match, fN, sN := compareBlast(fullTests, scopedTests)

		speed := float64(fullDur) / float64(best)
		matchStr := "ok"
		if !match {
			matchStr = fmt.Sprintf("DIFF full=%d scoped=%d", fN, sN)
		}
		fmt.Printf("%-44s %4d/%-2d %9s %8.1fx %5d/%-2d   %s\n",
			trim(rel(dir, sc.file), 44), sc.scopeN, total,
			best.Round(time.Millisecond), speed, sN, fN, matchStr)
	}
}

// buildScoped loads only the given import paths per module root and builds a
// call graph from them, mirroring the proposed scoped incremental rebuild.
func buildScoped(pkgsByMod map[string][]string, method callgraph.Method) *callgraph.Graph {
	// Build each module's scoped subgraph via BuildFromPackages.
	var graphs []*callgraph.Graph
	for modRoot, importPaths := range pkgsByMod {
		pkgs, err := callgraph.LoadPackages(modRoot, importPaths...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: load %s: %v\n", modRoot, err)
			continue
		}
		cg, err := callgraph.BuildFromPackages(pkgs, method)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: build %s: %v\n", modRoot, err)
			continue
		}
		graphs = append(graphs, cg)
	}
	if len(graphs) == 1 {
		return graphs[0]
	}
	// Multiple modules: return the first; multi-module merge is out of scope
	// for the prototype (single-module is the common case).
	if len(graphs) > 0 {
		return graphs[0]
	}
	return &callgraph.Graph{}
}

// blastInputs returns the affected test-package dirs and the changed file path
// in the form TestsCovering expects.
func blastInputs(g *depgraph.Graph, file string, depth int) (map[string]bool, []string) {
	dirs := map[string]bool{}
	for _, pkg := range g.AffectedTestPackages(file, depth) {
		if d, ok := g.DirFor(pkg); ok {
			dirs[d] = true
		}
	}
	return dirs, []string{file}
}

func compareBlast(full, scoped map[string][]string) (bool, int, int) {
	count := func(m map[string][]string) int {
		n := 0
		for _, v := range m {
			n += len(v)
		}
		return n
	}
	fN, sN := count(full), count(scoped)
	// Scoped must find at least every test the full graph finds.
	for dir, tests := range full {
		have := map[string]bool{}
		for _, t := range scoped[dir] {
			have[t] = true
		}
		for _, t := range tests {
			if !have[t] {
				return false, fN, sN
			}
		}
	}
	return true, fN, sN
}

// pick returns up to three scenarios: smallest, median, and largest scope.
func pick[T any](xs []T) []T {
	switch len(xs) {
	case 0:
		return nil
	case 1, 2:
		return xs
	default:
		return []T{xs[0], xs[len(xs)/2], xs[len(xs)-1]}
	}
}

func sourceFiles(dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func funcNames(file string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		return nil
	}
	var names []string
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			names = append(names, fn.Name.Name)
		}
	}
	return names
}

func totalPkgs(g *depgraph.Graph) int {
	// Approximate: one package per directory containing tracked files.
	seen := map[string]bool{}
	for dir := range g.DirFiles() {
		seen[dir] = true
	}
	return len(seen)
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "cgbench:", err)
	os.Exit(1)
}

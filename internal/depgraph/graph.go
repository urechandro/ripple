package depgraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
)

type listPackage struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// Graph is a reverse dependency graph built from `go list`.
type Graph struct {
	fileToImport map[string]string   // abs file path → import path
	dependents   map[string][]string // import path → packages that import it
	hasTests     map[string]bool     // import path → has test files
	pkgModule    map[string]string   // import path → module root dir
	pkgTestFiles map[string][]string // import path → abs paths of test files
	pkgDir       map[string]string   // import path → package directory
	pkgAllFiles  map[string][]string // import path → all Go files (source + test)
}

// Build walks root for every go.mod (i.e. every module) and runs `go list`
// in each, merging the results into a single graph.
func Build(root string) (*Graph, error) {
	modRoots, err := findModuleRoots(root)
	if err != nil {
		return nil, err
	}
	if len(modRoots) == 0 {
		// Fallback: treat root itself as the module root.
		modRoots = []string{root}
	}

	g := &Graph{
		fileToImport: make(map[string]string),
		dependents:   make(map[string][]string),
		hasTests:     make(map[string]bool),
		pkgModule:    make(map[string]string),
		pkgTestFiles: make(map[string][]string),
		pkgDir:       make(map[string]string),
		pkgAllFiles:  make(map[string][]string),
	}

	for _, modRoot := range modRoots {
		if err := g.ingest(modRoot); err != nil {
			// Non-fatal: log and continue so one broken module doesn't block others.
			fmt.Printf("warning: go list failed in %s: %v\n", modRoot, err)
		}
	}

	return g, nil
}

// findModuleRoots returns directories containing a go.mod file under root.
func findModuleRoots(root string) ([]string, error) {
	var roots []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			roots = append(roots, filepath.Dir(path))
		}
		return nil
	})
	return roots, err
}

// ingest runs `go list -json ./...` in modRoot and merges packages into g.
func (g *Graph) ingest(modRoot string) error {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = modRoot
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list: %w", err)
	}

	var pkgs []listPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p listPackage
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		pkgs = append(pkgs, p)
	}

	for _, p := range pkgs {
		allFiles := append(append(p.GoFiles, p.TestGoFiles...), p.XTestGoFiles...)
		for _, f := range allFiles {
			g.fileToImport[filepath.Join(p.Dir, f)] = p.ImportPath
		}
		testFiles := append(p.TestGoFiles, p.XTestGoFiles...)
		g.hasTests[p.ImportPath] = len(testFiles) > 0
		g.pkgModule[p.ImportPath] = modRoot
		g.pkgDir[p.ImportPath] = p.Dir
		for _, f := range testFiles {
			abs := filepath.Join(p.Dir, f)
			g.pkgTestFiles[p.ImportPath] = append(g.pkgTestFiles[p.ImportPath], abs)
			g.pkgAllFiles[p.ImportPath] = append(g.pkgAllFiles[p.ImportPath], abs)
		}
		for _, f := range p.GoFiles {
			g.pkgAllFiles[p.ImportPath] = append(g.pkgAllFiles[p.ImportPath], filepath.Join(p.Dir, f))
		}
	}

	// Build reverse dep map: for each package, record which packages import it.
	seen := map[string]map[string]bool{}
	for _, p := range pkgs {
		allImports := append(append(p.Imports, p.TestImports...), p.XTestImports...)
		for _, imp := range allImports {
			if _, ok := seen[imp]; !ok {
				seen[imp] = map[string]bool{}
			}
			if !seen[imp][p.ImportPath] {
				seen[imp][p.ImportPath] = true
				g.dependents[imp] = append(g.dependents[imp], p.ImportPath)
			}
		}
	}

	return nil
}

// AffectedTestPackages returns packages with tests that depend on the package
// owning changedFile. depth controls how far up the reverse dep graph to walk:
//
//	0  → only the changed package itself
//	1  → changed package + direct importers (good default for local dev)
//	-1 → full transitive closure
func (g *Graph) AffectedTestPackages(changedFile string, depth int) []string {
	startPkg, ok := g.fileToImport[changedFile]
	if !ok {
		return nil
	}

	type entry struct {
		pkg   string
		level int
	}

	visited := map[string]bool{startPkg: true}
	queue := []entry{{startPkg, 0}}
	var affected []string

	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]

		if g.hasTests[e.pkg] {
			affected = append(affected, e.pkg)
		}

		if depth >= 0 && e.level >= depth {
			continue
		}

		for _, dep := range g.dependents[e.pkg] {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, entry{dep, e.level + 1})
			}
		}
	}

	return affected
}

// FileCount returns the number of Go source files being tracked.
func (g *Graph) FileCount() int {
	return len(g.fileToImport)
}

// TestFiles returns the absolute paths of test files for the given package.
func (g *Graph) TestFiles(importPath string) []string {
	return g.pkgTestFiles[importPath]
}

// ModuleFor returns the module root directory for the given import path.
func (g *Graph) ModuleFor(importPath string) (string, bool) {
	modRoot, ok := g.pkgModule[importPath]
	return modRoot, ok
}

// DirFor returns the package directory for the given import path.
func (g *Graph) DirFor(importPath string) (string, bool) {
	dir, ok := g.pkgDir[importPath]
	return dir, ok
}

// DirFiles returns a map of directory → all Go files in that directory
// (source + test), suitable for building a call graph.
func (g *Graph) DirFiles() map[string][]string {
	seen := make(map[string]map[string]bool)
	for _, files := range g.pkgAllFiles {
		for _, f := range files {
			dir := filepath.Dir(f)
			if seen[dir] == nil {
				seen[dir] = make(map[string]bool)
			}
			seen[dir][f] = true
		}
	}
	result := make(map[string][]string, len(seen))
	for dir, fileSet := range seen {
		for f := range fileSet {
			result[dir] = append(result[dir], f)
		}
	}
	return result
}

// ModuleRoots returns the unique module root directories in the graph.
func (g *Graph) ModuleRoots() []string {
	seen := map[string]bool{}
	var roots []string
	for _, root := range g.pkgModule {
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

// FileToImport returns the import path for the package containing the given file.
func (g *Graph) FileToImport(file string) (string, bool) {
	ip, ok := g.fileToImport[file]
	return ip, ok
}

// SampleFile returns one arbitrary file path from the graph, useful for
// diagnosing path format mismatches.
func (g *Graph) SampleFile() string {
	for k := range g.fileToImport {
		return k
	}
	return "(empty graph)"
}

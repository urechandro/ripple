package depgraph

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// newTestGraph builds a graph from a compact description for unit testing.
//
//	packages: map of import path → list of Go files (basenames)
//	deps:     map of import path → list of import paths it depends on (forward deps)
//	tests:    set of import paths that have test files
//	dirs:     map of import path → directory
//	modRoot:  module root for all packages
func newTestGraph(packages map[string][]string, deps map[string][]string, tests map[string]bool, dirs map[string]string, modRoot string) *Graph {
	g := &Graph{
		fileToImport: make(map[string]string),
		dependents:   make(map[string][]string),
		hasTests:     make(map[string]bool),
		pkgModule:    make(map[string]string),
		pkgTestFiles: make(map[string][]string),
		pkgDir:       make(map[string]string),
		pkgAllFiles:  make(map[string][]string),
		modPackages:  make(map[string][]listPackage),
	}

	for pkg, files := range packages {
		dir := dirs[pkg]
		g.pkgModule[pkg] = modRoot
		g.pkgDir[pkg] = dir
		g.hasTests[pkg] = tests[pkg]
		for _, f := range files {
			abs := filepath.Join(dir, f)
			g.fileToImport[abs] = pkg
			g.pkgAllFiles[pkg] = append(g.pkgAllFiles[pkg], abs)
		}
	}

	// Build reverse deps from forward deps.
	seen := map[string]map[string]bool{}
	for pkg, imports := range deps {
		for _, imp := range imports {
			if seen[imp] == nil {
				seen[imp] = map[string]bool{}
			}
			if !seen[imp][pkg] {
				seen[imp][pkg] = true
				g.dependents[imp] = append(g.dependents[imp], pkg)
			}
		}
	}

	return g
}

// Graph topology used by most tests:
//
//	core ← service ← api
//	core ← util
//
// core and service have tests; api and util do not.
func makeLinearGraph() (*Graph, string) {
	modRoot := "/proj"
	packages := map[string][]string{
		"proj/core":    {"core.go"},
		"proj/service": {"service.go"},
		"proj/api":     {"api.go"},
		"proj/util":    {"util.go"},
	}
	deps := map[string][]string{
		"proj/service": {"proj/core"},
		"proj/api":     {"proj/service"},
		"proj/util":    {"proj/core"},
	}
	tests := map[string]bool{
		"proj/core":    true,
		"proj/service": true,
	}
	dirs := map[string]string{
		"proj/core":    "/proj/core",
		"proj/service": "/proj/service",
		"proj/api":     "/proj/api",
		"proj/util":    "/proj/util",
	}
	return newTestGraph(packages, deps, tests, dirs, modRoot), modRoot
}

func sorted(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

func TestAffectedTestPackages_Depth0(t *testing.T) {
	g, _ := makeLinearGraph()

	got := g.AffectedTestPackages("/proj/core/core.go", 0)
	want := []string{"proj/core"}
	if len(got) != len(want) || sorted(got)[0] != want[0] {
		t.Errorf("depth=0: got %v, want %v", got, want)
	}
}

func TestAffectedTestPackages_Depth1(t *testing.T) {
	g, _ := makeLinearGraph()

	got := sorted(g.AffectedTestPackages("/proj/core/core.go", 1))
	want := []string{"proj/core", "proj/service"}
	if len(got) != len(want) {
		t.Fatalf("depth=1: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("depth=1: got %v, want %v", got, want)
			break
		}
	}
}

func TestAffectedTestPackages_FullTransitive(t *testing.T) {
	g, _ := makeLinearGraph()

	// depth=-1 walks the full graph. api imports service which imports core,
	// but api has no tests, so only core and service should appear.
	got := sorted(g.AffectedTestPackages("/proj/core/core.go", -1))
	want := []string{"proj/core", "proj/service"}
	if len(got) != len(want) {
		t.Fatalf("depth=-1: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("depth=-1: got %v, want %v", got, want)
			break
		}
	}
}

func TestAffectedTestPackages_UnknownFile(t *testing.T) {
	g, _ := makeLinearGraph()

	got := g.AffectedTestPackages("/proj/nonexistent/foo.go", 1)
	if got != nil {
		t.Errorf("unknown file: got %v, want nil", got)
	}
}

func TestAffectedTestPackages_LeafChange(t *testing.T) {
	g, _ := makeLinearGraph()

	// Changing service.go at depth=1: service itself has tests, api is a
	// direct importer but has no tests.
	got := g.AffectedTestPackages("/proj/service/service.go", 1)
	want := []string{"proj/service"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("leaf change: got %v, want %v", got, want)
	}
}

func TestFileToImport(t *testing.T) {
	g, _ := makeLinearGraph()

	ip, ok := g.FileToImport("/proj/core/core.go")
	if !ok || ip != "proj/core" {
		t.Errorf("FileToImport: got (%q, %v), want (proj/core, true)", ip, ok)
	}

	_, ok = g.FileToImport("/proj/unknown.go")
	if ok {
		t.Error("FileToImport should return false for unknown files")
	}
}

func TestDirFor(t *testing.T) {
	g, _ := makeLinearGraph()

	dir, ok := g.DirFor("proj/service")
	if !ok || dir != "/proj/service" {
		t.Errorf("DirFor: got (%q, %v), want (/proj/service, true)", dir, ok)
	}

	_, ok = g.DirFor("proj/nonexistent")
	if ok {
		t.Error("DirFor should return false for unknown package")
	}
}

func TestModuleRoots(t *testing.T) {
	g, _ := makeLinearGraph()

	roots := g.ModuleRoots()
	if len(roots) != 1 || roots[0] != "/proj" {
		t.Errorf("ModuleRoots: got %v, want [/proj]", roots)
	}
}

func TestModuleForFile(t *testing.T) {
	// Create a temp dir with a go.mod so the walk-up logic works.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	g, _ := makeLinearGraph()

	// Known file resolves via graph.
	mod, ok := g.ModuleForFile("/proj/core/core.go")
	if !ok || mod != "/proj" {
		t.Errorf("known file: got (%q, %v), want (/proj, true)", mod, ok)
	}

	// Unknown file walks up to find go.mod.
	unknownFile := filepath.Join(subdir, "new.go")
	mod, ok = g.ModuleForFile(unknownFile)
	if !ok || mod != dir {
		t.Errorf("unknown file walk-up: got (%q, %v), want (%q, true)", mod, ok, dir)
	}
}

func TestReingestModules(t *testing.T) {
	// Create a real Go module in a temp dir so go list works.
	dir := t.TempDir()
	modDir := filepath.Join(dir, "mymod")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/mymod\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte("package mymod\n\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Build initial graph.
	g, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}

	ip, ok := g.FileToImport(filepath.Join(modDir, "main.go"))
	if !ok {
		t.Fatal("expected main.go to be in graph")
	}
	if ip != "example.com/mymod" {
		t.Fatalf("expected import path example.com/mymod, got %q", ip)
	}

	// Add a new file and reingest.
	if err := os.WriteFile(filepath.Join(modDir, "extra.go"), []byte("package mymod\n\nfunc Extra() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := g.ReingestModules([]string{modDir}); err != nil {
		t.Fatal(err)
	}

	_, ok = g.FileToImport(filepath.Join(modDir, "extra.go"))
	if !ok {
		t.Error("expected extra.go to appear in graph after reingest")
	}

	// Original file should still be present.
	_, ok = g.FileToImport(filepath.Join(modDir, "main.go"))
	if !ok {
		t.Error("expected main.go to still be in graph after reingest")
	}
}

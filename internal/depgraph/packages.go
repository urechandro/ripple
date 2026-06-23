package depgraph

import (
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// ReingestModulesFromPackages re-derives the graph for the given module roots
// from already-loaded go/packages results, instead of shelling out to
// `go list`. This lets a caller that already runs packages.Load for the call
// graph (the expensive type-checking pass) feed the dependency graph from the
// same load — one parse/type-check per rebuild instead of two.
//
// byRoot maps each module root directory to the packages loaded from it with
// Tests enabled. Roots not present are left untouched (their cached data
// survives), mirroring ReingestModules.
func (g *Graph) ReingestModulesFromPackages(byRoot map[string][]*packages.Package) {
	for modRoot, pkgs := range byRoot {
		g.modPackages[modRoot] = packagesToListPackages(modRoot, pkgs)
	}

	// Reconstruct all derived maps from the full cached data — same as
	// ReingestModules, just sourced from packages instead of `go list`.
	g.fileToImport = make(map[string]string)
	g.hasTests = make(map[string]bool)
	g.pkgModule = make(map[string]string)
	g.pkgTestFiles = make(map[string][]string)
	g.pkgDir = make(map[string]string)
	g.pkgAllFiles = make(map[string][]string)

	for modRoot, pkgs := range g.modPackages {
		g.applyPackages(modRoot, pkgs)
	}
	g.rebuildDependents()
}

// packagesToListPackages folds go/packages results (loaded with Tests:true)
// into the same per-package view `go list -json` produces. go/packages splits
// a package that has tests into variants:
//
//	"P"                normal build — source files only
//	"P [P.test]"       in-package test variant — source + in-package *_test.go
//	"P_test [P.test]"  external test variant — package P_test, *_test.go only
//	"P.test"           synthesized test main — ignored
//
// Dependencies pulled in by NeedDeps (stdlib, modules in the cache) are also
// present in the slice; we drop anything whose directory is outside modRoot so
// the result matches `go list ./...` scope (the module's own packages only).
func packagesToListPackages(modRoot string, pkgs []*packages.Package) []listPackage {
	byPath := map[string]*listPackage{}
	var order []string
	get := func(path string) *listPackage {
		lp, ok := byPath[path]
		if !ok {
			lp = &listPackage{ImportPath: path}
			byPath[path] = lp
			order = append(order, path)
		}
		return lp
	}

	for _, p := range pkgs {
		base, kind := classifyVariant(p.ID, p.PkgPath)
		if kind == variantTestMain {
			continue
		}
		dir := pkgDir(p)
		if !underRoot(dir, modRoot) {
			continue // dependency, not part of this module
		}

		lp := get(base)
		if lp.Dir == "" {
			lp.Dir = dir
		}
		imports := importPaths(p.Imports)
		switch kind {
		case variantNormal:
			lp.GoFiles = append(lp.GoFiles, baseNames(p.GoFiles)...)
			lp.Imports = imports
		case variantInPackageTest:
			// Source files also appear in the normal variant; only the
			// *_test.go files are new here.
			for _, f := range p.GoFiles {
				if strings.HasSuffix(f, "_test.go") {
					lp.TestGoFiles = append(lp.TestGoFiles, filepath.Base(f))
				}
			}
			lp.TestImports = imports
		case variantExternalTest:
			lp.XTestGoFiles = append(lp.XTestGoFiles, baseNames(p.GoFiles)...)
			lp.XTestImports = imports
		}
	}

	out := make([]listPackage, 0, len(order))
	for _, path := range order {
		out = append(out, *byPath[path])
	}
	return out
}

type variantKind int

const (
	variantNormal variantKind = iota
	variantInPackageTest
	variantExternalTest
	variantTestMain
)

// classifyVariant decodes a go/packages package ID into the base import path
// and which test variant (if any) it represents. IDs look like "P",
// "P [P.test]", "P_test [P.test]", or "P.test".
func classifyVariant(id, pkgPath string) (base string, kind variantKind) {
	open := strings.Index(id, " [")
	if open < 0 {
		if strings.HasSuffix(id, ".test") {
			return "", variantTestMain
		}
		return pkgPath, variantNormal
	}
	prefix := id[:open]
	inner := id[open+2 : len(id)-1] // strip " [" … "]"
	base = strings.TrimSuffix(inner, ".test")
	if prefix == base {
		return base, variantInPackageTest
	}
	return base, variantExternalTest
}

func pkgDir(p *packages.Package) string {
	switch {
	case len(p.GoFiles) > 0:
		return filepath.Dir(p.GoFiles[0])
	case len(p.CompiledGoFiles) > 0:
		return filepath.Dir(p.CompiledGoFiles[0])
	case len(p.OtherFiles) > 0:
		return filepath.Dir(p.OtherFiles[0])
	}
	return ""
}

// underRoot reports whether dir is modRoot or lives beneath it.
func underRoot(dir, modRoot string) bool {
	if dir == "" {
		return false
	}
	return dir == modRoot || strings.HasPrefix(dir, modRoot+string(filepath.Separator))
}

func baseNames(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = filepath.Base(f)
	}
	return out
}

func importPaths(imports map[string]*packages.Package) []string {
	out := make([]string, 0, len(imports))
	for path := range imports {
		out = append(out, path)
	}
	return out
}

package depgraph

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// loadModeForTest mirrors the mode the call graph loads with (Tests enabled),
// so the conversion is exercised against real go/packages variant output.
const loadModeForTest = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
	packages.NeedSyntax | packages.NeedTypesInfo

func TestReingestModulesFromPackages(t *testing.T) {
	modDir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(modDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/mymod\n\ngo 1.21\n")
	write("lib.go", "package mymod\n\nimport \"fmt\"\n\nfunc Hello() string { return fmt.Sprint(\"hi\") }\n")
	write("lib_test.go", "package mymod\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) { _ = Hello() }\n")

	cfg := &packages.Config{Mode: loadModeForTest, Dir: modDir, Tests: true}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatal(err)
	}

	g := &Graph{modPackages: map[string][]listPackage{}}
	g.ReingestModulesFromPackages(map[string][]*packages.Package{modDir: pkgs})

	// Source and test files map back to the module's package.
	if ip, ok := g.FileToImport(filepath.Join(modDir, "lib.go")); !ok || ip != "example.com/mymod" {
		t.Errorf("lib.go: got (%q,%v), want (example.com/mymod,true)", ip, ok)
	}
	if ip, ok := g.FileToImport(filepath.Join(modDir, "lib_test.go")); !ok || ip != "example.com/mymod" {
		t.Errorf("lib_test.go: got (%q,%v), want (example.com/mymod,true)", ip, ok)
	}

	// The in-package test file is recognised as a test file.
	testFiles := g.TestFiles("example.com/mymod")
	if len(testFiles) != 1 || filepath.Base(testFiles[0]) != "lib_test.go" {
		t.Errorf("TestFiles: got %v, want [lib_test.go]", testFiles)
	}

	// Directory resolves to the module dir.
	if dir, ok := g.DirFor("example.com/mymod"); !ok || dir != modDir {
		t.Errorf("DirFor: got (%q,%v), want (%q,true)", dir, ok, modDir)
	}

	// Dependencies pulled in by NeedDeps must NOT leak into the graph —
	// fmt is imported but lives outside the module root.
	if _, ok := g.DirFor("fmt"); ok {
		t.Error("stdlib dependency fmt leaked into the graph; dep filter failed")
	}
	if n := len(g.ModuleRoots()); n != 1 {
		t.Errorf("ModuleRoots: got %d, want 1 (deps should not add roots)", n)
	}
}

func TestClassifyVariant(t *testing.T) {
	cases := []struct {
		id, pkgPath string
		wantBase    string
		wantKind    variantKind
	}{
		{"example.com/p", "example.com/p", "example.com/p", variantNormal},
		{"example.com/p [example.com/p.test]", "example.com/p", "example.com/p", variantInPackageTest},
		{"example.com/p_test [example.com/p.test]", "example.com/p_test", "example.com/p", variantExternalTest},
		{"example.com/p.test", "example.com/p.test", "", variantTestMain},
	}
	for _, c := range cases {
		base, kind := classifyVariant(c.id, c.pkgPath)
		if base != c.wantBase || kind != c.wantKind {
			t.Errorf("classifyVariant(%q): got (%q,%d), want (%q,%d)", c.id, base, kind, c.wantBase, c.wantKind)
		}
	}
}

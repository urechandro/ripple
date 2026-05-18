package symbols

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		input      string
		wantStart  int
		wantCount  int
	}{
		{"10", 10, 1},
		{"1", 1, 1},
		{"42,3", 42, 3},
		{"1,0", 1, 0},
		{"100,1", 100, 1},
		{"5,10", 5, 10},
	}
	for _, tt := range tests {
		start, count := parseRange(tt.input)
		if start != tt.wantStart || count != tt.wantCount {
			t.Errorf("parseRange(%q) = (%d, %d), want (%d, %d)",
				tt.input, start, count, tt.wantStart, tt.wantCount)
		}
	}
}

func TestParseHunkLines(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []int
	}{
		{
			name: "single line change",
			diff: `diff --git a/foo.go b/foo.go
index abc..def 100644
--- a/foo.go
+++ b/foo.go
@@ -10,1 +10,1 @@ func foo() {
-    old
+    new`,
			want: []int{10},
		},
		{
			name: "multi-line addition",
			diff: `@@ -5,0 +6,3 @@ package main`,
			want: []int{6, 7, 8},
		},
		{
			name: "pure deletion",
			diff: `@@ -5,3 +5,0 @@ func bar() {`,
			want: nil,
		},
		{
			name: "multiple hunks",
			diff: `@@ -1,1 +1,1 @@ package main
-old
+new
@@ -20,0 +20,2 @@ func baz() {
+    line1
+    line2`,
			want: []int{1, 20, 21},
		},
		{
			name: "no hunk headers",
			diff: `just some random text`,
			want: nil,
		},
		{
			name: "single line insert no comma",
			diff: `@@ -10,0 +11 @@ func foo()`,
			want: []int{11},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHunkLines([]byte(tt.diff))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunFilter(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"TestFoo"}, "TestFoo"},
		{[]string{"TestFoo", "TestBar"}, "TestFoo|TestBar"},
		{[]string{"TestA", "TestB", "TestC"}, "TestA|TestB|TestC"},
	}
	for _, tt := range tests {
		got := RunFilter(tt.names)
		if got != tt.want {
			t.Errorf("RunFilter(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}

func TestInFile(t *testing.T) {
	src := `package example

func Foo() {}

func Bar(x int) int { return x }

type S struct{}

func (s S) Method() {}
`
	path := writeTempGo(t, src)
	names, err := InFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Foo", "Bar", "Method"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("InFile() = %v, want %v", names, want)
	}
}

func TestFunctionsAtLines(t *testing.T) {
	src := `package example

func Alpha() {
	_ = 1
}

func Beta() {
	_ = 2
}

func Gamma() {
	_ = 3
}
`
	path := writeTempGo(t, src)

	tests := []struct {
		name  string
		lines []int
		want  []string
	}{
		{"hit first func", []int{4}, []string{"Alpha"}},
		{"hit second func", []int{8}, []string{"Beta"}},
		{"hit two funcs", []int{4, 12}, []string{"Alpha", "Gamma"}},
		{"hit func signature line", []int{3}, []string{"Alpha"}},
		{"miss all funcs", []int{6}, nil},
		{"empty lines", []int{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := functionsAtLines(path, tt.lines)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiffCachedLines_Identical(t *testing.T) {
	src := []byte(`package example

func Foo() {}
`)
	path := writeTempGo(t, string(src))
	got := diffCachedLines(path, src)
	if len(got) != 0 {
		t.Errorf("identical content should return empty slice, got %v", got)
	}
}

func TestDiffCachedLines_Changed(t *testing.T) {
	cached := []byte(`package example

func Foo() {
	_ = 1
}
`)
	current := `package example

func Foo() {
	_ = 999
}
`
	path := writeTempGo(t, current)
	got := diffCachedLines(path, cached)
	if got == nil {
		t.Fatal("expected non-nil line numbers")
	}
	if len(got) == 0 {
		t.Fatal("expected changed lines, got empty")
	}
	found := false
	for _, l := range got {
		if l == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected line 4 in changed lines, got %v", got)
	}
}

func TestMarkProcessedAndChangedInFile(t *testing.T) {
	src := `package example

func Hello() {
	_ = "original"
}
`
	path := writeTempGo(t, src)

	// Snapshot the file.
	MarkProcessed(path)

	// No change — should report no change.
	names, source, err := ChangedInFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if source != "no change since last run" {
		t.Errorf("expected 'no change since last run', got %q", source)
	}
	if len(names) != 0 {
		t.Errorf("expected no changed symbols, got %v", names)
	}

	// Modify the file.
	modified := `package example

func Hello() {
	_ = "modified"
}
`
	if err := os.WriteFile(path, []byte(modified), 0644); err != nil {
		t.Fatal(err)
	}

	names, source, err = ChangedInFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if source != "diff vs last run" {
		t.Errorf("expected 'diff vs last run', got %q", source)
	}
	if !reflect.DeepEqual(names, []string{"Hello"}) {
		t.Errorf("expected [Hello], got %v", names)
	}

	// Clean up the global cache so other tests aren't affected.
	delete(lastRunContent, path)
}

func writeTempGo(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

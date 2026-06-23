package symbols

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// lastRunContent caches the file content at the time it was last processed.
// On the next change, we diff against this snapshot instead of HEAD so that
// only genuinely new changes trigger tests.
//
// In the -serve daemon this map is read/written from the request goroutine
// (MarkProcessed / ChangedInFile) and pruned from the file-watcher goroutine
// (Forget), so it is guarded by lastRunMu.
var (
	lastRunMu      sync.Mutex
	lastRunContent = map[string][]byte{}
)

// MarkProcessed snapshots the current content of filename so that the next
// call to ChangedInFile can diff against it instead of HEAD.
func MarkProcessed(filename string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	lastRunMu.Lock()
	lastRunContent[filename] = data
	lastRunMu.Unlock()
}

// Forget drops the cached snapshot for filename, if any. Callers should invoke
// this when a file is deleted or renamed away so the snapshot map doesn't
// retain content for paths that no longer exist — otherwise a long-running
// daemon accumulates a full copy of every file it ever processed.
func Forget(filename string) {
	lastRunMu.Lock()
	delete(lastRunContent, filename)
	lastRunMu.Unlock()
}

// ChangedInFile returns the names of functions/methods in filename whose body
// spans at least one changed line. It tries, in order:
//  1. Diff against the cached snapshot from the last run (precise, no accumulation)
//  2. git diff HEAD (precise, but cumulative across uncommitted saves)
//  3. All functions in the file (fallback)
//
// The returned source string describes which strategy was used.
func ChangedInFile(filename string) (names []string, source string, err error) {
	// 1. Diff against cached snapshot from last run.
	lastRunMu.Lock()
	cached, ok := lastRunContent[filename]
	lastRunMu.Unlock()
	if ok {
		lines := diffCachedLines(filename, cached)
		if lines != nil {
			if len(lines) == 0 {
				return nil, "no change since last run", nil
			}
			names, err = functionsAtLines(filename, lines)
			return names, "diff vs last run", err
		}
	}

	// 2. git diff HEAD.
	lines, gitOK := gitChangedLines(filename)
	if gitOK && len(lines) > 0 {
		names, err = functionsAtLines(filename, lines)
		return names, "git diff", err
	}

	// 3. Fallback: treat every function in the file as potentially changed.
	names, err = InFile(filename)
	return names, "file (no diff available)", err
}

// diffCachedLines diffs the current file content against a cached byte slice
// using git diff --no-index. Returns changed line numbers in the new file,
// nil on error, or empty slice if files are identical.
func diffCachedLines(filename string, cached []byte) []int {
	current, err := os.ReadFile(filename)
	if err != nil {
		return nil
	}
	if bytes.Equal(cached, current) {
		return []int{} // identical — no changes
	}

	tmp, err := os.CreateTemp("", "ripple-baseline-*.go")
	if err != nil {
		return nil
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(cached); err != nil {
		tmp.Close()
		return nil
	}
	tmp.Close()

	// git diff --no-index exits 1 when files differ (not a real error).
	cmd := exec.Command("git", "diff", "--no-index", "--unified=0", "--", tmp.Name(), filename)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && len(out) > 0 {
			return parseHunkLines(out)
		}
		return nil
	}
	return []int{} // exit 0 = identical
}

// InFile returns all function and method names defined in filename.
func InFile(filename string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		names = append(names, fn.Name.Name)
	}
	return names, nil
}

// functionsAtLines returns names of functions whose body contains any of the
// given (1-based) line numbers.
func functionsAtLines(filename string, changedLines []int) ([]string, error) {
	lineSet := make(map[int]bool, len(changedLines))
	for _, l := range changedLines {
		lineSet[l] = true
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line
		for line := start; line <= end; line++ {
			if lineSet[line] {
				names = append(names, fn.Name.Name)
				break
			}
		}
	}
	return names, nil
}

// gitChangedLines returns the set of new-file line numbers that git considers
// changed in filename. It tries `git diff HEAD` first (staged+unstaged vs last
// commit), then `git diff` (unstaged only) as a fallback for new files.
// Returns (nil, false) when git is unavailable or the file has no diff.
func gitChangedLines(filename string) ([]int, bool) {
	lines := runGitDiff("HEAD", filename)
	if lines != nil {
		return lines, true
	}
	lines = runGitDiff("", filename)
	if lines != nil {
		return lines, true
	}
	return nil, false
}

func runGitDiff(ref, filename string) []int {
	args := []string{"diff", "--unified=0"}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--", filename)

	out, err := exec.Command("git", args...).Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return parseHunkLines(out)
}

// parseHunkLines extracts new-file line numbers from unified diff hunk headers.
// A header looks like: @@ -a,b +c,d @@ ...
// The +c,d part means lines c through c+d-1 in the new file.
func parseHunkLines(diff []byte) []int {
	var lines []int
	for _, line := range bytes.Split(diff, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("@@")) {
			continue
		}
		fields := bytes.Fields(line)
		for _, f := range fields {
			if !bytes.HasPrefix(f, []byte("+")) || bytes.Equal(f, []byte("+")) {
				continue
			}
			s := string(f[1:]) // strip leading '+'
			start, count := parseRange(s)
			if count == 0 {
				// A zero-count hunk (pure deletion) has no new lines.
				continue
			}
			for i := 0; i < count; i++ {
				lines = append(lines, start+i)
			}
		}
	}
	return lines
}

// parseRange parses "start" or "start,count" from a diff range string.
func parseRange(s string) (start, count int) {
	if i := strings.IndexByte(s, ','); i >= 0 {
		start, _ = strconv.Atoi(s[:i])
		count, _ = strconv.Atoi(s[i+1:])
	} else {
		start, _ = strconv.Atoi(s)
		count = 1
	}
	return
}

// RunFilter builds a -run regex from a list of test names.
// Returns empty string if names is empty (meaning: run all tests).
func RunFilter(names []string) string {
	return strings.Join(names, "|")
}

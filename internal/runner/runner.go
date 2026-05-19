package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"time"
)

// TestEvent mirrors the JSON output of `go test -json`.
type TestEvent struct {
	Time    time.Time
	Action  string // run, pass, fail, skip, output, start, build-fail, build-output
	Package string
	Test    string  // empty for package-level events
	Elapsed float64 // seconds
	Output  string
}

// PackageTest describes a single package to test with an optional -run filter.
type PackageTest struct {
	ImportPath string
	RunFilter  string // empty = run all tests in the package
	Cover      bool   // pass -cover to go test
}

// Run executes `go test -json` for each module group in parallel and streams
// TestEvents into the provided channel. pkgsByModule maps module root dir →
// list of PackageTests to run within that module. Cancelling ctx kills all
// running test processes immediately.
func Run(ctx context.Context, pkgsByModule map[string][]PackageTest, extraFlags []string, events chan<- TestEvent) {
	var wg sync.WaitGroup
	for modRoot, pkgs := range pkgsByModule {
		wg.Add(1)
		go func(dir string, pkgs []PackageTest) {
			defer wg.Done()
			runInDir(ctx, dir, pkgs, extraFlags, events)
		}(modRoot, pkgs)
	}
	wg.Wait()
}

// runInDir groups packages by their RunFilter and issues one `go test` call per
// distinct filter so that -run applies correctly per package.
func runInDir(ctx context.Context, dir string, packages []PackageTest, extraFlags []string, events chan<- TestEvent) {
	// Group by (filter, cover): packages with the same settings share one invocation.
	type groupKey struct {
		filter string
		cover  bool
	}
	type group struct {
		key  groupKey
		pkgs []string
	}
	// Preserve insertion order for deterministic behaviour.
	var keyOrder []groupKey
	byKey := map[groupKey]*group{}
	for _, pt := range packages {
		k := groupKey{filter: pt.RunFilter, cover: pt.Cover}
		if _, ok := byKey[k]; !ok {
			keyOrder = append(keyOrder, k)
			byKey[k] = &group{key: k}
		}
		byKey[k].pkgs = append(byKey[k].pkgs, pt.ImportPath)
	}

	var wg sync.WaitGroup
	for _, k := range keyOrder {
		g := byKey[k]
		wg.Add(1)
		go func(k groupKey, pkgs []string) {
			defer wg.Done()
			execTest(ctx, dir, k.filter, k.cover, extraFlags, pkgs, events)
		}(g.key, g.pkgs)
	}
	wg.Wait()
}

func execTest(ctx context.Context, dir, runFilter string, cover bool, extraFlags, packages []string, events chan<- TestEvent) {
	args := []string{"test", "-json"}
	args = append(args, extraFlags...)
	if runFilter != "" {
		args = append(args, "-run", runFilter)
	}
	if cover {
		args = append(args, "-cover")
	}
	args = append(args, packages...)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emitBuildFail(packages, err.Error(), events)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		emitBuildFail(packages, err.Error(), events)
		return
	}
	if err := cmd.Start(); err != nil {
		emitBuildFail(packages, err.Error(), events)
		return
	}

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			events <- TestEvent{Action: "build-output", Output: scanner.Text() + "\n"}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var ev TestEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		events <- ev
	}

	cmd.Wait()
}

func emitBuildFail(packages []string, msg string, events chan<- TestEvent) {
	for _, pkg := range packages {
		events <- TestEvent{Action: "build-fail", Package: pkg, Output: msg}
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	callgraph "github.com/urechandro/go-callgraph"
	"github.com/urechandro/ripple/internal/depgraph"
	"github.com/urechandro/ripple/internal/symbols"
)

// runServe runs the long-running daemon protocol on stdin/stdout.
//
// One JSON request per stdin line. Recognised today:
//
//	{"id":"<opaque>","cmd":"run","files":["abs/path.go", ...]}
//	{"cmd":"shutdown"}                              // closes the daemon
//	{"cmd":"ping"}                                  // emits {"type":"pong",...} for liveness checks
//
// One NDJSON envelope per stdout line. The shapes (`header` / `event` /
// `result`) are the same as `-json -run -stream`; the daemon just adds an
// optional `id` field so consumers can correlate when they pipeline
// requests. An `error` envelope ends a request without a result.
//
// Stderr carries lifecycle messages (ready / incremental rebuild / shutdown)
// — anything not on stdout is not part of the wire format.
func runServe(root string, graph *depgraph.Graph, cgs map[string]*callgraph.Graph, skipPatterns []string, depth int, cgMethod callgraph.Method, testFlags []string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fatalf("watcher: %v", err)
	}
	defer watcher.Close()

	if err := watchGoPackageDirs(watcher, root); err != nil {
		fatalf("watching: %v", err)
	}

	st := &serveState{
		graph:        graph,
		cgs:          cgs,
		cgMethod:     cgMethod,
		dirtyModules: map[string]bool{},
	}

	go st.watchLoop(watcher)

	fmt.Fprintln(os.Stderr, "ripple: serve ready")

	dec := json.NewDecoder(os.Stdin)
	for {
		var req serveRequest
		if err := dec.Decode(&req); err != nil {
			// EOF or stdin closed — clean exit. Any other error is logged
			// but also treated as terminal: a corrupt stream can't recover.
			fmt.Fprintln(os.Stderr, "ripple: serve shutting down")
			return
		}
		switch req.Cmd {
		case "ping":
			emitStream(struct {
				Type string `json:"type"`
				ID   string `json:"id,omitempty"`
			}{"pong", req.ID})
		case "shutdown":
			fmt.Fprintln(os.Stderr, "ripple: serve shutting down")
			return
		case "run":
			st.handleRun(root, req, skipPatterns, depth, testFlags)
		default:
			emitStream(streamError{
				Type:    "error",
				ID:      req.ID,
				Message: fmt.Sprintf("unknown cmd %q", req.Cmd),
			})
		}
	}
}

type serveRequest struct {
	ID    string   `json:"id"`
	Cmd   string   `json:"cmd"`
	Files []string `json:"files"`
}

// serveState owns the warm graph + per-module callgraph cache and the set of
// modules whose call graph is stale because of a watcher event. All mutation
// happens behind `mu`; readers (the request handler) take it briefly to
// snapshot the dirty set before doing the actual rebuild.
type serveState struct {
	mu           sync.Mutex
	graph        *depgraph.Graph
	cgs          map[string]*callgraph.Graph
	cgMethod     callgraph.Method
	dirtyModules map[string]bool
}

func (s *serveState) markDirty(filePath string) {
	modRoot, ok := s.graph.ModuleForFile(filePath)
	if !ok {
		return
	}
	s.mu.Lock()
	s.dirtyModules[modRoot] = true
	s.mu.Unlock()
}

// takeDirty returns and clears the current dirty set. Callers rebuild without
// holding the mutex so unrelated watcher events can keep marking new dirt
// while the rebuild is in flight; anything that arrives after this call lands
// in the next dirty set and the next request picks it up.
func (s *serveState) takeDirty() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dirtyModules) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.dirtyModules))
	for m := range s.dirtyModules {
		out = append(out, m)
	}
	s.dirtyModules = map[string]bool{}
	return out
}

func (s *serveState) rebuild(dirty []string) {
	if len(dirty) == 0 {
		return
	}
	start := time.Now()
	s.graph.ReingestModules(dirty)
	cg, err := callgraph.Build(dirty, s.cgMethod)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ripple: incremental rebuild failed: %v\n", err)
		return
	}
	s.mu.Lock()
	for _, m := range dirty {
		s.cgs[m] = cg
	}
	s.mu.Unlock()
	fmt.Fprintf(os.Stderr, "ripple: incremental rebuild %d module(s) in %s\n",
		len(dirty), time.Since(start).Round(time.Millisecond))
}

func (s *serveState) snapshotCgs() map[string]*callgraph.Graph {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*callgraph.Graph, len(s.cgs))
	for k, v := range s.cgs {
		out[k] = v
	}
	return out
}

func (s *serveState) handleRun(root string, req serveRequest, skipPatterns []string, depth int, testFlags []string) {
	// Rebuild stale call graphs before we compute affected packages, otherwise
	// a newly added function in the dirty module would be invisible to
	// TestsCovering and we'd under-report tests.
	s.rebuild(s.takeDirty())

	files := make([]string, 0, len(req.Files))
	for _, f := range req.Files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !filepath.IsAbs(f) {
			f = filepath.Join(root, f)
		}
		files = append(files, f)
	}

	runJSONForFiles(
		root,
		s.graph,
		s.snapshotCgs(),
		skipPatterns,
		depth,
		true, // -run is implicit in serve mode
		true, // stream is always on in serve mode
		req.ID,
		files,
		testFlags,
	)

	// Snapshot file contents so the next request only sees deltas. Without
	// this, a save with no actual diff vs git HEAD would re-run the same
	// tests it ran last time — matches the TUI's MarkProcessed call after
	// each run.
	for _, f := range files {
		symbols.MarkProcessed(f)
	}
}

// watchLoop translates fsnotify events into dirty-module markings. The TUI's
// debouncer (80 ms) and structural-change detection live inside its bubbletea
// loop — in serve mode we rely on a much simpler rule: any write to a .go
// file marks its module dirty, and the *next* request rebuilds. New
// directories are added to the watcher on the fly so packages added
// post-startup are still picked up.
func (s *serveState) watchLoop(w *fsnotify.Watcher) {
	skip := map[string]bool{
		".git": true, "vendor": true, "node_modules": true,
		"dist": true, "testdata": true, ".idea": true, ".vscode": true,
	}
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					if !skip[filepath.Base(ev.Name)] {
						_ = w.Add(ev.Name)
					}
					continue
				}
			}
			isWrite := ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create)
			if !isWrite || !strings.HasSuffix(ev.Name, ".go") {
				continue
			}
			s.markDirty(ev.Name)
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}


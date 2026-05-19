package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"

	callgraph "github.com/urechandro/go-callgraph"
	"github.com/urechandro/ripple/internal/depgraph"
	"github.com/urechandro/ripple/internal/runner"
	"github.com/urechandro/ripple/internal/symbols"
)

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	passStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // bright green
	failStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // bright red
	runStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // bright yellow
	skipStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // gray
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// ── Domain types ──────────────────────────────────────────────────────────────

type packageResult struct {
	importPath string
	status     string // "running" | "pass" | "fail" | "build-fail" | "skipped"
	elapsed    float64
	tests      []testCase
	buildOut   []string // stderr lines when status == "build-fail"
	runFilter  string   // non-empty when only a subset of tests are run
	skipReason string   // the pattern that caused this package to be skipped
	coverage   string   // e.g. "42.3%" — populated when -cover is active
}

type testCase struct {
	name    string
	status  string // "running" | "pass" | "fail" | "skip"
	elapsed float64
	output  []string
}

func (p *packageResult) upsertTest(name string) *testCase {
	for i := range p.tests {
		if p.tests[i].name == name {
			return &p.tests[i]
		}
	}
	p.tests = append(p.tests, testCase{name: name, status: "running"})
	return &p.tests[len(p.tests)-1]
}

// ── Tea messages ──────────────────────────────────────────────────────────────

type fileChangedMsg struct{ path string }
type debouncedRunMsg struct{ gen int }
type testEventMsg struct {
	ev  runner.TestEvent
	gen int
}
type runDoneMsg struct{ gen int }
type graphRebuiltMsg struct {
	graph *depgraph.Graph
	cg    *callgraph.Graph
}

// ── Debug info ────────────────────────────────────────────────────────────────

type changedFileInfo struct {
	path         string
	symbols      []string
	symbolSource string // "git diff" or "file (git unavailable or no diff)"
}

type debugRun struct {
	files []changedFileInfo
	pkgs  []pkgDebug
}

type pkgDebug struct {
	importPath string
	dir        string
	tests      []string // tests found by call graph
	reason     string   // "filtered", "no blast radius", "dir unknown", "skipped"
}

// ── Model ─────────────────────────────────────────────────────────────────────

type Model struct {
	root         string
	graph        *depgraph.Graph
	cg           *callgraph.Graph
	watcher      *fsnotify.Watcher
	eventCh      chan runner.TestEvent
	skipPatterns []string
	depth        int
	cgMethod     callgraph.Method
	testFlags    []string

	pendingFiles      map[string]bool // files changed but not yet processed
	pendingStructural map[string]bool // structural changes awaiting rebuild
	debounceGen       int             // incremented each time a new file change arrives

	lastEvent       string // raw fsnotify path, for diagnostics
	lastChanged     string // description of what triggered the current run
	lastStatus      string // what happened with the last event (e.g. "no tests affected")
	rebuildingGraph bool
	debug           bool
	coverage        bool
	aborted         bool
	cancelRun       context.CancelFunc
	runGen          int
	debugRun        debugRun

	// Expected test count, computed before the run starts.
	// expectedTests is the number of specific test functions targeted (from call graph).
	// expectedUnfiltered is the number of packages running without a filter (count unknown).
	expectedTests      int
	expectedUnfiltered int
	packages        map[string]*packageResult
	pkgOrder        []string      // display order — populated lazily as events arrive
	pkgInOrder      map[string]bool // set for O(1) membership check
	running         bool
	width           int
	height          int
	viewport        viewport.Model
	ready           bool // true after first WindowSizeMsg

}

func New(root string, graph *depgraph.Graph, cg *callgraph.Graph, watcher *fsnotify.Watcher, skipPatterns []string, depth int, cgMethod callgraph.Method, testFlags []string) Model {
	return Model{
		root:         root,
		graph:        graph,
		cg:           cg,
		watcher:      watcher,
		eventCh:      make(chan runner.TestEvent, 256),
		packages:          make(map[string]*packageResult),
		pkgInOrder:        make(map[string]bool),
		pendingFiles:      make(map[string]bool),
		pendingStructural: make(map[string]bool),
		skipPatterns: skipPatterns,
		depth:        depth,
		cgMethod:     cgMethod,
		testFlags:    testFlags,
	}
}

// skipReason returns the first matching skip pattern, or "" if not skipped.
func (m Model) skipReason(importPath string) string {
	for _, p := range m.skipPatterns {
		if p != "" && strings.Contains(importPath, p) {
			return p
		}
	}
	return ""
}

func (m Model) Init() tea.Cmd {
	return listenForFileChange(m.watcher)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			if !m.rebuildingGraph {
				m.rebuildingGraph = true
				m.refreshViewport()
				return m, rebuildAll(m.root, m.cgMethod)
			}
		case "d":
			m.debug = !m.debug
			m.refreshViewport()
		case "v":
			m.coverage = !m.coverage
			m.refreshViewport()
		case "c":
			if m.running && m.cancelRun != nil {
				m.cancelRun()
			}
		case "a":
			if !m.running {
				return m, m.startRunAll()
			}
		}
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		return m, vpCmd

	case graphRebuiltMsg:
		if msg.graph != nil {
			m.graph = msg.graph
		}
		m.cg = msg.cg
		m.rebuildingGraph = false
		m.refreshViewport()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerH := lipgloss.Height(m.renderHeader())
		footerH := lipgloss.Height(m.renderFooter())
		vpH := max(0, m.height-headerH-footerH)
		if !m.ready {
			m.viewport = viewport.New(m.width, vpH)
			m.viewport.SetContent(m.renderBody())
			m.ready = true
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = vpH
		}

	// Accumulate changed files and (re)start the debounce window.
	case fileChangedMsg:
		m.lastEvent = msg.path
		m.pendingFiles[msg.path] = true
		if m.isStructuralChange(msg.path) {
			m.pendingStructural[msg.path] = true
		}
		m.debounceGen++
		gen := m.debounceGen
		return m, tea.Batch(
			listenForFileChange(m.watcher),
			scheduleRun(gen),
		)

	// Fires after the debounce window; stale messages are ignored via gen.
	case debouncedRunMsg:
		if msg.gen != m.debounceGen {
			return m, nil
		}

		files := m.pendingFiles
		m.pendingFiles = make(map[string]bool)

		structuralFiles := m.pendingStructural
		m.pendingStructural = make(map[string]bool)

		// Kick off an incremental graph rebuild for dirty modules.
		var rebuildCmd tea.Cmd
		if len(structuralFiles) > 0 && !m.rebuildingGraph {
			m.rebuildingGraph = true
			rebuildCmd = rebuildIncremental(m.graph, structuralFiles, m.cgMethod)
		}

		// Compute union of affected packages across all changed files.
		affectedSet := map[string]bool{}
		for path := range files {
			for _, pkg := range m.graph.AffectedTestPackages(path, m.depth) {
				affectedSet[pkg] = true
			}
		}

		// Always clear previous results immediately so stale output is never visible.
		m.packages = make(map[string]*packageResult)
		m.pkgOrder = nil
		m.pkgInOrder = make(map[string]bool)
		m.running = false

		if len(affectedSet) == 0 {
			m.lastStatus = dimStyle.Render("no tests affected")
			m.refreshViewport()
			return m, rebuildCmd
		}

		// Cancel any in-flight run before starting a new one.
		if m.cancelRun != nil {
			m.cancelRun()
		}
		// Fresh channel per run — old channels are closed by runTests,
		// which unblocks any stale listeners. No event bleed between runs.
		m.eventCh = make(chan runner.TestEvent, 256)
		m.lastStatus = ""
		m.aborted = false
		m.running = true
		m.expectedTests = 0
		m.expectedUnfiltered = 0
		m.runGen++
		var runCtx context.Context
		runCtx, m.cancelRun = context.WithCancel(context.Background())

		// Describe what changed in the header.
		if len(files) == 1 {
			for path := range files {
				m.lastChanged = path
			}
		} else {
			m.lastChanged = fmt.Sprintf("(%d files)", len(files))
		}

		// Collect per-file symbol info and build a union of all symbols.
		var fileInfos []changedFileInfo
		var allSymbols []string
		symbolsSeen := map[string]bool{}

		for path := range files {
			changedSyms, source, _ := symbols.ChangedInFile(path)
			symbols.MarkProcessed(path) // snapshot for next run's diff
			fileInfos = append(fileInfos, changedFileInfo{
				path:         path,
				symbols:      changedSyms,
				symbolSource: source,
			})
			for _, s := range changedSyms {
				if !symbolsSeen[s] {
					symbolsSeen[s] = true
					allSymbols = append(allSymbols, s)
				}
			}
		}
		// Stable display order.
		sort.Slice(fileInfos, func(i, j int) bool { return fileInfos[i].path < fileInfos[j].path })

		m.debugRun = debugRun{files: fileInfos}

		// Stable package order.
		affected := make([]string, 0, len(affectedSet))
		for pkg := range affectedSet {
			affected = append(affected, pkg)
		}
		sort.Strings(affected)

		// Build the set of affected dirs and run the CHA call graph search once.
		affectedDirSet := map[string]bool{}
		for _, pkg := range affected {
			if dir, ok := m.graph.DirFor(pkg); ok {
				affectedDirSet[dir] = true
			}
		}
		changedFilePaths := make([]string, 0, len(files))
		for path := range files {
			changedFilePaths = append(changedFilePaths, path)
		}
		testsByDir := m.cg.TestsCovering(affectedDirSet, changedFilePaths, allSymbols)

		var toRun []runner.PackageTest
		for _, pkg := range affected {
			if reason := m.skipReason(pkg); reason != "" {
				m.packages[pkg] = &packageResult{importPath: pkg, status: "skipped", skipReason: reason}
				m.pkgOrder = append(m.pkgOrder, pkg) // skipped is resolved immediately, show it now
				m.pkgInOrder[pkg] = true
				m.debugRun.pkgs = append(m.debugRun.pkgs, pkgDebug{
					importPath: pkg,
					reason:     "skipped [" + reason + "]",
				})
				continue
			}

			pt := runner.PackageTest{ImportPath: pkg, Cover: m.coverage}
			dbg := pkgDebug{importPath: pkg}

			if len(allSymbols) > 0 {
				dir, dirOK := m.graph.DirFor(pkg)
				dbg.dir = dir
				if !dirOK {
					dbg.reason = "dir unknown — graph stale, running all"
					m.expectedUnfiltered++
				} else {
					tests := testsByDir[dir]
					dbg.tests = tests
					if len(tests) == 0 {
						dbg.reason = "no blast radius"
						m.debugRun.pkgs = append(m.debugRun.pkgs, dbg)
						continue
					}
					pt.RunFilter = symbols.RunFilter(tests)
					dbg.reason = "filtered"
					m.expectedTests += len(tests)
				}
			} else {
				dbg.reason = "no symbols parsed, running all"
				m.expectedUnfiltered++
			}

			m.debugRun.pkgs = append(m.debugRun.pkgs, dbg)
			m.packages[pkg] = &packageResult{importPath: pkg, status: "running", runFilter: pt.RunFilter}
			// pkgOrder is NOT populated here — packages appear lazily when their first event arrives.
			toRun = append(toRun, pt)
		}

		m.viewport.GotoTop()
		m.refreshViewport()
		cmds := []tea.Cmd{
			runTests(runCtx, m.runGen, groupByModule(m.graph, toRun), m.testFlags, m.eventCh),
			listenForTestEvent(m.eventCh, m.runGen),
		}
		if rebuildCmd != nil {
			cmds = append(cmds, rebuildCmd)
		}
		return m, tea.Batch(cmds...)

	case testEventMsg:
		if msg.gen != m.runGen {
			return m, nil // stale event from a cancelled run
		}
		m.handleTestEvent(msg.ev)
		m.refreshViewport()
		return m, listenForTestEvent(m.eventCh, m.runGen)

	case runDoneMsg:
		if msg.gen != m.runGen {
			return m, nil // stale — a newer run has already started
		}
		m.running = false
		m.cancelRun = nil
		// Mark any packages that never finished as aborted.
		for _, pkg := range m.packages {
			if pkg.status == "running" {
				pkg.status = "aborted"
			}
		}
		m.refreshViewport()
	}

	return m, nil
}

func (m *Model) handleTestEvent(ev runner.TestEvent) {
	pkg, ok := m.packages[ev.Package]
	if !ok {
		return
	}
	if !m.pkgInOrder[ev.Package] {
		m.pkgOrder = append(m.pkgOrder, ev.Package)
		m.pkgInOrder[ev.Package] = true
	}

	switch ev.Action {
	case "pass":
		if ev.Test == "" {
			pkg.status = "pass"
			pkg.elapsed = ev.Elapsed
		} else {
			t := pkg.upsertTest(ev.Test)
			t.status = "pass"
			t.elapsed = ev.Elapsed
		}

	case "fail":
		if ev.Test == "" {
			if pkg.status != "fail" {
				pkg.status = "fail"
			}
			pkg.elapsed = ev.Elapsed
		} else {
			t := pkg.upsertTest(ev.Test)
			t.status = "fail"
			t.elapsed = ev.Elapsed
			pkg.status = "fail"
		}

	case "skip":
		if ev.Test != "" {
			t := pkg.upsertTest(ev.Test)
			t.status = "skip"
			t.elapsed = ev.Elapsed
		}

	case "run":
		if ev.Test != "" {
			pkg.upsertTest(ev.Test)
		}

	case "output":
		if ev.Test == "" {
			// Intercept coverage summary line instead of dumping it into buildOut.
			if strings.HasPrefix(strings.TrimSpace(ev.Output), "coverage:") {
				// e.g. "coverage: 42.3% of statements"
				parts := strings.Fields(strings.TrimSpace(ev.Output))
				if len(parts) >= 2 {
					pkg.coverage = parts[1] // "42.3%"
				}
			} else {
				pkg.buildOut = append(pkg.buildOut, ev.Output)
			}
		} else {
			t := pkg.upsertTest(ev.Test)
			t.output = append(t.output, ev.Output)
		}

	case "build-fail", "build-output":
		pkg.status = "build-fail"
		if ev.Output != "" {
			pkg.buildOut = append(pkg.buildOut, ev.Output)
		}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if !m.ready {
		return "\n  initialising…"
	}
	return m.renderHeader() + "\n" + m.viewport.View() + "\n" + m.renderFooter()
}

func (m *Model) refreshViewport() {
	if m.ready {
		m.viewport.SetContent(m.renderBody())
	}
}

func (m Model) renderHeader() string {
	title := headerStyle.Render("ripple")

	var statusStr string
	if m.running {
		statusStr = runStyle.Render("● running")
	} else if len(m.pkgOrder) == 0 {
		statusStr = dimStyle.Render(fmt.Sprintf("watching %d files, %d symbols", m.graph.FileCount(), m.cg.SymbolCount()))
	} else {
		statusStr = dimStyle.Render("done")
	}

	// Build expected-count hint that persists for the full run (and after).
	var countHint string
	if m.expectedTests > 0 || m.expectedUnfiltered > 0 {
		if m.expectedUnfiltered > 0 && m.expectedTests == 0 {
			countHint = dimStyle.Render("  all tests")
		} else if m.expectedUnfiltered > 0 {
			countHint = dimStyle.Render(fmt.Sprintf("  ~%d+ tests targeted", m.expectedTests))
		} else {
			countHint = dimStyle.Render(fmt.Sprintf("  ~%d tests targeted", m.expectedTests))
		}
	}

	var lines []string
	if m.lastEvent != "" {
		rel, _ := filepath.Rel(m.root, m.lastEvent)
		lines = append(lines, dimStyle.Render("  event: "+rel))
	}
	if m.lastStatus != "" {
		lines = append(lines, "  "+m.lastStatus)
	} else if m.lastChanged != "" {
		// lastChanged is either a single path or "(N files)"
		display := m.lastChanged
		if !strings.HasPrefix(display, "(") {
			rel, _ := filepath.Rel(m.root, display)
			display = rel
		}
		lines = append(lines, dimStyle.Render("  running tests for: "+display)+countHint)
	}

	suffix := ""
	if len(lines) > 0 {
		suffix = "\n" + strings.Join(lines, "\n")
	}

	return title + "  " + statusStr + suffix
}

func (m Model) renderBody() string {
	var b strings.Builder

	if m.debug {
		b.WriteString(m.renderDebug())
	}
	for _, ip := range m.pkgOrder {
		b.WriteString(renderPackage(m.packages[ip]))
	}
	return b.String()
}

func (m Model) renderDebug() string {
	if len(m.debugRun.files) == 0 {
		return dimStyle.Render("  ── debug: no run yet ──") + "\n\n"
	}

	sep := dimStyle.Render(strings.Repeat("─", 60))
	var b strings.Builder
	b.WriteString(sep + "\n")

	for _, fi := range m.debugRun.files {
		rel, _ := filepath.Rel(m.root, fi.path)
		b.WriteString(dimStyle.Render("  changed  ") + rel + "\n")
		if len(fi.symbols) > 0 {
			b.WriteString(dimStyle.Render("  symbols  ") + wrapList(fi.symbols, ", ", 11, m.width) + "\n")
		} else {
			b.WriteString(dimStyle.Render("  symbols  ") + failStyle.Render("none parsed") + "\n")
		}
		b.WriteString(dimStyle.Render("  source   "+fi.symbolSource) + "\n")
		b.WriteString("\n")
	}

	for _, d := range m.debugRun.pkgs {
		// package name — short form
		short := d.importPath
		if i := strings.LastIndex(short, "/"); i >= 0 {
			short = short[i+1:]
		}

		switch d.reason {
		case "filtered":
			b.WriteString(passStyle.Render("  ✓ "+short) + "\n")
			b.WriteString(dimStyle.Render("    run:   ") + wrapList(d.tests, " | ", 11, m.width) + "\n")
		case "no blast radius":
			b.WriteString(skipStyle.Render("  - "+short) + dimStyle.Render("  no blast radius") + "\n")
		default:
			icon := "  · "
			if strings.HasPrefix(d.reason, "skipped") {
				icon = "  - "
			}
			b.WriteString(dimStyle.Render(icon+short+"  "+d.reason) + "\n")
		}
		if d.dir != "" {
			relDir, _ := filepath.Rel(m.root, d.dir)
			b.WriteString(dimStyle.Render("    dir:   "+relDir) + "\n")
		}
	}

	b.WriteString(sep + "\n\n")
	return b.String()
}

func (m Model) renderFooter() string {
	var parts []string

	if !m.running && len(m.pkgOrder) > 0 {
		parts = append(parts, m.renderSummary())
	}

	scrollPct := int(m.viewport.ScrollPercent() * 100)
	scrollHint := dimStyle.Render(fmt.Sprintf("↑/↓ scroll  %d%%", scrollPct))
	qHint := dimStyle.Render("q quit")

	var rHint string
	if m.rebuildingGraph {
		rHint = runStyle.Render("r rebuilding graph…")
	} else {
		rHint = dimStyle.Render("r rebuild graph")
	}

	var dHint string
	if m.debug {
		dHint = passStyle.Render("d debug")
	} else {
		dHint = dimStyle.Render("d debug")
	}

	var vHint string
	if m.coverage {
		vHint = passStyle.Render("v coverage")
	} else {
		vHint = dimStyle.Render("v coverage")
	}

	aHint := dimStyle.Render("a run all")

	hints := "  " + scrollHint + "   " + rHint + "   " + aHint + "   " + dHint + "   " + vHint
	if m.running {
		hints += "   " + runStyle.Render("c cancel")
	}
	hints += "   " + qHint
	parts = append(parts, hints)

	return strings.Join(parts, "\n")
}

func renderPackage(pkg *packageResult) string {
	var b strings.Builder

	switch pkg.status {
	case "pass":
		coverStr := ""
		if pkg.coverage != "" {
			coverStr = dimStyle.Render("  " + pkg.coverage)
		}
		b.WriteString(passStyle.Render(fmt.Sprintf("  ✓  %-60s %.2fs", pkg.importPath, pkg.elapsed)) + coverStr)
	case "fail":
		b.WriteString(failStyle.Render(fmt.Sprintf("  ✗  %s", pkg.importPath)))
	case "build-fail":
		b.WriteString(failStyle.Render(fmt.Sprintf("  ✗  %s  (build failed)", pkg.importPath)))
	case "skipped":
		b.WriteString(skipStyle.Render(fmt.Sprintf("  -  %s  (skipped)", pkg.importPath)))
	case "running":
		b.WriteString(runStyle.Render(fmt.Sprintf("  ·  %s", pkg.importPath)))
	case "aborted":
		b.WriteString(skipStyle.Render(fmt.Sprintf("  ·  %s  (aborted)", pkg.importPath)))
	}
	b.WriteString("\n")

	// Show all tests.
	for _, t := range pkg.tests {
		switch t.status {
		case "pass":
			b.WriteString(passStyle.Render(fmt.Sprintf("       ✓ %-55s %.2fs", t.name, t.elapsed)))
			b.WriteString("\n")
		case "fail":
			b.WriteString(failStyle.Render(fmt.Sprintf("       ✗ %s (%.2fs)", t.name, t.elapsed)))
			b.WriteString("\n")
			for _, line := range t.output {
				trimmed := strings.TrimRight(line, "\n")
				if trimmed != "" {
					b.WriteString(dimStyle.Render("         "+trimmed) + "\n")
				}
			}
		case "skip":
			b.WriteString(skipStyle.Render(fmt.Sprintf("       - %s", t.name)))
			b.WriteString("\n")
		case "running":
			b.WriteString(runStyle.Render(fmt.Sprintf("       · %s", t.name)))
			b.WriteString("\n")
		}
	}

	// Show package-level output for any failure (panics, TestMain, build errors,
	// or any output that arrived without a test name).
	if pkg.status == "fail" || pkg.status == "build-fail" {
		for _, line := range pkg.buildOut {
			trimmed := strings.TrimRight(line, "\n")
			if trimmed != "" {
				b.WriteString(dimStyle.Render("       "+trimmed) + "\n")
			}
		}
	}

	return b.String()
}

func (m Model) renderSummary() string {
	var (
		testsPassed, testsFailed int
		pkgsPassed, pkgsFailed   int
		skipReasons              = map[string]int{} // reason → count
	)

	for _, pkg := range m.packages {
		switch pkg.status {
		case "pass":
			pkgsPassed++
			for _, t := range pkg.tests {
				if t.status == "pass" {
					testsPassed++
				}
			}
		case "fail", "build-fail":
			pkgsFailed++
			for _, t := range pkg.tests {
				switch t.status {
				case "pass":
					testsPassed++
				case "fail":
					testsFailed++
				}
			}
		case "skipped":
			skipReasons[pkg.skipReason]++
		}
	}

	pkgsRun := pkgsPassed + pkgsFailed
	var parts []string

	// Test counts.
	if testsPassed > 0 || testsFailed > 0 {
		testPart := fmt.Sprintf("%d tests", testsPassed+testsFailed)
		if testsFailed > 0 {
			testPart = failStyle.Render(fmt.Sprintf("%d failed", testsFailed)) +
				dimStyle.Render(", ") +
				passStyle.Render(fmt.Sprintf("%d passed", testsPassed))
		} else {
			testPart = passStyle.Render(fmt.Sprintf("%d passed", testsPassed))
		}
		// Append package context.
		if pkgsRun > 0 {
			pkgWord := "package"
			if pkgsRun > 1 {
				pkgWord = "packages"
			}
			testPart += dimStyle.Render(fmt.Sprintf(" in %d %s", pkgsRun, pkgWord))
		}
		parts = append(parts, testPart)
	} else if pkgsFailed > 0 {
		// build failures — no individual tests ran
		parts = append(parts, failStyle.Render(fmt.Sprintf("%d failed to build", pkgsFailed)))
	}

	// Skipped packages grouped by reason.
	for reason, count := range skipReasons {
		pkgWord := "package"
		if count > 1 {
			pkgWord = "packages"
		}
		parts = append(parts, skipStyle.Render(fmt.Sprintf("%d %s skipped", count, pkgWord))+
			dimStyle.Render(fmt.Sprintf(" [%s]", reason)))
	}

	return "  " + strings.Join(parts, dimStyle.Render("   "))
}

// ── Tea commands ──────────────────────────────────────────────────────────────

func listenForFileChange(w *fsnotify.Watcher) tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return nil
				}
				isWrite := ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create)
				isGo := strings.HasSuffix(ev.Name, ".go")
				if isWrite && isGo {
					return fileChangedMsg{path: ev.Name}
				}
			case _, ok := <-w.Errors:
				if !ok {
					return nil
				}
			}
		}
	}
}

// scheduleRun waits 80ms then fires debouncedRunMsg with the given generation
// counter. If a newer file change arrives before the timer fires, its gen will
// be higher and this message will be silently dropped.
func scheduleRun(gen int) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(80 * time.Millisecond)
		return debouncedRunMsg{gen: gen}
	}
}

func listenForTestEvent(ch <-chan runner.TestEvent, gen int) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil // channel closed, run finished
		}
		return testEventMsg{ev: ev, gen: gen}
	}
}

func groupByModule(g *depgraph.Graph, pkgs []runner.PackageTest) map[string][]runner.PackageTest {
	result := make(map[string][]runner.PackageTest)
	for _, pt := range pkgs {
		if modRoot, ok := g.ModuleFor(pt.ImportPath); ok {
			result[modRoot] = append(result[modRoot], pt)
		}
	}
	return result
}

func runTests(ctx context.Context, gen int, pkgsByModule map[string][]runner.PackageTest, testFlags []string, eventCh chan runner.TestEvent) tea.Cmd {
	return func() tea.Msg {
		runner.Run(ctx, pkgsByModule, testFlags, eventCh)
		close(eventCh) // unblock any listeners on this channel
		return runDoneMsg{gen: gen}
	}
}

func rebuildAll(root string, method callgraph.Method) tea.Cmd {
	return func() tea.Msg {
		graph, err := depgraph.Build(root)
		if err != nil {
			return graphRebuiltMsg{}
		}
		cg, _ := callgraph.Build(graph.ModuleRoots(), method)
		return graphRebuiltMsg{graph: graph, cg: cg}
	}
}

func rebuildIncremental(graph *depgraph.Graph, structuralFiles map[string]bool, method callgraph.Method) tea.Cmd {
	dirtyModules := map[string]bool{}
	for path := range structuralFiles {
		if modRoot, ok := graph.ModuleForFile(path); ok {
			dirtyModules[modRoot] = true
		}
	}
	if len(dirtyModules) == 0 {
		return nil
	}
	roots := make([]string, 0, len(dirtyModules))
	for r := range dirtyModules {
		roots = append(roots, r)
	}
	return func() tea.Msg {
		graph.ReingestModules(roots)
		cg, _ := callgraph.Build(graph.ModuleRoots(), method)
		return graphRebuiltMsg{graph: graph, cg: cg}
	}
}

func (m *Model) startRunAll() tea.Cmd {
	if m.cancelRun != nil {
		m.cancelRun()
	}
	m.eventCh = make(chan runner.TestEvent, 256)
	m.packages = make(map[string]*packageResult)
	m.pkgOrder = nil
	m.pkgInOrder = make(map[string]bool)
	m.lastStatus = ""
	m.lastChanged = "all packages"
	m.aborted = false
	m.running = true
	m.expectedTests = 0
	m.expectedUnfiltered = 0
	m.debugRun = debugRun{}
	m.runGen++
	var runCtx context.Context
	runCtx, m.cancelRun = context.WithCancel(context.Background())

	allPkgs := m.graph.TestPackages()
	sort.Strings(allPkgs)

	var toRun []runner.PackageTest
	for _, pkg := range allPkgs {
		if reason := m.skipReason(pkg); reason != "" {
			m.packages[pkg] = &packageResult{importPath: pkg, status: "skipped", skipReason: reason}
			m.pkgOrder = append(m.pkgOrder, pkg)
			m.pkgInOrder[pkg] = true
			continue
		}
		m.packages[pkg] = &packageResult{importPath: pkg, status: "running"}
		m.expectedUnfiltered++
		toRun = append(toRun, runner.PackageTest{ImportPath: pkg, Cover: m.coverage})
	}

	m.viewport.GotoTop()
	m.refreshViewport()
	return tea.Batch(
		runTests(runCtx, m.runGen, groupByModule(m.graph, toRun), m.testFlags, m.eventCh),
		listenForTestEvent(m.eventCh, m.runGen),
	)
}

func (m Model) isStructuralChange(path string) bool {
	if strings.HasSuffix(path, "go.mod") || strings.HasSuffix(path, "go.sum") {
		return true
	}
	_, known := m.graph.FileToImport(path)
	return !known
}

// wrapList joins items with sep and wraps them so no line exceeds totalWidth
// columns. Continuation lines are indented by prefixWidth spaces so they align
// with the first item. Falls back to a single line if totalWidth is unknown (0).
func wrapList(items []string, sep string, prefixWidth, totalWidth int) string {
	if totalWidth <= prefixWidth {
		return strings.Join(items, sep)
	}
	indent := strings.Repeat(" ", prefixWidth)
	maxLine := totalWidth - prefixWidth

	var lines []string
	current := ""
	for _, item := range items {
		if current == "" {
			current = item
		} else if len(current)+len(sep)+len(item) <= maxLine {
			current += sep + item
		} else {
			lines = append(lines, current)
			current = item
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n"+indent)
}

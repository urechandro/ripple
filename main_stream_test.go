package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The -stream envelope shapes are public contracts with downstream consumers
// (the ripple-editor Tauri app, future CI dashboards, etc.). A rename of one
// of these json tags is a breaking protocol change, so pin the wire format
// with a round-trip test.
func TestStreamEnvelopes(t *testing.T) {
	t.Run("header", func(t *testing.T) {
		h := streamHeader{
			Type: "header",
			ChangedFiles: []jsonChangedFile{
				{Path: "foo.go", Symbols: []string{"X"}, Source: "git diff"},
			},
			AffectedPackages: []jsonPackage{
				{ImportPath: "example.com/foo", Tests: []string{"TestX"}, RunFilter: "TestX"},
			},
		}
		assertJSON(t, h, `"type":"header"`, `"changed_files"`, `"affected_packages"`, `"path":"foo.go"`)
	})

	t.Run("event", func(t *testing.T) {
		e := streamEvent{
			Type:    "event",
			Action:  "pass",
			Package: "example.com/foo",
			Test:    "TestX",
			Elapsed: 0.12,
		}
		assertJSON(t, e, `"type":"event"`, `"action":"pass"`, `"package":"example.com/foo"`, `"test":"TestX"`, `"elapsed":0.12`)

		// Empty Test/Output should be omitted (package-level events shouldn't carry empty test fields).
		pkgOnly := streamEvent{Type: "event", Action: "start", Package: "example.com/foo"}
		out, err := json.Marshal(pkgOnly)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(out), `"test"`) {
			t.Errorf("package-level event leaked empty test field: %s", out)
		}
		if strings.Contains(string(out), `"output"`) {
			t.Errorf("package-level event leaked empty output field: %s", out)
		}
	})

	t.Run("result", func(t *testing.T) {
		r := streamResult{
			Type:             "result",
			ChangedFiles:     []jsonChangedFile{{Path: "foo.go"}},
			AffectedPackages: []jsonPackage{{ImportPath: "example.com/foo", Status: "pass"}},
			Summary:          &jsonSummary{TestsPassed: 3, PkgsPassed: 1},
		}
		assertJSON(t, r, `"type":"result"`, `"summary"`, `"tests_passed":3`, `"packages_passed":1`)
	})
}

func assertJSON(t *testing.T, v any, wantSubstrings ...string) {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
}

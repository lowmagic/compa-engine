package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// depthTree is a synthetic tree four levels deep, each file's and
// directory's modification time set, a directory's after its contents.
//
//	top.txt               depth 1
//	d1/                   depth 1
//	d1/mid.txt            depth 2
//	d1/d2/                depth 2
//	d1/d2/deep.log        depth 3
//	d1/d2/d3/             depth 3
//	d1/d2/d3/deepest.txt  depth 4
func depthTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"top.txt":              "hi",
		"d1/mid.txt":           "four",
		"d1/d2/deep.log":       "",
		"d1/d2/d3/deepest.txt": "seven..",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Times in another zone, one with a fraction of a second: the detail
	// line gives each in UTC, exactly.
	ist := time.FixedZone("IST", 5*3600+1800)
	for _, c := range []struct {
		name string
		mod  time.Time
	}{
		{"top.txt", time.Date(2026, 10, 1, 17, 30, 0, 0, ist)},
		{"d1/mid.txt", time.Date(2026, 10, 2, 0, 0, 1, 500_000_000, time.UTC)},
		{"d1/d2/deep.log", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{"d1/d2/d3/deepest.txt", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)},
		{"d1/d2/d3", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
		{"d1/d2", time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)},
		{"d1", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)},
	} {
		if err := os.Chtimes(filepath.Join(root, c.name), c.mod, c.mod); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func findIn(t *testing.T, root string, params map[string]any) string {
	t.Helper()
	return mustOK(t, run(t, root, prim("find", params)))
}

func TestFindDetails(t *testing.T) {
	root := depthTree(t)
	got := findIn(t, root, map[string]any{"type": "any", "details": true})
	want := "dir\t-\t2026-10-07T00:00:00Z\td1\n" +
		"dir\t-\t2026-10-06T00:00:00Z\td1/d2\n" +
		"dir\t-\t2026-10-05T00:00:00Z\td1/d2/d3\n" +
		"file\t7\t2026-10-04T00:00:00Z\td1/d2/d3/deepest.txt\n" +
		"file\t0\t2026-10-03T00:00:00Z\td1/d2/deep.log\n" +
		"file\t4\t2026-10-02T00:00:01.5Z\td1/mid.txt\n" +
		"file\t2\t2026-10-01T12:00:00Z\ttop.txt\n"
	if got != want {
		t.Errorf("find details: got\n%s\nwant\n%s", got, want)
	}
	// A path naming a file lists that file.
	if got := findIn(t, root, map[string]any{"path": "d1/mid.txt", "details": true}); got != "file\t4\t2026-10-02T00:00:01.5Z\td1/mid.txt\n" {
		t.Errorf("find details of a file: got %q", got)
	}
}

// The filters choose the same entries, in the same order, whether details
// are on or off: a detail line is the plain line with fields before it.
func TestFindDetailsKeepTheFilters(t *testing.T) {
	root := depthTree(t)
	for name, params := range map[string]map[string]any{
		"files":      {},
		"dirs":       {"type": "dir"},
		"glob":       {"type": "any", "glob": "*.txt"},
		"size":       {"min_size": 1, "max_size": 4},
		"newer":      {"type": "any", "newer_than": "2026-10-02T00:00:01Z"},
		"older":      {"type": "any", "older_than": "2026-10-04T00:00:00Z"},
		"depth":      {"type": "any", "min_depth": 2, "max_depth": 3},
		"under d1":   {"path": "d1", "glob": "d*", "type": "any", "max_depth": 1},
		"truncated":  {"type": "any", "max_results": 3},
		"everything": {"type": "any"},
	} {
		plain := run(t, root, prim("find", params))
		with := map[string]any{"details": true}
		for k, v := range params {
			with[k] = v
		}
		detailed := run(t, root, prim("find", with))
		if plain.Status != StatusOK || detailed.Status != StatusOK {
			t.Fatalf("%s: status %s %s, %s %s", name, plain.Status, plain.Reason, detailed.Status, detailed.Reason)
		}
		var paths []string
		for _, line := range splitNonEmpty(strings.TrimSuffix(string(detailed.Output), "\n")) {
			f := strings.SplitN(line, "\t", 4)
			if len(f) != 4 {
				t.Fatalf("%s: detail line %q has %d fields, want 4", name, line, len(f))
			}
			paths = append(paths, f[3])
		}
		if got, want := strings.Join(paths, "\n"), strings.TrimSuffix(string(plain.Output), "\n"); got != want {
			t.Errorf("%s: details listed\n%s\nwant\n%s", name, got, want)
		}
		if plain.Truncated != detailed.Truncated || stopBy(plain) != stopBy(detailed) {
			t.Errorf("%s: truncated %v stop %q, with details %v %q", name, plain.Truncated, stopBy(plain), detailed.Truncated, stopBy(detailed))
		}
	}
}

func stopBy(rec Record) string {
	if rec.Stopped == nil {
		return ""
	}
	return rec.Stopped.By
}

func TestFindDepth(t *testing.T) {
	root := depthTree(t)
	for _, c := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"max 1", map[string]any{"type": "any", "max_depth": 1}, "d1\ntop.txt\n"},
		{"max 1 files", map[string]any{"max_depth": 1}, "top.txt\n"},
		{"max 2", map[string]any{"type": "any", "max_depth": 2}, "d1\nd1/d2\nd1/mid.txt\ntop.txt\n"},
		{"min 3", map[string]any{"type": "any", "min_depth": 3}, "d1/d2/d3\nd1/d2/d3/deepest.txt\nd1/d2/deep.log\n"},
		{"min 2 max 3", map[string]any{"type": "any", "min_depth": 2, "max_depth": 3}, "d1/d2\nd1/d2/d3\nd1/d2/deep.log\nd1/mid.txt\n"},
		{"min equals max", map[string]any{"min_depth": 3, "max_depth": 3}, "d1/d2/deep.log\n"},
		// Depth counts from the path named, not the root.
		{"under d1, max 1", map[string]any{"path": "d1", "type": "any", "max_depth": 1}, "d1/d2\nd1/mid.txt\n"},
		{"under d1/d2, min 2", map[string]any{"path": "d1/d2", "min_depth": 2}, "d1/d2/d3/deepest.txt\n"},
		// A path naming a file is depth 0, as in find(1).
		{"a file, min 1", map[string]any{"path": "top.txt", "min_depth": 1}, ""},
		{"a file, max 1", map[string]any{"path": "top.txt", "max_depth": 1}, "top.txt\n"},
		// The other filters apply within the depths.
		{"glob within depth", map[string]any{"glob": "*.txt", "max_depth": 2}, "d1/mid.txt\ntop.txt\n"},
		{"dirs within depth", map[string]any{"type": "dir", "min_depth": 2}, "d1/d2\nd1/d2/d3\n"},
	} {
		if got := findIn(t, root, c.params); got != c.want {
			t.Errorf("%s: find %v = %q, want %q", c.name, c.params, got, c.want)
		}
	}
}

// max_results counts only entries within the depths: an entry past
// max_depth is never reached, so it is not a further match.
func TestFindDepthAndMaxResults(t *testing.T) {
	root := depthTree(t)
	rec := run(t, root, prim("find", map[string]any{"max_depth": 1, "max_results": 1}))
	if got := mustOK(t, rec); got != "top.txt\n" || rec.Truncated || rec.Stopped != nil {
		t.Errorf("max_depth 1, max_results 1: output %q truncated %v stopped %+v", got, rec.Truncated, rec.Stopped)
	}
	rec = run(t, root, prim("find", map[string]any{"type": "any", "max_depth": 2, "max_results": 2, "details": true}))
	got := mustOK(t, rec)
	if got != "dir\t-\t2026-10-07T00:00:00Z\td1\ndir\t-\t2026-10-06T00:00:00Z\td1/d2\n" || !rec.Truncated || stopBy(rec) != "match_limit" {
		t.Errorf("max_depth 2, max_results 2: output %q truncated %v stopped %+v", got, rec.Truncated, rec.Stopped)
	}
}

// The walk is pruned at max_depth: a directory there is never read, so one
// that cannot be read fails the walk only when the walk goes into it.
func TestFindPrunesAtMaxDepth(t *testing.T) {
	root := depthTree(t)
	locked := filepath.Join(root, "d1/d2/d3")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o700); err != nil {
			t.Errorf("restoring %s for its removal: %v", locked, err)
		}
	})
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("a directory with no permissions is still readable here, as for root")
	}
	if got := findIn(t, root, map[string]any{"type": "any", "max_depth": 3}); got != "d1\nd1/d2\nd1/d2/d3\nd1/d2/deep.log\nd1/mid.txt\ntop.txt\n" {
		t.Errorf("max_depth 3: got %q", got)
	}
	if rec := run(t, root, prim("find", map[string]any{"type": "any"})); rec.Status != StatusFailed {
		t.Errorf("no max_depth: status %s, want the unreadable directory to fail the walk", rec.Status)
	}
}

func TestFindRefusesBadDepths(t *testing.T) {
	root := depthTree(t)
	for name, params := range map[string]map[string]any{
		"negative max":  {"max_depth": -1},
		"negative min":  {"min_depth": -1},
		"max too deep":  {"max_depth": maxFindDepth + 1},
		"min too deep":  {"min_depth": maxFindDepth + 1},
		"min above max": {"min_depth": 3, "max_depth": 2},
		"wrong type":    {"max_depth": "two"},
		"details type":  {"details": "yes"},
	} {
		st := prim("find", params)
		if err := CheckStage(st); err == nil {
			t.Errorf("%s: CheckStage(%v) = nil, want an error", name, params)
		}
		if err := ValidateStage(st); err == nil {
			t.Errorf("%s: ValidateStage(%v) = nil, want an error", name, params)
		}
		if rec := run(t, root, st); rec.Status != StatusUnavailable || len(rec.Stages) != 0 || len(rec.Output) != 0 {
			t.Errorf("%s: run %v: status %s, %d stages, output %q", name, params, rec.Status, len(rec.Stages), rec.Output)
		}
	}
	for name, params := range map[string]map[string]any{
		"min only":       {"min_depth": 5},
		"max at bound":   {"max_depth": maxFindDepth},
		"min equals max": {"min_depth": 2, "max_depth": 2},
		"templated":      {"min_depth": "{{lo}}", "max_depth": "{{hi}}", "details": "{{d}}"},
	} {
		if err := CheckStage(prim("find", params)); err != nil {
			t.Errorf("%s: CheckStage(%v) = %v, want nil", name, params, err)
		}
	}
}

// A package using a parameter its engine does not know fails to load, as an
// engine older than these parameters refuses a package using them.
func TestFindUnknownParameterFailsTheLoad(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"expert.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata: {id: a.b, version: 0.1.0, summary: A test expert}\nspec: {engineApi: '>=0.2.0 <0.3.0', analyst: a.md, tests: [a.t]}\n",
		"a.md":        "x",
		"tests.yaml":  "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Tests\ntests: [{id: a.t, version: 0.1.0, run: [{find: {path: x, details: true, mtime_format: rfc3339}}]}]\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (expert.Loader{CheckStage: CheckStage}).Load(dir); err == nil || !strings.Contains(err.Error(), "mtime_format") {
		t.Fatalf("Load with an unknown find parameter: err = %v, want it naming mtime_format", err)
	}
}

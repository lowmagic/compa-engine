package collect_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/store"
)

// TestStoreHiddenFromCollection opens a case store where the deployment
// host puts it, in the case folder that is also the artifact root, and
// tries to read it with every primitive and with allowlisted commands.
func TestStoreHiddenFromCollection(t *testing.T) {
	if store.DirName != collect.StoreDir {
		t.Fatalf("the store lives in %s but collection excludes %s", store.DirName, collect.StoreDir)
	}
	// By its real path, as the engine gives the runner an artifact root.
	caseFolder, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(caseFolder, "0010-must-gather"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(caseFolder, "0010-must-gather", "x.log"), []byte("2026-10-01T00:00:00Z ok\n"), 0o600)
	dir, _, err := store.Location(caseFolder, t.TempDir(), "00000001")
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dir, "00000001", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.PutBlob([]byte("STORE-MARKER")); err != nil {
		t.Fatal(err)
	}
	// What a read of the database would show: its header and the case ID
	// the store records.
	leaked := func(out []byte) bool {
		o := string(out)
		return strings.Contains(o, "SQLite format") || strings.Contains(o, "case_id") || strings.Contains(o, "case.db") || strings.Contains(o, "STORE-MARKER")
	}

	r := &collect.Runner{}
	run := func(stages ...expert.Stage) collect.Record {
		return r.Run(context.Background(), collect.Request{ArtifactRoot: caseFolder, What: collect.What{Pipeline: stages}})
	}
	p := func(name string, params map[string]any) expert.Stage {
		return expert.Stage{Primitive: name, Params: params}
	}
	db := ".compa/case.db"
	for _, st := range []expert.Stage{
		p("read", map[string]any{"path": db}),
		p("find", map[string]any{"path": ".compa"}),
		p("grep", map[string]any{"pattern": "case_id", "path": db}),
		p("jq", map[string]any{"filter": ".", "path": db}),
		p("yq", map[string]any{"filter": ".", "path": db}),
		p("logs.window", map[string]any{"path": db, "start": "2026-01-01T00:00:00Z", "end": "2027-01-01T00:00:00Z"}),
		p("logs.count", map[string]any{"path": db}),
		p("diff", map[string]any{"a": "0010-must-gather/x.log", "b": db}),
		p("archive.list", map[string]any{"path": db}),
	} {
		if rec := run(st); rec.Status == collect.StatusOK || leaked(rec.Output) {
			t.Errorf("%s: status %s, output %q", st.Primitive, rec.Status, rec.Output)
		}
	}
	// Walks from the root pass the store by, and aggregate sees only what
	// they found.
	for _, stages := range [][]expert.Stage{
		{p("find", map[string]any{"type": "any"})},
		{p("grep", map[string]any{"pattern": "."})},
		{p("find", map[string]any{"type": "any"}), p("aggregate", map[string]any{"op": "unique"})},
	} {
		rec := run(stages...)
		if rec.Status != collect.StatusOK || leaked(rec.Output) || strings.Contains(string(rec.Output), ".compa") {
			t.Errorf("%s: status %s %s, output %q", stages[0].Primitive, rec.Status, rec.Reason, rec.Output)
		}
	}

	sb, err := collect.DefaultSandbox()
	if err != nil {
		collect.SkipWithoutSandbox(t, "commands not checked: "+err.Error())
	}
	r.Sandbox, r.ScratchParent = sb, t.TempDir()
	for _, b := range []string{"cat", "grep", "ls"} {
		path, err := exec.LookPath(b)
		if err != nil {
			t.Fatal(err)
		}
		r.Policy.Allow(b, path)
	}
	abs, _ := filepath.Abs(caseFolder)
	for _, a := range [][]string{
		{"cat", filepath.Join(abs, db)},
		{"ls", "-a", filepath.Join(abs, ".compa")},
		{"grep", "-r", "-a", "case_id", abs},
	} {
		if rec := run(expert.Stage{Argv: a}); leaked(rec.Output) {
			t.Errorf("%v: status %s, output %q", a, rec.Status, rec.Output)
		} else if a[0] != "grep" && rec.Status == collect.StatusOK {
			t.Errorf("%v succeeded", a)
		}
	}
	if rec := run(expert.Stage{Argv: []string{"cat", filepath.Join(abs, "0010-must-gather", "x.log")}}); rec.Status != collect.StatusOK {
		t.Errorf("positive control: %s %s", rec.Status, rec.Reason)
	}
}

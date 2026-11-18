// Package enginetest builds engines over the repository's synthetic
// fixtures, for the tests of the engine's clients outside package engine,
// such as the case chat: the stub case source over testdata/cases, the
// fixture must-gather as each case's folder, the fixture experts and routing
// library, a home of the test's own, and no model unless the test
// configures one, such as the scripted fixture provider (ScriptedModel),
// which sends nothing anywhere. Its engines are built from the engine's
// public API; what a client's tests need of the engine's own types and
// stores, the events and records a turn makes, is here too, so no client
// imports internal/.
package enginetest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/stubcases"
)

// testdata is the repository's testdata directory, found from this file,
// so a test in any package of the module reaches it.
func testdata(tb testing.TB) string {
	tb.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("enginetest: cannot find its own source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}

// Engine is an engine over the fixtures, its cases folders and home in the
// test's temporary directory; each stub case's folder holds the fixture
// must-gather.
func Engine(tb testing.TB) *engine.Engine {
	tb.Helper()
	return EngineWith(tb, "")
}

// EngineWith is Engine with more configuration: config, YAML holding
// top-level keys other than experts and routingLibrary, such as reasoning to
// give the companion a scripted fixture model, or comments.
func EngineWith(tb testing.TB, config string) *engine.Engine {
	tb.Helper()
	td := testdata(tb)
	tmp, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	cases := filepath.Join(tmp, "cases")
	ids, err := filepath.Glob(filepath.Join(td, "cases", "*.yaml"))
	if err != nil || len(ids) == 0 {
		tb.Fatalf("enginetest: no stub cases in %s: %v", td, err)
	}
	for _, f := range ids {
		id := filepath.Base(f[:len(f)-len(".yaml")])
		if err := os.CopyFS(filepath.Join(cases, id), os.DirFS(filepath.Join(td, "artifacts", "must-gather"))); err != nil {
			tb.Fatal(err)
		}
	}
	cfg := "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {local: [" +
		filepath.Join(td, "experts") + "]}\nroutingLibrary: " + filepath.Join(td, "routing", "library.yaml") + "\n" + config
	e, err := engine.New(engine.Options{
		Cases:     stubcases.New(filepath.Join(td, "cases")),
		Artifacts: localdir.New(cases),
		Home:      filepath.Join(tmp, "home"),
		Config:    []byte(cfg),
	})
	if err != nil {
		tb.Fatal(err)
	}
	return e
}

// Session opens a stub case on a new fixture engine as a client finds it
// once its opening ends: its details, attachments, comments and counts
// read. The session closes when the test ends.
func Session(tb testing.TB, caseID string) *engine.Session {
	tb.Helper()
	ctx := context.Background()
	s, err := Engine(tb).StartSession(ctx, caseID, nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	if err := s.ReadComments(ctx); err != nil {
		tb.Fatal(err)
	}
	if err := s.ReadCounts(ctx); err != nil {
		tb.Fatal(err)
	}
	return s
}

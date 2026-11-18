package evidence_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/store"
)

// The shared tool catalogs' contract, as experts reach a catalog tool.

const testdata = "../../testdata"

var artifacts = filepath.Join(testdata, "artifacts/must-gather")

// catalogIndex discovers the v1alpha2 fixtures and, in a local root, a
// third expert, fixture.compute, using fixture.core in the same range as
// fixture.storage.
func catalogIndex(t *testing.T) *expert.Index {
	t.Helper()
	local := filepath.Join(t.TempDir(), "compute")
	files := map[string]string{
		"expert.yaml": `apiVersion: compa.lowmag.cc/expert/v1alpha3
kind: Expert
metadata: {id: fixture.compute, version: 0.1.0, summary: A test expert}
spec:
  engineApi: '>=0.2.0 <0.3.0'
  analyst: analyst.md
  catalogs:
    - {id: fixture.core, version: '>=0.1.0 <0.2.0', tools: [node_ready]}
`,
		"analyst.md": "fixture.core.node_ready: a node that is not Ready runs no workloads.\n",
	}
	for name, content := range files {
		if err := os.MkdirAll(local, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(local, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx := expert.Loader{CheckStage: collect.CheckStage}.Discover([]expert.Root{
		{Path: filepath.Join(testdata, "catalogs"), Source: expert.Bundled},
		{Path: filepath.Dir(local), Source: expert.Local},
	})
	if len(idx.Errors) > 0 || len(idx.Warnings) > 0 {
		t.Fatalf("errors %v, warnings %v", idx.Errors, idx.Warnings)
	}
	return idx
}

func resolve(t *testing.T, idx *expert.Index, id string) expert.Resolved {
	t.Helper()
	r, err := idx.Resolve(id, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestCatalogToolIsOneEvidenceItem runs one catalog tool with the same
// parameters for two experts and checks the results are one piece of
// evidence, recorded under the catalog rather than either expert.
func TestCatalogToolIsOneEvidenceItem(t *testing.T) {
	idx := catalogIndex(t)
	s, err := store.Open(filepath.Join(t.TempDir(), store.DirName), "0420", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ev := evidence.New(s)
	runner := &collect.Runner{}

	var ids []string
	for _, id := range []string{"fixture.storage", "fixture.compute"} {
		r := resolve(t, idx, id)
		pkg, ok := r.PackageOf("fixture.core.node_ready")
		if !ok {
			t.Fatalf("%s does not offer fixture.core.node_ready", id)
		}
		rec := runner.RunTest(context.Background(), artifacts, pkg, "fixture.core.node_ready", map[string]any{"node": "worker-1"})
		if rec.Status != collect.StatusOK {
			t.Fatalf("%s: %s %s", id, rec.Status, rec.Reason)
		}
		if rec.What.Catalog != "fixture.core@0.1.1" || rec.What.Expert != "" || rec.What.TestVersion != "0.1.1" {
			t.Errorf("%s: record names %+v", id, rec.What)
		}
		if err := ev.Put(rec); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	if ids[0] != ids[1] {
		t.Errorf("evidence IDs %v differ", ids)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM evidence`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d evidence items (%v), want 1", n, err)
	}
	it, err := ev.Get(ids[0])
	if err != nil || it.Ref.Provenance.Source != "fixture.core.node_ready@0.1.1" {
		t.Errorf("provenance %+v, %v", it.Ref.Provenance, err)
	}
}

// TestCatalogToolsOffered checks each expert offers its own tests and the
// tools it lists, from the version it resolved, and nothing else from the
// catalog.
func TestCatalogToolsOffered(t *testing.T) {
	idx := catalogIndex(t)
	storage, network := resolve(t, idx, "fixture.storage"), resolve(t, idx, "fixture.network")
	for _, c := range []struct {
		r            expert.Resolved
		test, source string
	}{
		{storage, "storage.node_files", "fixture.storage@0.1.0"},
		{storage, "fixture.core.node_ready", "fixture.core@0.1.1"},
		{storage, "fixture.core.pods", ""},
		{network, "fixture.core.pods", "fixture.core@0.2.0"},
		{network, "fixture.core.degraded_operators", ""},
	} {
		pkg, ok := c.r.PackageOf(c.test)
		if c.source == "" {
			if ok {
				t.Errorf("%s offers %s, which it does not list", c.r.ID(), c.test)
			}
			continue
		}
		if !ok || pkg.ID()+"@"+pkg.Version() != c.source {
			t.Errorf("%s: %s from %v, want %s", c.r.ID(), c.test, pkg, c.source)
		}
	}
	var offered []string
	for _, tt := range storage.Offered() {
		offered = append(offered, tt.ID+"@"+tt.Version)
	}
	if got := fmt.Sprint(offered); got != "[storage.node_files@0.1.0 fixture.core.node_ready@0.1.1 fixture.core.degraded_operators@0.1.0]" {
		t.Errorf("offered %s", got)
	}
}

// TestCatalogToolUnallowlisted checks a catalog tool whose binary is not
// allowlisted reports unavailable and runs nothing.
func TestCatalogToolUnallowlisted(t *testing.T) {
	network := resolve(t, catalogIndex(t), "fixture.network")
	pkg, _ := network.PackageOf("fixture.core.pods")
	rec := (&collect.Runner{}).RunTest(context.Background(), artifacts, pkg, "fixture.core.pods", map[string]any{"namespace": "openshift-etcd"})
	if rec.Status != collect.StatusUnavailable || len(rec.Stages) != 0 || rec.What.Catalog != "fixture.core@0.2.0" {
		t.Errorf("got %s (%s), %d stages, %+v", rec.Status, rec.Reason, len(rec.Stages), rec.What)
	}
}

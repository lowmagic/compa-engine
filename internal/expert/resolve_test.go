package expert

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// copyDir copies a fixture package to dst, so a test can change what is
// installed without touching testdata.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		return copyFile(p, filepath.Join(dst, rel))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func install(t *testing.T, root string, fixtures ...string) {
	t.Helper()
	for _, f := range fixtures {
		copyDir(t, filepath.Join(catalogsRoot, f), filepath.Join(root, f))
	}
}

func toolIDs(tests []Test) string {
	ids := make([]string, len(tests))
	for i, t := range tests {
		ids[i] = t.ID + "@" + t.Version
	}
	return strings.Join(ids, " ")
}

// TestResolveCatalogVersionsPerExpert checks that two experts in one
// investigation each resolve the catalog within their own range, and that
// their pins keep those versions apart.
func TestResolveCatalogVersionsPerExpert(t *testing.T) {
	idx := Loader{}.Discover([]Root{{Path: catalogsRoot}})
	if len(idx.Errors) > 0 || len(idx.Warnings) > 0 {
		t.Fatalf("errors %v, warnings %v", idx.Errors, idx.Warnings)
	}
	for _, c := range []struct{ expert, version, tools string }{
		{"fixture.storage", "0.1.1", "fixture.core.node_ready@0.1.1 fixture.core.degraded_operators@0.1.0"},
		{"fixture.network", "0.2.0", "fixture.core.node_ready@0.2.0 fixture.core.pods@0.1.0"},
	} {
		r, err := idx.Resolve(c.expert, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Catalogs) != 1 || len(r.Warnings) != 0 {
			t.Fatalf("%s: catalogs %+v, warnings %v", c.expert, r.Catalogs, r.Warnings)
		}
		rc := r.Catalogs[0]
		if rc.Package.Version() != c.version || toolIDs(rc.Tools) != c.tools || rc.Unresolved != "" {
			t.Errorf("%s: resolved %s with %s (%q), want %s with %s", c.expert, rc.Package.Version(), toolIDs(rc.Tools), rc.Unresolved, c.version, c.tools)
		}

		pin := r.Pin()
		cp := pin.Catalogs[0]
		if cp.ID != "fixture.core" || cp.Range != rc.Ref.Version || cp.Version != c.version || cp.Digest != rc.Package.Digest || cp.Unresolved != "" {
			t.Errorf("%s: catalog pin %+v", c.expert, cp)
		}
		for _, tool := range rc.Tools {
			if cp.Tools[tool.ID] != tool.Version {
				t.Errorf("%s: pin records %s at %q, want %s", c.expert, tool.ID, cp.Tools[tool.ID], tool.Version)
			}
		}

		pinned, warnings, err := idx.Pinned(pin, nil)
		if err != nil || len(warnings) != 0 {
			t.Fatalf("%s: pinned: %v %v", c.expert, err, warnings)
		}
		if got := pinned.Catalogs[0]; got.Package.Version() != c.version || toolIDs(got.Tools) != c.tools {
			t.Errorf("%s: pinned %s with %s", c.expert, got.Package.Version(), toolIDs(got.Tools))
		}
	}
}

// TestCatalogPinSurvivesUpgrade upgrades the installed catalog within the
// expert's range, then deletes it, and checks the pin keeps the pinned
// version from its snapshot until an explicit re-pin.
func TestCatalogPinSurvivesUpgrade(t *testing.T) {
	root := t.TempDir()
	install(t, root, "storage", "core-0.1.0")
	store := &Store{Dir: t.TempDir()}

	r, err := Loader{}.Discover([]Root{{Path: root}}).Resolve("fixture.storage", "")
	if err != nil {
		t.Fatal(err)
	}
	pin := r.Pin()
	if pin.Catalogs[0].Version != "0.1.0" {
		t.Fatalf("pinned catalog %+v", pin.Catalogs[0])
	}
	if err := store.SaveResolved(r); err != nil {
		t.Fatal(err)
	}
	const fallback = "catalog fixture.core 0.1.0: installed copy is missing or changed; using the pinned snapshot"

	// Upgrade in place, from 0.1.0 to 0.1.1.
	if err := os.RemoveAll(filepath.Join(root, "core-0.1.0")); err != nil {
		t.Fatal(err)
	}
	install(t, root, "core-0.1.1")
	idx := Loader{}.Discover([]Root{{Path: root}})
	pinned, warnings, err := idx.Pinned(pin, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != fallback {
		t.Errorf("warnings = %q", warnings)
	}
	rc := pinned.Catalogs[0]
	if rc.Package.Version() != "0.1.0" || rc.Package.Digest != pin.Catalogs[0].Digest || toolIDs(rc.Tools) != "fixture.core.node_ready@0.1.0 fixture.core.degraded_operators@0.1.0" {
		t.Errorf("pinned %s %s with %s, want the pinned 0.1.0", rc.Package.Version(), rc.Package.Digest, toolIDs(rc.Tools))
	}
	if pinned.Source == Snapshot {
		t.Error("the expert's installed copy is unchanged and must be used")
	}

	// An explicit re-pin takes the upgrade.
	again, err := idx.Resolve("fixture.storage", "")
	if err != nil {
		t.Fatal(err)
	}
	if v := again.Pin().Catalogs[0].Version; v != "0.1.1" {
		t.Errorf("re-pin took %s, want 0.1.1", v)
	}

	// Deleted altogether, the catalog still comes from its snapshot; and
	// so does the expert, which discovery now refuses for want of its
	// catalog, so the pinned conversation goes on from both snapshots.
	if err := os.RemoveAll(filepath.Join(root, "core-0.1.1")); err != nil {
		t.Fatal(err)
	}
	idx = Loader{}.Discover([]Root{{Path: root}})
	pinned, warnings, err = idx.Pinned(pin, store)
	expertFallback := "expert fixture.storage 0.1.0: installed copy is missing or changed; using the pinned snapshot"
	if err != nil || len(warnings) != 2 || warnings[0] != expertFallback || warnings[1] != fallback || pinned.Catalogs[0].Package.Version() != "0.1.0" {
		t.Errorf("after deletion: %v, warnings %q", err, warnings)
	}
	if _, _, err := idx.Pinned(pin, nil); err == nil {
		t.Error("without a store, a missing catalog must be an error")
	}

	// A tampered snapshot is refused.
	hex := strings.TrimPrefix(pin.Catalogs[0].Digest, "sha256:")
	if err := os.WriteFile(filepath.Join(store.Dir, hex, "tests.yaml"), []byte(strings.Replace(mustRead(t, filepath.Join(catalogsRoot, "core-0.1.0", "tests.yaml")), "Ready condition", "Ready status", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := idx.Pinned(pin, store); err == nil || !strings.Contains(err.Error(), "catalog fixture.core 0.1.0: installed copy is missing or changed, and the snapshot failed") {
		t.Errorf("tampered snapshot: %v", err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestResolveUnmetCatalogs checks that a dependency on an installed
// catalog that is unmet — no version in range, or a listed tool the
// resolved version lacks — leaves the expert usable without the tools it
// cannot provide, warns at discovery and resolution, and is recorded as
// unresolved in the pin and kept so when the pin is restored. A catalog
// that is not there at all refuses the expert
// (TestDiscoverRefusesExpertWithoutItsCatalog).
func TestResolveUnmetCatalogs(t *testing.T) {
	storageWithAbsent := func(root string) {
		dir := filepath.Join(root, "storage")
		m := strings.Replace(mustRead(t, filepath.Join(dir, "expert.yaml")), "tools: [node_ready, degraded_operators]", "tools: [node_ready, degraded_operators, absent]", 1)
		if err := os.WriteFile(filepath.Join(dir, "expert.yaml"), []byte(m), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "analyst.md"), []byte(mustRead(t, filepath.Join(dir, "analyst.md"))+"\nfixture.core.absent is not used.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name       string
		setup      func(root string)
		unresolved string
		notOffered string
		version    string
		tools      string
	}{
		{"nothing in range", func(root string) { install(t, root, "core-0.2.0") }, "catalog fixture.core has no installed version in >=0.1.0 <0.2.0 (installed: 0.2.0)",
			"fixture.core.node_ready, fixture.core.degraded_operators", "", ""},
		{"tool absent", func(root string) { install(t, root, "core-0.1.0"); storageWithAbsent(root) },
			"catalog fixture.core 0.1.0, resolved from >=0.1.0 <0.2.0, does not define fixture.core.absent",
			"fixture.core.absent", "0.1.0", "fixture.core.node_ready@0.1.0 fixture.core.degraded_operators@0.1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			install(t, root, "storage")
			c.setup(root)
			idx := Loader{}.Discover([]Root{{Path: root}})
			if len(idx.Errors) > 0 {
				t.Fatal(idx.Errors)
			}
			want := "expert fixture.storage 0.1.0: " + c.unresolved + "; not offered: " + c.notOffered
			if len(idx.Warnings) != 1 || idx.Warnings[0] != want {
				t.Errorf("discovery warnings %q\nwant %q", idx.Warnings, want)
			}

			r, err := idx.Resolve("fixture.storage", "")
			if err != nil {
				t.Fatalf("an unmet dependency must not stop the expert: %v", err)
			}
			if len(r.Warnings) != 1 || r.Warnings[0] != want {
				t.Errorf("resolution warnings %q", r.Warnings)
			}
			if _, ok := r.Test("storage.node_files"); !ok {
				t.Error("the expert's own test must still be available")
			}
			rc := r.Catalogs[0]
			if rc.Unresolved != c.unresolved || toolIDs(rc.Tools) != c.tools || (rc.Package == nil) != (c.version == "") {
				t.Errorf("resolved catalog %+v", rc)
			}

			pin := r.Pin()
			cp := pin.Catalogs[0]
			if cp.Unresolved != c.unresolved || cp.Version != c.version || cp.Range != ">=0.1.0 <0.2.0" || len(cp.Tools) != strings.Count(c.tools, "@") {
				t.Errorf("catalog pin %+v", cp)
			}
			store := &Store{Dir: t.TempDir()}
			if err := store.SaveResolved(r); err != nil {
				t.Fatal(err)
			}

			pinned, warnings, err := idx.Pinned(pin, store)
			if err != nil {
				t.Fatal(err)
			}
			if len(warnings) != 1 || warnings[0] != want || len(pinned.Warnings) != 1 {
				t.Errorf("pinned warnings %q, on the expert %q", warnings, pinned.Warnings)
			}
			if got := pinned.Catalogs[0]; got.Unresolved != c.unresolved || toolIDs(got.Tools) != c.tools {
				t.Errorf("pinned catalog %+v", got)
			}
		})
	}
}

// TestDiscoverRefusesExpertWithoutItsCatalog: an expert naming a catalog
// that is not installed, or whose ID is an expert's, is refused when the
// roots are discovered, as a package that fails to load is, so a set
// composed without a catalog is found then and not when a case needs the
// tools; the packages beside it are used, and with the catalog installed
// the expert is too.
func TestDiscoverRefusesExpertWithoutItsCatalog(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(root string)
		why   string
	}{
		{"catalog missing", func(string) {}, "catalog fixture.core (>=0.1.0 <0.2.0) is not installed"},
		{"an expert's ID", func(root string) {
			writePackage(t, root, "core", with(baseFiles(), map[string]string{"expert.yaml": strings.Replace(baseManifest, "id: test.expert", "id: fixture.core", 1)}))
		}, "fixture.core (>=0.1.0 <0.2.0) is an expert, not a catalog"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			install(t, root, "storage")
			writePackage(t, root, "beside", baseFiles())
			c.setup(root)
			idx := Loader{}.Discover([]Root{{Path: root}})
			var le *LoadError
			if len(idx.Errors) != 1 || !errors.As(idx.Errors[0], &le) || le.Dir != filepath.Join(root, "storage") ||
				!strings.Contains(le.Error(), "expert fixture.storage 0.1.0 is not used: "+c.why) {
				t.Fatalf("discovery errors %v; want the expert refused for %q", idx.Errors, c.why)
			}
			if _, err := idx.Resolve("fixture.storage", ""); err == nil {
				t.Error("the expert without its catalog resolves")
			}
			if _, err := idx.Resolve("test.expert", ""); err != nil {
				t.Errorf("the package beside it: %v", err)
			}
			for _, w := range idx.Warnings {
				if strings.Contains(w, "fixture.storage") {
					t.Errorf("the refused expert is also warned about: %s", w)
				}
			}
		})
	}
	root := t.TempDir()
	install(t, root, "storage", "core-0.1.0")
	idx := Loader{}.Discover([]Root{{Path: root}})
	if len(idx.Errors) != 0 {
		t.Fatal(idx.Errors)
	}
	if r, err := idx.Resolve("fixture.storage", ""); err != nil || len(r.Warnings) != 0 {
		t.Errorf("with its catalog installed: %v, warnings %q", err, r.Warnings)
	}
}

// TestPinWithoutCatalogs checks the pin of an expert that uses no catalog
// has no catalog field, its engine_api the version it was made under.
func TestPinWithoutCatalogs(t *testing.T) {
	r, err := Loader{}.Discover([]Root{{Path: fixtureRoot}}).Resolve("openshift.platform", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r.Pin())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, " ") != "digest engine_api id tests version" || fields["engine_api"] != EngineAPIVersion {
		t.Errorf("pin = %s", b)
	}
}

// TestResolveEntryBelowHighest checks that an expert version other than the
// highest resolves its catalogs within its own range.
func TestResolveEntryBelowHighest(t *testing.T) {
	root := t.TempDir()
	install(t, root, "core-0.1.1", "core-0.2.0", "storage")
	newer := filepath.Join(root, "storage-0.2.0")
	copyDir(t, filepath.Join(catalogsRoot, "storage"), newer)
	m := strings.Replace(mustRead(t, filepath.Join(newer, "expert.yaml")), "version: 0.1.0,", "version: 0.2.0,", 1)
	if err := os.WriteFile(filepath.Join(newer, "expert.yaml"), []byte(strings.Replace(m, "'>=0.1.0 <0.2.0'", "'>=0.2.0 <0.3.0'", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := Loader{}.Discover([]Root{{Path: root}})
	if len(idx.Errors) > 0 {
		t.Fatal(idx.Errors)
	}
	for version, want := range map[string]string{"0.1.0": "0.1.1", "0.2.0": "0.2.0"} {
		e, ok := idx.Describe("fixture.storage", version)
		if !ok {
			t.Fatalf("fixture.storage %s not discovered", version)
		}
		r := idx.ResolveEntry(e)
		if len(r.Catalogs) != 1 || r.Catalogs[0].Package == nil || r.Catalogs[0].Package.Version() != want {
			t.Errorf("fixture.storage %s resolved %+v, want fixture.core %s", version, r.Catalogs, want)
		}
	}
}

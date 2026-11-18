package expert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func versioned(version string) map[string]string {
	return with(baseFiles(), map[string]string{"expert.yaml": strings.Replace(baseManifest, "version: 0.1.0,", "version: "+version+",", 1)})
}

func TestDiscoverFixtureExperts(t *testing.T) {
	c := Loader{}.Discover([]Root{{Path: fixtureRoot, Source: Bundled}})
	if len(c.Errors) > 0 {
		t.Fatalf("errors: %v", c.Errors)
	}
	var ids []string
	for _, d := range c.List() {
		ids = append(ids, d.ID+"@"+d.Version)
	}
	if got := strings.Join(ids, " "); got != "openshift.networking@0.1.0 openshift.platform@0.1.0" {
		t.Fatalf("List() = %s", got)
	}
}

func TestDiscoverReportsBadPackagesAndKeepsGoodOnes(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "good", baseFiles())
	writePackage(t, root, "bad", with(baseFiles(), map[string]string{"analyst.md": ""}))
	writePackage(t, root, "not-a-package", map[string]string{"notes.txt": "x"})
	c := Loader{}.Discover([]Root{{Path: root}, {Path: filepath.Join(root, "missing"), Source: Local}})
	if len(c.Errors) != 1 || !strings.Contains(c.Errors[0].Error(), "analyst.md") {
		t.Errorf("errors = %v", c.Errors)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "does not exist") {
		t.Errorf("warnings = %v", c.Warnings)
	}
	if _, err := c.Resolve("test.expert", ""); err != nil {
		t.Error(err)
	}
}

func TestDiscoverDuplicates(t *testing.T) {
	bundled, local := t.TempDir(), t.TempDir()
	writePackage(t, bundled, "a", baseFiles())
	writePackage(t, bundled, "b", baseFiles())
	c := Loader{}.Discover([]Root{{Path: bundled, Source: Bundled}})
	if len(c.Errors) != 1 || !strings.Contains(c.Errors[0].Error(), "defined twice") {
		t.Fatalf("errors = %v", c.Errors)
	}
	if _, err := c.Resolve("test.expert", ""); err == nil {
		t.Fatal("a duplicated expert must not resolve")
	}

	bundled = t.TempDir()
	writePackage(t, bundled, "a", baseFiles())
	writePackage(t, local, "a", with(baseFiles(), map[string]string{"analyst.md": "local guidance"}))
	for _, roots := range [][]Root{
		{{Path: bundled, Source: Bundled}, {Path: local, Source: Local}},
		{{Path: local, Source: Local}, {Path: bundled, Source: Bundled}},
	} {
		c = Loader{}.Discover(roots)
		if len(c.Errors) > 0 {
			t.Fatal(c.Errors)
		}
		if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "overrides the bundled copy") {
			t.Errorf("warnings = %v", c.Warnings)
		}
		e, err := c.Resolve("test.expert", "")
		if err != nil {
			t.Fatal(err)
		}
		if e.Source != Local || e.Analyst != "local guidance" {
			t.Errorf("resolved %s copy, want local", e.Source)
		}
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"0.1.0", "0.1.4", "0.2.0", "0.3.0-rc.1"} {
		writePackage(t, root, "v"+v, versioned(v))
	}
	c := Loader{}.Discover([]Root{{Path: root}})
	if len(c.Errors) > 0 {
		t.Fatal(c.Errors)
	}
	for constraint, want := range map[string]string{
		"":                  "0.2.0",
		">=0.1.0 <0.2.0":    "0.1.4",
		"0.1.0":             "0.1.0",
		">=0.3.0-rc.0":      "0.3.0-rc.1",
		">=0.2.0 || <0.1.1": "0.2.0",
	} {
		e, err := c.Resolve("test.expert", constraint)
		if err != nil {
			t.Errorf("%q: %v", constraint, err)
		} else if e.Version() != want {
			t.Errorf("%q resolved %s, want %s", constraint, e.Version(), want)
		}
	}
	if _, err := c.Resolve("test.expert", ">=1.0.0"); err == nil {
		t.Error("want error when nothing satisfies the constraint")
	}
	if _, err := c.Resolve("no.such", ""); err == nil {
		t.Error("want error for an unknown expert")
	}
}

// TestPinnedSurvivesUpgrade pins an expert, replaces the installed copy with
// a new version, and checks the investigation still gets the pinned one
// until it is explicitly upgraded.
func TestPinnedSurvivesUpgrade(t *testing.T) {
	root := t.TempDir()
	dir := writePackage(t, root, "expert", baseFiles())
	store := &Store{Dir: t.TempDir()}

	c := Loader{}.Discover([]Root{{Path: root}})
	e, err := c.Resolve("test.expert", "")
	if err != nil {
		t.Fatal(err)
	}
	pin := e.Pin()
	if err := store.Save(e.Package); err != nil {
		t.Fatal(err)
	}
	if pin.Tests["test.one"] != "0.1.0" || pin.EngineAPI != EngineAPIVersion {
		t.Errorf("pin = %+v", pin)
	}

	pkg, warnings, err := c.Pinned(pin, store)
	if err != nil || len(warnings) != 0 || pkg.Dir != dir {
		t.Fatalf("before upgrade: pkg %v, warnings %q, err %v", pkg.Package, warnings, err)
	}

	// Upgrade the installed copy in place.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	writePackage(t, root, "expert", with(versioned("0.2.0"), map[string]string{"analyst.md": "new guidance"}))
	c = Loader{}.Discover([]Root{{Path: root}})

	pkg, warnings, err = c.Pinned(pin, store)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Version() != "0.1.0" || pkg.Analyst != "guidance" || pkg.Digest != pin.Digest {
		t.Errorf("got %s %q, want the pinned 0.1.0", pkg.Version(), pkg.Analyst)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "using the pinned snapshot") {
		t.Errorf("warnings = %q", warnings)
	}

	// Explicit upgrade: resolve again and pin the result.
	e, err = c.Resolve("test.expert", "")
	if err != nil || e.Version() != "0.2.0" {
		t.Fatalf("upgrade resolved %v, %v", e.Package, err)
	}

	if _, _, err := c.Pinned(pin, nil); err == nil {
		t.Error("without a store, a changed installed copy must be an error")
	}
}

func TestStoreRejectsChangedPackage(t *testing.T) {
	dir := writePackage(t, t.TempDir(), "p", baseFiles())
	pkg, err := Loader{}.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The installed copy changes after it was loaded and before it is saved.
	if err := os.WriteFile(filepath.Join(dir, "analyst.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir()}
	if err := store.Save(pkg); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("got %v", err)
	}
	if _, err := store.Load(pkg.pin()); err == nil {
		t.Fatal("a rejected snapshot must not be stored")
	}
}

func TestStoreRejectsMalformedDigest(t *testing.T) {
	if _, err := (Store{Dir: t.TempDir()}).Load(Pin{Digest: "sha256:../../etc"}); err == nil {
		t.Fatal("want error")
	}
}

// TestDiscoverLoadedAndIndexWarnings checks that discovery keeps every
// package that loaded, usable or not, and separates warnings about roots
// and precedence from warnings about a package.
func TestDiscoverLoadedAndIndexWarnings(t *testing.T) {
	root, bundled, local := t.TempDir(), t.TempDir(), t.TempDir()
	writePackage(t, root, "a", baseFiles())
	writePackage(t, root, "b", baseFiles())
	writePackage(t, root, "bad", with(baseFiles(), map[string]string{"analyst.md": ""}))
	writePackage(t, bundled, "c", versioned("0.2.0"))
	writePackage(t, local, "c", versioned("0.2.0"))
	unexplained := with(v2Files(), map[string]string{"analyst.md": "nothing named"})
	unexplained["expert.yaml"] = strings.Replace(unexplained["expert.yaml"], "id: x.storage", "id: x.unexplained", 1)
	writePackage(t, local, "u", unexplained)
	missing := filepath.Join(root, "missing")

	idx := Loader{}.Discover([]Root{{Path: root, Source: Local}, {Path: bundled, Source: Bundled}, {Path: local, Source: Local}, {Path: missing, Source: Local}})
	var loaded []string
	for _, e := range idx.Loaded {
		loaded = append(loaded, filepath.Base(e.Dir)+"="+e.ID()+"@"+e.Version()+"/"+e.Source.String())
	}
	if got, want := strings.Join(loaded, " "), "a=test.expert@0.1.0/local b=test.expert@0.1.0/local c=test.expert@0.2.0/bundled c=test.expert@0.2.0/local u=x.unexplained@0.1.0/local"; got != want {
		t.Errorf("Loaded = %s, want %s", got, want)
	}
	if _, ok := idx.Describe("test.expert", "0.1.0"); ok {
		t.Error("a duplicated package is resolvable")
	}
	if len(idx.IndexWarnings) != 2 || !strings.Contains(idx.IndexWarnings[0], "overrides the bundled copy") || !strings.Contains(idx.IndexWarnings[1], missing+" (local) does not exist") {
		t.Errorf("IndexWarnings = %v", idx.IndexWarnings)
	}
	for _, w := range idx.Warnings {
		if strings.Contains(w, "x.unexplained") {
			return
		}
	}
	t.Errorf("the package's own warning is not in Warnings: %v", idx.Warnings)
}

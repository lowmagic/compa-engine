package engine_test

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/cli"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/stubcases"
)

// bundle is a deployment's bundle built from the fixture experts and routing
// library.
func bundle(t *testing.T) fstest.MapFS {
	t.Helper()
	b := fstest.MapFS{}
	err := filepath.WalkDir("../testdata/experts", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel("../testdata/experts", p)
		b["experts/"+filepath.ToSlash(rel)] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile("../testdata/routing/library.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b["routing/library.yaml"] = &fstest.MapFile{Data: lib}
	return b
}

func run(t *testing.T, o engine.Options, args ...string) string {
	t.Helper()
	var out, errs bytes.Buffer
	if code := cli.Run(context.Background(), args, o, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("compa %v: %d\n%s%s", args, code, out.String(), errs.String())
	}
	return out.String()
}

// TestBundledExperts: a deployment's bundle is written under the home
// directory once, named by its content; it is the bundled expert root and
// the routing library, so opening a case suggests the expert its routing key names,
// journaled once however often the case is reopened. A changed bundle is
// written beside the old one.
func TestBundledExperts(t *testing.T) {
	home := t.TempDir()
	o := engine.Options{Cases: stubcases.New("../testdata/cases"), Artifacts: localdir.New(t.TempDir()), Home: home, Bundle: bundle(t)}
	out := run(t, o, "case", "open", "00000001")
	if !strings.Contains(out, "experts: openshift.networking 0.1.0, openshift.platform 0.1.0 installed; suggested openshift.networking 0.1.0 (key openshift-networking)") {
		t.Errorf("case open:\n%s", out)
	}
	dirs, _ := filepath.Glob(filepath.Join(home, ".compa", "bundle", "*"))
	if len(dirs) != 1 {
		t.Fatalf("bundle written to %v", dirs)
	}
	run(t, o, "case", "open", "00000001")
	if again, _ := filepath.Glob(filepath.Join(home, ".compa", "bundle", "*")); len(again) != 1 {
		t.Errorf("reopening wrote the bundle again: %v", again)
	}

	changed := bundle(t)
	changed["experts/README.md"] = &fstest.MapFile{Data: []byte("changed\n")}
	o.Bundle = changed
	run(t, o, "case", "open", "00000001")
	if dirs, _ := filepath.Glob(filepath.Join(home, ".compa", "bundle", "*")); len(dirs) != 2 {
		t.Errorf("a changed bundle: %v", dirs)
	}
}

// TestConfiguredExpertsOverrideTheBundle: configuration naming an expert
// root and a routing library is used instead of the bundle.
func TestConfiguredExpertsOverrideTheBundle(t *testing.T) {
	root, _ := filepath.Abs("../testdata/catalogs")
	cfg := "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {bundled: " + root + "}\n"
	o := engine.Options{Cases: stubcases.New("../testdata/cases"), Artifacts: localdir.New(t.TempDir()), Home: t.TempDir(), Bundle: bundle(t), Config: []byte(cfg)}
	out := run(t, o, "case", "open", "00000001")
	// The bundle's experts are not installed; the bundle's routing
	// library still is the library, and says it names them.
	if !strings.Contains(out, "experts: fixture.network 0.1.0, fixture.storage 0.1.0 installed;") || !strings.Contains(out, "names 2 experts not installed, ignored: openshift.networking, openshift.platform") {
		t.Errorf("case open:\n%s", out)
	}
}

// compa version says what the binary was built from, or that it carries no
// commit, as a test binary may not (in live use an older compa than the
// one built ran, and nothing said so).
func TestVersionCommand(t *testing.T) {
	o := engine.Options{Cases: stubcases.New("../testdata/cases"), Artifacts: localdir.New(t.TempDir()), Home: t.TempDir()}
	if out := run(t, o, "version"); !strings.HasPrefix(out, "compa") || strings.Count(out, "\n") != 1 {
		t.Errorf("version: %q", out)
	}
}

// partialBundle is a bundle of the fixture packages named, each a
// directory under ../testdata, with the fixture routing library, which
// names openshift.platform and openshift.networking.
func partialBundle(t *testing.T, packages ...string) fstest.MapFS {
	t.Helper()
	b := fstest.MapFS{}
	for _, pkg := range packages {
		src := filepath.Join("../testdata", pkg)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(p)
			rel, _ := filepath.Rel(src, p)
			b["experts/"+filepath.Base(pkg)+"/"+filepath.ToSlash(rel)] = &fstest.MapFile{Data: data}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	lib, err := os.ReadFile("../testdata/routing/library.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b["routing/library.yaml"] = &fstest.MapFile{Data: lib}
	return b
}

// TestPartialBundle: a deployment may bundle only some of the experts.
// The routing library's entries for experts that are not installed are
// ignored and said, the installed experts are suggested, and a case opens.
// An expert with its catalog works; one bundled without its catalog is
// refused when the bundle loads, said as a package that could not be
// loaded, the packages beside it still installed.
func TestPartialBundle(t *testing.T) {
	open := func(b fstest.MapFS, id string) string {
		o := engine.Options{Cases: stubcases.New("../testdata/cases"), Artifacts: localdir.New(t.TempDir()), Home: t.TempDir(), Bundle: b}
		return run(t, o, "case", "open", id)
	}
	// Case 00000002's key, openshift, has openshift.platform as its primary.
	out := open(partialBundle(t, "experts/networking"), "00000002")
	for _, want := range []string{
		"experts: openshift.networking 0.1.0 installed; suggested openshift.networking 0.1.0 (key openshift, secondary)",
		"the routing library fixture-1 names 1 expert not installed, ignored: openshift.platform",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a bundle without the key's primary expert lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "could not be loaded") {
		t.Errorf("a partial bundle reports a load error:\n%s", out)
	}

	out = open(partialBundle(t, "experts/networking", "catalogs/storage", "catalogs/core-0.1.1"), "00000001")
	if !strings.Contains(out, "experts: fixture.storage 0.1.0, openshift.networking 0.1.0 installed;") || strings.Contains(out, "could not be loaded") {
		t.Errorf("an expert bundled with its catalog:\n%s", out)
	}

	out = open(partialBundle(t, "experts/networking", "catalogs/storage"), "00000001")
	for _, want := range []string{
		"experts: openshift.networking 0.1.0 installed;",
		"1 package could not be loaded:",
		"expert fixture.storage 0.1.0 is not used: catalog fixture.core (>=0.1.0 <0.2.0) is not installed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("an expert bundled without its catalog lacks %q:\n%s", want, out)
		}
	}
}

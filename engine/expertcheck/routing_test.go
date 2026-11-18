package expertcheck_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine/expertcheck"
)

const keysDoc = `apiVersion: compa.lowmag.cc/routing/v1alpha1
kind: RoutingLibrary
revision: keys-1
keys:
  - id: alpha
    name: Alpha
    aliases: [alpha-one]
    tags: [alpha_tag]
    provenance: [{source: synthetic, ref: alpha.md}]
  - id: beta
    name: Beta
`

// expertYAML is a synthetic expert manifest serving keys, as YAML.
func expertYAML(id, keys string) string {
	return "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata: {id: " + id + ", version: 0.1.0, summary: A synthetic expert}\nspec:\n  engineApi: '>=0.2.0 <0.3.0'\n  analyst: analyst.md\n  routing: {keys: " + keys + "}\n"
}

// experts writes synthetic experts under dir/experts, by directory name.
func experts(t *testing.T, dir string, manifests map[string]string) {
	t.Helper()
	for name, m := range manifests {
		p := filepath.Join(dir, "experts", name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, body := range map[string]string{"expert.yaml": m, "analyst.md": "Synthetic guidance.\n"} {
			if err := os.WriteFile(filepath.Join(p, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestCompileLibrary: each manifest's routing keys put its expert under
// the key with its relationship, primary where none is given, and its
// manifest as provenance, experts in ID order, the keys' names, tags and
// provenance kept; the library reads back by the engine's rules, a name
// resolving case aside.
func TestCompileLibrary(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	experts(t, dir, map[string]string{
		"y": expertYAML("syn.y", "[{id: beta, relationship: secondary}, alpha]"),
		"x": expertYAML("syn.x", "[alpha]"),
	})
	got, err := expertcheck.CompileLibrary([]byte(keysDoc), "lib-1", "experts")
	if err != nil {
		t.Fatal(err)
	}
	want := `# Compiled by compa expert routing from a keys file and the experts'
# spec.routing.keys; do not edit.
apiVersion: compa.lowmag.cc/routing/v1alpha1
kind: RoutingLibrary
revision: lib-1
keys:
  - id: alpha
    name: Alpha
    aliases:
      - alpha-one
    tags:
      - alpha_tag
    experts:
      - id: syn.x
        relationship: primary
        provenance:
          - source: syn.x
            ref: experts/x/expert.yaml#spec.routing.keys
      - id: syn.y
        relationship: primary
        provenance:
          - source: syn.y
            ref: experts/y/expert.yaml#spec.routing.keys
    provenance:
      - source: synthetic
        ref: alpha.md
  - id: beta
    name: Beta
    experts:
      - id: syn.y
        relationship: secondary
        provenance:
          - source: syn.y
            ref: experts/y/expert.yaml#spec.routing.keys
`
	if string(got) != want {
		t.Errorf("library:\n%s\nwant:\n%s", got, want)
	}
	k, err := expertcheck.LoadKeys(got)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := k.Canonical("ALPHA-one"); !ok || id != "alpha" || k.Revision() != "lib-1" {
		t.Errorf("Canonical = %q, %v; revision %s", id, ok, k.Revision())
	}
}

// TestCompileLibraryRefuses: a manifest naming an alias or a key the keys
// do not name, keys that name experts, and keys whose names clash, case
// aside, are refused, each said.
func TestCompileLibraryRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	experts(t, dir, map[string]string{
		"x": expertYAML("syn.x", "[alpha-one]"),
		"z": expertYAML("syn.z", "[gamma]"),
	})
	_, err := expertcheck.CompileLibrary([]byte(keysDoc), "lib-1", "experts")
	for _, want := range []string{`syn.x declares "alpha-one", an alias of alpha; declare the key's ID`, `syn.z declares routing key "gamma", which the keys do not name`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v; want it saying %s", err, want)
		}
	}
	withExperts := keysDoc + "    experts: [{id: syn.x}]\n"
	if _, err := expertcheck.CompileLibrary([]byte(withExperts), "lib-1", t.TempDir()); err == nil || !strings.Contains(err.Error(), "the keys name experts under beta") {
		t.Errorf("keys naming experts: %v", err)
	}
	clash := keysDoc + "    aliases: [ALPHA]\n"
	if _, err := expertcheck.LoadKeys([]byte(clash)); err == nil || !strings.Contains(err.Error(), "names both") {
		t.Errorf("a name of two keys: %v", err)
	}
}

// TestRoutingCommand: compa expert routing fails until the library is
// written, writes it at the revision given, passes over it unchanged, and
// once a manifest changes fails and refuses to write it under the same
// revision, until a new one is given.
func TestRoutingCommand(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	experts(t, dir, map[string]string{"x": expertYAML("syn.x", "[alpha]")})
	if err := os.WriteFile("keys.yaml", []byte(keysDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		var out bytes.Buffer
		return expertcheck.Routing(context.Background(), append([]string{"--keys", "keys.yaml", "--library", "library.yaml"}, args...), &out)
	}
	if err := run("experts"); !errors.Is(err, expertcheck.ErrFailed) {
		t.Errorf("no library yet: %v", err)
	}
	if err := run("--write", "experts"); !errors.Is(err, expertcheck.ErrUsage) {
		t.Errorf("a first write with no revision: %v", err)
	}
	if err := run("--write", "--revision", "lib-1", "experts"); err != nil {
		t.Fatal(err)
	}
	if err := run("experts"); err != nil {
		t.Errorf("the written library: %v", err)
	}
	experts(t, dir, map[string]string{"x": expertYAML("syn.x", "[beta]")})
	if err := run("experts"); !errors.Is(err, expertcheck.ErrFailed) || !strings.HasPrefix(err.Error(), "expert routing failed: ") {
		t.Errorf("a stale library: %v", err)
	}
	if err := run("--write", "experts"); err == nil || !strings.Contains(err.Error(), "changed under revision lib-1") {
		t.Errorf("a changed library under its revision: %v", err)
	}
	if err := run("--write", "--revision", "lib-2", "experts"); err != nil {
		t.Fatal(err)
	}
	if lib, _ := os.ReadFile("library.yaml"); !strings.Contains(string(lib), "revision: lib-2") {
		t.Errorf("library:\n%s", lib)
	}
}

// TestVersions: in a git checkout, a package whose content changed since
// the base with its version unchanged fails the check, naming it; a new
// version, or a change to the directory's own Go file alone, passes.
func TestVersions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	g := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", append([]string{"-c", "user.name=synthetic", "-c", "user.email=synthetic@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := func(version string) string {
		return "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Catalog\nmetadata: {id: syn.c, version: " + version + ", title: Synthetic}\n"
	}
	write("catalogs/c/catalog.yaml", manifest("0.1.0"))
	write("catalogs/c/tests.yaml", "one\n")
	write("catalogs/c/c.go", "package c\n")
	g("init", "-q")
	g("add", ".")
	g("commit", "-q", "-m", "base")
	check := func() error {
		var out bytes.Buffer
		return expertcheck.Versions(context.Background(), []string{"--base", "HEAD", "catalogs"}, &out)
	}
	write("catalogs/c/c.go", "package c // embeds the directory\n")
	if err := check(); err != nil {
		t.Errorf("a Go file alone: %v", err)
	}
	write("catalogs/c/tests.yaml", "two\n")
	if err := check(); err == nil || !strings.HasPrefix(err.Error(), "expert versions failed: ") || !strings.Contains(err.Error(), "c (syn.c 0.1.0)") {
		t.Errorf("content changed, version kept: %v", err)
	}
	write("catalogs/c/catalog.yaml", manifest("0.1.1"))
	if err := check(); err != nil {
		t.Errorf("a new version: %v", err)
	}
}

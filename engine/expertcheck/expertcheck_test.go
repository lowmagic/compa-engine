package expertcheck_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine/expertcheck"
)

const mustGather = "../../testdata/artifacts/must-gather"

func load(t *testing.T) *expertcheck.Packages {
	t.Helper()
	pkgs, err := expertcheck.Load("../../testdata/catalogs")
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// A catalog tool runs against a fixture root at the highest installed
// version, 0.2.0, whose parameter is name rather than node, and a step running a command is unavailable, since no sandbox
// or binary is given.
func TestRun(t *testing.T) {
	pkgs := load(t)
	ctx := context.Background()
	got := pkgs.Run(ctx, mustGather, "fixture.core.node_ready", map[string]any{"name": "worker-1"})
	if got.Status != "ok" || got.Output != "{\"name\":\"worker-1\",\"ready\":\"False\"}\n" {
		t.Errorf("node_ready: %+v", got)
	}
	if got := pkgs.Run(ctx, mustGather, "fixture.core.node_ready", map[string]any{"node": "worker-1"}); got.Status != "unavailable" || got.Reason == "" {
		t.Errorf("0.1.1's parameter: %+v", got)
	}
	if got := pkgs.Run(ctx, mustGather, "fixture.core.pods", map[string]any{"namespace": "default"}); got.Status != "unavailable" || !strings.Contains(got.Reason, "not allowlisted") {
		t.Errorf("a command: %+v", got)
	}
	if got := pkgs.Run(ctx, mustGather, "fixture.core.nope", nil); got.Status != "unavailable" {
		t.Errorf("an unknown tool: %+v", got)
	}
}

// The three paths the experts repository's copy of the runner read are
// refused, and nothing of them is returned.
func TestRunRefusals(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		".compa/hidden.txt":                          "HIDDEN-MARKER\n",
		"x/auth/kubeconfig":                          "CRED-MARKER\n",
		"cluster-scoped-resources/core/nodes/n.yaml": "kind: Node\n",
	} {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"alias": ".compa/hidden.txt", "kc": "x/auth/kubeconfig"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	pkgRoot := t.TempDir()
	dir := filepath.Join(pkgRoot, "probe-files")
	os.MkdirAll(dir, 0o755)
	for n, c := range map[string]string{
		"catalog.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Catalog\nmetadata: {id: probe.files, version: 0.1.0, title: Probe}\nspec: {engineApi: '>=0.2.0 <0.3.0', tests: [probe.files.head]}\n",
		"tests.yaml":   "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Tests\ntests: [{id: probe.files.head, version: 0.1.0, description: First lines of a file., params: {file: {type: string, maxLength: 100}}, preconditions: [{exists: '{{file}}'}], run: [{read: {path: '{{file}}', head: 5}}], fixtures: fixtures/head}]\n",
	} {
		os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "fixtures/head"), 0o755)
	pkgs, err := expertcheck.Load(pkgRoot)
	if err != nil {
		t.Fatal(err)
	}
	pkgs.CredentialFiles = []string{".openshift_install_state.json", "kubeadmin-password", "auth/kubeconfig"}
	for file, why := range map[string]string{
		"alias":             "excluded from collection",
		"x/auth/kubeconfig": "holds only credentials",
		"kc":                "holds only credentials",
	} {
		got := pkgs.Run(context.Background(), root, "probe.files.head", map[string]any{"file": file})
		if got.Status == "ok" || !strings.Contains(got.Reason, why) || strings.Contains(got.Output, "MARKER") {
			t.Errorf("%s: %+v; want refused, %s", file, got, why)
		}
	}
}

// A root holding a package that does not load fails Load.
func TestLoadRefusesABrokenRoot(t *testing.T) {
	if _, err := expertcheck.Load("../testdata/expertcheck/broken"); err == nil {
		t.Error("a broken package loaded")
	}
}

// Importing the package must not bring in the terminal libraries, the case store,
// a model provider or deployment code: the experts repository builds it
// for every check.
func TestDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, banned := range []string{"charm.land/", "modernc.org/", "google.golang.org/genai", "github.com/anthropics/", "github.com/lowmagic/compa/", "compa-engine/engine\n"} {
			if strings.Contains(dep+"\n", banned) {
				t.Errorf("imports %s", dep)
			}
		}
	}
}

// install copies fixture packages from testdata/catalogs into a new root.
func install(t *testing.T, fixtures ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range fixtures {
		if err := os.CopyFS(filepath.Join(root, f), os.DirFS(filepath.Join("../../testdata/catalogs", f))); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A root holding only some of the packages is checked and run as any
// other: an expert with its catalog loads and its catalog's tool runs; an
// expert whose catalog is not in the root fails Load and the check, the
// error naming the catalog.
func TestPartialRoot(t *testing.T) {
	pkgs, err := expertcheck.Load(install(t, "storage", "core-0.1.1"))
	if err != nil {
		t.Fatalf("an expert with its catalog: %v", err)
	}
	if got := pkgs.Run(context.Background(), mustGather, "fixture.core.node_ready", map[string]any{"node": "worker-1"}); got.Status != "ok" {
		t.Errorf("the catalog's tool in a partial root: %+v", got)
	}

	without := install(t, "storage")
	if _, err := expertcheck.Load(without); err == nil || !strings.Contains(err.Error(), "catalog fixture.core (>=0.1.0 <0.2.0) is not installed") {
		t.Errorf("Load of an expert without its catalog = %v, want it refused naming the catalog", err)
	}
	var out strings.Builder
	if err := expertcheck.Command(context.Background(), []string{without}, &out); err == nil || !strings.Contains(out.String(), "is not used: catalog fixture.core") {
		t.Errorf("the check of an expert without its catalog: %v\n%s", err, out.String())
	}
}

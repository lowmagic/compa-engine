package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// excludedLayout is an artifact root holding a store and a source's state
// directory, and links that try to reach them from inside the root.
func excludedLayout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".compa/case.db":       "STORE-MARKER\n",
		".compa/notes.json":    `{"m": "STORE-MARKER"}`,
		".compa/notes.yaml":    "m: STORE-MARKER\n",
		".statedir/attach":     "STATE-MARKER\n",
		"data/ok.log":          "2026-10-01T00:00:00Z ok\n",
		"data/.compa/kept.txt": "nested\n",
		".compafoo/kept.txt":   "lookalike\n",
		"data/notes/ok.json":   `{"m": "ok"}`,
		"data/notes/ok.yaml":   "m: ok\n",
		"data/logs/server.log": "2026-10-01T00:00:00Z ok\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"data/store":  "../.compa",
		"data/db":     "../.compa/case.db",
		"data/state":  "../.statedir/attach",
		"tricky":      "data/../.compa",
		"data/chain1": "chain2",
		"data/chain2": "../.compa/case.db",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runIn(r *Runner, root string, stages ...expert.Stage) Record {
	return r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: stages}})
}

func TestExcludedPathsAreRefused(t *testing.T) {
	root := excludedLayout(t)
	r := &Runner{Exclude: []string{".statedir"}}
	for _, st := range []expert.Stage{
		prim("read", map[string]any{"path": ".compa/case.db"}),
		prim("read", map[string]any{"path": "./.compa/case.db"}),
		prim("read", map[string]any{"path": "data/../.compa/case.db"}),
		prim("read", map[string]any{"path": "data/db"}),
		prim("read", map[string]any{"path": "data/chain1"}),
		prim("read", map[string]any{"path": "data/state"}),
		prim("read", map[string]any{"path": ".statedir/attach"}),
		prim("read", map[string]any{"path": "data/store/case.db"}),
		prim("read", map[string]any{"path": "tricky/case.db"}),
		prim("find", map[string]any{"path": ".compa"}),
		prim("find", map[string]any{"path": "data/store"}),
		prim("grep", map[string]any{"pattern": "MARKER", "path": "tricky"}),
		prim("grep", map[string]any{"pattern": "MARKER", "path": ".compa/case.db"}),
		prim("jq", map[string]any{"filter": ".", "path": ".compa/notes.json"}),
		prim("yq", map[string]any{"filter": ".", "path": ".compa/notes.yaml"}),
		prim("logs.window", map[string]any{"path": ".compa/case.db", "start": "2026-01-01T00:00:00Z", "end": "2027-01-01T00:00:00Z"}),
		prim("logs.count", map[string]any{"path": ".compa/case.db"}),
		prim("diff", map[string]any{"a": "data/ok.log", "b": "data/db"}),
		prim("archive.list", map[string]any{"path": ".compa/case.tgz"}),
	} {
		rec := runIn(r, root, st)
		if rec.Status == StatusOK || !strings.Contains(rec.Reason, "excluded from collection") || strings.Contains(string(rec.Output), "MARKER") {
			t.Errorf("%s %v: status %s, reason %q, output %q", st.Primitive, st.Params, rec.Status, rec.Reason, rec.Output)
		}
		for _, p := range rec.PathsRead {
			if strings.HasPrefix(p, ".compa/") || strings.HasPrefix(p, ".statedir/") {
				t.Errorf("%s %v read %s", st.Primitive, st.Params, p)
			}
		}
	}
}

func TestWalksSkipExcludedPaths(t *testing.T) {
	root := excludedLayout(t)
	r := &Runner{Exclude: []string{".statedir"}}
	for _, st := range []expert.Stage{
		prim("find", map[string]any{"type": "any"}),
		prim("find", map[string]any{"type": "any", "details": true}),
		prim("grep", map[string]any{"pattern": "MARKER"}),
		prim("jq", map[string]any{"filter": ".", "path": ".", "glob": "*.json"}),
		prim("yq", map[string]any{"filter": ".", "path": ".", "glob": "*.yaml"}),
	} {
		rec := runIn(r, root, st)
		out := string(rec.Output)
		if rec.Status != StatusOK || strings.Contains(out, "MARKER") || strings.Contains(out, ".compa/case.db") || strings.Contains(out, ".statedir") {
			t.Errorf("%s %v: status %s %s, output %q", st.Primitive, st.Params, rec.Status, rec.Reason, out)
		}
		for _, p := range rec.PathsRead {
			if strings.HasPrefix(p, ".compa/") || strings.HasPrefix(p, ".statedir/") {
				t.Errorf("%s %v read %s", st.Primitive, st.Params, p)
			}
		}
	}
	found := mustOK(t, runIn(r, root, prim("find", map[string]any{})))
	for _, want := range []string{"data/ok.log", "data/.compa/kept.txt", ".compafoo/kept.txt"} {
		if !strings.Contains(found, want+"\n") {
			t.Errorf("find lost %s, which is not excluded: %q", want, found)
		}
	}
}

func TestOnlyTheTopLevelIsExcluded(t *testing.T) {
	root := excludedLayout(t)
	r := &Runner{}
	for path, want := range map[string]string{"data/.compa/kept.txt": "nested", ".compafoo/kept.txt": "lookalike", "data/ok.log": "ok"} {
		if got := mustOK(t, runIn(r, root, prim("read", map[string]any{"path": path}))); !strings.Contains(got, want) {
			t.Errorf("read %s = %q", path, got)
		}
	}
	// With no configured exclusions the store is still excluded, and the
	// source's state directory is not.
	if rec := runIn(r, root, prim("read", map[string]any{"path": ".compa/case.db"})); rec.Status == StatusOK {
		t.Error("read the store with no exclusions configured")
	}
	if got := mustOK(t, runIn(r, root, prim("read", map[string]any{"path": ".statedir/attach"}))); !strings.Contains(got, "STATE-MARKER") {
		t.Errorf("read .statedir/attach = %q", got)
	}
}

func TestBadExclusionIsUnavailable(t *testing.T) {
	root := excludedLayout(t)
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		rec := runIn(&Runner{Exclude: []string{name}}, root, prim("read", map[string]any{"path": "data/ok.log"}))
		if rec.Status != StatusUnavailable || !strings.Contains(rec.Reason, "exclusion") {
			t.Errorf("exclusion %q: status %s, reason %q", name, rec.Status, rec.Reason)
		}
	}
}

// TestSandboxDeniesExcludedPaths is the sandbox side: commands reach the
// artifact root directly, so only the sandbox stands between them and the
// store.
func TestSandboxDeniesExcludedPaths(t *testing.T) {
	r := sandboxRunner(t, "cat", "grep", "ls")
	r.Exclude = []string{".statedir"}
	root := realRoot(t, excludedLayout(t))
	cmd := func(a ...string) Record { return runIn(r, root, argv(a...)) }

	if got := mustOK(t, cmd("cat", filepath.Join(root, "data/ok.log"))); !strings.Contains(got, "ok") {
		t.Fatalf("positive control: cat data/ok.log = %q", got)
	}
	for _, a := range [][]string{
		{"cat", filepath.Join(root, ".compa/case.db")},
		{"cat", filepath.Join(root, "data/db")},
		{"cat", filepath.Join(root, "data/store/case.db")},
		{"cat", filepath.Join(root, ".statedir/attach")},
		{"ls", filepath.Join(root, ".compa")},
		{"ls", filepath.Join(root, ".statedir")},
	} {
		if rec := cmd(a...); rec.Status == StatusOK || strings.Contains(string(rec.Output), "MARKER") || strings.Contains(string(rec.Output), "case.db") {
			t.Errorf("%v: status %s, output %q", a, rec.Status, rec.Output)
		}
	}
	// grep -r reports the denied directories and carries on; what matters
	// is that nothing beneath them reaches the output.
	rec := cmd("grep", "-r", "MARKER", root)
	if strings.Contains(string(rec.Output), "MARKER") {
		t.Errorf("grep -r found the store or state: %q", rec.Output)
	}
	if got := mustOK(t, cmd("ls", "-a", root)); !strings.Contains(got, "data") {
		t.Errorf("ls of the root = %q", got)
	}
}

// Files that hold only credentials — an installer's state, the kubeadmin
// password and the admin kubeconfig under auth/ — are refused when named,
// through a link too, and passed over by walks. An installer's log and a
// kubeconfig elsewhere are read: redaction on egress handles their
// secrets.
func TestCredentialsOnlyFilesAreNeverRead(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"install/.openshift_install_state.json": `{"CRED-MARKER": 1}`,
		"install/auth/kubeadmin-password":       "CRED-MARKER\n",
		"install/auth/kubeconfig":               "CRED-MARKER\n",
		"install/.openshift_install.log":        "level=info msg=\"install complete\"\n",
		"cluster/kubeconfig":                    "kept\n",
	} {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("CRED-MARKER\n"))
	zw.Close()
	if err := os.WriteFile(filepath.Join(root, "install/auth/kubeadmin-password.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("install/auth/kubeadmin-password", filepath.Join(root, "pw")); err != nil {
		t.Fatal(err)
	}
	r := &Runner{CredentialFiles: installerCredentials}
	for _, st := range []expert.Stage{
		prim("read", map[string]any{"path": "install/.openshift_install_state.json"}),
		prim("read", map[string]any{"path": "install/auth/kubeadmin-password"}),
		prim("read", map[string]any{"path": "install/auth/kubeconfig"}),
		prim("read", map[string]any{"path": "pw"}),
		prim("read", map[string]any{"path": "install/auth/kubeadmin-password.gz"}),
		prim("jq", map[string]any{"filter": ".", "path": "install/.openshift_install_state.json"}),
		prim("grep", map[string]any{"pattern": "MARKER", "path": "install/auth/kubeconfig"}),
	} {
		rec := runIn(r, root, st)
		if rec.Status == StatusOK || !strings.Contains(rec.Reason, "holds only credentials") || strings.Contains(string(rec.Output), "MARKER") {
			t.Errorf("%s %v: status %s, reason %q, output %q", st.Primitive, st.Params, rec.Status, rec.Reason, rec.Output)
		}
	}
	for _, st := range []expert.Stage{
		prim("find", map[string]any{"type": "any"}),
		prim("find", map[string]any{"type": "any", "details": true}),
		prim("grep", map[string]any{"pattern": "MARKER"}),
		prim("jq", map[string]any{"filter": ".", "path": ".", "glob": "*.json"}),
	} {
		rec := runIn(r, root, st)
		if rec.Status != StatusOK || strings.Contains(string(rec.Output), "MARKER") || strings.Contains(string(rec.Output), "kubeadmin-password") {
			t.Errorf("%s %v: status %s %s, output %q", st.Primitive, st.Params, rec.Status, rec.Reason, rec.Output)
		}
	}
	if out := mustOK(t, runIn(r, root, prim("read", map[string]any{"path": "install/.openshift_install.log"}))); !strings.Contains(out, "install complete") {
		t.Errorf("the installer's log: %q", out)
	}
	mustOK(t, runIn(r, root, prim("read", map[string]any{"path": "cluster/kubeconfig"})))
}

// installerCredentials are the names a deployment for OpenShift passes.
var installerCredentials = []string{".openshift_install_state.json", "kubeadmin-password", "auth/kubeconfig"}

// The names are the deployment's: another product's are refused the same
// way, and with none given such a file is read like any other.
func TestCredentialFilesAreTheDeployments(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"app/secrets/master.key":    "CRED-MARKER\n",
		"app/config/master.key.txt": "kept\n",
		"install/auth/kubeconfig":   "CRED-MARKER\n",
	} {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := &Runner{CredentialFiles: []string{"secrets/master.key"}}
	if rec := runIn(other, root, prim("read", map[string]any{"path": "app/secrets/master.key"})); rec.Status == StatusOK || !strings.Contains(rec.Reason, "holds only credentials") {
		t.Errorf("another product's credential file: %s %q", rec.Status, rec.Reason)
	}
	mustOK(t, runIn(other, root, prim("read", map[string]any{"path": "app/config/master.key.txt"})))
	mustOK(t, runIn(other, root, prim("read", map[string]any{"path": "install/auth/kubeconfig"})))
	mustOK(t, runIn(&Runner{}, root, prim("read", map[string]any{"path": "app/secrets/master.key"})))
}

// A test whose precondition names a refused path says why it was refused,
// not that the path does not exist; a path that is missing still says so.
func TestPreconditionSaysWhyRefused(t *testing.T) {
	dir := t.TempDir()
	for n, c := range map[string]string{
		"expert.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata: {id: a.b, version: 0.1.0, summary: A test expert}\nspec: {engineApi: '>=0.2.0 <0.3.0', analyst: a.md, tests: [a.head]}\n",
		"a.md":        "x",
		"tests.yaml":  "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Tests\ntests: [{id: a.head, version: 0.1.0, params: {file: {type: string, maxLength: 100}}, preconditions: [{exists: '{{file}}'}], run: [{read: {path: '{{file}}', head: 5}}]}]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg, err := expert.Loader{CheckStage: CheckStage}.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := excludedLayout(t)
	if err := os.MkdirAll(filepath.Join(root, "x/auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "x/auth/kubeconfig"), []byte("CRED-MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("x/auth/kubeconfig", filepath.Join(root, "kc")); err != nil {
		t.Fatal(err)
	}
	r := &Runner{CredentialFiles: installerCredentials}
	for file, want := range map[string]string{
		"data/chain1":       errExcluded.Error(),
		"x/auth/kubeconfig": errCredentials.Error(),
		"kc":                errCredentials.Error(),
		"data/missing.log":  "does not exist",
	} {
		rec := r.RunTest(context.Background(), root, pkg, "a.head", map[string]any{"file": file})
		if rec.Status != StatusUnavailable || !strings.Contains(rec.Reason, want) || len(rec.Output) != 0 {
			t.Errorf("%s: status %s, reason %q, output %q; want unavailable, saying it %s", file, rec.Status, rec.Reason, rec.Output, want)
		}
	}
}

// A grep that reaches its match limit stops reading, so corrupt data later
// in a gzipped file does not fail it; a grep that must read on into the
// corrupt data fails.
func TestGrepStopsAtLimitBeforeCorruptData(t *testing.T) {
	var plain bytes.Buffer
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&plain, "line %d %x\n", i, sha256.Sum256([]byte{byte(i), byte(i >> 8)}))
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(plain.Bytes())
	w.Close()
	data := gz.Bytes()
	for i := len(data) * 3 / 4; i < len(data)-8; i++ {
		data[i] ^= 0xff
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.log.gz"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	rec := runIn(r, root, prim("grep", map[string]any{"path": "x.log.gz", "pattern": "^line", "max_matches": 1}))
	if want := fmt.Sprintf("x.log.gz:1:line 0 %x\n", sha256.Sum256([]byte{0, 0})); rec.Status != StatusOK || !rec.Truncated || string(rec.Output) != want {
		t.Errorf("limit reached early: %s truncated=%v %q %q", rec.Status, rec.Truncated, rec.Reason, rec.Output)
	}
	if rec := runIn(r, root, prim("grep", map[string]any{"path": "x.log.gz", "pattern": "^no such line", "max_matches": 1})); rec.Status == StatusOK {
		t.Errorf("reading into the corrupt data: %s, want a failure", rec.Status)
	}
}

package expertcheck_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine/expertcheck"
	"github.com/lowmagic/compa-engine/internal/schema"
)

const casesHead = "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Cases\n"

// probe writes a catalog of two tools into a new package root: head, the
// first lines of a file, and list, the entries under the root with their
// details, each with fixtures; cases maps a tool's short name to its
// cases.yaml, written where given. The fixture root is fixtures/root, with
// two synthetic files.
func probe(t *testing.T, cases map[string]string) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "probe")
	files := map[string]string{
		"catalog.yaml":          "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Catalog\nmetadata: {id: probe.files, version: 0.1.0, title: Probe}\nspec: {engineApi: '>=0.2.0 <0.3.0', tests: [probe.files.head, probe.files.list]}\n",
		"tests.yaml":            "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Tests\ntests:\n  - {id: probe.files.head, version: 0.1.0, description: First lines of a file., params: {file: {type: string, maxLength: 100}, lines: {type: integer, default: 2, minimum: 1, maximum: 10}}, preconditions: [{exists: '{{file}}'}], run: [{read: {path: '{{file}}', head: '{{lines}}'}}], fixtures: fixtures/head}\n  - {id: probe.files.list, version: 0.1.0, description: Every file with its details., run: [{find: {details: true}}], fixtures: fixtures/list}\n",
		"fixtures/root/a.log":   "one\ntwo\nthree\n",
		"fixtures/root/b/c.txt": "synthetic\n",
	}
	for tool, c := range cases {
		files["fixtures/"+tool+"/cases.yaml"] = c
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"fixtures/head", "fixtures/list"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, dir
}

// report is the part of the check's JSON report these tests read.
type report struct {
	Packages []struct {
		ID    string
		Cases struct {
			Run      int
			Failures []struct{ Tool, Case, Problem string }
		}
	}
	Totals struct{ Errors, Cases int }
}

func check(t *testing.T, args ...string) (report, error) {
	t.Helper()
	var out bytes.Buffer
	err := expertcheck.Command(context.Background(), append([]string{"--json"}, args...), &out)
	if verr := schema.ValidateJSON(schema.ExpertCheck, out.Bytes()); verr != nil {
		t.Fatalf("report does not validate: %v\n%s", verr, out.String())
	}
	var r report
	if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
		t.Fatal(jerr)
	}
	return r, err
}

func problems(r report) []string {
	var out []string
	for _, p := range r.Packages {
		for _, f := range p.Cases.Failures {
			out = append(out, f.Tool+" | "+f.Case+" | "+f.Problem)
		}
	}
	return out
}

// TestCases: compa expert check runs each case through the engine's
// runner and compares status, reason, truncation and output exactly; a
// different output, a reason worded otherwise, a repeated name and a
// required label no case covers each fail the check, named by tool and
// case, and a test with fixtures but no cases fails too.
func TestCases(t *testing.T) {
	head := casesHead + "tool: probe.files.head\nroot: ../root\nmodified: 2026-03-01T12:00:00Z\nrequires: [normal, missing, bounded]\ncases:\n" +
		"  - {name: two lines, covers: [normal], params: {file: a.log}, status: ok, output: \"one\\ntwo\\n\"}\n" +
		"  - {name: wrong line, params: {file: a.log, lines: 1}, status: ok, output: \"two\\n\"}\n" +
		"  - {name: missing, covers: [missing], params: {file: none.log}, status: unavailable, reason: does not exist, output: ''}\n" +
		"  - {name: two lines, params: {file: b/c.txt}, status: ok, output: \"synthetic\\n\"}\n"
	root, _ := probe(t, map[string]string{"head": head})
	r, err := check(t, root)
	if err == nil {
		t.Fatal("the check passed")
	}
	want := []string{
		"probe.files.head | two lines | the name is given to another case of the file",
		"probe.files.head |  | no case covers bounded, which the file requires",
		`probe.files.head | wrong line | output line 1 is "one", want "two"`,
		`probe.files.head | missing | reason "precondition: none.log does not exist in the artifacts", want "does not exist"`,
		"probe.files.list |  | fixtures/list/cases.yaml: no cases: a test with fixtures has its cases there",
	}
	if got := problems(r); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("failures:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.Totals.Cases != 4 || r.Totals.Errors != len(want) {
		t.Errorf("totals %+v", r.Totals)
	}
}

// TestCasesFileRules: a cases file naming another tool than the test whose
// fixtures hold it, or a root outside its package, is not run.
func TestCasesFileRules(t *testing.T) {
	list := casesHead + "tool: probe.files.list\nroot: ../root\nmodified: 2026-03-01T12:00:00Z\ncases:\n  - {name: all, status: ok, output: ''}\n"
	root, dir := probe(t, map[string]string{
		"head": casesHead + "tool: probe.files.list\nroot: ../root\nmodified: 2026-03-01T12:00:00Z\ncases:\n  - {name: x, status: ok, output: ''}\n",
		"list": strings.Replace(list, "root: ../root", "root: ../../..", 1),
	})
	r, err := check(t, root)
	got := strings.Join(problems(r), "\n")
	if err == nil || r.Totals.Cases != 0 ||
		!strings.Contains(got, "probe.files.head |  | "+filepath.Join(dir, "fixtures/head/cases.yaml")+": tool probe.files.list, but the file is probe.files.head's fixtures") ||
		!strings.Contains(got, "probe.files.list |  | "+filepath.Join(dir, "fixtures/list/cases.yaml")+": root ../../.. is outside the package") {
		t.Errorf("failures:\n%s", got)
	}
}

// TestCasesUpdate: --update writes each case's status, reason, truncation
// and output from its run, leaving the file's comments, notes and the
// style of its parameters as they were, after which the check passes; the
// root's entries carry the file's modification time, which a listing
// shows.
func TestCasesUpdate(t *testing.T) {
	head := "# Synthetic, for this test.\n" + casesHead + "tool: probe.files.head\nroot: ../root\nmodified: 2026-03-01T12:00:00Z\ncases:\n" +
		"  - name: two lines\n    note: Kept as written.\n    params: {file: a.log}\n    status: failed\n    reason: wrong\n    output: ''\n" +
		"  - name: missing\n    params: {file: none.log}\n    status: ok\n    output: ''\n"
	list := casesHead + "tool: probe.files.list\nroot: ../root\nmodified: 2026-03-01T12:00:00Z\ncases:\n  - name: every file\n    status: ok\n    output: ''\n"
	root, dir := probe(t, map[string]string{"head": head, "list": list})
	var out bytes.Buffer
	if err := expertcheck.Command(context.Background(), []string{"--update", root}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "fixtures/head/cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Synthetic, for this test.\n" + casesHead + "tool: probe.files.head\nroot: ../root\nmodified: 2026-03-01T12:00:00Z\ncases:\n" +
		"  - name: two lines\n    note: Kept as written.\n    params: {file: a.log}\n    status: ok\n    output: |\n      one\n      two\n" +
		"  - name: missing\n    params: {file: none.log}\n    status: unavailable\n    reason: 'precondition: none.log does not exist in the artifacts'\n    output: ''\n"
	if string(data) != want {
		t.Errorf("updated:\n%s\nwant:\n%s", data, want)
	}
	listed, err := os.ReadFile(filepath.Join(dir, "fixtures/list/cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(listed), "    output: |\n      file\t14\t2026-03-01T12:00:00Z\ta.log\n      file\t10\t2026-03-01T12:00:00Z\tb/c.txt\n") {
		t.Errorf("the listing does not carry the fixture time:\n%s", listed)
	}
	if r, err := check(t, root); err != nil || r.Totals.Cases != 3 {
		t.Errorf("after the update: %v, %+v", err, problems(r))
	}
}

// TestRunCases: a cases file outside any package runs by the same rules
// over a root that holds symlinks, which no package may, refusing the
// files it names as holding only credentials, directly and through a
// link.
func TestRunCases(t *testing.T) {
	pkgRoot, _ := probe(t, nil)
	pkgs, err := expertcheck.Load(pkgRoot)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	art := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(filepath.Join(art, "x/auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(art, "x/auth/kubeconfig"), []byte("MARKER, not a credential\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("x/auth/kubeconfig", filepath.Join(art, "kc")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "cases.yaml")
	body := casesHead + "tool: probe.files.head\nroot: artifacts\nmodified: 2026-03-01T12:00:00Z\ncredentials: [auth/kubeconfig]\ncases:\n" +
		"  - {name: named, params: {file: x/auth/kubeconfig}, status: unavailable, reason: 'precondition: x/auth/kubeconfig holds only credentials and is never read', output: ''}\n" +
		"  - {name: linked, params: {file: kc}, status: unavailable, reason: 'precondition: kc holds only credentials and is never read', output: ''}\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := pkgs.RunCases(context.Background(), file, false)
	if err != nil || res.Run != 2 || len(res.Failures) != 0 {
		t.Errorf("RunCases: %v, %+v", err, res)
	}
}

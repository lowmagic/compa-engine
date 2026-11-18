package cli_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/schema"
)

const (
	catalogsRoot = "../../testdata/catalogs"
	checkRoots   = "../testdata/expertcheck/"
)

// checkReport is the part of the check's JSON report the tests read.
type checkReport struct {
	Packages []struct {
		Kind, ID, Version, Dir string
		Usable                 bool
		Tests, Warnings        []string
		Catalogs               []struct {
			ID, Range, Version, Unmet string
			Tools                     []string
		}
	}
	Failed []struct {
		Dir      string
		Problems []string
	}
	Errors, Warnings []string
	Totals           struct{ Packages, Errors, Warnings int }
}

// check runs compa expert check --json, validates the report against its
// schema and decodes it.
func (e *cliEnv) check(args ...string) (checkReport, int) {
	e.t.Helper()
	out, errs, code := e.run(append([]string{"expert", "check", "--json"}, args...)...)
	if code == 2 {
		e.t.Fatalf("usage error: %s", errs)
	}
	if err := schema.ValidateJSON(schema.ExpertCheck, []byte(out)); err != nil {
		e.t.Fatalf("report does not validate: %v\n%s", err, out)
	}
	var r checkReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		e.t.Fatal(err)
	}
	return r, code
}

func TestExpertCheckReportValidates(t *testing.T) {
	e := newEnv(t)
	for _, roots := range [][]string{
		{catalogsRoot},
		{checkRoots + "broken"},
		{catalogsRoot, checkRoots + "unmet", checkRoots + "clash", "nowhere"},
	} {
		r, _ := e.check(roots...)
		if r.Totals.Packages != len(r.Packages) {
			t.Errorf("%v: totals %+v for %d packages", roots, r.Totals, len(r.Packages))
		}
	}
}

func TestExpertCheckUsage(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"expert", "check"},
		{"expert", "check", "--json"},
		{"expert", "check", "--strict", catalogsRoot},
	} {
		_, errs, code := e.run(args...)
		if code != 2 || !strings.Contains(errs, "expert check [--fail-on-warning] [--json] [--update] <root> [root …]") {
			t.Errorf("compa %v: exit %d\n%s", args, code, errs)
		}
	}
}

func (r checkReport) pkg(t *testing.T, id string) int {
	t.Helper()
	for i, p := range r.Packages {
		if p.ID == id {
			return i
		}
	}
	t.Fatalf("no package %s in %+v", id, r.Packages)
	return -1
}

func TestExpertCheckValidPackages(t *testing.T) {
	e := newEnv(t)
	r, code := e.check(catalogsRoot)
	if code != 0 || r.Totals.Errors != 0 || r.Totals.Warnings != 0 || r.Totals.Packages != 5 {
		t.Fatalf("exit %d, totals %+v", code, r.Totals)
	}
	var order []string
	for _, p := range r.Packages {
		order = append(order, p.Kind+" "+p.ID+" "+p.Version)
		if !p.Usable {
			t.Errorf("%s %s is not usable", p.ID, p.Version)
		}
	}
	if got, want := strings.Join(order, ", "), "catalog fixture.core 0.1.0, catalog fixture.core 0.1.1, catalog fixture.core 0.2.0, expert fixture.network 0.1.0, expert fixture.storage 0.1.0"; got != want {
		t.Errorf("packages %s, want %s", got, want)
	}
	storage := r.Packages[r.pkg(t, "fixture.storage")]
	if len(storage.Catalogs) != 1 {
		t.Fatalf("storage catalogs %+v", storage.Catalogs)
	}
	c := storage.Catalogs[0]
	if c.ID != "fixture.core" || c.Range != ">=0.1.0 <0.2.0" || c.Version != "0.1.1" || c.Unmet != "" || strings.Join(c.Tools, " ") != "fixture.core.node_ready fixture.core.degraded_operators" {
		t.Errorf("storage resolves %+v", c)
	}
	if core := r.Packages[0]; strings.Join(core.Tests, " ") != "fixture.core.node_ready fixture.core.degraded_operators fixture.core.pods" {
		t.Errorf("catalog tools %v", core.Tests)
	}
}

func TestExpertCheckRejects(t *testing.T) {
	e := newEnv(t)
	root := checkRoots + "broken"
	r, code := e.check(root)
	if code != 1 || r.Totals.Errors != 6 || len(r.Packages) != 1 || r.Packages[0].ID != "check.good" {
		t.Fatalf("exit %d, totals %+v, packages %+v", code, r.Totals, r.Packages)
	}
	want := map[string]string{
		"binary":    `tests.yaml: check.binary.one: run[0]: binary "kubectl" is not declared in spec.binaries`,
		"escape":    `spec.analyst: "docs/../../outside.md" escapes the package`,
		"manifest":  "expert.yaml: does not conform to compa.lowmag.cc/expert/manifest/v1alpha3: /metadata: missing properties 'version', 'summary'",
		"primitive": "tests.yaml: does not conform to compa.lowmag.cc/expert/tests/v1alpha2: /tests/0/run/0: additional properties 'tail' not allowed",
		"parameter": `tests.yaml: check.parameter.one: run[0]: find: unknown field "depth"`,
		"splice":    `tests.yaml: check.splice.one: run[0]: "-n={{ns}}" splices a parameter into a string`,
	}
	for _, f := range r.Failed {
		name := filepath.Base(f.Dir)
		if f.Dir != filepath.Join(root, name) || len(f.Problems) != 1 || !strings.HasPrefix(f.Problems[0], want[name]) {
			t.Errorf("%s: problems %v, want one starting %q", f.Dir, f.Problems, want[name])
		}
		delete(want, name)
	}
	if len(want) > 0 {
		t.Errorf("not reported: %v", want)
	}

	out, errs, code := e.run("expert", "check", root)
	if code != 1 || !strings.Contains(out, "failed "+root+"/splice\n  error: tests.yaml: check.splice.one") || errs != "compa: expert check failed: 6 errors, 0 warnings\n" {
		t.Errorf("text: exit %d\n%s%s", code, out, errs)
	}
}

func TestExpertCheckUnexplainedTool(t *testing.T) {
	e := newEnv(t)
	roots := []string{catalogsRoot, checkRoots + "unexplained"}
	r, code := e.check(roots...)
	p := r.Packages[r.pkg(t, "check.unexplained")]
	if code != 0 || r.Totals.Warnings != 1 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "analyst.md: does not mention fixture.core.node_ready") {
		t.Fatalf("exit %d, totals %+v, warnings %v", code, r.Totals, p.Warnings)
	}
	if len(p.Catalogs) != 1 || p.Catalogs[0].Version != "0.1.1" || strings.Join(p.Catalogs[0].Tools, " ") != "fixture.core.node_ready" {
		t.Errorf("the tool is still offered: %+v", p.Catalogs)
	}
	if _, code := e.check(append([]string{"--fail-on-warning"}, roots...)...); code != 1 {
		t.Errorf("--fail-on-warning: exit %d", code)
	}
}

func TestExpertCheckUnmetRange(t *testing.T) {
	e := newEnv(t)
	roots := []string{catalogsRoot, checkRoots + "unmet"}
	r, code := e.check(roots...)
	p := r.Packages[r.pkg(t, "check.unmet")]
	if code != 0 || r.Totals.Warnings != 1 || len(p.Catalogs) != 1 {
		t.Fatalf("exit %d, totals %+v, package %+v", code, r.Totals, p)
	}
	c := p.Catalogs[0]
	if c.Version != "" || len(c.Tools) != 0 || !strings.Contains(c.Unmet, "catalog fixture.core has no installed version in >=0.3.0 <0.4.0 (installed: 0.2.0, 0.1.1, 0.1.0)") {
		t.Errorf("unmet dependency %+v", c)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "not offered: fixture.core.node_ready") {
		t.Errorf("warnings %v", p.Warnings)
	}
	if _, code := e.check(append(roots, "--fail-on-warning")...); code != 1 {
		t.Errorf("--fail-on-warning: exit %d", code)
	}
}

func TestExpertCheckIDClash(t *testing.T) {
	e := newEnv(t)
	r, code := e.check(catalogsRoot, checkRoots+"clash")
	if code != 1 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "fixture.core is the ID of an expert ("+checkRoots+"clash/core) and of a catalog") {
		t.Fatalf("exit %d, errors %v", code, r.Errors)
	}
	for _, p := range r.Packages {
		if p.ID == "fixture.core" && p.Usable {
			t.Errorf("%s %s at %s is usable", p.Kind, p.Version, p.Dir)
		}
	}
	// With the catalog gone, every expert using it is warned about too.
	if r.Totals.Warnings != 2 {
		t.Errorf("totals %+v", r.Totals)
	}
}

func TestExpertCheckMissingRoot(t *testing.T) {
	e := newEnv(t)
	r, code := e.check(catalogsRoot, "nowhere")
	if code != 1 || len(r.Errors) != 1 || r.Errors[0] != "expert root nowhere does not exist" || r.Totals.Packages != 5 {
		t.Errorf("exit %d, errors %v, totals %+v", code, r.Errors, r.Totals)
	}
}

// TestExpertCheckIsDeterministicAndWritesNothing runs the check twice over
// every fixture root, in text and JSON, comparing the output and the files
// under the roots, the home directory and the case folders.
func TestExpertCheckIsDeterministicAndWritesNothing(t *testing.T) {
	e := newEnv(t)
	roots := []string{catalogsRoot, checkRoots + "broken", checkRoots + "unexplained", checkRoots + "unmet", checkRoots + "clash", "nowhere"}
	watched := append([]string{e.home, e.cases}, roots[:5]...)
	before := snapshot(t, watched)
	for _, format := range [][]string{nil, {"--json"}} {
		args := append(append([]string{"expert", "check"}, format...), roots...)
		out1, errs1, code1 := e.run(args...)
		out2, errs2, code2 := e.run(args...)
		if out1 != out2 || errs1 != errs2 || code1 != code2 || code1 != 1 {
			t.Errorf("%v: two runs differ, or exit %d", format, code1)
		}
	}
	if after := snapshot(t, watched); after != before {
		t.Errorf("files changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func snapshot(t *testing.T, dirs []string) string {
	t.Helper()
	var b strings.Builder
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %v %d %d\n", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

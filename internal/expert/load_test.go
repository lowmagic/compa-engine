package expert

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureRoot = "../../testdata/experts"

// writePackage creates a package in a new directory under root from a map
// of relative path to content.
func writePackage(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const baseManifest = `apiVersion: compa.lowmag.cc/expert/v1alpha3
kind: Expert
metadata: {id: test.expert, version: 0.1.0, summary: A test expert}
spec:
  engineApi: '>=0.2.0 <0.3.0'
  analyst: analyst.md
  binaries: [omc]
  tests: [test.one]
`

const baseTests = `apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Tests
tests:
  - id: test.one
    version: 0.1.0
    params: {ns: {type: string, default: default}}
    run:
      - [omc, get, pods, -n, '{{ns}}']
`

func baseFiles() map[string]string {
	return map[string]string{"expert.yaml": baseManifest, "analyst.md": "guidance", "tests.yaml": baseTests}
}

func with(files map[string]string, changes map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range files {
		out[k] = v
	}
	for k, v := range changes {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func TestLoadFixtureExperts(t *testing.T) {
	for _, name := range []string{"platform", "networking"} {
		pkg, err := Loader{}.Load(filepath.Join(fixtureRoot, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(pkg.Digest, "sha256:") || pkg.Analyst == "" || len(pkg.Tests) != 2 {
			t.Errorf("%s: unexpected package %+v", name, pkg)
		}
	}
}

func TestLoadAccepts(t *testing.T) {
	dir := writePackage(t, t.TempDir(), "p", baseFiles())
	pkg, err := Loader{}.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := pkg.Manifest.Metadata.ID; got != "test.expert" {
		t.Errorf("id = %q", got)
	}
	if _, ok := pkg.Test("test.one"); !ok {
		t.Error("test.one not found")
	}
}

// spec.routing names the routing keys the expert serves under keys; a bare
// ID is a primary relationship.
func TestLoadRoutingKeys(t *testing.T) {
	manifest := baseManifest + "  routing: {keys: [openshift, {id: openshift-networking, relationship: secondary}]}\n"
	pkg, err := Loader{}.Load(writePackage(t, t.TempDir(), "p", with(baseFiles(), map[string]string{"expert.yaml": manifest})))
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyRef{{ID: "openshift", Relationship: "primary"}, {ID: "openshift-networking", Relationship: "secondary"}}
	if got := pkg.Manifest.Spec.Routing.Keys; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("routing keys = %+v, want %+v", got, want)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]string
		want    string
	}{
		{"incompatible engine API", map[string]string{"expert.yaml": strings.Replace(baseManifest, "'>=0.2.0 <0.3.0'", "'>=0.3.0 <0.4.0'", 1)}, "does not include engine API " + EngineAPIVersion},
		{"malformed engine API", map[string]string{"expert.yaml": strings.Replace(baseManifest, "'>=0.2.0 <0.3.0'", "'^0.1'", 1)}, "spec.engineApi"},
		{"no summary", map[string]string{"expert.yaml": strings.Replace(baseManifest, ", summary: A test expert", "", 1)}, "/metadata: missing property 'summary'"},
		{"summary over its bound", map[string]string{"expert.yaml": strings.Replace(baseManifest, "summary: A test expert", "summary: "+strings.Repeat("x", 161), 1)}, "/metadata/summary"},
		{"analysis requirements", map[string]string{"expert.yaml": baseManifest + "  analysis:\n    taskTypes: [analysis.diagnose]\n    outputSchema: compa.lowmag.cc/analysis/proposal/v1alpha1\n"}, "additional properties 'analysis' not allowed"},
		{"suggested evidence", map[string]string{"expert.yaml": baseManifest + "  evidence: {suggested: [test.one]}\n"}, "additional properties 'evidence' not allowed"},
		{"routing keys under the old name", map[string]string{"expert.yaml": baseManifest + "  routing: {sbrs: [openshift]}\n"}, "/spec/routing: additional properties 'sbrs' not allowed"},
		{"malformed manifest", map[string]string{"expert.yaml": "apiVersion: [unclosed"}, "expert.yaml: yaml"},
		{"schema violation", map[string]string{"expert.yaml": strings.Replace(baseManifest, "kind: Expert", "kind: Agent", 1)}, "/kind"},
		{"missing manifest", map[string]string{"expert.yaml": ""}, "expert.yaml: missing"},
		{"missing analyst", map[string]string{"analyst.md": ""}, `file "analyst.md" not found`},
		{"reference escapes", map[string]string{"expert.yaml": baseManifest + "  references: [refs/../../outside.md]\n"}, "escapes the package"},
		{"unnormalized reference", map[string]string{"expert.yaml": baseManifest + "  references: [refs/./a.md]\n", "refs/a.md": "x"}, "not a normalized path"},
		{"test listed but undefined", map[string]string{"expert.yaml": strings.Replace(baseManifest, "[test.one]", "[test.one, test.two]", 1)}, "test.two is not defined"},
		{"test defined but unlisted", map[string]string{"expert.yaml": strings.Replace(baseManifest, "[test.one]", "[]", 1)}, "not listed in spec.tests"},
		{"tests file missing", map[string]string{"tests.yaml": ""}, "tests.yaml is missing"},
		{"undeclared binary", map[string]string{"expert.yaml": strings.Replace(baseManifest, "binaries: [omc]", "binaries: [oc]", 1)}, `binary "omc" is not declared`},
		{"templated binary", map[string]string{"tests.yaml": strings.Replace(baseTests, "[omc, get", "['{{ns}}', get", 1)}, "the binary cannot be a parameter"},
		{"spliced parameter", map[string]string{"tests.yaml": strings.Replace(baseTests, "'{{ns}}'", "'ns={{ns}}'", 1)}, "splices a parameter"},
		{"undeclared parameter", map[string]string{"tests.yaml": strings.Replace(baseTests, "'{{ns}}'", "'{{other}}'", 1)}, `undeclared parameter "other"`},
		{"unknown primitive", map[string]string{"tests.yaml": strings.Replace(baseTests, "- [omc, get, pods, -n, '{{ns}}']", "- curl: http://example.com", 1)}, "tests.yaml"},
		{"bad default", map[string]string{"tests.yaml": strings.Replace(baseTests, "{type: string, default: default}", "{type: integer, default: lots}", 1)}, "default: want a integer"},
		{"duplicate test", map[string]string{"tests.yaml": baseTests + "  - {id: test.one, version: 0.1.0, run: [[omc]]}\n"}, "defined more than once"},
		{"test under the engine's tools", map[string]string{
			"expert.yaml": strings.Replace(baseManifest, "[test.one]", "[evidence.read]", 1),
			"tests.yaml":  strings.Replace(baseTests, "id: test.one", "id: evidence.read", 1)}, "test IDs beginning evidence. are the engine's own tools"},
		{"names a model in the summary", map[string]string{"expert.yaml": strings.Replace(baseManifest, "summary: A test expert", "summary: Tuned for Claude", 1)}, `names a reasoning harness, model provider or model ("claude")`},
		{"names a model in the title", map[string]string{"expert.yaml": strings.Replace(baseManifest, "version: 0.1.0,", "version: 0.1.0, title: Tuned for Gemini3,", 1)}, `("gemini")`},
		{"names a harness", map[string]string{"expert.yaml": strings.Replace(baseManifest, "version: 0.1.0,", "version: 0.1.0, description: run under Pi only,", 1)}, `("pi")`},
		{"mcp tool not read-only", map[string]string{"expert.yaml": baseManifest + "  mcp:\n    command: [python3, mcp/s.py]\n    tools: [{name: x.write, risk: write, inputSchema: {type: object}, outputSchema: {type: object}}]\n"}, `risk "write" is not allowed`},
		{"mcp schema invalid", map[string]string{"expert.yaml": baseManifest + "  mcp:\n    command: [python3, mcp/s.py]\n    tools: [{name: x.read, risk: read_only, inputSchema: {type: 7}, outputSchema: {type: object}}]\n"}, "inputSchema"},
		{"mcp external schema ref", map[string]string{"expert.yaml": baseManifest + "  mcp:\n    command: [python3, mcp/s.py]\n    tools: [{name: x.read, risk: read_only, inputSchema: {$ref: 'https://example.com/s.json'}, outputSchema: {type: object}}]\n"}, "inputSchema"},
		{"interpret target missing", map[string]string{"tests.yaml": baseTests + "    interpret: guide.md#x\n"}, `file "guide.md" not found`},
		{"fixtures missing", map[string]string{"tests.yaml": baseTests + "    fixtures: fixtures/none\n"}, `directory "fixtures/none" not found`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writePackage(t, t.TempDir(), "p", with(baseFiles(), c.changes))
			_, err := Loader{}.Load(dir)
			var le *LoadError
			if !errors.As(err, &le) {
				t.Fatalf("want *LoadError, got %v", err)
			}
			if !strings.Contains(le.Error(), c.want) {
				t.Fatalf("error does not contain %q:\n%v", c.want, le)
			}
		})
	}
}

func TestLoadAllowsWordsContainingForbiddenTokens(t *testing.T) {
	m := strings.Replace(baseManifest, "version: 0.1.0,", "version: 0.1.0, description: API pipeline for opinions,", 1)
	dir := writePackage(t, t.TempDir(), "p", with(baseFiles(), map[string]string{"expert.yaml": m}))
	if _, err := (Loader{}).Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	dir := writePackage(t, t.TempDir(), "p", baseFiles())
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	_, err := Loader{}.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "symlinks are not allowed") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRejectsSymlinkedPackageDir(t *testing.T) {
	root := t.TempDir()
	dir := writePackage(t, root, "real", baseFiles())
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := (Loader{}).Load(link); err == nil {
		t.Fatal("want error for a symlinked package directory")
	}
}

func TestLoadEnforcesLimits(t *testing.T) {
	dir := writePackage(t, t.TempDir(), "p", with(baseFiles(), map[string]string{"big.txt": strings.Repeat("x", 2048)}))
	_, err := Loader{Limits: Limits{MaxFiles: 10, MaxFileBytes: 1024, MaxTotalBytes: 1 << 20, MaxDepth: 4}}.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "file limit") {
		t.Fatalf("file size: got %v", err)
	}
	_, err = Loader{Limits: Limits{MaxFiles: 2, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxDepth: 4}}.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "4 files exceeds") {
		t.Fatalf("file count: got %v", err)
	}
	deep := writePackage(t, t.TempDir(), "p", with(baseFiles(), map[string]string{"a/b/c/d/e.txt": "x"}))
	_, err = Loader{Limits: Limits{MaxFiles: 10, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxDepth: 3}}.Load(deep)
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("depth: got %v", err)
	}
}

func TestStageChecker(t *testing.T) {
	dir := writePackage(t, t.TempDir(), "p", baseFiles())
	l := Loader{CheckStage: func(s Stage) error {
		if len(s.Argv) > 0 && s.Argv[1] == "get" {
			return errors.New("checker rejected it")
		}
		return nil
	}}
	if _, err := l.Load(dir); err == nil || !strings.Contains(err.Error(), "checker rejected it") {
		t.Fatalf("got %v", err)
	}
}

func TestDigest(t *testing.T) {
	root := t.TempDir()
	a, err := Loader{}.Load(writePackage(t, root, "a", baseFiles()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Loader{}.Load(writePackage(t, root, "b", baseFiles()))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Error("identical packages in different directories must have the same digest")
	}
	c, err := Loader{}.Load(writePackage(t, root, "c", with(baseFiles(), map[string]string{"analyst.md": "changed"})))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == c.Digest {
		t.Error("changed content must change the digest")
	}
	d, err := Loader{}.Load(writePackage(t, root, "d", with(baseFiles(), map[string]string{"extra/analyst.md": "guidance"})))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == d.Digest {
		t.Error("an added file must change the digest")
	}
}

func TestCheckParamValue(t *testing.T) {
	min, max := 1.0, 10.0
	cases := []struct {
		p    Param
		v    any
		fail bool
	}{
		{Param{Type: "string"}, "x", false},
		{Param{Type: "string"}, 1.0, true},
		{Param{Type: "string", Pattern: "^[a-z]+$"}, "ABC", true},
		{Param{Type: "string", MaxLength: 2}, "abc", true},
		{Param{Type: "integer", Minimum: &min, Maximum: &max}, 5.0, false},
		{Param{Type: "integer"}, 1.5, true},
		{Param{Type: "integer", Minimum: &min}, 0.0, true},
		{Param{Type: "integer", Maximum: &max}, 11.0, true},
		{Param{Type: "number"}, 1.5, false},
		{Param{Type: "boolean"}, true, false},
		{Param{Type: "boolean"}, "true", true},
		{Param{Type: "string", Enum: []any{"a", "b"}}, "c", true},
		{Param{Type: "integer", Enum: []any{1.0, 2.0}}, 2.0, false},
	}
	for _, c := range cases {
		if err := CheckParamValue(c.p, c.v); (err != nil) != c.fail {
			t.Errorf("CheckParamValue(%+v, %v) = %v, want failure %v", c.p, c.v, err, c.fail)
		}
	}
}

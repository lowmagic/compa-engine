package expert

import (
	"errors"
	"strings"
	"testing"
)

const catalogsRoot = "../../testdata/catalogs"

const catalogManifest = `apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Catalog
metadata: {id: x.core, version: 0.1.0}
spec:
  engineApi: '>=0.2.0 <0.3.0'
  binaries: [omc]
  tests: [x.core.pods]
`

const catalogTests = `apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Tests
tests:
  - id: x.core.pods
    version: 0.1.0
    description: Pods in one namespace, as JSON.
    params: {ns: {type: string, default: default}}
    run:
      - [omc, get, pods, -n, '{{ns}}', -o, json]
    fixtures: fixtures/pods
`

func catalogFiles() map[string]string {
	return map[string]string{"catalog.yaml": catalogManifest, "tests.yaml": catalogTests, "fixtures/pods/README.md": "x"}
}

// withNodesTool adds a second tool, x.core.nodes, to catalog files.
func withNodesTool(files map[string]string) map[string]string {
	return with(files, map[string]string{
		"catalog.yaml":             strings.Replace(files["catalog.yaml"], "[x.core.pods]", "[x.core.pods, x.core.nodes]", 1),
		"tests.yaml":               files["tests.yaml"] + "  - id: x.core.nodes\n    version: 0.1.0\n    description: Node resource files.\n    run:\n      - find: {path: nodes}\n    fixtures: fixtures/nodes\n",
		"fixtures/nodes/README.md": "x",
	})
}

const v2Manifest = `apiVersion: compa.lowmag.cc/expert/v1alpha3
kind: Expert
metadata: {id: x.storage, version: 0.1.0, summary: A test expert}
spec:
  engineApi: '>=0.2.0 <0.3.0'
  analyst: analyst.md
  catalogs:
    - {id: x.core, version: '>=0.1.0 <0.2.0', tools: [pods]}
  tests: [storage.one]
`

const v2Tests = `apiVersion: compa.lowmag.cc/expert/v1alpha2
kind: Tests
tests:
  - id: storage.one
    version: 0.1.0
    run:
      - find: {path: nodes}
`

func v2Files() map[string]string {
	return map[string]string{"expert.yaml": v2Manifest, "tests.yaml": v2Tests, "analyst.md": "Read x.core.pods for restarts."}
}

func TestDiscoverV1alpha2Fixtures(t *testing.T) {
	idx := Loader{}.Discover([]Root{{Path: catalogsRoot, Source: Bundled}})
	if len(idx.Errors) > 0 || len(idx.Warnings) > 0 {
		t.Fatalf("errors %v, warnings %v", idx.Errors, idx.Warnings)
	}
	var got []string
	for _, d := range idx.List() {
		got = append(got, d.Kind+":"+d.ID+"@"+d.Version)
	}
	want := "Catalog:fixture.core@0.2.0 Catalog:fixture.core@0.1.1 Catalog:fixture.core@0.1.0 Expert:fixture.network@0.1.0 Expert:fixture.storage@0.1.0"
	if strings.Join(got, " ") != want {
		t.Fatalf("List() = %s\nwant      %s", strings.Join(got, " "), want)
	}
	e, err := idx.Resolve("fixture.storage", "")
	if err != nil {
		t.Fatal(err)
	}
	if ids := e.Manifest.Spec.Catalogs[0].ToolIDs(); strings.Join(ids, " ") != "fixture.core.node_ready fixture.core.degraded_operators" {
		t.Errorf("ToolIDs() = %v", ids)
	}
	if _, err := idx.Resolve("fixture.core", ""); err == nil || !strings.Contains(err.Error(), "is a tool catalog") {
		t.Errorf("resolving a catalog as an expert: %v", err)
	}
}

func TestLoadCatalog(t *testing.T) {
	pkg, err := Loader{}.Load(writePackage(t, t.TempDir(), "c", catalogFiles()))
	if err != nil {
		t.Fatal(err)
	}
	if !pkg.IsCatalog() || pkg.Analyst != "" || len(pkg.Tests) != 1 || len(pkg.Warnings) != 0 {
		t.Errorf("unexpected catalog %+v", pkg)
	}
	// A tool's summary, one line offered in place of its description.
	files := with(catalogFiles(), map[string]string{"tests.yaml": strings.Replace(catalogTests, "    description: Pods in one namespace, as JSON.\n", "    description: Pods in one namespace, as JSON.\n    summary: Pods in a namespace.\n", 1)})
	pkg, err = Loader{}.Load(writePackage(t, t.TempDir(), "c", files))
	if err != nil || pkg.Tests[0].Summary != "Pods in a namespace." {
		t.Errorf("summary: %v %+v", err, pkg)
	} // The kinds of capture its tools read, with their marks.
	files = with(catalogFiles(), map[string]string{"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [cluster-scoped-resources]}]\n"})
	pkg, err = Loader{}.Load(writePackage(t, t.TempDir(), "c", files))
	if err != nil || len(pkg.Manifest.Spec.Captures) != 1 || pkg.Manifest.Spec.Captures[0].Marks[0] != "cluster-scoped-resources" {
		t.Errorf("captures: %v %+v", err, pkg)
	}
	// A kind's identity tool, which takes the attachment's directory.
	files = with(catalogFiles(), map[string]string{
		"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [cluster-scoped-resources], identity: pods}]\n",
		"tests.yaml":   strings.Replace(catalogTests, "{ns: {type: string, default: default}}", "{dir: {type: string}, ns: {type: string, default: default}}", 1),
	})
	pkg, err = Loader{}.Load(writePackage(t, t.TempDir(), "c", files))
	if err != nil || pkg.Manifest.Spec.Captures[0].Identity != "pods" {
		t.Errorf("identity: %v %+v", err, pkg)
	}
}

// TestLoadV1alpha2Rejects checks that each breach of a catalog or v1alpha2
// rule gives exactly one problem, naming the file and the field.
func TestLoadV1alpha2Rejects(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		changes map[string]string
		want    string
	}{
		{"catalog range malformed", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "'>=0.1.0 <0.2.0'", "'^0.1'", 1)}, "expert.yaml: spec.catalogs[0].version:"},
		{"catalog range without an upper bound", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "'>=0.1.0 <0.2.0'", "'>=0.1.0'", 1)}, `expert.yaml: spec.catalogs[0].version: range ">=0.1.0" has no upper bound`},
		{"catalog range across a breaking minor", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "'>=0.1.0 <0.2.0'", "'>=0.1.0 <1.0.0'", 1)}, `expert.yaml: spec.catalogs[0].version: range ">=0.1.0 <1.0.0" admits versions from 0.2.0`},
		{"catalog range over two minors", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "'>=0.1.0 <0.2.0'", "'>=0.1.0 <0.2.0 || >=0.2.0 <0.3.0'", 1)}, "expert.yaml: spec.catalogs[0].version: range \">=0.1.0 <0.2.0 || >=0.2.0 <0.3.0\" spans the lines"},
		{"catalog listed twice", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "tools: [pods]}\n", "tools: [pods]}\n    - {id: x.core, version: '>=0.1.0 <0.2.0', tools: [nodes]}\n", 1)}, "expert.yaml: spec.catalogs[1].id: catalog x.core is already listed at spec.catalogs[0]"},
		{"own test under a catalog's ID", v2Files(), map[string]string{
			"expert.yaml": strings.Replace(v2Manifest, "[storage.one]", "[x.core.mine]", 1),
			"tests.yaml":  strings.Replace(v2Tests, "storage.one", "x.core.mine", 1),
		}, "expert.yaml: spec.tests: x.core.mine is under the ID of catalog x.core"},
		{"tests file of another version", v2Files(), map[string]string{"tests.yaml": strings.Replace(v2Tests, "v1alpha2", "v1alpha1", 1)}, "tests.yaml: does not conform to compa.lowmag.cc/expert/tests/v1alpha2: /apiVersion"},
		{"unknown apiVersion", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "v1alpha3", "v1alpha4", 1)}, `expert.yaml: apiVersion: "compa.lowmag.cc/expert/v1alpha4" is not one this engine reads (compa.lowmag.cc/expert/v1alpha3)`},
		{"expert at v1alpha2", v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "v1alpha3", "v1alpha2", 1)}, `expert.yaml: apiVersion: "compa.lowmag.cc/expert/v1alpha2" is not one this engine reads (compa.lowmag.cc/expert/v1alpha3)`},
		{"manifest not a mapping", v2Files(), map[string]string{"expert.yaml": "just text\n"}, `expert.yaml: apiVersion: "" is not one this engine reads`},
		{"catalog at v1alpha1", catalogFiles(), map[string]string{"catalog.yaml": strings.Replace(catalogManifest, "v1alpha2", "v1alpha1", 1)}, `catalog.yaml: apiVersion: "compa.lowmag.cc/expert/v1alpha1" is not one this engine reads (compa.lowmag.cc/expert/v1alpha2)`},
		{"catalog with analyst text", catalogFiles(), map[string]string{"catalog.yaml": catalogManifest + "  analyst: analyst.md\n", "analyst.md": "x"}, "catalog.yaml: does not conform to compa.lowmag.cc/expert/catalog/v1alpha2: /spec: additional properties 'analyst' not allowed"},
		{"catalog and expert manifests", catalogFiles(), map[string]string{"expert.yaml": v2Manifest}, "expert.yaml and catalog.yaml: a package is an expert or a catalog, not both"},
		{"catalog engine API excludes the engine's", catalogFiles(), map[string]string{"catalog.yaml": strings.Replace(catalogManifest, "'>=0.2.0 <0.3.0'", "'>=0.1.0 <0.1.1'", 1)}, "spec.engineApi: \">=0.1.0 <0.1.1\" does not include engine API " + EngineAPIVersion},
		{"catalog names a model", catalogFiles(), map[string]string{"catalog.yaml": strings.Replace(catalogManifest, "version: 0.1.0}", "version: 0.1.0, title: Tools for Claude}", 1)}, `catalog.yaml.metadata.title: names a reasoning harness, model provider or model ("claude")`},
		{"tool outside the catalog's ID", catalogFiles(), map[string]string{
			"catalog.yaml": strings.Replace(catalogManifest, "[x.core.pods]", "[other.pods]", 1),
			"tests.yaml":   strings.Replace(catalogTests, "id: x.core.pods", "id: other.pods", 1),
		}, "tests.yaml: other.pods: id: a tool's ID is the catalog ID and one more segment, x.core.<name>"},
		{"tool two segments under the catalog", catalogFiles(), map[string]string{
			"catalog.yaml": strings.Replace(catalogManifest, "[x.core.pods]", "[x.core.pods.all]", 1),
			"tests.yaml":   strings.Replace(catalogTests, "id: x.core.pods", "id: x.core.pods.all", 1),
		}, "tests.yaml: x.core.pods.all: id: a tool's ID is the catalog ID and one more segment"},
		{"tool without description", catalogFiles(), map[string]string{"tests.yaml": strings.Replace(catalogTests, "    description: Pods in one namespace, as JSON.\n", "", 1)}, "tests.yaml: x.core.pods: description: missing"},
		{"tool without fixtures", catalogFiles(), map[string]string{"tests.yaml": strings.Replace(catalogTests, "    fixtures: fixtures/pods\n", "", 1)}, "tests.yaml: x.core.pods: fixtures: missing"},
		{"tool with interpret", catalogFiles(), map[string]string{"tests.yaml": catalogTests + "    interpret: analyst.md#pods\n"}, "tests.yaml: x.core.pods: interpret: not allowed in a catalog"},
		{"tool summary over a line", catalogFiles(), map[string]string{"tests.yaml": strings.Replace(catalogTests, "    description: Pods in one namespace, as JSON.\n", "    description: Pods in one namespace, as JSON.\n    summary: \""+strings.Repeat("x", 121)+"\"\n", 1)}, "tests.yaml: does not conform to compa.lowmag.cc/expert/tests/v1alpha2: /tests/0/summary"},
		{"capture mark a path", catalogFiles(), map[string]string{"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [a/b]}]\n"}, "catalog.yaml: does not conform to compa.lowmag.cc/expert/catalog/v1alpha2: /spec/captures/0/marks/0"},
		{"capture identity names no tool", catalogFiles(), map[string]string{"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [x], identity: absent}]\n"}, "catalog.yaml: spec.captures[0].identity: absent is not a tool of catalog x.core"},
		{"capture identity a full ID", catalogFiles(), map[string]string{"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [x], identity: x.core.pods}]\n"}, "catalog.yaml: does not conform to compa.lowmag.cc/expert/catalog/v1alpha2: /spec/captures/0/identity"},
		{"capture identity takes no dir", catalogFiles(), map[string]string{"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [x], identity: pods}]\n"}, "catalog.yaml: spec.captures[0].identity: tool x.core.pods takes no string parameter dir"},
		{"capture identity needs another parameter", catalogFiles(), map[string]string{
			"catalog.yaml": catalogManifest + "  captures: [{kind: must-gather, marks: [x], identity: pods}]\n",
			"tests.yaml":   strings.Replace(catalogTests, "{ns: {type: string, default: default}}", "{dir: {type: string}, ns: {type: string}}", 1),
		}, "catalog.yaml: spec.captures[0].identity: tool x.core.pods requires parameter ns; an identity tool is given only dir"},
		{"tool binary undeclared", catalogFiles(), map[string]string{"catalog.yaml": strings.Replace(catalogManifest, "  binaries: [omc]\n", "", 1)}, `tests.yaml: x.core.pods: run[0]: binary "omc" is not declared in spec.binaries`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writePackage(t, t.TempDir(), "p", with(c.files, c.changes))
			_, err := Loader{}.Load(dir)
			var le *LoadError
			if !errors.As(err, &le) {
				t.Fatalf("want *LoadError, got %v", err)
			}
			if len(le.Problems) != 1 || !strings.Contains(le.Problems[0], c.want) {
				t.Fatalf("want one problem containing %q, got:\n%s", c.want, strings.Join(le.Problems, "\n"))
			}
		})
	}
}

func TestLoadWarnsOnUnexplainedTool(t *testing.T) {
	m := strings.Replace(v2Manifest, "tools: [pods]", "tools: [pods, nodes]", 1)
	for analyst, want := range map[string][]string{
		"Read x.core.pods for restarts.":                      {"x.core.nodes"},
		"See x.core.pods_all and x.core.nodes.":               {"x.core.pods"},
		"Neither tool. Not x.core.pods.extra or Xx.core.pods": {"x.core.pods", "x.core.nodes"},
		"x.core.pods, then x.core.nodes.":                     nil,
	} {
		files := with(v2Files(), map[string]string{"expert.yaml": m, "analyst.md": analyst})
		pkg, err := Loader{}.Load(writePackage(t, t.TempDir(), "p", files))
		if err != nil {
			t.Fatal(err)
		}
		if len(pkg.Warnings) != len(want) {
			t.Errorf("%q: warnings %v, want one for each of %v", analyst, pkg.Warnings, want)
			continue
		}
		for i, id := range want {
			if !strings.Contains(pkg.Warnings[i], "analyst.md: does not mention "+id+", which spec.catalogs lists") {
				t.Errorf("%q: warning %q, want one naming %s", analyst, pkg.Warnings[i], id)
			}
		}
	}

	root := t.TempDir()
	writePackage(t, root, "storage", with(v2Files(), map[string]string{"expert.yaml": m}))
	writePackage(t, root, "core", withNodesTool(catalogFiles()))
	idx := Loader{}.Discover([]Root{{Path: root}})
	if len(idx.Errors) > 0 {
		t.Fatal(idx.Errors)
	}
	if len(idx.Warnings) != 1 || !strings.Contains(idx.Warnings[0], "expert x.storage 0.1.0 at ") || !strings.Contains(idx.Warnings[0], "does not mention x.core.nodes") {
		t.Errorf("warnings = %v", idx.Warnings)
	}
	if _, err := idx.Resolve("x.storage", ""); err != nil {
		t.Errorf("an expert with an unexplained tool must still resolve: %v", err)
	}
}

// TestDiscoverIDClashWithDuplicates checks that a clash between kinds
// drops the ID whatever duplicate copies of either kind discovery rejected
// first.
func TestDiscoverIDClashWithDuplicates(t *testing.T) {
	expertAsCore := with(baseFiles(), map[string]string{"expert.yaml": strings.Replace(baseManifest, "id: test.expert", "id: x.core", 1)})
	for _, order := range []string{"EC", "CE", "ECC", "CEE", "EEC", "CCE"} {
		root := t.TempDir()
		for i, kind := range order {
			files := expertAsCore
			if kind == 'C' {
				files = catalogFiles()
			}
			writePackage(t, root, string(rune('a'+i)), files)
		}
		idx := Loader{}.Discover([]Root{{Path: root}})
		clash := 0
		for _, err := range idx.Errors {
			if strings.Contains(err.Error(), "x.core is the ID of an expert") {
				clash++
			}
		}
		if clash != 1 {
			t.Errorf("%s: want the clash reported once, got errors %v", order, idx.Errors)
		}
		for _, d := range idx.List() {
			if d.ID == "x.core" {
				t.Errorf("%s: %s %s is still listed", order, d.Kind, d.Version)
			}
		}
	}
}

func TestDiscoverIDClashBetweenKinds(t *testing.T) {
	expertAsCore := with(v2Files(), map[string]string{"expert.yaml": strings.Replace(v2Manifest, "id: x.storage, version: 0.1.0", "id: x.core, version: 0.2.0", 1)})
	expertAsCore["expert.yaml"] = strings.Replace(expertAsCore["expert.yaml"], "  catalogs:\n    - {id: x.core, version: '>=0.1.0 <0.2.0', tools: [pods]}\n", "", 1)
	expertAsCore["analyst.md"] = "guidance"

	root := t.TempDir()
	writePackage(t, root, "catalog", catalogFiles())
	writePackage(t, root, "expert", expertAsCore)
	writePackage(t, root, "other", baseFiles())
	bundled, local := t.TempDir(), t.TempDir()
	writePackage(t, bundled, "catalog", catalogFiles())
	writePackage(t, local, "expert", with(expertAsCore, map[string]string{"expert.yaml": strings.Replace(expertAsCore["expert.yaml"], "version: 0.2.0", "version: 0.1.0", 1)}))

	for name, roots := range map[string][]Root{
		"different versions, one root":     {{Path: root}},
		"same version, local over bundled": {{Path: bundled, Source: Bundled}, {Path: local, Source: Local}},
	} {
		idx := Loader{}.Discover(roots)
		if len(idx.Errors) != 1 || !strings.Contains(idx.Errors[0].Error(), "x.core is the ID of an expert") || !strings.Contains(idx.Errors[0].Error(), "neither is used") {
			t.Errorf("%s: errors = %v", name, idx.Errors)
		}
		for _, d := range idx.List() {
			if d.ID == "x.core" {
				t.Errorf("%s: %s %s is still listed", name, d.Kind, d.Version)
			}
		}
		if _, err := idx.Resolve("x.core", ""); err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Errorf("%s: resolve = %v", name, err)
		}
	}
}

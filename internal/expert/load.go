package expert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lowmagic/compa-engine/internal/schema"
	"github.com/lowmagic/compa-engine/internal/semver"
)

// EngineAPIVersion is the Expert API version this engine implements. An
// expert's engineApi range must contain it. Compatible additions before 1.0
// take a patch version, so existing <0.3.0 ranges still include it: 0.2.1
// adds a catalog capture's identity tool; 0.2.2 adds expert reference
// files read on request.
const EngineAPIVersion = "0.2.2"

// ReservedTestPrefix begins the IDs of the engine's own tools, offered
// beside every expert's tests since engine API 0.1.2; no package may define
// a test under it.
const ReservedTestPrefix = "evidence."

const (
	ManifestFile  = "expert.yaml"
	CatalogFile   = "catalog.yaml"
	TestsFileName = "tests.yaml"
)

const (
	// APIVersionExpert is an expert manifest's apiVersion.
	APIVersionExpert = "compa.lowmag.cc/expert/v1alpha3"
	// APIVersionV1alpha2 is a catalog's and a tests file's apiVersion.
	APIVersionV1alpha2 = "compa.lowmag.cc/expert/v1alpha2"
	KindExpert         = "Expert"
	KindCatalog        = "Catalog"
)

// schemas maps a manifest file and its apiVersion to the schemas of the
// manifest and its tests.yaml, which is at v1alpha2 for both kinds.
var schemas = map[string]map[string][2]schema.ID{
	ManifestFile: {
		APIVersionExpert: {schema.ExpertManifestV1alpha3, schema.ExpertTestsV1alpha2},
	},
	CatalogFile: {
		APIVersionV1alpha2: {schema.ExpertCatalog, schema.ExpertTestsV1alpha2},
	},
}

// Limits bound what a package may contain, so a malformed or hostile
// package cannot exhaust memory or disk while it is loaded.
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxDepth      int
}

// DefaultLimits are generous for text packages with small fixtures.
var DefaultLimits = Limits{MaxFiles: 2000, MaxFileBytes: 4 << 20, MaxTotalBytes: 32 << 20, MaxDepth: 16}

const (
	// ReferenceWarnBytes is the soft bound past which Loader.Load warns
	// on a declared reference file (compa expert check).
	ReferenceWarnBytes = 32 << 10
	// ReferenceMaxBytes is the hard bound past which read_reference
	// refuses a reference file.
	ReferenceMaxBytes = 64 << 10
)

// Package is a loaded and validated expert or tool catalog. A catalog has
// no analyst text or references.
type Package struct {
	Dir      string
	Manifest Manifest
	Tests    []Test
	Analyst  string
	// Warnings are problems that do not stop the package loading.
	Warnings []string
	// References maps each declared reference path to its text.
	References map[string]string
	// Digest is "sha256:<hex>" over every file's relative path and content.
	Digest string
	// Files lists every file in the package, relative and slash-separated.
	Files []string
}

func (p *Package) ID() string      { return p.Manifest.Metadata.ID }
func (p *Package) Version() string { return p.Manifest.Metadata.Version }
func (p *Package) IsCatalog() bool { return p.Manifest.Kind == KindCatalog }

// kind names the package's kind in messages.
func (p *Package) kind() string {
	if p.IsCatalog() {
		return "catalog"
	}
	return "expert"
}

func (p *Package) Test(id string) (Test, bool) {
	for _, t := range p.Tests {
		if t.ID == id {
			return t, true
		}
	}
	return Test{}, false
}

// LoadError lists every problem found in one package.
type LoadError struct {
	Dir      string
	Problems []string
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("expert package %s: %s", e.Dir, strings.Join(e.Problems, "; "))
}

// StageChecker validates one pipeline stage beyond what the schema checks,
// such as a primitive's parameter names and types. The collection package
// supplies it; the loader runs it on every stage of every test.
type StageChecker func(Stage) error

type Loader struct {
	Limits     Limits
	CheckStage StageChecker
}

type loadState struct {
	dir      string
	problems []string
	files    map[string]bool
	dirs     map[string]bool
}

func (s *loadState) addf(format string, args ...any) {
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

// Load reads and validates the package rooted at dir. It never executes
// anything in the package.
func (l Loader) Load(dir string) (*Package, error) {
	limits := l.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	st := &loadState{dir: dir, files: map[string]bool{}, dirs: map[string]bool{".": true}}
	fail := func() (*Package, error) { return nil, &LoadError{Dir: dir, Problems: st.problems} }

	info, err := os.Lstat(dir)
	if err != nil {
		st.addf("%v", err)
		return fail()
	}
	if !info.IsDir() {
		st.addf("not a directory")
		return fail()
	}
	digest, files := st.walk(limits)
	if len(st.problems) > 0 {
		return fail()
	}

	pkg := &Package{Dir: dir, Digest: digest, Files: files, References: map[string]string{}}
	file := ManifestFile
	if st.files[CatalogFile] {
		if st.files[ManifestFile] {
			st.addf("%s and %s: a package is an expert or a catalog, not both", ManifestFile, CatalogFile)
			return fail()
		}
		file = CatalogFile
	}
	manifestDoc, ok := st.decodeManifest(file, &pkg.Manifest)
	if !ok {
		return fail()
	}
	ids := schemas[file][pkg.Manifest.APIVersion]
	st.checkManifest(pkg, file, manifestDoc)
	if st.files[TestsFileName] {
		var tf TestsFile
		if _, ok := st.decode(TestsFileName, ids[1], &tf); !ok {
			// Its problem is reported; every listed test would otherwise be
			// reported again as undefined.
			return fail()
		}
		pkg.Tests = tf.Tests
	}
	st.checkTests(pkg, l.CheckStage)
	if len(st.problems) > 0 {
		return fail()
	}
	if !pkg.IsCatalog() {
		pkg.Analyst = st.read(pkg.Manifest.Spec.Analyst)
	}
	for _, ref := range pkg.Manifest.Spec.References {
		before := len(st.problems)
		data := st.read(ref.Path)
		if len(st.problems) == before {
			switch {
			case !utf8.ValidString(data):
				st.addf("spec.references: %q is not valid UTF-8 text", ref.Path)
			case strings.TrimSpace(data) == "":
				st.addf("spec.references: %q is empty", ref.Path)
			}
		}
		pkg.References[ref.Path] = data
	}
	if len(st.problems) > 0 {
		return fail()
	}
	pkg.Warnings = append(pkg.Warnings, unexplainedTools(pkg)...)
	pkg.Warnings = append(pkg.Warnings, unexplainedReferences(pkg)...)
	pkg.Warnings = append(pkg.Warnings, referenceWarnings(pkg)...)
	return pkg, nil
}

// decodeManifest selects the manifest's schema by its file and apiVersion,
// so a package newer than the engine is reported as such rather than as a
// confusing schema error.
func (st *loadState) decodeManifest(file string, m *Manifest) (any, bool) {
	if !st.files[file] {
		st.addf("%s: missing", file)
		return nil, false
	}
	doc, err := schema.DecodeYAML([]byte(st.read(file)))
	if err != nil {
		st.addf("%s: %v", file, err)
		return nil, false
	}
	top, _ := doc.(map[string]any)
	version, _ := top["apiVersion"].(string)
	ids, ok := schemas[file][version]
	if !ok {
		known := make([]string, 0, len(schemas[file]))
		for v := range schemas[file] {
			known = append(known, v)
		}
		sort.Strings(known)
		st.addf("%s: apiVersion: %q is not one this engine reads (%s)", file, version, strings.Join(known, ", "))
		return nil, false
	}
	return st.validate(file, ids[0], doc, m)
}

var (
	idToken   = regexp.MustCompile(`[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*`)
	pathToken = regexp.MustCompile(`[A-Za-z0-9_./-]+`)
)

// unexplainedTools warns about each catalog tool the expert lists that its
// analyst text never names by full ID: the analyst text is where a tool's
// output is given its meaning for this expert. IDs are matched as whole
// dotted tokens, so naming fixture.core.pods_all does not explain
// fixture.core.pods.
func unexplainedTools(pkg *Package) []string {
	if len(pkg.Manifest.Spec.Catalogs) == 0 {
		return nil
	}
	named := map[string]bool{}
	for _, tok := range idToken.FindAllString(pkg.Analyst, -1) {
		named[tok] = true
	}
	var warnings []string
	for _, ref := range pkg.Manifest.Spec.Catalogs {
		for _, id := range ref.ToolIDs() {
			if !named[id] {
				warnings = append(warnings, fmt.Sprintf("%s: does not mention %s, which spec.catalogs lists; explain what its output means for this expert, naming it by that ID", pkg.Manifest.Spec.Analyst, id))
			}
		}
	}
	return warnings
}

// unexplainedReferences warns about each declared reference whose path
// analyst text never names: the analyst text says when a reference is
// worth reading. Paths are matched as whole path tokens (with trailing
// sentence periods trimmed), so foo/refs/a.md or refs/a.md.old do not
// satisfy refs/a.md.
func unexplainedReferences(pkg *Package) []string {
	if len(pkg.Manifest.Spec.References) == 0 {
		return nil
	}
	named := map[string]bool{}
	for _, tok := range pathToken.FindAllString(pkg.Analyst, -1) {
		named[strings.TrimRight(tok, ".")] = true
	}
	var warnings []string
	for _, ref := range pkg.Manifest.Spec.References {
		if !named[ref.Path] {
			warnings = append(warnings, fmt.Sprintf("%s: does not mention %s, which spec.references lists; say when to read it, naming it by that path", pkg.Manifest.Spec.Analyst, ref.Path))
		}
	}
	return warnings
}

// referenceWarnings warns about declared reference files past the soft
// or hard size bounds.
func referenceWarnings(pkg *Package) []string {
	var warnings []string
	for _, ref := range pkg.Manifest.Spec.References {
		n := len(pkg.References[ref.Path])
		switch {
		case n > ReferenceMaxBytes:
			warnings = append(warnings, fmt.Sprintf("%s: %d bytes exceeds the %d-byte reference limit; read_reference will refuse it", ref.Path, n, ReferenceMaxBytes))
		case n > ReferenceWarnBytes:
			warnings = append(warnings, fmt.Sprintf("%s: %d bytes exceeds the %d-byte soft reference bound; consider splitting it", ref.Path, n, ReferenceWarnBytes))
		}
	}
	return warnings
}

// walk visits every entry, enforcing limits and rejecting symlinks and
// special files, and computes the content digest. Each file contributes its
// path, a NUL, its length, a NUL and its content, in sorted path order.
func (st *loadState) walk(limits Limits) (string, []string) {
	var files []string
	var total int64
	err := filepath.WalkDir(st.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(st.dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.Count(rel, "/")+1 > limits.MaxDepth {
			st.addf("%s: deeper than %d levels", rel, limits.MaxDepth)
			return filepath.SkipDir
		}
		switch t := d.Type(); {
		case t&fs.ModeSymlink != 0:
			st.addf("%s: symlinks are not allowed", rel)
			return nil
		case t.IsDir():
			st.dirs[rel] = true
			return nil
		case !t.IsRegular():
			st.addf("%s: not a regular file", rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > limits.MaxFileBytes {
			st.addf("%s: %d bytes exceeds the %d-byte file limit", rel, info.Size(), limits.MaxFileBytes)
		}
		total += info.Size()
		files = append(files, rel)
		st.files[rel] = true
		return nil
	})
	if err != nil {
		st.addf("%v", err)
	}
	if len(files) > limits.MaxFiles {
		st.addf("%d files exceeds the %d-file limit", len(files), limits.MaxFiles)
	}
	if total > limits.MaxTotalBytes {
		st.addf("%d bytes exceeds the %d-byte package limit", total, limits.MaxTotalBytes)
	}
	if len(st.problems) > 0 {
		return "", nil
	}
	sort.Strings(files)
	digest, err := digestFiles(st.dir, files, limits.MaxFileBytes)
	if err != nil {
		st.addf("%v", err)
	}
	return digest, files
}

func digestFiles(dir string, files []string, maxFile int64) (string, error) {
	h := sha256.New()
	for _, rel := range files {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		info, err := f.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("%s: not a regular file", rel)
		}
		if err == nil {
			fmt.Fprintf(h, "%s\x00%d\x00", rel, info.Size())
			var n int64
			n, err = io.Copy(h, io.LimitReader(f, maxFile+1))
			if err == nil && n != info.Size() {
				err = fmt.Errorf("%s: changed while it was read", rel)
			}
		}
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (st *loadState) read(rel string) string {
	data, err := os.ReadFile(filepath.Join(st.dir, filepath.FromSlash(rel)))
	if err != nil {
		st.addf("%v", err)
	}
	return string(data)
}

// decode reads a YAML file, validates it against id and decodes it into v.
// It returns the generic document for further checks.
func (st *loadState) decode(rel string, id schema.ID, v any) (any, bool) {
	if !st.files[rel] {
		st.addf("%s: missing", rel)
		return nil, false
	}
	doc, err := schema.DecodeYAML([]byte(st.read(rel)))
	if err != nil {
		st.addf("%s: %v", rel, err)
		return nil, false
	}
	return st.validate(rel, id, doc, v)
}

func (st *loadState) validate(rel string, id schema.ID, doc any, v any) (any, bool) {
	err := schema.Validate(id, doc)
	if err == nil {
		var j []byte
		if j, err = json.Marshal(doc); err == nil {
			err = json.Unmarshal(j, v)
		}
	}
	if err != nil {
		st.addf("%s: %v", rel, err)
		return nil, false
	}
	return doc, true
}

// packagePath checks that p is a clean relative path inside the package.
func (st *loadState) packagePath(field, p string) (string, bool) {
	clean := path.Clean(p)
	if path.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") {
		st.addf("%s: %q escapes the package", field, p)
		return "", false
	}
	if clean != strings.TrimSuffix(p, "/") {
		st.addf("%s: %q is not a normalized path", field, p)
		return "", false
	}
	return clean, true
}

func (st *loadState) requireFile(field, p string) {
	if clean, ok := st.packagePath(field, p); ok && !st.files[clean] {
		st.addf("%s: file %q not found", field, p)
	}
}

func (st *loadState) requireDir(field, p string) {
	if clean, ok := st.packagePath(field, p); ok && !st.dirs[clean] {
		st.addf("%s: directory %q not found", field, p)
	}
}

func (st *loadState) checkManifest(pkg *Package, file string, doc any) {
	spec := pkg.Manifest.Spec
	r, err := semver.ParseRange(spec.EngineAPI)
	if err != nil {
		st.addf("spec.engineApi: %v", err)
	} else if !r.Contains(semver.MustParse(EngineAPIVersion)) {
		st.addf("spec.engineApi: %q does not include engine API %s", spec.EngineAPI, EngineAPIVersion)
	}
	if !pkg.IsCatalog() {
		st.requireFile("spec.analyst", spec.Analyst)
	}
	st.checkCatalogRefs(file, spec)
	seenRefs := map[string]int{}
	for i, ref := range spec.References {
		where := fmt.Sprintf("spec.references[%d].path", i)
		if j, dup := seenRefs[ref.Path]; dup {
			st.addf("%s: %s: reference %q is already listed at spec.references[%d]", file, where, ref.Path, j)
			continue
		}
		seenRefs[ref.Path] = i
		switch ref.Path {
		case spec.Analyst:
			if spec.Analyst != "" {
				st.addf("%s: %s: %q is spec.analyst, not a reference file", file, where, ref.Path)
				continue
			}
		case ManifestFile, CatalogFile, TestsFileName:
			st.addf("%s: %s: %q is a package definition file, not a reference file", file, where, ref.Path)
			continue
		}
		st.requireFile(where, ref.Path)
	}
	for _, s := range spec.Routing.Signals {
		if s.Matches != "" {
			if _, err := regexp.Compile(s.Matches); err != nil {
				st.addf("spec.routing.signals: fact %s: %v", s.Fact, err)
			}
		}
	}
	if spec.MCP != nil {
		for _, tool := range spec.MCP.Tools {
			if tool.Risk != "read_only" {
				st.addf("spec.mcp.tools: %s: risk %q is not allowed; only read_only tools are accepted", tool.Name, tool.Risk)
			}
			for name, s := range map[string]map[string]any{"inputSchema": tool.InputSchema, "outputSchema": tool.OutputSchema} {
				if err := schema.CheckInline(s); err != nil {
					st.addf("spec.mcp.tools: %s: %s: %v", tool.Name, name, err)
				}
			}
		}
	}
	for _, hit := range namesHarnessOrModel(file, doc) {
		st.addf("%s: names a reasoning harness, model provider or model (%q); experts declare requirements, not harnesses or models", hit.path, hit.token)
	}
}

func (st *loadState) checkCatalogRefs(file string, spec Spec) {
	first := map[string]int{}
	for i, ref := range spec.Catalogs {
		where := fmt.Sprintf("%s: spec.catalogs[%d]", file, i)
		if j, dup := first[ref.ID]; dup {
			st.addf("%s.id: catalog %s is already listed at spec.catalogs[%d]; list each catalog once, with every tool used from it", where, ref.ID, j)
			continue
		}
		first[ref.ID] = i
		r, err := semver.ParseRange(ref.Version)
		if err == nil {
			err = r.CheckCompatible()
		}
		if err != nil {
			st.addf("%s.version: %v", where, err)
		}
	}
	for _, id := range spec.Tests {
		for _, ref := range spec.Catalogs {
			if id == ref.ID || strings.HasPrefix(id, ref.ID+".") {
				st.addf("%s: spec.tests: %s is under the ID of catalog %s, which spec.catalogs declares; the expert's own tests need IDs outside its catalogs", file, id, ref.ID)
				break
			}
		}
	}
}

func (st *loadState) checkTests(pkg *Package, check StageChecker) {
	declared := map[string]bool{}
	for _, id := range pkg.Manifest.Spec.Tests {
		declared[id] = true
	}
	if len(declared) > 0 && !st.files[TestsFileName] {
		st.addf("spec.tests: %s is missing", TestsFileName)
		return
	}
	binaries := map[string]bool{}
	for _, b := range pkg.Manifest.Spec.Binaries {
		binaries[b] = true
	}
	defined := map[string]bool{}
	for _, t := range pkg.Tests {
		where := "tests.yaml: " + t.ID
		if defined[t.ID] {
			st.addf("%s: defined more than once", where)
		}
		defined[t.ID] = true
		if !declared[t.ID] {
			st.addf("%s: not listed in spec.tests", where)
		}
		if strings.HasPrefix(t.ID, ReservedTestPrefix) {
			st.addf("%s: test IDs beginning %s are the engine's own tools; name the test otherwise", where, ReservedTestPrefix)
		}
		st.checkParams(where, t.Params)
		for i, stage := range t.Run {
			st.checkStage(fmt.Sprintf("%s: run[%d]", where, i), stage, t.Params, binaries, check)
		}
		for _, pre := range t.Preconditions {
			st.checkTemplates(where+": preconditions", pre.Exists, t.Params)
		}
		if pkg.IsCatalog() {
			st.checkTool(pkg.ID(), where, t)
		} else if t.Interpret != "" {
			st.requireFile(where+": interpret", strings.SplitN(t.Interpret, "#", 2)[0])
		}
		if t.Fixtures != "" {
			st.requireDir(where+": fixtures", t.Fixtures)
		}
	}
	for id := range declared {
		if !defined[id] {
			st.addf("spec.tests: %s is not defined in %s", id, TestsFileName)
		}
	}
	if pkg.IsCatalog() {
		st.checkIdentities(pkg)
	}
}

// checkIdentities applies the rules of a capture's identity tool: it is a
// tool of the same catalog, and it takes the attachment's directory as a
// string parameter dir and nothing else it needs, since the engine gives it
// only that.
func (s *loadState) checkIdentities(pkg *Package) {
	for i, c := range pkg.Manifest.Spec.Captures {
		if c.Identity == "" {
			continue
		}
		where := fmt.Sprintf("%s: spec.captures[%d].identity", CatalogFile, i)
		id := pkg.ID() + "." + c.Identity
		t, ok := pkg.Test(id)
		if !ok {
			s.addf("%s: %s is not a tool of catalog %s", where, c.Identity, pkg.ID())
			continue
		}
		if p, ok := t.Params[IdentityParam]; !ok || p.Type != "string" {
			s.addf("%s: tool %s takes no string parameter %s; an identity tool is given the attachment's directory as %s", where, id, IdentityParam, IdentityParam)
		}
		for _, name := range slices.Sorted(maps.Keys(t.Params)) {
			if name != IdentityParam && t.Params[name].Default == nil {
				s.addf("%s: tool %s requires parameter %s; an identity tool is given only %s", where, id, name, IdentityParam)
			}
		}
	}
}

// checkTool applies the rules a catalog's tool has beyond an expert's test.
// A tool's ID is its catalog's and one more segment, so two catalogs cannot
// define one tool. A catalog says what a tool returns, never what it means:
// that is each expert's analyst text, since the same output means
// different things to different experts.
func (st *loadState) checkTool(catalog, where string, t Test) {
	if name, ok := strings.CutPrefix(t.ID, catalog+"."); !ok || strings.Contains(name, ".") {
		st.addf("%s: id: a tool's ID is the catalog ID and one more segment, %s.<name>", where, catalog)
	}
	if t.Description == "" {
		st.addf("%s: description: missing; say what the tool collects and the shape of its output", where)
	}
	if t.Fixtures == "" {
		st.addf("%s: fixtures: missing; every tool has a fixtures directory", where)
	}
	if t.Interpret != "" {
		st.addf("%s: interpret: not allowed in a catalog; each expert's analyst text explains what a tool's output means", where)
	}
}

func (st *loadState) checkParams(where string, params map[string]Param) {
	for name, p := range params {
		w := where + ": params." + name
		if p.Pattern != "" {
			if _, err := regexp.Compile(p.Pattern); err != nil {
				st.addf("%s: pattern: %v", w, err)
				continue
			}
		}
		for _, e := range p.Enum {
			if err := CheckParamValue(p, e); err != nil {
				st.addf("%s: enum value %v: %v", w, e, err)
			}
		}
		if p.Default != nil {
			if err := CheckParamValue(p, p.Default); err != nil {
				st.addf("%s: default: %v", w, err)
			}
		}
	}
}

var templateRef = regexp.MustCompile(`^\{\{([a-z][a-z0-9_]*)\}\}$`)

// checkTemplates enforces whole-value substitution: a value is either
// exactly {{name}} for a declared parameter, or contains no "{{" at all.
func (st *loadState) checkTemplates(where, value string, params map[string]Param) {
	if m := templateRef.FindStringSubmatch(value); m != nil {
		if _, ok := params[m[1]]; !ok {
			st.addf("%s: %q refers to undeclared parameter %q", where, value, m[1])
		}
		return
	}
	if strings.Contains(value, "{{") {
		st.addf("%s: %q splices a parameter into a string; a parameter must be a whole value", where, value)
	}
}

func (st *loadState) checkStage(where string, s Stage, params map[string]Param, binaries map[string]bool, check StageChecker) {
	if s.Primitive == "" {
		if strings.Contains(s.Argv[0], "{{") {
			st.addf("%s: the binary cannot be a parameter", where)
		} else if !binaries[s.Argv[0]] {
			st.addf("%s: binary %q is not declared in spec.binaries", where, s.Argv[0])
		}
		for _, arg := range s.Argv[1:] {
			st.checkTemplates(where, arg, params)
		}
	} else {
		if s.Shorthand != nil {
			st.checkTemplates(where, *s.Shorthand, params)
		}
		walkStrings(s.Params, func(v string) { st.checkTemplates(where, v, params) })
	}
	if check != nil {
		if err := check(s); err != nil {
			st.addf("%s: %v", where, err)
		}
	}
}

func walkStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case map[string]any:
		for _, e := range x {
			walkStrings(e, fn)
		}
	case []any:
		for _, e := range x {
			walkStrings(e, fn)
		}
	}
}

func CheckParamValue(p Param, v any) error {
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want a string, got %T", v)
		}
		if p.MaxLength > 0 && len(s) > p.MaxLength {
			return fmt.Errorf("longer than %d", p.MaxLength)
		}
		if p.Pattern != "" {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				return err
			}
			if !re.MatchString(s) {
				return fmt.Errorf("%q does not match %s", s, p.Pattern)
			}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("want a boolean, got %T", v)
		}
	case "integer", "number":
		f, ok := number(v)
		if !ok {
			return fmt.Errorf("want a %s, got %T", p.Type, v)
		}
		if p.Type == "integer" && f != float64(int64(f)) {
			return fmt.Errorf("want an integer, got %v", v)
		}
		if p.Minimum != nil && f < *p.Minimum {
			return fmt.Errorf("%v is below the minimum %v", v, *p.Minimum)
		}
		if p.Maximum != nil && f > *p.Maximum {
			return fmt.Errorf("%v is above the maximum %v", v, *p.Maximum)
		}
	default:
		return fmt.Errorf("unknown type %q", p.Type)
	}
	if len(p.Enum) > 0 {
		for _, e := range p.Enum {
			if equalValues(e, v) {
				return nil
			}
		}
		return fmt.Errorf("%v is not one of %v", v, p.Enum)
	}
	return nil
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func equalValues(a, b any) bool {
	if fa, ok := number(a); ok {
		fb, ok := number(b)
		return ok && fa == fb
	}
	return a == b
}

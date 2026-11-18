// Package schema validates documents against the engine's embedded public
// schemas (package api).
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/lowmagic/compa-engine/api"
)

// ID names a schema by its versioned identifier, as documents carry it.
type ID string

const (
	// ExpertManifestV1alpha3 adds metadata.summary to the expert manifest
	// and removes spec.analysis and spec.evidence; catalogs and tests stay
	// at v1alpha2, whose manifest schema holds the definitions they share
	// (docs/EXPERT_API.md).
	ExpertManifestV1alpha3 ID = "compa.lowmag.cc/expert/manifest/v1alpha3"
	ExpertManifestV1alpha2 ID = "compa.lowmag.cc/expert/manifest/v1alpha2"
	ExpertTestsV1alpha2    ID = "compa.lowmag.cc/expert/tests/v1alpha2"
	ExpertScope            ID = "compa.lowmag.cc/expert/scope/v1alpha2"
	ExpertCatalog          ID = "compa.lowmag.cc/expert/catalog/v1alpha2"
	// ExpertIdentity is one line of a catalog's capture identity tool's
	// output, which the engine validates before the inventory uses it.
	ExpertIdentity ID = "compa.lowmag.cc/expert/identity/v1alpha2"
	// ExpertCases are one test's fixture cases, which compa expert check
	// runs (docs/EXPERT_API.md §Fixture cases).
	ExpertCases ID = "compa.lowmag.cc/expert/cases/v1alpha2"
	// ExpertCheck is compa expert check's report, read by the experts
	// repository's CI.
	ExpertCheck    ID = "compa.lowmag.cc/expert/check/v1alpha2"
	RoutingLibrary ID = "compa.lowmag.cc/routing/library/v1alpha1"
	RoutingTerms   ID = "compa.lowmag.cc/routing/terms/v1alpha1"
	Config         ID = "compa.lowmag.cc/config/config/v1alpha1"
	// LedgerUpdate is the case ledger's update, the arguments of the
	// ledger model's ledger_update call (docs/MODEL_API.md §The case
	// ledger).
	LedgerUpdate ID = "compa.lowmag.cc/ledger/update/v1alpha2"
	// CheckFindings is an epistemic check's findings, the arguments of the
	// editor's and the reviewer's check_findings call.
	CheckFindings ID = "compa.lowmag.cc/check/findings/v1alpha1"
)

const (
	schemaURLPrefix  = "https://"
	schemaFileSuffix = ".schema.json"
)

// All are every schema the engine validates against.
var All = []ID{ExpertManifestV1alpha3, ExpertManifestV1alpha2, ExpertTestsV1alpha2, ExpertCatalog, ExpertIdentity, ExpertCases, ExpertScope, ExpertCheck, RoutingLibrary, RoutingTerms, Config, LedgerUpdate, CheckFindings}

var (
	compileOnce sync.Once
	compiled    map[ID]*jsonschema.Schema
	compileErr  error
)

func compileAll() {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	// Resources are added before compiling so that cross-schema $refs
	// resolve to embedded documents and nothing is fetched.
	for _, id := range All {
		doc, err := embedded(id)
		if err != nil {
			compileErr = err
			return
		}
		if err := c.AddResource(schemaURLPrefix+string(id), doc); err != nil {
			compileErr = fmt.Errorf("schema %s: %w", id, err)
			return
		}
	}
	compiled = make(map[ID]*jsonschema.Schema, len(All))
	for _, id := range All {
		s, err := c.Compile(schemaURLPrefix + string(id))
		if err != nil {
			compileErr = fmt.Errorf("schema %s: %w", id, err)
			return
		}
		compiled[id] = s
	}
}

// embedded loads the schema file for id: compa.lowmag.cc/<area>/<kind>/<version>
// is stored at <area>/<version>/<kind>.schema.json.
func embedded(id ID) (any, error) {
	parts := strings.Split(string(id), "/")
	if len(parts) != 4 {
		return nil, fmt.Errorf("schema %s: malformed id", id)
	}
	name := path.Join(parts[1], parts[3], parts[2]+schemaFileSuffix)
	data, err := fs.ReadFile(api.Schemas, name)
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", id, err)
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

// Validate checks doc, a value decoded from JSON or by DecodeYAML, against
// the schema id. A validation failure is returned as *Error.
func Validate(id ID, doc any) error {
	compileOnce.Do(compileAll)
	if compileErr != nil {
		return compileErr
	}
	s, ok := compiled[id]
	if !ok {
		return fmt.Errorf("unknown schema %s", id)
	}
	if err := s.Validate(doc); err != nil {
		if ve, ok := err.(*jsonschema.ValidationError); ok {
			return newError(id, ve)
		}
		return err
	}
	return nil
}

func ValidateJSON(id ID, data []byte) error {
	doc, err := DecodeJSON(data)
	if err != nil {
		return err
	}
	return Validate(id, doc)
}

// DecodeJSON decodes data into the value form Validate expects, keeping
// numbers exact.
func DecodeJSON(data []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

// DecodeYAML decodes a single YAML document into the same value form as
// DecodeJSON, by way of JSON, so YAML-only types (timestamps, non-string
// keys) are rejected rather than validated in a shape JSON cannot express.
func DecodeYAML(data []byte) (any, error) {
	var v any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("yaml: more than one document")
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	return DecodeJSON(j)
}

// Error is a schema validation failure. Problems holds one line per failing
// leaf, as "<instance location>: <message>", sorted.
type Error struct {
	Schema   ID
	Problems []string
}

func (e *Error) Error() string {
	return Refusal(e.Schema) + strings.Join(e.Problems, "; ")
}

// Refusal begins an Error's message against id, before its problems.
func Refusal(id ID) string {
	return "does not conform to " + string(id) + ": "
}

func newError(id ID, ve *jsonschema.ValidationError) *Error {
	var problems []string
	collectLeaves(ve, &problems)
	sort.Strings(problems)
	return &Error{Schema: id, Problems: problems}
}

var printer = message.NewPrinter(language.English)

// collectLeaves reports the failures with no further causes; the inner
// nodes of the tree only say that a subschema failed.
func collectLeaves(ve *jsonschema.ValidationError, out *[]string) {
	if len(ve.Causes) == 0 {
		loc := "/" + strings.Join(ve.InstanceLocation, "/")
		*out = append(*out, loc+": "+ve.ErrorKind.LocalizedString(printer))
		return
	}
	for _, c := range ve.Causes {
		collectLeaves(c, out)
	}
}

// CheckInline compiles doc as a standalone JSON Schema, such as an expert
// MCP tool's declared input or output schema. References outside the
// document are not resolved and fail.
func CheckInline(doc map[string]any) error {
	_, err := CompileInline(doc)
	return err
}

func CompileInline(doc map[string]any) (*jsonschema.Schema, error) {
	j, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	v, err := DecodeJSON(j)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	const loc = "urn:compa:inline"
	if err := c.AddResource(loc, v); err != nil {
		return nil, err
	}
	return c.Compile(loc)
}

type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference %s is not allowed", url)
}

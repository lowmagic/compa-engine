package collect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// Paths are relative to the artifact
// root. Zero values mean the documented default.
type (
	findParams struct {
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		Type       string `json:"type"` // file (default), dir, any
		MinSize    int64  `json:"min_size"`
		MaxSize    int64  `json:"max_size"`
		NewerThan  string `json:"newer_than"` // RFC 3339
		OlderThan  string `json:"older_than"`
		MaxResults int    `json:"max_results"`
		// MinDepth and MaxDepth count from Path, an entry directly inside
		// it at depth 1, as find(1) does; 0 sets no limit.
		MinDepth int `json:"min_depth"`
		MaxDepth int `json:"max_depth"`
		// Details puts each entry's kind, size and modification time
		// before its path.
		Details bool `json:"details"`
	}
	grepParams struct {
		Pattern       string `json:"pattern"`
		Path          string `json:"path"`
		Glob          string `json:"glob"`
		IgnoreCase    bool   `json:"ignore_case"`
		ContextBefore int    `json:"context_before"`
		ContextAfter  int    `json:"context_after"`
		MaxMatches    int    `json:"max_matches"`
		// From is which matches max_matches keeps: start, the first
		// (default), or end, the last.
		From string `json:"from"`
		// Keep is how many lines from the start of each file are kept
		// alongside its matches, such as a table's header. 0 keeps none.
		Keep int `json:"keep"`
	}
	readParams struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Head      int    `json:"head"`
		Tail      int    `json:"tail"`
		StartByte int64  `json:"start_byte"`
		EndByte   int64  `json:"end_byte"`
	}
	queryParams struct { // jq and yq
		Filter string `json:"filter"`
		Path   string `json:"path"`
		Glob   string `json:"glob"`
		Raw    bool   `json:"raw"`
	}
	windowParams struct {
		Path   string `json:"path"`
		Start  string `json:"start"` // RFC 3339, inclusive
		End    string `json:"end"`   // RFC 3339, exclusive
		Layout string `json:"layout"`
		// Skip is how many characters of a line come before its stamp,
		// such as klog's level letter or an opening bracket.
		Skip int `json:"skip"`
		// Anchor dates stamps whose layout has no year: RFC 3339, usually
		// when the logs were captured.
		Anchor string `json:"anchor"`
		// Zone applies to stamps whose layout has no zone: an IANA name or
		// a fixed offset such as +05:30. UTC when empty.
		Zone string `json:"zone"`
	}
	countParams struct {
		Path      string `json:"path"`
		Pattern   string `json:"pattern"`
		MaxGroups int    `json:"max_groups"`
	}
	aggregateParams struct {
		Op    string `json:"op"` // count, count_by, unique
		Field string `json:"field"`
	}
	diffParams struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	archiveParams struct {
		Path       string `json:"path"`
		MaxEntries int    `json:"max_entries"`
	}
)

// primitiveSpec describes a primitive's parameters and its shorthand field:
// in YAML, `jq: '.items'` means {filter: '.items'}.
type primitiveSpec struct {
	params    func() any
	shorthand string
	required  []string
}

var primitives = map[string]primitiveSpec{
	"find":         {func() any { return &findParams{} }, "glob", nil},
	"grep":         {func() any { return &grepParams{} }, "pattern", []string{"pattern"}},
	"read":         {func() any { return &readParams{} }, "path", []string{"path"}},
	"jq":           {func() any { return &queryParams{} }, "filter", []string{"filter"}},
	"yq":           {func() any { return &queryParams{} }, "filter", []string{"filter"}},
	"logs.window":  {func() any { return &windowParams{} }, "", []string{"start", "end"}},
	"logs.count":   {func() any { return &countParams{} }, "pattern", nil},
	"aggregate":    {func() any { return &aggregateParams{} }, "op", []string{"op"}},
	"diff":         {func() any { return &diffParams{} }, "", []string{"a", "b"}},
	"archive.list": {func() any { return &archiveParams{} }, "path", []string{"path"}},
}

// Primitives lists the built-in primitive names, sorted.
func Primitives() []string {
	out := make([]string, 0, len(primitives))
	for name := range primitives {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// stageParams returns a primitive stage's parameters as a map, expanding
// the shorthand form.
func stageParams(s expert.Stage) (map[string]any, error) {
	spec, ok := primitives[s.Primitive]
	if !ok {
		return nil, fmt.Errorf("unknown primitive %q", s.Primitive)
	}
	if s.Shorthand != nil {
		if spec.shorthand == "" {
			return nil, fmt.Errorf("%s takes no shorthand; give its parameters as a map", s.Primitive)
		}
		return map[string]any{spec.shorthand: *s.Shorthand}, nil
	}
	if s.Params == nil {
		return map[string]any{}, nil
	}
	return s.Params, nil
}

// decodeParams decodes a parameter map into the primitive's typed struct,
// rejecting unknown names, wrong types, missing required parameters and
// values its check refuses.
func decodeParams(name string, m map[string]any) (any, error) {
	spec := primitives[name]
	for _, r := range spec.required {
		if v, ok := m[r]; !ok || v == "" {
			return nil, fmt.Errorf("%s: missing required parameter %q", name, r)
		}
	}
	j, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	p := spec.params()
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("%s: %v", name, strings.TrimPrefix(err.Error(), "json: "))
	}
	if c, ok := p.(interface{ check() error }); ok {
		if err := c.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return p, nil
}

// maxFindDepth bounds find's depth limits, far deeper than any capture's
// tree.
const maxFindDepth = 64

// maxGrepKeep bounds grep's kept leading lines per file, far more than
// a table header or file preamble.
const maxGrepKeep = 100

// check refuses a keep count out of range, so a package giving one fails
// to load and a run giving one runs nothing.
func (p *grepParams) check() error {
	if p.Keep < 0 || p.Keep > maxGrepKeep {
		return fmt.Errorf("keep must be from 0 to %d, not %d", maxGrepKeep, p.Keep)
	}
	return nil
}

// check refuses depth limits out of range, so a package giving one fails
// to load and a run giving one runs nothing.
func (p *findParams) check() error {
	for _, d := range []struct {
		name  string
		value int
	}{{"min_depth", p.MinDepth}, {"max_depth", p.MaxDepth}} {
		if d.value < 0 || d.value > maxFindDepth {
			return fmt.Errorf("%s must be from 0 to %d, not %d", d.name, maxFindDepth, d.value)
		}
	}
	if p.MaxDepth > 0 && p.MinDepth > p.MaxDepth {
		return fmt.Errorf("min_depth %d is more than max_depth %d", p.MinDepth, p.MaxDepth)
	}
	return nil
}

// CheckStage validates a test stage at load time. Template values
// ({{name}}) stand in for parameters of any type, so a templated field is
// checked by name only; its value is checked when the test runs.
func CheckStage(s expert.Stage) error {
	if s.Primitive == "" {
		return nil
	}
	m, err := stageParams(s)
	if err != nil {
		return err
	}
	probe := map[string]any{}
	spec := primitives[s.Primitive]
	fields := jsonFields(spec.params())
	for k, v := range m {
		if str, ok := v.(string); ok && strings.HasPrefix(str, "{{") {
			kind, known := fields[k]
			if !known {
				return fmt.Errorf("%s: unknown parameter %q", s.Primitive, k)
			}
			probe[k] = zeroFor(kind)
			continue
		}
		probe[k] = v
	}
	_, err = decodeParams(s.Primitive, probe)
	return err
}

func jsonFields(v any) map[string]reflect.Kind {
	out := map[string]reflect.Kind{}
	t := reflect.TypeOf(v).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type.Kind()
	}
	return out
}

// zeroFor is a stand-in value of a field's type; strings are non-empty so
// a templated required parameter counts as present.
func zeroFor(k reflect.Kind) any {
	switch k {
	case reflect.String:
		return "x"
	case reflect.Bool:
		return false
	}
	return 0
}

// ValidateStage checks a stage given at run time, in a model's tool
// call. Unlike CheckStage, template values are not special: a tool
// call's parameters are literal.
func ValidateStage(s expert.Stage) error {
	if s.Primitive == "" {
		if len(s.Argv) == 0 {
			return fmt.Errorf("empty command")
		}
		return nil
	}
	m, err := stageParams(s)
	if err != nil {
		return err
	}
	_, err = decodeParams(s.Primitive, m)
	return err
}

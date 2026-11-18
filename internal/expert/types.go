// Package expert discovers, validates, resolves and pins expert packages:
// versioned, data-only domain packages (docs/EXPERT_API.md). Nothing in a
// package is executed while it is discovered or loaded.
package expert

import "encoding/json"

// Manifest is a decoded expert.yaml or catalog.yaml. Field names follow the
// schemas compa.lowmag.cc/expert/manifest/v1alpha3 and
// compa.lowmag.cc/expert/catalog/v1alpha2, whose fields are a subset.
type Manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
	// Summary is an expert's one line in the expert index (v1alpha3).
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
}

type Spec struct {
	EngineAPI  string       `json:"engineApi"`
	Analyst    string       `json:"analyst"`
	References []string     `json:"references,omitempty"`
	Routing    Routing      `json:"routing"`
	Tests      []string     `json:"tests,omitempty"`
	Catalogs   []CatalogRef `json:"catalogs,omitempty"`
	Binaries   []string     `json:"binaries,omitempty"`
	// Captures are the kinds of capture a catalog's tools read, each with
	// the names that mark one; a catalog that reads any file
	// declares none.
	Captures []Capture `json:"captures,omitempty"`
	MCP      *MCP      `json:"mcp,omitempty"`
}

// Capture is a kind of capture, such as must-gather, and the names of
// files or directories whose presence within an attachment marks one.
// Identity is the short name of the catalog's tool that identifies each
// capture of the kind beneath an attachment's directory (engine API
// 0.2.1): it takes the parameter dir and writes one identity object a
// line (compa.lowmag.cc/expert/identity/v1alpha2).
type Capture struct {
	Kind     string   `json:"kind"`
	Marks    []string `json:"marks"`
	Identity string   `json:"identity,omitempty"`
}

// IdentityParam is the parameter an identity tool takes: the attachment's
// directory, relative to the artifact root.
const IdentityParam = "dir"

// CatalogRef is an expert's dependency on a tool catalog: a version range
// and the short names of the tools it uses from it.
type CatalogRef struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Tools   []string `json:"tools"`
}

func (r CatalogRef) ToolIDs() []string {
	ids := make([]string, len(r.Tools))
	for i, t := range r.Tools {
		ids[i] = r.ID + "." + t
	}
	return ids
}

type Routing struct {
	Keys       []KeyRef `json:"keys,omitempty"`
	Signals    []Signal `json:"signals,omitempty"`
	Components []string `json:"components,omitempty"`
}

// KeyRef is a routing key the expert serves. In YAML it is either a bare ID, which
// means a primary relationship, or {id, relationship}.
type KeyRef struct {
	ID           string `json:"id"`
	Relationship string `json:"relationship,omitempty"`
}

// UnmarshalJSON reads a bare ID as a primary relationship.
func (r *KeyRef) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		*r = KeyRef{ID: id, Relationship: "primary"}
		return nil
	}
	type plain KeyRef
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Relationship == "" {
		p.Relationship = "primary"
	}
	*r = KeyRef(p)
	return nil
}

// Signal is a routing condition on one normalized case fact. Exactly one of
// the condition fields is set.
type Signal struct {
	Fact    string `json:"fact"`
	Equals  any    `json:"equals,omitempty"`
	In      []any  `json:"in,omitempty"`
	Present *bool  `json:"present,omitempty"`
	Matches string `json:"matches,omitempty"`
}

type MCP struct {
	Command []string  `json:"command"`
	Network bool      `json:"network,omitempty"`
	Budget  MCPBudget `json:"budget"`
	Tools   []MCPTool `json:"tools"`
}

type MCPBudget struct {
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	MemoryMB       int `json:"memoryMB,omitempty"`
	OutputBytes    int `json:"outputBytes,omitempty"`
}

type MCPTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Risk         string         `json:"risk"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema"`
}

// TestsFile is a decoded tests.yaml (compa.lowmag.cc/expert/tests/v1alpha2).
type TestsFile struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Tests      []Test `json:"tests"`
}

// Test is an investigative test: a pipeline of primitives and argv commands
// with typed parameters.
type Test struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Summary is one line offered in place of the description, which a
	// model may ask for (v1alpha2).
	Summary       string           `json:"summary,omitempty"`
	Params        map[string]Param `json:"params,omitempty"`
	Preconditions []Precondition   `json:"preconditions,omitempty"`
	Run           []Stage          `json:"run"`
	Output        *Output          `json:"output,omitempty"`
	Interpret     string           `json:"interpret,omitempty"`
	Fixtures      string           `json:"fixtures,omitempty"`
}

// Param is a typed test parameter. A parameter without a default is required.
type Param struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []any    `json:"enum,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Minimum     *float64 `json:"minimum,omitempty"`
	Maximum     *float64 `json:"maximum,omitempty"`
	MaxLength   int      `json:"maxLength,omitempty"`
}

type Precondition struct {
	Exists string `json:"exists"`
}

type Output struct {
	Format      string `json:"format,omitempty"`
	Description string `json:"description,omitempty"`
}

// Stage is one pipeline stage: an argv command or a single primitive.
type Stage struct {
	Argv      []string
	Primitive string
	// Params holds the primitive's parameters. A string shorthand in YAML
	// is kept in Shorthand, for the primitive to map to its primary field.
	Params    map[string]any
	Shorthand *string
}

func (s *Stage) UnmarshalJSON(data []byte) error {
	var argv []string
	if err := json.Unmarshal(data, &argv); err == nil {
		*s = Stage{Argv: argv}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	for name, raw := range obj {
		st := Stage{Primitive: name}
		var short string
		if err := json.Unmarshal(raw, &short); err == nil {
			st.Shorthand = &short
		} else if err := json.Unmarshal(raw, &st.Params); err != nil {
			return err
		}
		*s = st
	}
	return nil
}

func (s Stage) MarshalJSON() ([]byte, error) {
	if s.Primitive == "" {
		return json.Marshal(s.Argv)
	}
	if s.Shorthand != nil {
		return json.Marshal(map[string]any{s.Primitive: *s.Shorthand})
	}
	params := s.Params
	if params == nil {
		params = map[string]any{}
	}
	return json.Marshal(map[string]any{s.Primitive: params})
}

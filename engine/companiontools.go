package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/model"
)

// The companion's tools (docs/MODEL_API.md §Tools): declared once, in this
// order, on every call, so the cached prefix holding them never changes.
// Each call is checked before it runs and refused with its reason as its
// result otherwise. A collection's result is stored as evidence, named
// once for the case, and returned as an excerpt.

// Bounds of what a tool returns to the model: an excerpt of a stored
// result, and how long a timeout a call may ask for.
const (
	excerptLines = 400
	excerptBytes = 16 << 10
	timeoutArg   = "timeout_seconds"
)

// tool is one entry of the table: its declaration and how it runs.
type tool struct {
	decl model.Tool
	run  func(ctx context.Context, c companion.Call) companion.Result
}

func (t tool) Decl() model.Tool { return t.decl }

func (t tool) Run(ctx context.Context, c companion.Call) companion.Result { return t.run(ctx, c) }

// refused is a call's refusal, which the model reads as why.
func refused(format string, args ...any) companion.Result {
	return companion.Result{Text: fmt.Sprintf(format, args...), IsError: true}
}

// toolkit is what the tools of one case reach.
type toolkit struct {
	e *Engine
	c *openCase
}

// companionTools is the case's tool table, in its fixed order, each tool
// that returns evidence declaring its call's question (frame).
func (e *Engine) companionTools(ctx context.Context, c *openCase) []companion.Tool {
	k := toolkit{e: e, c: c}
	var out []companion.Tool
	for _, p := range primitiveTools {
		out = append(out, k.primitive(p))
	}
	out = append(out, k.command(), k.runTool(), k.describeTool())
	out = append(out, k.evidenceTools()...)
	out = append(out, k.loadExpert())
	if e.hasReferences(ctx, c) {
		out = append(out, k.readReference())
	}
	out = append(out, k.consult())
	out = append(out, k.lookups()...)
	for i, tl := range out {
		out[i] = frame(tl)
	}
	return out
}

// primitiveTool is a primitive as a tool: its name, with each . written _,
// its description and its parameters.
type primitiveTool struct {
	name, primitive, description string
	params                       map[string]any
	required                     []string
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func integer(desc string, lo int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": lo}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func enum(desc string, vals ...any) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

// primitiveTools are the engine's primitives offered directly
// (docs/COLLECTION.md); aggregate takes a previous stage's output, so it is
// a stage of command's pipeline only.
var primitiveTools = []primitiveTool{
	{name: "find", primitive: "find", description: "List files or directories under the artifact root, in path order: by glob on the base name, size, modification time and depth; details adds each entry's kind, size and time.",
		params: map[string]any{
			"path": strProp("Directory under the artifact root; the root by default."), "glob": strProp("Glob on the entry's base name."),
			"type": enum("Entries to list.", "file", "dir", "any"), "min_size": integer("Bytes, at least.", 0), "max_size": integer("Bytes, at most.", 0),
			"newer_than": strProp("RFC 3339; modified strictly after."), "older_than": strProp("RFC 3339; modified strictly before."),
			"max_results": integer("At most this many entries.", 1), "min_depth": integer("From path, as find(1) counts; 0 sets none.", 0),
			"max_depth": integer("From path, as find(1) counts; 0 sets none.", 0), "details": boolean("Each entry's kind, size and modification time before its path."),
		}},
	{name: "grep", primitive: "grep", description: "Lines matching an RE2 pattern in a file, or in files under a directory by glob; gzipped files read decompressed.",
		params: map[string]any{
			"pattern": strProp("RE2 pattern."), "path": strProp("File or directory under the artifact root."), "glob": strProp("Glob on base names under path."),
			"ignore_case": boolean("Match ignoring case."), "context_before": integer("Lines before each match.", 0), "context_after": integer("Lines after each match.", 0),
			"max_matches": integer("Matches kept.", 1), "from": enum("Which matches max_matches keeps: the first or the last.", "start", "end"),
		}, required: []string{"pattern"}},
	{name: "read", primitive: "read", description: "A line or byte range of one file, or its head or tail; gzipped files read decompressed for lines.",
		params: map[string]any{
			"path": strProp("File under the artifact root."), "start_line": integer("First line, from 1.", 1), "end_line": integer("Last line.", 1),
			"head": integer("First lines.", 1), "tail": integer("Last lines.", 1), "start_byte": integer("First byte, from 0.", 0), "end_byte": integer("Byte to stop before.", 0),
		}, required: []string{"path"}},
	{name: "jq", primitive: "jq", description: "A jq filter over a JSON file, or files by glob.",
		params:   map[string]any{"filter": strProp("jq filter."), "path": strProp("File or directory under the artifact root."), "glob": strProp("Glob on base names under path."), "raw": boolean("Strings unquoted.")},
		required: []string{"filter"}},
	{name: "yq", primitive: "yq", description: "A jq filter over a YAML file, or files by glob, such as a must-gather's resources.",
		params:   map[string]any{"filter": strProp("jq filter."), "path": strProp("File or directory under the artifact root."), "glob": strProp("Glob on base names under path."), "raw": boolean("Strings unquoted.")},
		required: []string{"filter"}},
	{name: "logs_window", primitive: "logs.window", description: "A log's lines whose leading time stamp falls in [start, end); a line without one follows the line before it.",
		params: map[string]any{
			"path": strProp("Log file under the artifact root."), "start": strProp("RFC 3339, inclusive."), "end": strProp("RFC 3339, exclusive."),
			"layout": strProp("Go time layout of the stamp; RFC 3339 and common forms tried without one."), "skip": integer("Characters before the stamp.", 0),
			"anchor": strProp("RFC 3339 time dating stamps without a year, usually the capture's."), "zone": strProp("IANA zone or offset for stamps without one; UTC by default."),
		}, required: []string{"start", "end"}},
	{name: "logs_count", primitive: "logs.count", description: "Repeated lines of a log counted, with each's first and last occurrence.",
		params: map[string]any{"path": strProp("Log file under the artifact root."), "pattern": strProp("RE2 pattern a line must match."), "max_groups": integer("Groups kept.", 1)}},
	{name: "diff", primitive: "diff", description: "Compare two files or two resources.",
		params: map[string]any{"a": strProp("First file under the artifact root."), "b": strProp("Second file.")}, required: []string{"a", "b"}},
	{name: "archive_list", primitive: "archive.list", description: "The members of an archive.",
		params: map[string]any{"path": strProp("Archive under the artifact root."), "max_entries": integer("Members listed.", 1)}, required: []string{"path"}},
}

// schema is a tool's parameter schema: its properties and required list,
// and a timeout a collecting call may ask for.
func schema(props map[string]any, required []string, collects bool) map[string]any {
	p := make(map[string]any, len(props)+1)
	for k, v := range props {
		p[k] = v
	}
	if collects {
		p[timeoutArg] = integer("Seconds this run may take, beyond the default, up to the ceiling.", 1)
	}
	s := map[string]any{"type": "object", "properties": p, "additionalProperties": false}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, n := range required {
			r[i] = n
		}
		s["required"] = r
	}
	return s
}

// paramsSchema is an expert test's typed parameters as a JSON schema, a
// parameter with no default required.
func paramsSchema(params map[string]expert.Param) map[string]any {
	props := make(map[string]any, len(params))
	var required []string
	for name, p := range params {
		var m map[string]any
		// A Param is plain values with JSON tags, so it round-trips.
		b, _ := json.Marshal(p)
		_ = json.Unmarshal(b, &m)
		props[name] = m
		if paramRequired(p) {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return schema(props, required, false)
}

// paramRequired is whether a call must give a parameter: one with no
// default, as collect.ResolveParams checks it.
func paramRequired(p expert.Param) bool { return p.Default == nil }

// signatureParams bounds the parameters a tool's signature names.
const signatureParams = 8

// signature is a tool's ID and its parameters as load_expert lists them,
// from the schema run_tool checks a call against: the required ones
// first, each marked *, then the rest, each set sorted by name, with a
// type that is not a string after it; past signatureParams, a count of
// the rest. For example node_ready(node*, since:integer).
func signature(t expert.Test) string {
	names := make([]string, 0, len(t.Params))
	for n := range t.Params {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := paramRequired(t.Params[names[i]]), paramRequired(t.Params[names[j]])
		if ri != rj {
			return ri
		}
		return names[i] < names[j]
	})
	var b strings.Builder
	b.WriteString(t.ID)
	b.WriteString("(")
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		if i == signatureParams {
			fmt.Fprintf(&b, "%d more", len(names)-i)
			break
		}
		p := t.Params[n]
		b.WriteString(n)
		if paramRequired(p) {
			b.WriteString("*")
		}
		if p.Type != "" && p.Type != "string" {
			b.WriteString(":" + p.Type)
		}
	}
	b.WriteString(")")
	return b.String()
}

// callTimeout takes a call's timeout from its arguments: the time it asked
// for, which the runner caps, and the arguments without it.
func callTimeout(args map[string]any) (map[string]any, time.Duration) {
	v, ok := args[timeoutArg]
	if !ok {
		return args, 0
	}
	out := make(map[string]any, len(args))
	for n, x := range args {
		if n != timeoutArg {
			out[n] = x
		}
	}
	n, _ := number(v)
	return out, time.Duration(n) * time.Second
}

func (k toolkit) primitive(p primitiveTool) companion.Tool {
	return tool{
		decl: model.Tool{Name: p.name, Description: p.description, Parameters: schema(p.params, p.required, true)},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			args, d := callTimeout(c.Args)
			st := expert.Stage{Primitive: p.primitive, Params: args}
			if err := collect.ValidateStage(st); err != nil {
				return refused("%s: %v", p.name, err)
			}
			return k.collect(ctx, d, collect.What{Pipeline: []expert.Stage{st}}, p.name+" "+compact(args))
		},
	}
}

func (k toolkit) command() companion.Tool {
	props := map[string]any{
		"argv": map[string]any{"type": "array", "description": "The command and its arguments, run without a shell: no pipes, globs or variables are expanded.", "items": map[string]any{"type": "string"}},
		"then": map[string]any{"type": "array", "description": "Primitive stages that read the command's output in turn, such as {\"primitive\": \"jq\", \"params\": {\"filter\": \".items[]\"}} or {\"primitive\": \"aggregate\", \"params\": {\"op\": \"count_by\", \"field\": \"reason\"}}.",
			"items": map[string]any{"type": "object", "properties": map[string]any{"primitive": strProp("A primitive's name, as find, grep, jq, logs.window, aggregate."), "params": map[string]any{"type": "object", "description": "Its parameters."}}, "required": []any{"primitive"}}},
	}
	return tool{
		decl: model.Tool{Name: "command", Description: "Run an allowlisted read-only command, such as omc, over the artifacts in the sandbox, its output optionally passed through primitive stages. Allowed binaries: " + strings.Join(k.e.runner().Policy.Allowlist(), ", ") + ".",
			Parameters: schema(props, []string{"argv"}, true)},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			args, d := callTimeout(c.Args)
			argv, ok := stringList(args["argv"])
			if !ok || len(argv) == 0 {
				return refused("command: argv is a list of strings, the binary first")
			}
			stages := []expert.Stage{{Argv: argv}}
			then, _ := args["then"].([]any)
			for i, s := range then {
				m, _ := s.(map[string]any)
				name, _ := m["primitive"].(string)
				params, _ := m["params"].(map[string]any)
				st := expert.Stage{Primitive: name, Params: params}
				if err := collect.ValidateStage(st); err != nil {
					return refused("command: then[%d]: %v", i, err)
				}
				stages = append(stages, st)
			}
			if err := k.e.runner().Check(stages); err != nil {
				return refused("command: %v", err)
			}
			return k.collect(ctx, d, collect.What{Pipeline: stages}, strings.Join(argv, " "))
		},
	}
}

func stringList(v any) ([]string, bool) {
	l, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// compact is a call's arguments in one short line, for a result's heading.
func compact(args map[string]any) string {
	// Arguments came from JSON, so they encode again.
	b, _ := json.Marshal(args)
	return text.Ellipsis(string(b), 200)
}

// collect runs a collection through the runner, stores its record as
// evidence and returns it as an excerpt under its name.
func (k toolkit) collect(ctx context.Context, d time.Duration, what collect.What, label string) companion.Result {
	if k.c.root == "" {
		return refused("This case has no artifacts to read.")
	}
	runner := k.e.runner()
	if d > 0 {
		runner = runner.Allowing(d)
	}
	rec := runner.Run(ctx, collect.Request{ArtifactRoot: k.c.root, What: what})
	return k.stored(rec, label)
}

// stored keeps a collection's record as evidence and returns its excerpt.
func (k toolkit) stored(rec collect.Record, label string) companion.Result {
	if err := k.c.ev.Put(rec); err != nil {
		return refused("The result could not be stored: %v", err)
	}
	it := evidence.FromRecord(rec)
	return k.excerpt(it, label)
}

// excerpt is a stored item as a tool returns it: its name, what made it
// and its size, then any note on how it ran, then its first lines under
// their stored numbers, at most excerptLines and excerptBytes, saying where
// to read on.
func (k toolkit) excerpt(it evidence.Item, label string) companion.Result {
	name, err := evidenceName(k.c, it.Ref.ID)
	if err != nil {
		return refused("The result was stored but could not be named: %v", err)
	}
	in := it.Input()
	var b strings.Builder
	b.WriteString(excerptHead(name, label, it))
	b.WriteString("\n")
	if in.Ref.Note != "" {
		for _, l := range strings.Split(in.Ref.Note, "\n") {
			fmt.Fprintf(&b, "! %s\n", l)
		}
	}
	lines := strings.SplitAfter(string(it.Content), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	width := len(strconv.Itoa(min(len(lines), excerptLines)))
	shown := 0
	for i, l := range lines {
		if i >= excerptLines || b.Len()+len(l) > excerptBytes {
			break
		}
		fmt.Fprintf(&b, "%*d  %s", width, i+1, strings.TrimSuffix(l, "\n"))
		b.WriteString("\n")
		shown = i + 1
	}
	if shown < len(lines) {
		fmt.Fprintf(&b, "[lines 1–%d of %d shown; read on with evidence_read %s from %d]\n", shown, len(lines), name, shown+1)
	}
	r := companion.Result{Text: b.String(), Evidence: []string{name}}
	switch {
	case shown == len(lines):
		r.Shows = []companion.Shown{{Name: name}}
	case shown > 0:
		r.Shows = []companion.Shown{{Name: name, From: 1, To: shown}}
	}
	return r
}

// An excerpt's head separates its fields with excerptSep and ends with
// excerptLowerBound when the size is only a lower bound; excerptSize
// matches the size it writes.
const (
	excerptSep        = " · "
	excerptLowerBound = " at least"
)

var excerptSize = regexp.MustCompile(`^[0-9]+ lines?, [0-9]+ bytes?$`)

// excerptHead is an excerpt's first line: the stored item's short name,
// what made it, and its size.
func excerptHead(name, label string, it evidence.Item) string {
	h := name + excerptSep + label + excerptSep + text.Plural(int(it.Size.Lines), "line") + ", " + text.Plural(int(it.Size.Bytes), "byte")
	if it.Size.LowerBound {
		h += excerptLowerBound
	}
	return h
}

// ExcerptEvidence is the short name of the item a tool's result is an
// excerpt of, read from the head excerptHead wrote, or nil for a result
// that is not one. A stored conversation keeps a result's text only, so a
// client restoring it names the evidence from this.
func ExcerptEvidence(result string) []string {
	head, _, _ := strings.Cut(result, "\n")
	name, rest, ok := strings.Cut(head, excerptSep)
	if !ok || !shortName.MatchString(name) {
		return nil
	}
	i := strings.LastIndex(rest, excerptSep)
	if i < 0 {
		return nil
	}
	size := strings.TrimSuffix(rest[i+len(excerptSep):], excerptLowerBound)
	if !excerptSize.MatchString(size) {
		return nil
	}
	return []string{name}
}

// toolPackage finds the package that defines a tool or test: an expert the
// conversation pinned, at its pinned version, where one offers it, so an
// upgrade reaches the conversation only through /compact or a new one;
// otherwise the highest version of each installed package that has it,
// catalogs and experts alike.
func (k toolkit) toolPackage(ctx context.Context, conv, id string) (*expert.Package, expert.Test, bool, error) {
	pinned, err := k.pinned(ctx, conv)
	if err != nil {
		return nil, expert.Test{}, false, err
	}
	for _, r := range pinned {
		if t, ok := r.Test(id); ok {
			return r.Package, t, true, nil
		}
		for _, rc := range r.Catalogs {
			for _, t := range rc.Tools {
				if t.ID == id {
					return rc.Package, t, true, nil
				}
			}
		}
	}
	var found *expert.Package
	var test expert.Test
	idx := k.e.experts().idx
	seen := map[string]bool{}
	for _, d := range idx.List() {
		if seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		entry, ok := idx.Describe(d.ID, d.Version)
		if !ok {
			continue
		}
		if t, ok := entry.Test(id); ok && found == nil {
			found, test = entry.Package, t
		}
	}
	return found, test, found != nil, nil
}

func (k toolkit) runTool() companion.Tool {
	props := map[string]any{
		"id":     strProp("The tool's or test's full ID, as an expert's guidance names it, such as openshift.gather.operator_conditions."),
		"params": map[string]any{"type": "object", "description": "Its parameters, as describe_tool gives them; {} where it takes none."},
	}
	return tool{
		decl: model.Tool{Name: "run_tool", Description: "Run a catalog tool or an expert's test by its ID, from any installed package, over the artifacts.", Parameters: schema(props, []string{"id"}, true)},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			args, d := callTimeout(c.Args)
			id, _ := args["id"].(string)
			pkg, t, ok, err := k.toolPackage(ctx, c.Conversation, id)
			if err != nil {
				return refused("run_tool %s: %v", id, err)
			}
			if !ok {
				return refused("run_tool: no installed package defines %q", id)
			}
			params, _ := args["params"].(map[string]any)
			if _, err := collect.ResolveParams(t, params); err != nil {
				return refused("run_tool %s: %v; describe_tool %s gives its parameters", id, err, id)
			}
			if k.c.root == "" {
				return refused("This case has no artifacts to read.")
			}
			runner := k.e.runner()
			if d > 0 {
				runner = runner.Allowing(d)
			}
			rec := runner.RunTest(ctx, k.c.root, pkg, id, params)
			return k.stored(rec, id+"@"+t.Version+" "+compact(params))
		},
	}
}

func (k toolkit) describeTool() companion.Tool {
	return tool{
		decl: model.Tool{Name: "describe_tool", Description: "A catalog tool's or expert test's full description and parameter schema, by its ID.",
			Parameters: schema(map[string]any{"id": strProp("The tool's full ID.")}, []string{"id"}, false)},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			id, _ := c.Args["id"].(string)
			_, t, ok, err := k.toolPackage(ctx, c.Conversation, id)
			if err != nil {
				return refused("describe_tool %s: %v", id, err)
			}
			if !ok {
				return refused("describe_tool: no installed package defines %q", id)
			}
			// A schema of plain values marshals.
			params, _ := json.MarshalIndent(paramsSchema(t.Params), "", "  ")
			return companion.Result{Text: fmt.Sprintf("%s@%s\n\n%s\n\nParameters, for run_tool's params:\n%s", t.ID, t.Version, strings.TrimSpace(t.Description), params)}
		},
	}
}

// evidenceTools are the evidence tools over the case's store, by short
// name: views of what is stored, not stored again, so a line cited from
// one is the stored item's line.
func (k toolkit) evidenceTools() []companion.Tool {
	var out []companion.Tool
	for _, t := range evidenceTools {
		name := strings.ReplaceAll(t.ID, ".", "_")
		params := map[string]expert.Param{}
		for n, p := range t.Params {
			if n == "id" || n == "ids" {
				p.Pattern = ""
				p.Description = "Evidence by short name, such as e12"
				if n == "ids" {
					p.Description += ", up to 10 separated by spaces"
				}
			}
			params[n] = p
		}
		id := t.ID
		out = append(out, tool{
			decl: model.Tool{Name: name, Description: strings.ReplaceAll(t.Description, "ID", "name"), Parameters: paramsSchema(params)},
			run: func(ctx context.Context, c companion.Call) companion.Result {
				args := make(map[string]any, len(c.Args))
				for n, v := range c.Args {
					args[n] = v
				}
				for _, n := range []string{"id", "ids"} {
					s, ok := args[n].(string)
					if !ok {
						continue
					}
					var ids []string
					for _, f := range strings.Fields(s) {
						evID, err := evidenceByName(k.c, f)
						if err != nil {
							return refused("%s: %v", name, err)
						}
						ids = append(ids, evID)
					}
					args[n] = strings.Join(ids, " ")
				}
				rec, read := evidenceToolsFor(k.c, withdrawn(k.c))[id](ctx, args)
				if rec.Status != collect.StatusOK {
					return refused("%s: %s", name, k.names(rec.Reason))
				}
				r := companion.Result{Text: k.names(string(rec.Output))}
				for _, l := range read {
					// Each item read was named to be asked for.
					n, err := evidenceName(k.c, l.id)
					if err != nil {
						return refused("%s: %v", name, err)
					}
					r.Shows = append(r.Shows, companion.Shown{Name: n, From: l.from, To: l.to})
				}
				return r
			},
		})
	}
	return out
}

var evidenceIDsIn = regexp.MustCompile(evidenceIDPattern)

// names writes each evidence ID in s as its short name, naming any not
// named yet.
func (k toolkit) names(s string) string {
	return evidenceIDsIn.ReplaceAllStringFunc(s, func(id string) string {
		if n, err := evidenceName(k.c, id); err == nil {
			return n
		}
		return id
	})
}

// evidenceName is an evidence item's short name for the case, given the
// first time it is asked for and never changed.
func evidenceName(c *openCase, id string) (string, error) {
	db := c.st.DB()
	var seq int64
	err := db.QueryRow(`SELECT seq FROM evidence_names WHERE evidence_id = ?`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		// Only an item not named yet is inserted: an insert that conflicts
		// would spend a number of the sequence, leaving a gap.
		if _, err := db.Exec(`INSERT INTO evidence_names (evidence_id) VALUES (?) ON CONFLICT (evidence_id) DO NOTHING`, id); err != nil {
			return "", fmt.Errorf("naming %s: %w", id, err)
		}
		err = db.QueryRow(`SELECT seq FROM evidence_names WHERE evidence_id = ?`, id).Scan(&seq)
	}
	if err != nil {
		return "", fmt.Errorf("naming %s: %w", id, err)
	}
	return "e" + strconv.FormatInt(seq, 10), nil
}

// isNamed reports whether an evidence item has a short name yet.
func isNamed(c *openCase, id string) (bool, error) {
	var seq int64
	err := c.st.DB().QueryRow(`SELECT seq FROM evidence_names WHERE evidence_id = ?`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

var shortName = regexp.MustCompile(`^e([1-9][0-9]*)$`)

// errNoEvidence is a short name that names no stored item.
var errNoEvidence = errors.New("no evidence")

// evidenceByName is the item a short name, or an evidence ID, names.
func evidenceByName(c *openCase, name string) (string, error) {
	if IsEvidenceID(name) {
		return name, nil
	}
	m := shortName.FindStringSubmatch(name)
	if m == nil {
		return "", fmt.Errorf("%q is not evidence's name, such as e12", name)
	}
	var id string
	err := c.st.DB().QueryRow(`SELECT evidence_id FROM evidence_names WHERE seq = ?`, m[1]).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w %s", errNoEvidence, name)
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	return id, nil
}

// resolveExpert is the expert a conversation would load: as it pinned it,
// or the highest installed version, snapshotted so its pin holds after an
// upgrade.
func (k toolkit) resolveExpert(ctx context.Context, conv, id string) (expert.Resolved, error) {
	pinned, err := k.pinned(ctx, conv)
	if err != nil {
		return expert.Resolved{}, err
	}
	for _, r := range pinned {
		if r.ID() == id {
			return r, nil
		}
	}
	r, err := k.e.resolve(id, "")
	if err != nil {
		return expert.Resolved{}, err
	}
	if err := k.e.snapshots(k.c).SaveResolved(r); err != nil {
		return expert.Resolved{}, fmt.Errorf("snapshotting %s %s: %w", r.ID(), r.Version(), err)
	}
	return r, nil
}

// expertHeading begins an expert's guidance, before its ID and version.
const expertHeading = "# Expert "

const (
	readReferenceTool = "read_reference"
	referenceHeading  = "# Reference "
)

func (e *Engine) hasReferences(ctx context.Context, c *openCase) bool {
	idx := e.experts().idx
	for _, d := range e.installedExperts() {
		if entry, ok := idx.Describe(d.ID, d.Version); ok && len(entry.Manifest.Spec.References) > 0 {
			return true
		}
	}
	if c == nil || c.st == nil {
		return false
	}
	conv, err := (companion.Store{DB: c.st.DB()}).Latest(ctx)
	if err != nil {
		return false
	}
	snap := e.snapshots(c)
	for _, raw := range conv.Pins {
		var pin expert.Pin
		if err := json.Unmarshal(raw, &pin); err != nil {
			continue
		}
		if r, _, err := idx.Pinned(pin, snap); err == nil && len(r.Manifest.Spec.References) > 0 {
			return true
		}
	}
	return false
}

// expertGuidance is an expert's guidance as load_expert and a consult
// give it: its analyst text, then its own tests and its catalogs' tools at
// the versions resolved, sorted by ID, each with its parameters and by its
// one-line summary, or its description's first sentence, with a note on a
// tool whose capture kind the case lacks; then its declared references,
// each with its summary. A tool listed already in the conversation, by the
// same line, is named by ID only, on one line; one whose line differs, as
// a catalog tool at another version may, is listed.
func (k toolkit) expertGuidance(r expert.Resolved, listed map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, expertHeading+"%s %s\n\n%s\n", r.ID(), r.Version(), strings.TrimSpace(r.Analyst))
	tools, absent := offeredByCapture(k.c.root, r)
	sort.Slice(tools, func(i, j int) bool { return tools[i].ID < tools[j].ID })
	var fresh []expert.Test
	var seen []string
	for _, t := range tools {
		if line, ok := listed[t.ID]; ok && line == toolLine(t) {
			seen = append(seen, t.ID)
		} else {
			fresh = append(fresh, t)
		}
	}
	if len(tools) > 0 {
		b.WriteString("\n## Its tools, run with run_tool\n\n")
		if len(fresh) > 0 {
			b.WriteString("Each is listed with its parameters: * marks a required one, and a type follows one that is not a string. describe_tool gives a tool's full description and schema.\n\n")
		}
		for _, t := range fresh {
			b.WriteString(toolLine(t))
			if why := absent[t.ID]; why != "" {
				fmt.Fprintf(&b, " (%s)", why)
			}
			b.WriteString("\n")
		}
		if len(seen) > 0 {
			if len(fresh) > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "Also uses, listed above: %s.\n", strings.Join(seen, ", "))
		}
	}
	var servable []expert.Reference
	for _, ref := range r.Manifest.Spec.References {
		if len(r.References[ref.Path]) <= expert.ReferenceMaxBytes {
			servable = append(servable, ref)
		}
	}
	if len(servable) > 0 {
		b.WriteString("\n## Its references, read with read_reference\n\n")
		for _, ref := range servable {
			fmt.Fprintf(&b, "- `%s`: %s\n", ref.Path, strings.Join(strings.Fields(ref.Summary), " "))
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "\nNote: %s\n", w)
	}
	return b.String()
}

// toolLine is how guidance lists a tool, before any capture note.
func toolLine(t expert.Test) string {
	return fmt.Sprintf("- `%s`: %s", signature(t), strings.Join(strings.Fields(oneLine(t)), " "))
}

func refHeading(id, version, refPath string) string {
	return referenceHeading + id + " " + version + " " + refPath + "\n"
}

func (k toolkit) referenceAlreadyRead(ctx context.Context, conv, id, version, refPath string) (bool, error) {
	msgs, err := (companion.Store{DB: k.c.st.DB()}).Messages(ctx, conv)
	if err != nil {
		return false, err
	}
	head := refHeading(id, version, refPath)
	for _, m := range msgs {
		if m.Kind != companion.KindResults {
			continue
		}
		for _, res := range m.Results {
			if res.Name == readReferenceTool && !res.IsError && strings.HasPrefix(res.Text, head) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (k toolkit) readReference() companion.Tool {
	props := map[string]any{
		"expert": strProp("The loaded expert's ID."),
		"path":   strProp("The reference path, as the expert's guidance lists it."),
	}
	type refKey struct{ conv, id, version, path string }
	var mu sync.Mutex
	seen := map[refKey]bool{}
	return tool{
		decl: model.Tool{
			Name:        readReferenceTool,
			Description: "Read a reference file declared by a loaded expert, by expert ID and path as listed when the expert loaded. Returns domain guidance, not evidence.",
			Parameters:  schema(props, []string{"expert", "path"}, false),
		},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			expertID, _ := c.Args["expert"].(string)
			refPath, _ := c.Args["path"].(string)
			if strings.TrimSpace(expertID) == "" || strings.TrimSpace(refPath) == "" {
				return refused("read_reference: expert and path are required")
			}
			clean := path.Clean(refPath)
			if path.IsAbs(refPath) || strings.Contains(refPath, "\\") || clean == ".." || strings.HasPrefix(clean, "../") {
				return refused("read_reference %s: path %q escapes the package", expertID, refPath)
			}
			pins, err := k.pins(ctx, c.Conversation, false)
			if err != nil {
				return refused("read_reference %s: %v", expertID, err)
			}
			var pin expert.Pin
			found := false
			for _, p := range pins {
				if p.ID == expertID {
					pin, found = p, true
					break
				}
			}
			if !found {
				return refused("read_reference: expert %q is not loaded in this conversation", expertID)
			}
			r, _, err := k.e.experts().idx.Pinned(pin, k.e.snapshots(k.c))
			if err != nil {
				return refused("read_reference %s: %v", expertID, err)
			}
			body, ok := r.References[refPath]
			if !ok {
				return refused("read_reference %s: %q is not a declared reference", expertID, refPath)
			}
			if len(body) > expert.ReferenceMaxBytes {
				return refused("read_reference %s %s: %d bytes exceeds the %d-byte limit", expertID, refPath, len(body), expert.ReferenceMaxBytes)
			}
			mu.Lock()
			key := refKey{conv: c.Conversation, id: r.ID(), version: r.Version(), path: refPath}
			already := seen[key]
			if !already {
				already, err = k.referenceAlreadyRead(ctx, c.Conversation, r.ID(), r.Version(), refPath)
				if err != nil {
					mu.Unlock()
					return refused("read_reference %s: %v", expertID, err)
				}
				seen[key] = true
			}
			mu.Unlock()
			if already {
				return companion.Result{Text: fmt.Sprintf("%s %s was already read in this conversation; its text is above.", expertID, refPath)}
			}
			return companion.Result{Text: refHeading(r.ID(), r.Version(), refPath) + "\n" + strings.TrimSpace(body) + "\n"}
		},
	}
}

func (k toolkit) loadExpert() companion.Tool {
	return tool{
		decl: model.Tool{Name: "load_expert", Description: "Load an installed expert's guidance and tools into this conversation, by its ID from the expert index.",
			Parameters: schema(map[string]any{"id": strProp("The expert's ID.")}, []string{"id"}, false)},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			id, _ := c.Args["id"].(string)
			loaded, listed, err := k.listedTools(ctx, c.Conversation)
			if err != nil {
				return refused("load_expert: %v", err)
			} else if loaded[id] {
				return companion.Result{Text: fmt.Sprintf("%s is already loaded in this conversation; its guidance is above.", id)}
			}
			r, err := k.resolveExpert(ctx, c.Conversation, id)
			if err != nil {
				return refused("load_expert %s: %v", id, err)
			}
			pin, err := json.Marshal(r.Pin())
			if err != nil {
				return refused("load_expert %s: %v", id, err)
			}
			return companion.Result{Text: k.expertGuidance(r, listed), Pin: pin}
		},
	}
}

// pins are the experts a conversation has pinned, and with inherited
// those of the conversation a consult runs under.
func (k toolkit) pins(ctx context.Context, conv string, inherited bool) ([]expert.Pin, error) {
	st := companion.Store{DB: k.c.st.DB()}
	var out []expert.Pin
	for conv != "" {
		c, err := st.Conversation(ctx, conv)
		if err != nil {
			return nil, err
		}
		for _, raw := range c.Pins {
			var p expert.Pin
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		if !inherited {
			break
		}
		conv = c.Parent
	}
	return out, nil
}

// listedTools are the experts a conversation itself has loaded, by ID, and
// the tools their guidance listed there, by ID, each with its line: those
// each offers as pinned, read from the conversation's stored pins, so they
// hold across a reopen and include the engine's preload.
func (k toolkit) listedTools(ctx context.Context, conv string) (map[string]bool, map[string]string, error) {
	pins, err := k.pins(ctx, conv, false)
	if err != nil {
		return nil, nil, err
	}
	loaded, listed := map[string]bool{}, map[string]string{}
	for _, p := range pins {
		loaded[p.ID] = true
		r, _, err := k.e.experts().idx.Pinned(p, k.e.snapshots(k.c))
		if err != nil {
			return nil, nil, err
		}
		tools, _ := offeredByCapture(k.c.root, r)
		for _, t := range tools {
			listed[t.ID] = toolLine(t)
		}
	}
	return loaded, listed, nil
}

// pinned are the experts a conversation, or the one a consult runs under,
// has pinned, each as pinned: the installed copy while it matches its
// digest, otherwise its snapshot.
func (k toolkit) pinned(ctx context.Context, conv string) ([]expert.Resolved, error) {
	pins, err := k.pins(ctx, conv, true)
	if err != nil {
		return nil, err
	}
	out := make([]expert.Resolved, 0, len(pins))
	for _, p := range pins {
		r, _, err := k.e.experts().idx.Pinned(p, k.e.snapshots(k.c))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// consultTool is the consult tool's name: its result is the consult's
// answer, a model's text, so it shows no evidence, the names in it
// included (unrecordedNames); the consult's own reads are its record's.
const consultTool = "consult"

func (k toolkit) consult() companion.Tool {
	props := map[string]any{
		"expert":   strProp("The expert's ID, from the expert index."),
		"question": strProp("The question and the context the expert needs: observations, attributed reports and your open questions kept apart; evidence names and lines; the material correction or contradiction, if any."),
	}
	return tool{
		decl: model.Tool{Name: consultTool, Description: "Ask one expert one self-contained question in a conversation of its own, which reads the case and runs tools as it needs; returns its answer, for you to weigh.",
			Parameters: schema(props, []string{"expert", "question"}, false)},
		run: func(ctx context.Context, c companion.Call) companion.Result {
			if c.Consult == nil {
				return refused("consult: a consult may not consult another expert")
			}
			id, _ := c.Args["expert"].(string)
			q, _ := c.Args["question"].(string)
			if strings.TrimSpace(q) == "" {
				return refused("consult: the question is empty")
			}
			r, err := k.resolveExpert(ctx, c.Conversation, id)
			if err != nil {
				return refused("consult %s: %v", id, err)
			}
			pin, err := json.Marshal(r.Pin())
			if err != nil {
				return refused("consult %s: %v", id, err)
			}
			g := k.expertGuidance(r, nil)
			answer, err := c.Consult(ctx, id, g, q, pin)
			if err != nil {
				return refused("consult %s did not finish: %v", id, err)
			}
			return companion.Result{Text: fmt.Sprintf("%s answers (a model's reading, in a consult):\n\n%s", id, answer)}
		},
	}
}

// lookups are each registered reference service's operations as tools,
// named for the service: kcs_search and kcs_fetch for a service kcs.
func (k toolkit) lookups() []companion.Tool {
	var out []companion.Tool
	for _, offer := range k.e.lookupOffers() {
		name := offer.Service + "_" + offer.Operation
		out = append(out, tool{
			decl: model.Tool{Name: name, Description: offer.Description, Parameters: paramsSchema(offer.Params)},
			run: func(ctx context.Context, c companion.Call) companion.Result {
				params := map[string]any{}
				for n, p := range offer.Params {
					v, ok := c.Args[n]
					if !ok {
						if p.Default == nil {
							return refused("%s: %s is required", name, n)
						}
						v = p.Default
					}
					if err := expert.CheckParamValue(p, v); err != nil {
						return refused("%s: %s: %v", name, n, err)
					}
					params[n] = v
				}
				it, _, err := k.e.runLookup(ctx, k.c, Lookup{Service: offer.Service, Operation: offer.Operation, Params: params}, true)
				if err != nil {
					return refused("%s: %v", name, err)
				}
				return k.excerpt(it, name+" "+compact(params))
			},
		})
	}
	return out
}

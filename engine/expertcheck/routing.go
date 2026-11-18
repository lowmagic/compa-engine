package expertcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/routing"
	"github.com/lowmagic/compa-engine/internal/schema"
)

// Keys are a routing library's keys, read by the engine's own rules: its
// schema, and every ID and alias naming one key, case aside.
type Keys struct {
	revision string
	keys     []routing.Key
	lib      *routing.Library
}

// LoadKeys reads a routing library document (api/routing/v1alpha1), with
// or without experts, by the rules the engine loads a library with.
func LoadKeys(data []byte) (*Keys, error) {
	doc, err := schema.DecodeYAML(data)
	if err == nil {
		err = schema.Validate(schema.RoutingLibrary, doc)
	}
	if err != nil {
		return nil, err
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var d struct {
		Revision string        `json:"revision"`
		Keys     []routing.Key `json:"keys"`
	}
	if err := json.Unmarshal(j, &d); err != nil {
		return nil, err
	}
	lib, err := routing.NewLibrary(d.Revision, d.Keys)
	if err != nil {
		return nil, err
	}
	return &Keys{revision: d.Revision, keys: d.Keys, lib: lib}, nil
}

// Revision is the document's revision.
func (k *Keys) Revision() string { return k.revision }

// Canonical is the ID of the key a name, ID or alias names, case aside.
func (k *Keys) Canonical(name string) (string, bool) {
	key, ok := k.lib.Canonical(name)
	return key.ID, ok
}

// libraryHead opens every library CompileLibrary writes.
const libraryHead = "# Compiled by compa expert routing from a keys file and the experts'\n# spec.routing.keys; do not edit.\n"

// CompileLibrary compiles a routing library at revision from keys, a
// routing library document naming no expert, and the expert manifests
// under the roots, which load as compa expert check loads them: each
// manifest's spec.routing.keys adds its expert under the key it names,
// with its relationship and its manifest as provenance. A key the keys do
// not name, an alias in place of its key's ID, an expert in the keys, and
// a result the engine's loader would refuse are errors.
func CompileLibrary(keys []byte, revision string, roots ...string) ([]byte, error) {
	k, err := LoadKeys(keys)
	if err != nil {
		return nil, fmt.Errorf("the keys: %w", err)
	}
	var problems []string
	byID := map[string]int{}
	out := make([]routing.Key, len(k.keys))
	for i, key := range k.keys {
		if len(key.Experts) > 0 {
			problems = append(problems, fmt.Sprintf("the keys name experts under %s; the manifests name them", key.ID))
		}
		out[i] = key
		out[i].Experts = nil
		byID[key.ID] = i
	}
	var rs []expert.Root
	for _, r := range roots {
		rs = append(rs, expert.Root{Path: r, Source: expert.Local})
	}
	idx := loader.Discover(rs)
	if err := errors.Join(idx.Errors...); err != nil {
		return nil, err
	}
	for _, e := range idx.Loaded {
		if e.IsCatalog() {
			continue
		}
		ref := filepath.ToSlash(filepath.Join(e.Dir, "expert.yaml")) + "#spec.routing.keys"
		for _, kr := range e.Manifest.Spec.Routing.Keys {
			id, ok := k.Canonical(kr.ID)
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s declares routing key %q, which the keys do not name", e.ID(), kr.ID))
				continue
			case id != kr.ID:
				problems = append(problems, fmt.Sprintf("%s declares %q, an alias of %s; declare the key's ID", e.ID(), kr.ID, id))
				continue
			}
			key := &out[byID[id]]
			key.Experts = append(key.Experts, routing.Relationship{
				ExpertID: e.ID(), Relationship: kr.Relationship,
				Provenance: []routing.Provenance{{Source: e.ID(), Ref: ref}},
			})
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	for i := range out {
		sort.SliceStable(out[i].Experts, func(a, b int) bool { return out[i].Experts[a].ExpertID < out[i].Experts[b].ExpertID })
	}
	var b bytes.Buffer
	b.WriteString(libraryHead)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(libraryYAML(revision, out)); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if _, err := LoadKeys(b.Bytes()); err != nil {
		return nil, fmt.Errorf("the compiled library: %w", err)
	}
	return b.Bytes(), nil
}

// libraryYAML is a library in the schema's field order.
func libraryYAML(revision string, keys []routing.Key) any {
	type provenance struct {
		Source string `yaml:"source"`
		Ref    string `yaml:"ref,omitempty"`
	}
	prov := func(ps []routing.Provenance) []provenance {
		var out []provenance
		for _, p := range ps {
			out = append(out, provenance{p.Source, p.Ref})
		}
		return out
	}
	type relationship struct {
		ID           string       `yaml:"id"`
		Relationship string       `yaml:"relationship,omitempty"`
		Provenance   []provenance `yaml:"provenance,omitempty"`
	}
	type key struct {
		ID         string         `yaml:"id"`
		Name       string         `yaml:"name,omitempty"`
		Aliases    []string       `yaml:"aliases,omitempty"`
		Components []string       `yaml:"components,omitempty"`
		Tags       []string       `yaml:"tags,omitempty"`
		Experts    []relationship `yaml:"experts,omitempty"`
		Provenance []provenance   `yaml:"provenance,omitempty"`
	}
	ks := []key{}
	for _, k := range keys {
		var rels []relationship
		for _, r := range k.Experts {
			rels = append(rels, relationship{r.ExpertID, r.Relationship, prov(r.Provenance)})
		}
		ks = append(ks, key{k.ID, k.Name, k.Aliases, k.Components, k.Tags, rels, prov(k.Provenance)})
	}
	return struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Revision   string `yaml:"revision"`
		Keys       []key  `yaml:"keys"`
	}{"compa.lowmag.cc/routing/v1alpha1", "RoutingLibrary", revision, ks}
}

// Routing is compa expert routing, given the arguments after "expert
// routing": it compiles the library from --keys and the roots' manifests
// and fails unless --library holds it; with --write it writes it, at the
// library's revision, or --revision, which content changed under the
// library's revision needs.
func Routing(_ context.Context, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("expert routing", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	keysFile := fl.String("keys", "", "")
	libFile := fl.String("library", "", "")
	write := fl.Bool("write", false, "")
	revision := fl.String("revision", "", "")
	roots, err := parseInterleaved(fl, args)
	if err != nil || len(roots) == 0 || *keysFile == "" || *libFile == "" || *revision != "" && !*write {
		return fmt.Errorf("%w: expert routing --keys <file> --library <file> [--write [--revision <revision>]] <root> [root …]", ErrUsage)
	}
	keys, err := os.ReadFile(*keysFile)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(*libFile)
	current := ""
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !*write {
			return fmt.Errorf("expert routing %w: %s does not exist; write it with --write --revision <revision>", ErrFailed, *libFile)
		}
	case err != nil:
		return err
	default:
		lib, err := LoadKeys(old)
		if err != nil {
			return fmt.Errorf("%s: %w", *libFile, err)
		}
		current = lib.Revision()
	}
	compiled := func(rev string) ([]byte, error) {
		b, err := CompileLibrary(keys, rev, roots...)
		if err != nil {
			return nil, fmt.Errorf("expert routing %w: %w", ErrFailed, err)
		}
		return b, nil
	}
	if !*write {
		b, err := compiled(current)
		if err != nil {
			return err
		}
		if !bytes.Equal(b, old) {
			return fmt.Errorf("expert routing %w: %s is not what the keys and manifests compile to; write it with --write and a new --revision", ErrFailed, *libFile)
		}
		fmt.Fprintf(out, "%s: revision %s, compiled from %s and %s\n", *libFile, current, *keysFile, strings.Join(roots, ", "))
		return nil
	}
	rev := *revision
	if rev == "" {
		rev = current
	}
	if rev == "" {
		return fmt.Errorf("%w: %s has no revision yet; give one with --revision", ErrUsage, *libFile)
	}
	b, err := compiled(rev)
	if err != nil {
		return err
	}
	if rev == current {
		if !bytes.Equal(b, old) {
			return fmt.Errorf("expert routing %w: the library changed under revision %s; give it a new --revision", ErrFailed, current)
		}
	}
	if err := os.WriteFile(*libFile, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: revision %s written\n", *libFile, rev)
	return nil
}

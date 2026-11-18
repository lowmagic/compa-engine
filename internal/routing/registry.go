// Package routing maps case facts to candidate experts, deterministically
// and with the reasons recorded. It owns the routing key registry interface; the
// registry content is a versioned library maintained with the experts.
package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/schema"
)

type Provenance struct {
	Source string `json:"source"`
	Ref    string `json:"ref,omitempty"`
}

type Relationship struct {
	ExpertID     string       `json:"id"`
	Relationship string       `json:"relationship,omitempty"`
	Provenance   []Provenance `json:"provenance,omitempty"`
}

// Key is one routing key of the library: its canonical ID, the names,
// components and tags that resolve to it, and the experts it maps to.
type Key struct {
	ID         string         `json:"id"`
	Name       string         `json:"name,omitempty"`
	Aliases    []string       `json:"aliases,omitempty"`
	Components []string       `json:"components,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	Experts    []Relationship `json:"experts,omitempty"`
	Provenance []Provenance   `json:"provenance,omitempty"`
}

// Registry answers routing key questions. Lookups by alias, component or tag are
// case-insensitive.
type Registry interface {
	// Revision identifies the registry content, for the journal.
	Revision() string
	// Canonical resolves a routing key ID or alias to its canonical entry.
	Canonical(nameOrAlias string) (Key, bool)
	// Experts is the forward index: the experts related to a canonical routing key.
	Experts(key string) []Relationship
	// KeysForComponent and KeysForTag are the reverse indexes. Both return
	// canonical routing key IDs, sorted.
	KeysForComponent(component string) []string
	KeysForTag(tag string) []string
	// ExpertIDs are the experts the registry relates to any key, each
	// once, sorted.
	ExpertIDs() []string
}

// Library is an in-memory Registry built from a routing library document.
type Library struct {
	revision    string
	byID        map[string]Key
	byName      map[string]string // lower-cased ID or alias -> canonical ID
	byComponent map[string][]string
	byTag       map[string][]string
}

type libraryDoc struct {
	Revision string `json:"revision"`
	Keys     []Key  `json:"keys"`
}

func LoadLibrary(path string) (*Library, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := schema.DecodeYAML(data)
	if err == nil {
		err = schema.Validate(schema.RoutingLibrary, doc)
	}
	if err != nil {
		return nil, fmt.Errorf("routing library %s: %w", path, err)
	}
	j, _ := json.Marshal(doc)
	var lib libraryDoc
	if err := json.Unmarshal(j, &lib); err != nil {
		return nil, fmt.Errorf("routing library %s: %w", path, err)
	}
	l, err := NewLibrary(lib.Revision, lib.Keys)
	if err != nil {
		return nil, fmt.Errorf("routing library %s: %w", path, err)
	}
	return l, nil
}

// NewLibrary indexes entries. An ID or alias that names two different routing keys
// is an error: the registry must resolve every name to one canonical routing key.
func NewLibrary(revision string, keys []Key) (*Library, error) {
	l := &Library{
		revision:    revision,
		byID:        map[string]Key{},
		byName:      map[string]string{},
		byComponent: map[string][]string{},
		byTag:       map[string][]string{},
	}
	var problems []string
	claim := func(name, id string) {
		key := strings.ToLower(name)
		if prev, ok := l.byName[key]; ok && prev != id {
			problems = append(problems, fmt.Sprintf("%q names both %s and %s", name, prev, id))
			return
		}
		l.byName[key] = id
	}
	for _, s := range keys {
		if _, dup := l.byID[s.ID]; dup {
			problems = append(problems, fmt.Sprintf("routing key %s is defined twice", s.ID))
			continue
		}
		for i := range s.Experts {
			if s.Experts[i].Relationship == "" {
				s.Experts[i].Relationship = "primary"
			}
		}
		l.byID[s.ID] = s
		claim(s.ID, s.ID)
		for _, a := range s.Aliases {
			claim(a, s.ID)
		}
		for _, c := range s.Components {
			k := strings.ToLower(c)
			l.byComponent[k] = append(l.byComponent[k], s.ID)
		}
		for _, t := range s.Tags {
			k := strings.ToLower(t)
			l.byTag[k] = append(l.byTag[k], s.ID)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	for _, m := range []map[string][]string{l.byComponent, l.byTag} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return l, nil
}

func (l *Library) Revision() string { return l.revision }

// ExpertIDs are the experts the library relates to any key, each once,
// sorted.
func (l *Library) ExpertIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range l.byID {
		for _, r := range k.Experts {
			if !seen[r.ExpertID] {
				seen[r.ExpertID] = true
				out = append(out, r.ExpertID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Canonical resolves a key's ID or alias, ignoring case.
func (l *Library) Canonical(name string) (Key, bool) {
	id, ok := l.byName[strings.ToLower(name)]
	if !ok {
		return Key{}, false
	}
	return l.byID[id], true
}

// Experts are the experts a canonical key maps to.
func (l *Library) Experts(key string) []Relationship {
	return l.byID[key].Experts
}

// KeysForComponent are the canonical keys a component belongs to, sorted.
func (l *Library) KeysForComponent(component string) []string {
	return l.byComponent[strings.ToLower(component)]
}

// KeysForTag are the canonical keys a tag suggests, sorted.
func (l *Library) KeysForTag(tag string) []string {
	return l.byTag[strings.ToLower(tag)]
}

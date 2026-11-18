package routing

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// Facts are the bounded, normalized case facts routing uses.
type Facts struct {
	// Keys are the routing keys as the case source reports them; IDs or aliases.
	Keys       []string
	Components []string
	Tags       []string
	// Values are normalized facts such as cluster.network_type, as observed.
	Values map[string]any
	// Absent names facts known not to hold: a producer read the evidence
	// completely and reported the fact missing. A fact in neither Values
	// nor Absent is unknown; missing or truncated collection is not absence.
	Absent map[string]bool
}

// Override is the engineer's routing choice. Include adds experts as
// candidates regardless of signals; Exclude removes them.
type Override struct {
	Include []string
	Exclude []string
}

// Match kinds, strongest first. Weights rank explicit ownership above
// component matches above symptom-level signals; tags only suggest.
const (
	MatchOverride     = "override"
	MatchKey          = "key"
	MatchComponent    = "component"
	MatchComponentKey = "component_key"
	MatchSignal       = "signal"
	MatchTagKey       = "tag_key"
)

var weights = map[string]int{
	MatchOverride:     1000,
	MatchKey:          100,
	MatchComponent:    40,
	MatchComponentKey: 30,
	MatchSignal:       20,
	MatchTagKey:       10,
}

// secondaryPercent scales a secondary routing key relationship against a primary one.
const secondaryPercent = 60

// Match is one reason an expert was suggested.
type Match struct {
	Kind         string `json:"kind"`
	Value        string `json:"value"`
	Key          string `json:"key,omitempty"`
	Relationship string `json:"relationship,omitempty"`
	// Source is "manifest", "registry" or "engineer".
	Source     string       `json:"source"`
	Weight     int          `json:"weight"`
	Provenance []Provenance `json:"provenance,omitempty"`
}

// Candidate is a suggested expert. A suggestion is a recommendation, not an
// assertion of ownership or root cause.
type Candidate struct {
	ExpertID string  `json:"expert_id"`
	Version  string  `json:"version"`
	Score    int     `json:"score"`
	Matches  []Match `json:"matches"`
	// Contradicted lists signals whose fact is present with another value.
	Contradicted []string `json:"contradicted,omitempty"`
}

type Exclusion struct {
	ExpertID string `json:"expert_id"`
	Reason   string `json:"reason"`
}

// Result is a routing decision, recorded in the journal.
type Result struct {
	Candidates []Candidate `json:"candidates"`
	Exclusions []Exclusion `json:"exclusions,omitempty"`
	// Ambiguous is set when more than one candidate shares the top score.
	Ambiguous bool `json:"ambiguous"`
	// UnknownKeys are reported routing keys the registry does not know.
	UnknownKeys      []string `json:"unknown_keys,omitempty"`
	RegistryRevision string   `json:"registry_revision"`
}

type Router struct {
	registry Registry
	experts  map[string]*expert.Package
	// Problems are manifest routing key declarations the registry does not know.
	Problems []string
	// Uninstalled are the experts the registry names that are not among
	// the packages given, sorted: their entries are ignored, so a library
	// compiled over more experts than a deployment installs still routes.
	Uninstalled []string
}

// NewRouter indexes experts, one package per ID. Manifest routing key declarations
// are checked against the registry: experts do not invent routing key IDs.
func NewRouter(registry Registry, packages []*expert.Package) (*Router, error) {
	r := &Router{registry: registry, experts: map[string]*expert.Package{}}
	for _, p := range packages {
		if _, dup := r.experts[p.ID()]; dup {
			return nil, fmt.Errorf("expert %s given twice", p.ID())
		}
		r.experts[p.ID()] = p
		for _, s := range p.Manifest.Spec.Routing.Keys {
			if c, ok := registry.Canonical(s.ID); !ok {
				r.Problems = append(r.Problems, fmt.Sprintf("expert %s declares routing key %q, which registry %s does not know", p.ID(), s.ID, registry.Revision()))
			} else if c.ID != s.ID {
				r.Problems = append(r.Problems, fmt.Sprintf("expert %s declares routing key alias %q; use the canonical ID %q", p.ID(), s.ID, c.ID))
			}
		}
	}
	sort.Strings(r.Problems)
	for _, id := range registry.ExpertIDs() {
		if _, ok := r.experts[id]; !ok {
			r.Uninstalled = append(r.Uninstalled, id)
		}
	}
	return r, nil
}

// Route ranks candidate experts for the facts. Ties are broken by expert ID,
// so the same input always gives the same output.
func (r *Router) Route(f Facts, o Override) Result {
	res := Result{RegistryRevision: r.registry.Revision()}
	matches := map[string][]Match{}
	add := func(id string, m Match) {
		if _, ok := r.experts[id]; !ok {
			return
		}
		for _, prev := range matches[id] {
			if prev.Kind == m.Kind && prev.Value == m.Value && prev.Key == m.Key && prev.Source == m.Source {
				return
			}
		}
		matches[id] = append(matches[id], m)
	}
	relWeight := func(base int, rel string) int {
		if rel == "secondary" {
			return base * secondaryPercent / 100
		}
		return base
	}
	// An installed expert's manifest says how it relates to a routing key it
	// names; the library's relationship counts only for a routing key the manifest
	// does not name, so a local package that demotes or drops one is not
	// overruled by the bundled library. The library's provenance stays with
	// the manifest's match.
	declared := func(id, key string) (string, bool) {
		p, ok := r.experts[id]
		if !ok {
			return "", false
		}
		for _, s := range p.Manifest.Spec.Routing.Keys {
			if c, ok := r.registry.Canonical(s.ID); ok && c.ID == key {
				return s.Relationship, true
			}
		}
		return "", false
	}
	keyMatches := func(kind, value, key string) {
		entry, _ := r.registry.Canonical(key)
		libraryProvenance := map[string][]Provenance{}
		for _, rel := range r.registry.Experts(key) {
			provenance := append(append([]Provenance{}, entry.Provenance...), rel.Provenance...)
			if _, ok := declared(rel.ExpertID, key); ok {
				libraryProvenance[rel.ExpertID] = provenance
				continue
			}
			add(rel.ExpertID, Match{Kind: kind, Value: value, Key: key, Relationship: rel.Relationship, Source: "registry", Weight: relWeight(weights[kind], rel.Relationship), Provenance: provenance})
		}
		for id := range r.experts {
			if rel, ok := declared(id, key); ok {
				add(id, Match{Kind: kind, Value: value, Key: key, Relationship: rel, Source: "manifest", Weight: relWeight(weights[kind], rel), Provenance: libraryProvenance[id]})
			}
		}
	}

	for _, name := range f.Keys {
		c, ok := r.registry.Canonical(name)
		if !ok {
			res.UnknownKeys = append(res.UnknownKeys, name)
			continue
		}
		keyMatches(MatchKey, name, c.ID)
	}
	for _, comp := range f.Components {
		for id, p := range r.experts {
			for _, ec := range p.Manifest.Spec.Routing.Components {
				if strings.EqualFold(ec, comp) {
					add(id, Match{Kind: MatchComponent, Value: comp, Source: "manifest", Weight: weights[MatchComponent]})
				}
			}
		}
		for _, key := range r.registry.KeysForComponent(comp) {
			keyMatches(MatchComponentKey, comp, key)
		}
	}
	for _, tag := range f.Tags {
		for _, key := range r.registry.KeysForTag(tag) {
			keyMatches(MatchTagKey, tag, key)
		}
	}
	contradicted := map[string][]string{}
	for id, p := range r.experts {
		for _, s := range p.Manifest.Spec.Routing.Signals {
			switch evaluate(s, f) {
			case signalMatch:
				add(id, Match{Kind: MatchSignal, Value: describeSignal(s), Source: "manifest", Weight: weights[MatchSignal]})
			case signalContradicted:
				contradicted[id] = append(contradicted[id], describeSignal(s))
			}
		}
	}
	for _, id := range o.Include {
		if _, ok := r.experts[id]; ok {
			add(id, Match{Kind: MatchOverride, Value: "include", Source: "engineer", Weight: weights[MatchOverride]})
		} else {
			res.Exclusions = append(res.Exclusions, Exclusion{ExpertID: id, Reason: "engineer included it, but it is not installed"})
		}
	}
	excluded := map[string]bool{}
	for _, id := range o.Exclude {
		excluded[id] = true
	}

	for id, ms := range matches {
		if excluded[id] {
			res.Exclusions = append(res.Exclusions, Exclusion{ExpertID: id, Reason: "excluded by the engineer"})
			continue
		}
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].Weight != ms[j].Weight {
				return ms[i].Weight > ms[j].Weight
			}
			return fmt.Sprint(ms[i]) < fmt.Sprint(ms[j])
		})
		c := Candidate{ExpertID: id, Version: r.experts[id].Version(), Matches: ms}
		// Repeated evidence of the same kind for the same routing key (registry and
		// manifest agreeing) counts once.
		counted := map[string]bool{}
		for _, m := range ms {
			k := m.Kind + "\x00" + m.Value + "\x00" + m.Key
			if !counted[k] {
				counted[k] = true
				c.Score += m.Weight
			}
		}
		c.Contradicted = contradicted[id]
		sort.Strings(c.Contradicted)
		res.Candidates = append(res.Candidates, c)
	}
	sort.Slice(res.Candidates, func(i, j int) bool {
		if res.Candidates[i].Score != res.Candidates[j].Score {
			return res.Candidates[i].Score > res.Candidates[j].Score
		}
		return res.Candidates[i].ExpertID < res.Candidates[j].ExpertID
	})
	sort.Slice(res.Exclusions, func(i, j int) bool { return res.Exclusions[i].ExpertID < res.Exclusions[j].ExpertID })
	res.Ambiguous = len(res.Candidates) > 1 && res.Candidates[0].Score == res.Candidates[1].Score
	return res
}

type signalResult int

const (
	signalAbsent signalResult = iota
	signalMatch
	signalContradicted
)

// evaluate is three-valued: a fact neither observed nor known absent is
// unknown and neither matches nor contradicts a signal, whatever it asks.
func evaluate(s expert.Signal, f Facts) signalResult {
	v, present := f.Values[s.Fact]
	if s.Present != nil {
		switch {
		case present:
			if *s.Present {
				return signalMatch
			}
			return signalContradicted
		case f.Absent[s.Fact]:
			if *s.Present {
				return signalContradicted
			}
			return signalMatch
		}
		return signalAbsent
	}
	if !present {
		return signalAbsent
	}
	ok := false
	switch {
	case s.Equals != nil:
		ok = sameValue(s.Equals, v)
	case s.In != nil:
		for _, e := range s.In {
			if sameValue(e, v) {
				ok = true
				break
			}
		}
	case s.Matches != "":
		re, err := regexp.Compile(s.Matches)
		ok = err == nil && re.MatchString(fmt.Sprint(v))
	}
	if ok {
		return signalMatch
	}
	return signalContradicted
}

// sameValue compares numbers numerically and anything else by equality,
// so 3 and 3.0 match but 3 and "3" do not.
func sameValue(a, b any) bool {
	if x, ok := toFloat(a); ok {
		y, ok := toFloat(b)
		return ok && x == y
	}
	switch a.(type) {
	case string, bool:
		return a == b
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func describeSignal(s expert.Signal) string {
	switch {
	case s.Present != nil:
		return fmt.Sprintf("%s present=%v", s.Fact, *s.Present)
	case s.Equals != nil:
		return fmt.Sprintf("%s == %v", s.Fact, s.Equals)
	case s.In != nil:
		return fmt.Sprintf("%s in %v", s.Fact, s.In)
	}
	return fmt.Sprintf("%s =~ %s", s.Fact, s.Matches)
}

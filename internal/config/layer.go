package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lowmagic/compa-engine/internal/schema"
)

// Base is the engine's base layer, beneath every other: the house style
// and the intentions for case comments every deployment starts from.
//
//go:embed base.yaml
var Base []byte

// Layer is one configuration document and where it came from: Name says
// so to the engineer, Dir is what its relative paths resolve against.
type Layer struct {
	Name string
	Data []byte
	Dir  string
}

// Layered lays each document over the ones before it: maps merge key by
// key, and a list or a value replaces what is beneath it, so a later file
// holds only what it changes. A profile given another model and no effort
// keeps the effort beneath only where the new model lists it. Each layer
// must be a valid configuration by itself, its errors naming it, and its
// relative paths resolve against its own directory.
func Layered(layers ...Layer) (*Config, error) {
	merged := map[string]any{}
	// inherited are the profiles whose effort comes from beneath the layer
	// that last named their model.
	inherited := map[string]bool{}
	for _, l := range layers {
		if len(strings.TrimSpace(string(l.Data))) == 0 {
			continue
		}
		doc, err := schema.DecodeYAML(l.Data)
		if err == nil {
			err = schema.Validate(schema.Config, doc)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.Name, err)
		}
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: not a mapping", l.Name)
		}
		absolute(m, l.Dir)
		for name, p := range section(m, "reasoning", "profiles") {
			p, _ := p.(map[string]any)
			_, model := p["model"]
			_, effort := p["effort"]
			if model || effort {
				inherited[name] = model && !effort
			}
		}
		merged = merge(merged, m).(map[string]any)
	}
	dropInheritedEfforts(merged, inherited)
	if len(merged) == 0 {
		return &Config{}, nil
	}
	j, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	c, err := Parse(j, "")
	if err != nil {
		return nil, fmt.Errorf("the layered configuration: %w", err)
	}
	return c, nil
}

// section is the map at path in a configuration document, or nil.
func section(m map[string]any, path ...string) map[string]any {
	for _, k := range path {
		m, _ = m[k].(map[string]any)
	}
	return m
}

// dropInheritedEfforts drops each inherited profile's effort that its
// model does not list: the layer beneath chose it for another model. A
// model that is not defined is left for Parse to name.
func dropInheritedEfforts(merged map[string]any, inherited map[string]bool) {
	profiles, models := section(merged, "reasoning", "profiles"), section(merged, "reasoning", "models")
	for name, ok := range inherited {
		p, _ := profiles[name].(map[string]any)
		model, _ := p["model"].(string)
		m, defined := models[model].(map[string]any)
		if !ok || !defined {
			continue
		}
		if efforts, _ := m["efforts"].([]any); !slices.Contains(efforts, p["effort"]) {
			delete(p, "effort")
		}
	}
}

// absolute makes a layer's relative paths absolute against its directory.
// A path starting with ~ is left for resolve.
func absolute(m map[string]any, dir string) {
	fix := func(v any) any {
		s, ok := v.(string)
		if !ok || s == "" || filepath.IsAbs(s) || s == "~" || strings.HasPrefix(s, "~/") {
			return v
		}
		if dir == "" {
			dir, _ = os.Getwd()
		}
		return filepath.Join(dir, s)
	}
	if e, ok := m["experts"].(map[string]any); ok {
		for _, k := range []string{"bundled", "snapshots"} {
			if v, ok := e[k]; ok {
				e[k] = fix(v)
			}
		}
		if l, ok := e["local"].([]any); ok {
			for i := range l {
				l[i] = fix(l[i])
			}
		}
	}
	if v, ok := m["routingLibrary"]; ok {
		m["routingLibrary"] = fix(v)
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		if b, ok := r["barredAccounts"].(map[string]any); ok {
			if v, ok := b["list"]; ok {
				b["list"] = fix(v)
			}
		}
	}
}

func merge(under, over any) any {
	u, uok := under.(map[string]any)
	o, ook := over.(map[string]any)
	if !uok || !ook {
		return over
	}
	out := make(map[string]any, len(u)+len(o))
	for k, v := range u {
		out[k] = v
	}
	for k, v := range o {
		out[k] = merge(out[k], v)
	}
	return out
}

package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLayered: a later file changes only what it names — a profile added
// beside the defaults' own, a list replaced whole — its relative paths
// resolve against its own directory, and an invalid layer is named.
func TestLayered(t *testing.T) {
	defaults := Layer{Name: "defaults", Data: []byte(`apiVersion: compa.lowmag.cc/config/v1alpha1
kind: Config
collection: {exclude: [.statedir, .claude]}
reasoning:
  maxEgressClass: external_provider
  models:
    gemini-3.8-flash: {provider: vertex-gemini, model: gemini-3.8-flash}
  profiles:
    cheap_summarize: {model: gemini-3.8-flash}
    deep_analysis: {model: gemini-3.8-flash}
`)}
	user := Layer{Name: "/home/u/.config/compa/config.yaml", Dir: "/home/u/.config/compa", Data: []byte(`apiVersion: compa.lowmag.cc/config/v1alpha1
kind: Config
experts: {local: [mine]}
collection: {exclude: [.statedir]}
reasoning:
  vertex: {project: p1, location: global}
  models:
    claude-haiku-4-5: {provider: vertex-anthropic, model: claude-haiku-4-5}
  profiles:
    deep_analysis: {model: claude-haiku-4-5}
`)}
	c, err := Layered(defaults, user)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Reasoning
	if r.Profiles["cheap_summarize"].Model != "gemini-3.8-flash" || r.Profiles["deep_analysis"].Model != "claude-haiku-4-5" || r.MaxEgressClass != "external_provider" || r.Vertex.Project != "p1" {
		t.Errorf("reasoning %+v", r)
	}
	if strings.Join(c.Collection.Exclude, " ") != ".statedir" {
		t.Errorf("a list replaces: %v", c.Collection.Exclude)
	}
	roots, _ := c.ExpertRoots()
	if len(roots) != 1 || roots[0].Path != filepath.Join("/home/u/.config/compa", "mine") {
		t.Errorf("roots %+v", roots)
	}
	if _, err := Layered(defaults, Layer{Name: "bad.yaml", Data: []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning: {maxEgressClass: 3}\n")}); err == nil || !strings.HasPrefix(err.Error(), "bad.yaml: ") {
		t.Errorf("invalid layer: %v", err)
	}
	if c, err := Layered(); err != nil || c == nil {
		t.Errorf("no layers: %v", err)
	}
}

// TestBarredAccountsPath: the list's path resolves against the directory
// of the layer that names it, as the engine loads configuration, so where
// compa is started does not change which list is read; with none configured
// the rule is off.
func TestBarredAccountsPath(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	c, err := Layered(Layer{Name: "/etc/compa/config.yaml", Dir: "/etc/compa", Data: []byte(head + "reasoning:\n  barredAccounts: {list: pii/blocked.txt, overrideInstructions: Look the account up first.}\n")})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.BarredAccountsPath(); got != "/etc/compa/pii/blocked.txt" {
		t.Errorf("BarredAccountsPath() = %q", got)
	}
	if got := c.Reasoning.BarredAccounts.OverrideInstructions; got != "Look the account up first." {
		t.Errorf("OverrideInstructions = %q", got)
	}
	c, err = Layered(Layer{Name: "/etc/compa/config.yaml", Dir: "/etc/compa", Data: []byte(head)})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.BarredAccountsPath(); got != "" {
		t.Errorf("BarredAccountsPath() with none = %q", got)
	}
	if _, err := Layered(Layer{Name: "bad.yaml", Data: []byte(head + "reasoning:\n  barredAccounts: {overrideInstructions: x}\n")}); err == nil {
		t.Error("a barredAccounts without a list loaded")
	}
}

// TestLayerChangingAModelDropsAnInheritedEffort: a layer that gives a
// profile another model and no effort keeps the effort beneath only where
// the new model lists it. A model that takes none is sent none, one that
// lists it keeps it, and one that takes others asks for one; an effort the
// layer names itself is checked as given.
func TestLayerChangingAModelDropsAnInheritedEffort(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	defaults := Layer{Name: "defaults", Data: []byte(head + `reasoning:
  models:
    standard: {provider: vertex-gemini, model: gemini-3.8-flash, efforts: [low, medium, high]}
    lite: {provider: vertex-gemini, model: gemini-3.5-flash-lite, efforts: [minimal, low, medium, high]}
    deep: {provider: vertex-gemini, model: gemini-3.8-pro, efforts: [high]}
    sonnet: {provider: vertex-anthropic, model: claude-sonnet-4-6}
  profiles:
    companion: {model: standard, effort: medium}
    editor: {model: standard, effort: low}
`)}
	user := func(profiles string) Layer {
		return Layer{Name: "user.yaml", Data: []byte(head + "reasoning:\n  profiles:\n" + profiles)}
	}
	c, err := Layered(defaults, user("    companion: {model: sonnet}\n    editor: {model: lite}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Reasoning.Profiles["companion"]; p != (Profile{Model: "sonnet"}) {
		t.Errorf("the companion on a model that takes no effort: %+v", p)
	}
	if p := c.Reasoning.Profiles["editor"]; p != (Profile{Model: "lite", Effort: "low"}) {
		t.Errorf("the editor on a model that lists its effort: %+v", p)
	}
	if _, err := Layered(defaults, user("    companion: {model: deep}\n")); err == nil || !strings.Contains(err.Error(), `effort "": model deep takes high`) {
		t.Errorf("a model that takes other efforts: %v", err)
	}
	if _, err := Layered(defaults, user("    companion: {model: sonnet, effort: medium}\n")); err == nil || !strings.Contains(err.Error(), "model sonnet takes no effort") {
		t.Errorf("an effort the layer names: %v", err)
	}
}

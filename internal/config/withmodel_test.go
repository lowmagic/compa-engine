package config

import (
	"strings"
	"testing"
)

// The engineer's model for a task keeps the profile's effort where the
// new model takes it, takes one it lists, and refuses one it does not, a
// model not configured, and an effort for a model that takes none.
func TestWithModel(t *testing.T) {
	c := &Config{Reasoning: Reasoning{
		Models: map[string]Model{
			"standard": {Provider: "vertex-gemini", Model: "gemini-3.8-flash", Efforts: []string{"low", "medium", "high"}},
			"lite":     {Provider: "vertex-gemini", Model: "gemini-3.5-flash-lite", Efforts: []string{"minimal", "low", "medium", "high"}},
			"plain":    {Provider: "fixture", Model: "fixture-plain"},
		},
		Profiles: map[string]Profile{"companion": {Model: "standard", Effort: "medium"}},
	}}
	pc := c.Profile("companion")
	got, err := c.WithModel(pc, "lite", "")
	if err != nil || got.Model != "gemini-3.5-flash-lite" || got.Effort != "medium" || got.Source != "engineer" {
		t.Errorf("lite: %+v %v", got, err)
	}
	if got, err := c.WithModel(pc, "lite", "minimal"); err != nil || got.Effort != "minimal" {
		t.Errorf("lite minimal: %+v %v", got, err)
	}
	if got, err := c.WithModel(pc, "plain", ""); err != nil || got.Effort != "" {
		t.Errorf("plain: %+v %v", got, err)
	}
	for _, c2 := range []struct{ model, effort, want string }{
		{"standard", "minimal", "takes the efforts low, medium, high, not minimal"},
		{"plain", "low", "plain takes no effort"},
		{"pro", "", `no model "pro" is configured; the models are lite, plain, standard`},
	} {
		if _, err := c.WithModel(pc, c2.model, c2.effort); err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s %s: %v, want %q", c2.model, c2.effort, err, c2.want)
		}
	}
}

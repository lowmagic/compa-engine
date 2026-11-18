package engine

import (
	"path/filepath"
	"testing"
)

// TestBaseConfigUnderTheDeployment: the engine's base layer lies beneath a
// deployment's configuration, as built and as layered with the user's: a
// deployment that names no comments drafts with the base's style and
// intentions, and one that names intentions has exactly its own, the
// base's style kept unless it names one too.
func TestBaseConfigUnderTheDeployment(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	ids := func(e *Engine) []string {
		var out []string
		for _, in := range e.config.Comments.Intentions {
			out = append(out, in.ID)
		}
		return out
	}
	open := func(config string) *Engine {
		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "none"))
		e, err := New(Options{Cases: StubCases("../testdata/cases"), Artifacts: LocalDir(home), Home: home, Config: []byte(config)})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, config := range []string{"", head} {
		e := open(config)
		for _, step := range []string{"built", "layered"} {
			if step == "layered" {
				if err := e.LayerConfig(""); err != nil {
					t.Fatal(err)
				}
			}
			if got := ids(e); len(got) != 4 || got[0] != "internal-observations" || e.config.Comments.Style == "" {
				t.Errorf("%q, %s: intentions %v, style %q; want the base's", config, step, got, e.config.Comments.Style)
			}
		}
	}
	e := open(head + "comments:\n  intentions: [{id: only, label: Only, audience: internal, purpose: The one.}]\n")
	base := e.config.Comments.Style
	if err := e.LayerConfig(""); err != nil {
		t.Fatal(err)
	}
	if got := ids(e); len(got) != 1 || got[0] != "only" || e.config.Comments.Style != base || base == "" {
		t.Errorf("a deployment's intentions: %v, style %q", got, e.config.Comments.Style)
	}
}

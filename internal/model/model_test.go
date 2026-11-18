package model_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/fixture"
)

// An unknown count survives a round trip as unavailable and a measured
// zero as zero; neither becomes the other.
func TestCountRoundTrip(t *testing.T) {
	for _, c := range []model.Count{model.Unavailable, model.Reported(0), model.Estimated(12, model.EstimatorVersion)} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var back model.Count
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if back != c {
			t.Errorf("round trip of %+v = %+v via %s", c, back, b)
		}
	}
	if b, _ := json.Marshal(model.Unavailable); string(b) != `"unavailable"` {
		t.Errorf("unavailable written as %s", b)
	}
}

// Keywords lists the schema's keywords at every depth, and never a
// property's name.
func TestKeywords(t *testing.T) {
	schema := map[string]any{
		"type": "object", "required": []any{"path"},
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "maxLength": 1024},
			"lines": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}},
			"mode":  map[string]any{"anyOf": []any{map[string]any{"enum": []any{"a"}}, map[string]any{"const": "b"}}},
		},
	}
	want := []string{"anyOf", "const", "enum", "items", "maxLength", "minimum", "properties", "required", "type"}
	if d := cmp.Diff(want, model.Keywords(schema)); d != "" {
		t.Errorf("Keywords (-want +got):\n%s", d)
	}
}

// The fixture provider streams its answer's text in its pieces and returns
// its calls, recording each request.
func TestFixtureRoundTrip(t *testing.T) {
	p := fixture.New()
	p.Respond = func(_ context.Context, r model.Request) (*fixture.Answer, error) {
		a := fixture.Text("The router ", "is pending.")
		a.Calls = []model.ToolCall{{ID: "c1", Name: "grep", Args: map[string]any{"pattern": "Pending"}}}
		return a, nil
	}
	var pieces []string
	r := model.Request{Model: "fixture-model", Messages: []model.Message{{Role: model.RoleUser, Text: "what failed?"}}}
	out, err := p.Call(context.Background(), r, func(d model.Delta) { pieces = append(pieces, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "The router is pending." || len(pieces) != 2 || len(out.Calls) != 1 || !out.Usage.InputTokens.Known {
		t.Errorf("answer %q pieces %q calls %v usage %+v", out.Text, pieces, out.Calls, out.Usage)
	}
	if got := p.Requests(); len(got) != 1 || got[0].Messages[0].Text != "what failed?" {
		t.Errorf("requests %+v", got)
	}
}

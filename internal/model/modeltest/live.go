// Package modeltest holds what the providers' opt-in live tests share.
// Live tests run only when COMPA_LIVE_VERTEX=1, carry Live in their names so
// their results are reported apart from the deterministic suite, and send
// only synthetic text.
package modeltest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
)

// Vertex skips t unless live tests are enabled, and returns the project and
// location from the environment.
func Vertex(t *testing.T) (project, location string) {
	t.Helper()
	if os.Getenv("COMPA_LIVE_VERTEX") != "1" {
		t.Skip("live Vertex test; set COMPA_LIVE_VERTEX=1 to run")
	}
	project = first(os.Getenv("GOOGLE_CLOUD_PROJECT"), os.Getenv("ANTHROPIC_VERTEX_PROJECT_ID"))
	location = first(os.Getenv("GOOGLE_CLOUD_LOCATION"), os.Getenv("CLOUD_ML_REGION"), "global")
	if project == "" {
		t.Fatal("set GOOGLE_CLOUD_PROJECT or ANTHROPIC_VERTEX_PROJECT_ID")
	}
	return project, location
}

// Model is the model a live test uses: env's value, or def.
func Model(env, def string) string { return first(os.Getenv(env), def) }

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Tools are two tools whose parameter schemas use, between them, every
// keyword in keywords and no other, so a turn that runs proves the
// provider accepts each.
func Tools(t *testing.T, keywords []string) []model.Tool {
	t.Helper()
	grep := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "An RE2 pattern.", "maxLength": 200, "pattern": "^.+$"},
			"max":     map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 50},
			"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []any{"pattern"},
		"additionalProperties": false,
	}
	read := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "A path under the artifact root."},
			"from": map[string]any{"type": "string", "enum": []any{"start", "end"}},
		},
		"required": []any{"path"},
	}
	used := map[string]bool{}
	for _, s := range []map[string]any{grep, read} {
		for _, k := range model.Keywords(s) {
			used[k] = true
		}
	}
	var got []string
	for k := range used {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, keywords) {
		t.Fatalf("the live tools use keywords %v, want exactly %v", got, keywords)
	}
	return []model.Tool{
		{Name: "grep", Description: "Search the case's files for lines matching a pattern.", Parameters: grep},
		{Name: "read", Description: "Read a file of the case.", Parameters: read},
	}
}

// caseBlock is synthetic log text over Vertex's 4,096-token minimum for an
// explicit cache by the engine's estimate, so the prefix through it can be
// cached.
func caseBlock() string {
	var b strings.Builder
	b.WriteString("Case 00000001 (synthetic): worker-1 went NotReady after an upgrade.\n")
	for i := range 700 {
		fmt.Fprintf(&b, "2026-10-01T10:%02d:%02dZ node worker-%d kubelet: synthetic status line %d\n", i/60%60, i%60, i%3+1, i)
	}
	return b.String()
}

// TwoToolTurn runs one engineer turn through p: the model is asked to call
// both tools at once, the results go back, and it answers. It fails unless
// the first call makes a tool call, the second answers in text, both report
// usage, and the second reads part of its input from cache.
func TwoToolTurn(t *testing.T, p model.Provider, modelID, effort string, keywords []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r := model.Request{
		Model: modelID, Effort: effort, MaxOutputTokens: 2048,
		System: "You help a support engineer read a case's files. Use the tools when asked. Answer in two sentences at most.",
		Tools:  Tools(t, keywords),
		Messages: []model.Message{
			{Role: model.RoleUser, Text: caseBlock(), Break: true},
			{Role: model.RoleUser, Text: "Call both tools now, in this one response: grep with pattern NotReady, and read with path nodes/worker-1.log. Do not answer before their results.", Break: true},
		},
	}
	var streamed strings.Builder
	first, err := p.Call(ctx, r, func(d model.Delta) { streamed.WriteString(d.Text) })
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	logCall(t, "first", first)
	if len(first.Calls) == 0 {
		t.Fatalf("the first call made no tool call; answer %q", first.Text)
	}
	if streamed.String() != first.Text {
		t.Errorf("streamed %q, answer %q", streamed.String(), first.Text)
	}
	var results []model.ToolResult
	for _, c := range first.Calls {
		text := "e1 (12 lines): 2026-10-01T10:12:30Z node worker-1 kubelet: NotReady: PLEG is not healthy"
		if c.Name == "read" {
			text = "e2 (3 lines): 1│ kubelet started\n2│ PLEG is not healthy: pleg was last seen active 3m0s ago\n3│ node NotReady"
		}
		results = append(results, model.ToolResult{CallID: c.ID, Name: c.Name, Text: text})
	}
	r.Messages = append(r.Messages,
		model.Message{Role: model.RoleModel, Text: first.Text, Calls: first.Calls, Break: true},
		model.Message{Role: model.RoleUser, Results: results, Text: "Now answer: why is worker-1 NotReady?"},
	)
	r.NoCalls = true
	second, err := p.Call(ctx, r, nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	logCall(t, "second", second)
	if second.Text == "" {
		t.Errorf("the second call answered no text; calls %v", second.Calls)
	}
	for _, out := range []*model.Response{first, second} {
		if !out.Usage.InputTokens.Known || !out.Usage.OutputTokens.Known {
			t.Errorf("usage not reported: %+v", out.Usage)
		}
	}
	if u := second.Usage.CacheReadTokens; !u.Known || u.Value == 0 {
		t.Errorf("the second call read nothing from cache: %s", u)
	}
}

func logCall(t *testing.T, name string, out *model.Response) {
	t.Helper()
	u, _ := json.Marshal(out.Usage)
	calls, _ := json.Marshal(out.Calls)
	t.Logf("%s: model %s, calls %s, usage %s, answer %q", name, out.ModelID, calls, u, out.Text)
}

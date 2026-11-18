package vertexgemini_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/modeltest"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// TestLiveTwoToolTurn runs one turn with two tools on Vertex Gemini. What
// it shows about the engine: the provider streams free text and function
// calls, Vertex accepts every keyword in Keywords, results sent back with
// thought signatures are accepted, and the second call reads the
// conversation's prefix from the explicit cache. Whether the model calls
// both tools is the model's.
func TestLiveTwoToolTurn(t *testing.T) {
	project, location := modeltest.Vertex(t)
	p, err := vertexgemini.New(context.Background(), vertexgemini.Options{Project: project, Location: location, CacheTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	modeltest.TwoToolTurn(t, p, modeltest.Model("COMPA_LIVE_GEMINI_MODEL", "gemini-3.8-flash"), "low", vertexgemini.Keywords)
}

// TestLiveOpeningCache runs a turn shaped as the companion's opening, at
// about a quarter of a real opening's size: the case block, the engine's
// preloaded guidance, the ask, then one tool call a response, one of them
// a second load_expert, each result sent back alone as the engine sends
// it. What it shows about the engine: each call's cache outcome, Vertex's
// error where making a cache failed, and the estimate of what a cache
// holds beside the cache read Vertex reports. It fails where Vertex
// refused a cache, or none was made past the opening's first two calls
// although the uncached part outgrew a quarter of the cache. How many
// calls the model makes is the model's.
func TestLiveOpeningCache(t *testing.T) {
	project, location := modeltest.Vertex(t)
	p, err := vertexgemini.New(context.Background(), vertexgemini.Options{Project: project, Location: location, CacheTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	modelID := modeltest.Model("COMPA_LIVE_GEMINI_MODEL", "gemini-3.8-flash")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tools := append(modeltest.Tools(t, vertexgemini.Keywords), model.Tool{Name: "load_expert", Description: "Load an expert's guidance.", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []any{"id"}}})
	r := model.Request{
		Model: modelID, Effort: "low", MaxOutputTokens: 2048, Tools: tools,
		System: "You help a support engineer read a synthetic case's files. Work in exactly this order, one tool call per response and no text until the last step: " +
			"1. grep NotReady; 2. read nodes/worker-1.log; 3. load_expert with id openshift.install-upgrade; 4. grep PLEG; 5. read nodes/worker-2.log; 6. grep etcd; 7. grep upgrade; " +
			"then answer in two sentences.\n\n" + synthetic("prompt", 1500),
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "# The case\n\n" + synthetic("case", 3000), Break: true},
			{Role: model.RoleModel, Calls: []model.ToolCall{{ID: "preload-load_expert-2", Name: "load_expert", Args: map[string]any{"id": "openshift.virtualization"}}}},
			{Role: model.RoleUser, Results: []model.ToolResult{{CallID: "preload-load_expert-2", Name: "load_expert", Text: "# Expert openshift.virtualization 0.0.1 (synthetic)\n\n" + synthetic("guidance", 4000)}}},
			{Role: model.RoleUser, Text: "Open the case: follow the order you were given."},
		},
	}
	sizes := map[string]int{"grep": 1200, "read": 2000, "load_expert": 2500}
	var cost float64
	made := 0
	for n := 1; n <= 9; n++ {
		// The engine's break: the newest message ends this call.
		r.Messages[len(r.Messages)-1].Break = true
		r.NoCalls = n == 9
		out, err := p.Call(ctx, r, nil)
		if err != nil {
			t.Fatalf("call %d: %v", n, err)
		}
		if c, ok := pricing.Vertex().Estimate(vertexgemini.ID, modelID, out.Usage, time.Now()); ok && c.Amount != nil {
			cost += *c.Amount
		}
		c := out.Cache
		if c == nil {
			t.Fatalf("call %d: no cache outcome", n)
		}
		if c.Outcome == model.CacheMade {
			made++
			// Making a cache bills what it holds as input; estimated from
			// the cache read beside it.
			cost += float64(out.Usage.CacheReadTokens.Value) * 0.75 / 1e6
		}
		held := "none"
		if c.Tokens != nil {
			held = c.Tokens.String()
		}
		calls, _ := json.Marshal(out.Calls)
		t.Logf("call %d: cache %s, %d messages held, estimated %s (system and messages, no tools), error %q; reported input %s, cache read %s; calls %s, answer %q",
			n, c.Outcome, c.Messages, held, c.Error, out.Usage.InputTokens, out.Usage.CacheReadTokens, calls, out.Text)
		if c.Outcome == model.CacheRefused {
			t.Errorf("call %d: Vertex refused the cache: %s", n, c.Error)
		}
		if len(out.Calls) == 0 {
			break
		}
		r.Messages = append(r.Messages, model.Message{Role: model.RoleModel, Text: out.Text, Calls: out.Calls})
		var results []model.ToolResult
		for _, call := range out.Calls {
			text := synthetic(call.Name, sizes[call.Name])
			if call.Name == "load_expert" {
				text = "# Expert openshift.install-upgrade 0.0.1 (synthetic)\n\n" + text
			}
			results = append(results, model.ToolResult{CallID: call.ID, Name: call.Name, Text: text})
		}
		r.Messages = append(r.Messages, model.Message{Role: model.RoleUser, Results: results})
	}
	t.Logf("estimated spend $%.4f, caches made %d", cost, made)
	if made < 3 {
		t.Errorf("caches made %d, want the opening's two and at least one as the turn grew", made)
	}
}

// synthetic is synthetic log text of about tokens tokens by the engine's
// estimate.
func synthetic(label string, tokens int) string {
	var b strings.Builder
	for i := 0; model.EstimateTokens(b.String()) < int64(tokens); i++ {
		fmt.Fprintf(&b, "2026-10-01T10:%02d:%02dZ %s worker-%d kubelet: synthetic status line %d, PLEG relist took %dms\n", i/60%60, i%60, label, i%3+1, i, 100+i%900)
	}
	return b.String()
}

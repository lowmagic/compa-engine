package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/modeltest"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// TestLiveCompanionTurn runs one engineer turn of the companion on the
// stub case, its folder the synthetic must-gather, on each Vertex
// provider. What it shows about the engine: the case block, the preloaded
// expert's call and result, the tool table and the results of the tools
// the model calls are all accepted by the provider, and the turn ends in
// an answer with every call recorded. Which tools the model calls, and what
// it answers, are the model's.
func TestLiveCompanionTurn(t *testing.T) {
	project, location := modeltest.Vertex(t)
	for _, c := range []struct {
		name, provider, model, effort string
		make                          func(ctx context.Context) (model.Provider, error)
	}{
		{"gemini", vertexgemini.ID, modeltest.Model("COMPA_LIVE_GEMINI_MODEL", "gemini-3.8-flash"), "low", func(ctx context.Context) (model.Provider, error) {
			return vertexgemini.New(ctx, vertexgemini.Options{Project: project, Location: location, CacheTTL: 5 * time.Minute})
		}},
		{"claude", vertexanthropic.ID, modeltest.Model("COMPA_LIVE_CLAUDE_MODEL", "claude-sonnet-4-6"), "", func(ctx context.Context) (model.Provider, error) {
			return vertexanthropic.New(ctx, vertexanthropic.Options{Project: project, Location: location})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := companionSession(t, nil)
			p, err := c.make(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.e.config.Reasoning.MaxEgressClass = "external_provider"
			s.e.config.Reasoning.Models = map[string]config.Model{"live": {Provider: c.provider, Model: c.model}}
			s.e.config.Reasoning.Profiles = map[string]config.Profile{"companion": {Model: "live", Effort: c.effort}}
			s.e.models = map[string]model.Provider{c.provider: p}
			var tools atomic.Int64 // the calls of one response report from goroutines of their own
			reply, err := s.Talk(ctx, "What do the OVN controller logs say about worker-1? Read them with the tools before you answer.", func(e TalkEvent) {
				if e.Kind == companion.EventToolDone {
					t.Logf("tool %s: %.120q", e.Call.Name, e.Result.Text)
					tools.Add(1)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d tool calls ran; answer: %s", tools.Load(), reply.Text)
			if reply.Text == "" {
				t.Error("no answer")
			}
			rows, err := s.c.st.DB().Query(`SELECT record FROM invocations WHERE task_type = 'companion' ORDER BY started_at, id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				var j string
				if err := rows.Scan(&j); err != nil {
					t.Fatal(err)
				}
				var rec InvocationRecord
				if err := json.Unmarshal([]byte(j), &rec); err != nil {
					t.Fatal(err)
				}
				n++
				t.Logf("call %d: %s, input %s, cache read %s, output %s", n, rec.Outcome, rec.Usage.InputTokens, rec.Usage.CacheReadTokens, rec.Usage.OutputTokens)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Error("no call recorded")
			}
		})
	}
}

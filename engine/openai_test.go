package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
)

// TestOpenAIEndpointUnderTheGate: an OpenAI-compatible endpoint is held to
// the class the deployment declares for it: under a lower
// reasoning.maxEgressClass its calls are refused and recorded
// egress_refused, the endpoint never reached and its key never read;
// allowed, its requests are redacted by that class's policy, its key is
// read from the variable configuration names, and the cost it reports is
// recorded as reported.
func TestOpenAIEndpointUnderTheGate(t *testing.T) {
	const token = "synthetic0token0abcdefghijkl"
	var calls atomic.Int64
	var body atomic.Value
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"model":"example/local-model","choices":[{"index":0,"delta":{"content":"The DNS pod is Ready."},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":900,"completion_tokens":7,"cost":0.0003,"prompt_tokens_details":{"cached_tokens":0}}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("COMPA_TEST_OPENAI_KEY", "synthetic-key")

	s, f := companionSession(t, nil)
	r := &s.e.config.Reasoning
	r.OpenAI = config.OpenAI{BaseURL: srv.URL + "/v1", APIKeyEnv: "COMPA_TEST_OPENAI_KEY", EgressClass: "internal_network"}
	r.Models["local"] = config.Model{Provider: "openai", Model: "example/local-model"}
	r.Profiles["companion"] = config.Profile{Model: "local"}
	ctx := context.Background()
	question := "Is the DNS pod Ready? The proxy sends Authorization: Bearer " + token

	_, err := s.Talk(ctx, question, nil)
	if !errors.Is(err, companion.ErrEgressClass) || !strings.Contains(err.Error(), "internal_network") {
		t.Errorf("Talk under local_only: %v, want the egress refusal naming internal_network", err)
	}
	if calls.Load() != 0 {
		t.Error("the endpoint was reached under local_only")
	}
	if p, ok := s.e.models["openai"].(*deferredProvider); !ok || p.made != nil {
		t.Error("the provider was made under local_only")
	}

	r.MaxEgressClass = "internal_network"
	reply, err := s.Talk(ctx, question, nil)
	if err != nil {
		t.Fatalf("Talk under internal_network: %v", err)
	}
	if reply.Text != "The DNS pod is Ready." || calls.Load() != 1 || len(f.Requests()) != 0 {
		t.Errorf("answer %q, %d requests to the endpoint, %d to the fixture", reply.Text, calls.Load(), len(f.Requests()))
	}
	sent, _ := body.Load().(string)
	if strings.Contains(sent, token) || !strings.Contains(sent, "[REDACTED:token]") {
		t.Errorf("the request as sent holds the secret, or no placeholder for it")
	}
	if a, _ := auth.Load().(string); a != "Bearer synthetic-key" {
		t.Errorf("Authorization %q", a)
	}
	ids := companionCalls(t, s)
	rec, err := invocations.Get(s.c.st.DB(), ids[len(ids)-1])
	if err != nil {
		t.Fatal(err)
	}
	if rec.Cost.Fidelity != model.FidelityReported || rec.Cost.Amount == nil || *rec.Cost.Amount != 0.0003 || rec.Cost.Currency != "USD" ||
		rec.Model.Provider != "openai" || rec.Capabilities.EgressClass != "internal_network" {
		t.Errorf("the call's record: cost %+v, provider %q, egress %q", rec.Cost, rec.Model.Provider, rec.Capabilities.EgressClass)
	}
}

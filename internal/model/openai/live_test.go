package openai_test

import (
	"os"
	"testing"

	"github.com/lowmagic/compa-engine/internal/model/modeltest"
	"github.com/lowmagic/compa-engine/internal/model/openai"
)

// TestLiveTwoToolTurn runs one turn with two tools through OpenRouter, the
// key in OPENROUTER_API_KEY, on COMPA_LIVE_OPENAI_MODEL
// (anthropic/claude-haiku-5.5 by default) with cache marks; opt-in with
// COMPA_LIVE_OPENAI=1. What it shows about the engine: the provider
// streams free text and tool calls, the endpoint accepts every keyword in
// Keywords and the results sent back, its usage is reported, and the
// second call reads the prefix the first call's cache marks cached.
// Whether the model calls both tools is the model's.
func TestLiveTwoToolTurn(t *testing.T) {
	if os.Getenv("COMPA_LIVE_OPENAI") != "1" {
		t.Skip("live OpenRouter test; set COMPA_LIVE_OPENAI=1 and OPENROUTER_API_KEY to run")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("set OPENROUTER_API_KEY")
	}
	p, err := openai.New(openai.Options{BaseURL: "https://openrouter.ai/api/v1", APIKey: key, EgressClass: "external_provider", CacheMarks: true})
	if err != nil {
		t.Fatal(err)
	}
	modeltest.TwoToolTurn(t, p, modeltest.Model("COMPA_LIVE_OPENAI_MODEL", "anthropic/claude-haiku-5.5"), "", openai.Keywords)
}

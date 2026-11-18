package pricing_test

import (
	"math"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
)

// id is a model as the table is looked up by: its provider and model ID.
type id struct{ provider, model string }

var (
	r   = model.Reported
	oct = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
)

func gemini(in, cached, out, thoughts int64) model.Usage {
	return model.Usage{InputTokens: r(in), CacheReadTokens: r(cached), CacheWriteTokens: model.Unavailable,
		OutputTokens: r(out), ReasoningTokens: r(thoughts)}
}

func claude(in, read, write, out int64) model.Usage {
	return model.Usage{InputTokens: r(in), CacheReadTokens: r(read), CacheWriteTokens: r(write),
		OutputTokens: r(out), ReasoningTokens: model.Unavailable}
}

func TestEstimate(t *testing.T) {
	tab := pricing.Vertex()
	for _, c := range []struct {
		name  string
		model id
		usage model.Usage
		at    time.Time
		want  float64
	}{
		// 3,395 × 0.75 + 458 × 3.75 + 983 × 3.75, per million.
		{"gemini introductory", id{"vertex-gemini", "gemini-3.8-flash"}, gemini(3395, 0, 458, 983), oct, 0.00795},
		{"gemini standard from 2027", id{"vertex-gemini", "gemini-3.8-flash"}, gemini(3395, 0, 458, 983), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 0.0159},
		// 5,000 × 0.30 + 4,000 × 0.03 + 200 × 2.50 + 300 × 2.50, per million.
		{"gemini flash-lite", id{"vertex-gemini", "gemini-3.5-flash-lite"}, gemini(5000, 4000, 200, 300), oct, 0.00287},
		{"gemini cached input", id{"vertex-gemini", "gemini-2.5-flash"}, gemini(1000, 9000, 100, 0), oct, 0.00082},
		{"gemini long context", id{"vertex-gemini", "gemini-2.5-pro"}, gemini(250_000, 0, 1000, 0), oct, 0.64},
		// 3,807 × 1 + 658 × 5, per million, with the reported model's date suffix.
		{"claude dated model ID", id{"vertex-anthropic", "claude-haiku-4-5-20251001"}, claude(3807, 0, 0, 658), oct, 0.007097},
		{"claude cache", id{"vertex-anthropic", "claude-sonnet-4-6"}, claude(100, 10_000, 2000, 500), oct, 0.0183},
	} {
		got, ok := tab.Estimate(c.model.provider, c.model.model, c.usage, c.at)
		if !ok {
			t.Errorf("%s: not estimated", c.name)
			continue
		}
		if math.Abs(*got.Amount-c.want) > 1e-9 || got.Currency != "USD" || got.Fidelity != model.FidelityEstimated || got.Estimator != "vertex-global-2026-10-31" {
			t.Errorf("%s: %+v amount %v, want %v", c.name, got, *got.Amount, c.want)
		}
	}
}

func TestNoEstimate(t *testing.T) {
	tab := pricing.Vertex()
	flash := id{"vertex-gemini", "gemini-2.5-flash"}
	unknownOutput := gemini(100, 0, 0, 0)
	unknownOutput.OutputTokens = model.Unavailable
	estimatedInput := gemini(100, 0, 10, 0)
	estimatedInput.InputTokens = model.Estimated(100, "x")
	unbilledWrites := gemini(100, 0, 10, 0)
	unbilledWrites.CacheWriteTokens = r(50)
	// Without its own row, Flash-Lite is not priced as gemini-3.5-flash,
	// whose ID begins its own.
	lite := id{"vertex-gemini", "gemini-3.5-flash-lite"}
	before := *tab
	before.Models = nil
	for _, m := range tab.Models {
		if m.Model != lite.model {
			before.Models = append(before.Models, m)
		}
	}
	if got, ok := before.Estimate(lite.provider, lite.model, gemini(1, 0, 1, 0), oct); ok {
		t.Errorf("gemini-3.5-flash-lite before its row: estimated %+v", got)
	}
	for name, c := range map[string]struct {
		m id
		u model.Usage
	}{
		"a model not in the table":             {id{"vertex-gemini", "gemini-2.5-flash-lite"}, gemini(1, 0, 1, 0)},
		"a provider not in the table":          {id{"openai", "gemini-2.5-flash"}, gemini(1, 0, 1, 0)},
		"a billed category unavailable":        {flash, unknownOutput},
		"an estimated count":                   {flash, estimatedInput},
		"tokens in a category it doesn't bill": {flash, unbilledWrites},
	} {
		if got, ok := tab.Estimate(c.m.provider, c.m.model, c.u, oct); ok {
			t.Errorf("%s: estimated %+v", name, got)
		}
	}
}

// The table lists Gemini's explicit cache storage per million tokens an
// hour, as the Vertex page did when retrieved; a model it lists none for
// has none.
func TestCacheStorage(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for modelID, want := range map[string]float64{"gemini-3.8-flash": 1.00, "gemini-3.5-flash-lite": 1.00, "gemini-3.1-pro": 4.50} {
		if got, ok := pricing.Vertex().CacheStorage("vertex-gemini", modelID, at); !ok || got != want {
			t.Errorf("%s: %v %v, want %v", modelID, got, ok, want)
		}
	}
	if _, ok := pricing.Vertex().CacheStorage("vertex-anthropic", "claude-haiku-4-5", at); ok {
		t.Error("a storage rate for a model the table lists none for")
	}
}

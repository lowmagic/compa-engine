package invocations_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
	"github.com/lowmagic/compa-engine/internal/store"
)

var (
	r  = model.Reported
	t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
)

func usage(in, read, write, out, thought model.Count) model.Usage {
	return model.Usage{InputTokens: in, CacheReadTokens: read, CacheWriteTokens: write, OutputTokens: out, ReasoningTokens: thought,
		Normalization: model.NormalizationNativeDisjoint}
}

func cost(amount float64, currency, fidelity string) pricing.Cost {
	return pricing.Cost{Amount: &amount, Currency: currency, Fidelity: fidelity}
}

var noCost = pricing.Unavailable

func record(id, task, harness, modelID string, u model.Usage, c pricing.Cost, i int) invocations.Record {
	return invocations.Record{InvocationID: id, InvestigationID: "case-1", TaskType: task, HarnessID: harness,
		Model: invocations.ModelIdentity{Provider: "vertex-gemini", ModelID: modelID, Source: "engine_selected"},
		Usage: u, Cost: c, Outcome: "answered", StartedAt: t0.Add(time.Duration(i) * time.Second)}
}

func open(t *testing.T, recs ...invocations.Record) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir(), "0420", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, rec := range recs {
		if err := invocations.Put(s.DB(), rec); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// [P3] An unknown count is stored and read back as unknown, never as zero,
// and a measured zero as zero.
func TestUnknownStaysUnknown(t *testing.T) {
	rec := record("i1", "companion", "companion", "gemini-3.8-flash",
		usage(r(100), r(0), model.Unavailable, r(20), model.Unavailable), noCost, 0)
	s := open(t, rec)
	got, err := invocations.Get(s.DB(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	u := got.Usage
	if u.CacheWriteTokens.Known || u.ReasoningTokens.Known || !u.CacheReadTokens.Known || u.CacheReadTokens.Value != 0 || u.InputTokens.Value != 100 {
		t.Errorf("usage after a round trip: %+v", u)
	}
	if got.Cost.Amount != nil || got.Cost.Fidelity != "unavailable" {
		t.Errorf("cost %+v", got.Cost)
	}
}

func one(t *testing.T, aggs []invocations.Aggregate) invocations.Aggregate {
	t.Helper()
	if len(aggs) != 1 {
		t.Fatalf("aggregates %d, want 1: %+v", len(aggs), aggs)
	}
	return aggs[0]
}

// [P3] A sum over partially unknown values is a floor naming the gap;
// estimated and reported values are never combined.
func TestAggregateCompleteness(t *testing.T) {
	estimated := usage(model.Estimated(500, "words1.3+symbols-v1"), r(0), r(0), r(10), model.Unavailable)
	s := open(t,
		record("i1", "companion", "companion", "gemini-3.8-flash", usage(r(100), r(0), r(0), r(20), r(5)), noCost, 0),
		record("i2", "companion", "companion", "gemini-3.8-flash", usage(model.Unavailable, r(0), r(0), r(30), r(7)), noCost, 1),
		record("i3", "companion", "companion", "gemini-3.8-flash", estimated, noCost, 2),
	)
	aggs, err := invocations.AggregateUsage(s.DB(), "case-1", invocations.ByCase)
	if err != nil {
		t.Fatal(err)
	}
	a := one(t, aggs)
	in := a.Tokens["input_tokens"]
	if a.Invocations != 3 {
		t.Errorf("invocations %d", a.Invocations)
	}
	if in.Reported != 100 || in.Estimated != 500 || in.Unavailable != 1 || !in.Floor() {
		t.Errorf("input %+v: the unknown is named, the estimate kept apart", in)
	}
	if out := a.Tokens["output_tokens"]; out.Reported != 60 || out.Floor() {
		t.Errorf("output %+v", out)
	}
	if th := a.Tokens["reasoning_tokens"]; th.Reported != 12 || th.Unavailable != 1 {
		t.Errorf("reasoning %+v", th)
	}
	if len(a.Harnesses) != 1 || a.Harnesses[0] != "companion" {
		t.Errorf("the excluded record's harness was counted: %v", a.Harnesses)
	}
}

// [P3] Priced and unpriced tokens are reported apart, reported and
// estimated costs are never combined, currencies are not converted, and
// no rate is imputed from priced invocations onto unpriced ones.
func TestAggregateCost(t *testing.T) {
	u := usage(r(1000), r(0), r(0), r(100), r(0))
	s := open(t,
		record("i1", "companion", "other", "gemini-3.1-flash-lite", u, cost(0.25, "USD", model.FidelityReported), 0),
		record("i2", "companion", "companion", "gemini-3.8-flash", u, cost(0.10, "USD", model.FidelityEstimated), 1),
		record("i3", "companion", "companion", "gemini-3.8-flash", u, cost(0.05, "EUR", model.FidelityReported), 2),
		record("i4", "companion", "companion", "gemini-3.8-flash", u, noCost, 3),
	)
	a := one(t, mustAggregate(t, s, invocations.ByCase))
	c := a.Cost
	if c.Reported["USD"] != 0.25 || c.Reported["EUR"] != 0.05 || c.Estimated["USD"] != 0.10 || len(c.Reported) != 2 || len(c.Estimated) != 1 {
		t.Errorf("costs %+v", c)
	}
	if c.PricedTokens != 2200 || c.EstimatedPricedTokens != 1100 || c.UnpricedTokens != 1100 {
		t.Errorf("tokens priced %d, estimated-priced %d, unpriced %d", c.PricedTokens, c.EstimatedPricedTokens, c.UnpricedTokens)
	}
	if len(a.Harnesses) != 2 || len(a.Models) != 2 {
		t.Errorf("a mixed group says so: harnesses %v models %v", a.Harnesses, a.Models)
	}
}

func mustAggregate(t *testing.T, s *store.Store, by ...invocations.Dimension) []invocations.Aggregate {
	t.Helper()
	aggs, err := invocations.AggregateUsage(s.DB(), "case-1", by...)
	if err != nil {
		t.Fatal(err)
	}
	return aggs
}

// [P3] Usage groups by task type, harness, model and conversation.
func TestAggregateGrouping(t *testing.T) {
	u := usage(r(100), r(900), r(0), r(10), r(0))
	conv := func(rec invocations.Record, conversation string, turn int) invocations.Record {
		rec.ConversationID, rec.Turn = conversation, turn
		return rec
	}
	s := open(t,
		conv(record("i1", "companion", "companion", "gemini-3.8-flash", u, noCost, 0), "conv-1", 1),
		conv(record("i2", "companion", "companion", "claude-haiku-4-5", u, noCost, 1), "conv-1", 2),
		conv(record("i3", "consult", "companion", "gemini-3.8-flash", u, noCost, 2), "conv-2", 1),
	)
	byTask := mustAggregate(t, s, invocations.ByTaskType, invocations.ByModel)
	if len(byTask) != 3 || byTask[0].Key[invocations.ByTaskType] != "companion" || byTask[0].Key[invocations.ByModel] != "vertex-gemini/claude-haiku-4-5" {
		t.Errorf("by task and model %+v", byTask)
	}
	convs := mustAggregate(t, s, invocations.ByConversation)
	if len(convs) != 2 || convs[0].Key[invocations.ByConversation] != "conv-1" || convs[0].Invocations != 2 || convs[0].Tokens["cache_read_tokens"].Reported != 1800 {
		t.Errorf("by conversation %+v", convs)
	}
	if h := one(t, mustAggregate(t, s, invocations.ByHarness)); h.Key[invocations.ByHarness] != "companion" || h.Invocations != 3 {
		t.Errorf("by harness %+v", h)
	}
	if _, err := invocations.AggregateUsage(s.DB(), "case-1", "colour"); err == nil {
		t.Error("an unknown dimension was accepted")
	}
	got, _ := invocations.Get(s.DB(), "i2")
	if got.ConversationID != "conv-1" || got.Turn != 2 {
		t.Errorf("turn record %+v", got)
	}
}

func TestTotalTaskUsage(t *testing.T) {
	reported := record("main", "companion", "companion", "gemini-3.8-flash",
		usage(r(100), r(900), r(0), r(20), model.Unavailable), cost(0.25, "USD", model.FidelityReported), 0)
	reported.ConversationID = "old-main"
	estimated := record("consult", "companion", "companion", "gemini-3.8-flash",
		usage(model.Estimated(50, "fixture-v1"), r(100), r(0), r(0), model.Unavailable), cost(0.10, "USD", model.FidelityEstimated), 1)
	estimated.ConversationID = "consult-under-old-main"
	zero := record("compacted-main", "companion", "companion", "gemini-3.8-flash",
		usage(r(0), r(0), model.Unavailable, r(0), model.Unavailable), cost(0, "USD", model.FidelityReported), 2)
	zero.ConversationID, zero.RebuiltFrom = "new-main", "old-main"
	unrelated := record("other-task", "other-task", "companion", "gemini-3.8-flash", reported.Usage, reported.Cost, 3)
	otherCase := record("other-case", "companion", "companion", "gemini-3.8-flash", reported.Usage, reported.Cost, 4)
	otherCase.InvestigationID = "case-2"
	ledger := record("ledger", "comments.ledger", "companion", "gemini-3.8-flash", reported.Usage, reported.Cost, 5)
	s := open(t, reported, estimated, zero, unrelated, otherCase, ledger)
	before, err := s.Changes()
	if err != nil {
		t.Fatal(err)
	}
	got, err := invocations.TotalTaskUsage(s.DB(), "case-1", "companion")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(invocations.Of(reported, estimated, zero), got); diff != "" {
		t.Errorf("case/task total (-want +got):\n%s", diff)
	}
	both, err := invocations.TotalTaskUsage(s.DB(), "case-1", "companion", "comments.ledger")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(invocations.Of(reported, estimated, zero, ledger), both); diff != "" {
		t.Errorf("case total of two tasks (-want +got):\n%s", diff)
	}
	empty, err := invocations.TotalTaskUsage(s.DB(), "missing-case", "companion")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(invocations.Of(), empty); diff != "" {
		t.Errorf("empty total (-want +got):\n%s", diff)
	}
	after, err := s.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("usage hydration wrote to the store: changes %d -> %d", before, after)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := invocations.TotalTaskUsage(s.DB(), "case-1", "companion"); err == nil {
		t.Error("an unreadable store returned a trustworthy total")
	}
}

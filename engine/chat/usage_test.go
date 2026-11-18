package chat

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// usageFixture is an engine whose companion answers every call with one
// stored answer, so tests can insert precise, synthetic billing records
// between sessions.
func usageFixture(t *testing.T) *engine.Engine {
	t.Helper()
	return enginetest.EngineWith(t, enginetest.ScriptedModel(t, `{"turns":[{"answers":[{"text":"A stored fixture answer."}]}]}`))
}

func usageSession(t *testing.T, e *engine.Engine) *engine.Session {
	t.Helper()
	s, err := e.StartSession(t.Context(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func usageRecord(id string, u engine.TokenUsage, amount float64, fidelity string) engine.InvocationRecord {
	return engine.InvocationRecord{
		InvocationID: id, InvestigationID: "00000001", TaskType: "companion", HarnessID: "companion",
		Model: engine.ModelIdentity{Provider: "fixture", ModelID: "priced-fixture"},
		Usage: u, Cost: enginetest.Cost{Amount: &amount, Currency: "USD", Fidelity: fidelity},
		StartedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
}

func TestUsageSurvivesReopenAndCompaction(t *testing.T) {
	e := usageFixture(t)
	s := usageSession(t, e)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	h := start(t, s, Options{NoOpen: true})
	h.say("Investigate this synthetic problem.")
	h.say("/compact")
	if len(h.m.records) != 2 || h.m.usage.Invocations != 2 {
		t.Fatalf("opening and compaction calls: records %d, totals %d", len(h.m.records), h.m.usage.Invocations)
	}
	dir := s.Store().Dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// These persisted historical calls carry actual amounts, cache reads,
	// measured zeros, estimates and unknown categories. They are not rates
	// inferred from the fixture provider's unpriced calls.
	u := engine.TokenUsage{
		InputTokens: enginetest.Reported(100), CacheReadTokens: enginetest.Reported(900), CacheWriteTokens: enginetest.Reported(0),
		OutputTokens: enginetest.Reported(20), ReasoningTokens: enginetest.Unavailable, Normalization: enginetest.NormalizationNativeDisjoint,
	}
	main := usageRecord("historical-priced-main", u, 0.25, engine.FidelityReported)
	main.ConversationID = h.m.records[0].ConversationID
	u.InputTokens, u.OutputTokens = enginetest.Estimated(50, "fixture-v1"), enginetest.Reported(0)
	consult := usageRecord("historical-priced-consult", u, 0.125, engine.FidelityEstimated)
	consult.ConversationID = "historical-consult"
	enginetest.PutInvocations(t, dir, s.CaseID(), main, consult)
	want := engine.UsageOf(append(append([]engine.InvocationRecord{}, h.m.records...), main, consult)...)
	s = usageSession(t, e)
	restored := start(t, s, Options{NoOpen: true})
	if diff := cmp.Diff(want, restored.m.usage); diff != "" {
		t.Errorf("stored usage after reopening (-want +got):\n%s", diff)
	}
	if len(restored.m.records) != 0 {
		t.Error("hydration populated this-process invocation records")
	}
	if !strings.Contains(restored.out(), "A stored fixture answer.") {
		t.Errorf("saved conversation disappeared:\n%s", restored.out())
	}
	for _, part := range []string{"$0.25", "$0.12 est", "cached"} {
		if !strings.Contains(usageShown(restored.m.usage), part) {
			t.Errorf("stored usage lacks %q: %s", part, usageShown(restored.m.usage))
		}
	}
	before, err := s.CompanionTotalUsage()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, before); diff != "" {
		t.Errorf("--no-open changed the stored calls (-want +got):\n%s", diff)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = usageSession(t, e)
	restored = start(t, s, Options{NoOpen: true})
	if diff := cmp.Diff(before, restored.m.usage); diff != "" {
		t.Errorf("spend/cache changed across close/reopen (-before +after):\n%s", diff)
	}
	restored.say("Continue after reopening.")
	if len(restored.m.records) != 1 {
		t.Fatalf("next turn produced %d this-process records, want 1", len(restored.m.records))
	}
	want = engine.UsageOf(append(append([]engine.InvocationRecord{}, h.m.records...), main, consult, restored.m.records[0])...)
	if diff := cmp.Diff(want, restored.m.usage); diff != "" {
		t.Errorf("next call double counted or lost history (-want +got):\n%s", diff)
	}
}

func TestUsageReadFailureKeepsSavedConversation(t *testing.T) {
	e := usageFixture(t)
	s := usageSession(t, e)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	h := start(t, s, Options{NoOpen: true})
	h.say("Store a synthetic answer.")
	if len(h.m.records) != 1 {
		t.Fatalf("got %d invocation records, want 1", len(h.m.records))
	}
	dir := s.Store().Dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A broken cost prevents a trustworthy total, but is not needed to
	// restore the saved answer's attribution and usage.
	enginetest.ExecStore(t, dir, s.CaseID(), `UPDATE invocations SET record = json_set(record, '$.cost', 'unreadable')`)
	s = usageSession(t, e)
	h = start(t, s, Options{NoOpen: true})
	for _, part := range []string{"saved model usage could not be read", "A stored fixture answer."} {
		if !strings.Contains(h.out(), part) {
			t.Errorf("usage failure omitted %q:\n%s", part, h.out())
		}
	}
}

func TestUsageSnapshotsNeverRegress(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	unknown := usageRecord("unknown", engine.TokenUsage{}, 0, engine.FidelityReported)
	unknown.Cost = enginetest.CostUnavailable
	zeroUsage := engine.TokenUsage{
		InputTokens: enginetest.Reported(0), CacheReadTokens: enginetest.Reported(0), CacheWriteTokens: enginetest.Reported(0),
		OutputTokens: enginetest.Reported(0), ReasoningTokens: enginetest.Reported(0),
	}
	zero := usageRecord("zero", zeroUsage, 0, engine.FidelityReported)
	older, newer := engine.UsageOf(unknown), engine.UsageOf(unknown, zero)
	h.update(usageMsg{total: newer})
	h.update(usageMsg{total: older})
	h.update(usageMsg{err: errors.New("synthetic snapshot failure")})
	h.settle()
	if diff := cmp.Diff(newer, h.m.usage); diff != "" {
		t.Errorf("a delayed/failed snapshot erased usage (-want +got):\n%s", diff)
	}
	if got := h.m.usage.Tokens["cache_read_tokens"]; got.Reported != 0 || got.Unavailable != 1 {
		t.Errorf("unknown and measured zero were combined: %+v", got)
	}
	if got, ok := h.m.usage.Cost.Reported["USD"]; !ok || got != 0 {
		t.Errorf("measured zero cost was lost: %+v", h.m.usage.Cost)
	}
	// The same ordering protection applies to asynchronous call records;
	// a failed individual record must not discard a newer valid total.
	h.update(recordMsg{rec: zero, total: older})
	h.update(recordMsg{err: errors.New("synthetic record failure"), total: newer})
	h.settle()
	if diff := cmp.Diff(newer, h.m.usage); diff != "" {
		t.Errorf("record delivery regressed usage (-want +got):\n%s", diff)
	}
	if len(h.m.records) != 1 || !strings.Contains(h.out(), "synthetic snapshot failure") {
		t.Errorf("records/warnings lost: %d records, output %q", len(h.m.records), h.out())
	}
}

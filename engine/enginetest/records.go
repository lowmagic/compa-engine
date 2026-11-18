package enginetest

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
	"github.com/lowmagic/compa-engine/internal/store"
)

// What a turn reports in a TalkEvent and records in an InvocationRecord
// has the engine's own types, which a client never builds; a client's
// tests build them here, to feed the client events and records of their
// own.
type (
	// ToolCall is a tool call a model made: TalkEvent.Call.
	ToolCall = model.ToolCall
	// ModelCall is one model call made: TalkEvent.Invocation.
	ModelCall = companion.Invocation
	// ToolResult is what a tool call returned: TalkEvent.Result.
	ToolResult = companion.Result
	// Cost is a call's cost: InvocationRecord.Cost.
	Cost = pricing.Cost
)

// CostUnavailable is the cost of a call nothing priced.
var CostUnavailable = pricing.Unavailable

// Unavailable is a token category the provider did not report.
var Unavailable = model.Unavailable

// NormalizationNativeDisjoint is TokenUsage.Normalization for a provider
// that reports its token categories apart.
const NormalizationNativeDisjoint = model.NormalizationNativeDisjoint

// Reported is a token count the provider reported.
func Reported(n int64) engine.TokenCount { return model.Reported(n) }

// Estimated is a token count the engine estimated, by estimator.
func Estimated(n int64, estimator string) engine.TokenCount { return model.Estimated(n, estimator) }

// ScriptedModel is configuration, a reasoning block for EngineWith, giving
// the companion the scripted fixture model: the fixture provider answering
// each call from script, its JSON script of turns, which sends nothing
// anywhere.
func ScriptedModel(tb testing.TB, script string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		tb.Fatal(err)
	}
	return "reasoning:\n  maxEgressClass: local_only\n  models:\n    fixture-model: {provider: fixture, model: " +
		strconv.Quote(path) + "}\n  profiles:\n    companion: {model: fixture-model}\n"
}

// PutInvocations records calls in the store of a case, by the store's
// directory (CaseStore.Store), as if the case had made them: the
// history of usage a test reopens the case on. The case's own sessions
// must be closed, since a store is held by one opener at a time.
func PutInvocations(tb testing.TB, dir, caseID string, recs ...engine.InvocationRecord) {
	tb.Helper()
	st, err := store.Open(dir, caseID, store.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	defer st.Close()
	for _, rec := range recs {
		if err := invocations.Put(st.DB(), rec); err != nil {
			tb.Fatal(err)
		}
	}
}

// ExecStore runs one statement on the store of a case, by the store's
// directory, for a test that damages what the store holds to see what a
// client makes of it. The case's own sessions must be closed.
func ExecStore(tb testing.TB, dir, caseID, query string, args ...any) {
	tb.Helper()
	st, err := store.Open(dir, caseID, store.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(query, args...); err != nil {
		tb.Fatal(err)
	}
}

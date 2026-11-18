package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// historyCases gives StartSession the same synthetic case as companionSession.
type historyCases struct{ nopCases }

func (historyCases) Case(context.Context, string) (Case, error) {
	return companionCase, nil
}

// The companion fixture keeps its store in a temporary directory apart
// from its read-only artifact tree. Point the source at that directory to
// reopen the persisted case through the public session lifecycle.
func reopenHistorySession(t *testing.T, s *Session) *Session {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.e.cases = historyCases{}
	s.e.artifacts = &snapshotArtifacts{root: filepath.Dir(s.c.storeDir)}
	reopened, err := s.e.StartSession(t.Context(), s.CaseID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	return reopened
}

// historyDialogue is the engineer/model text a history consumer can show,
// without internal instructions, tool calls or results.
func historyDialogue(messages []ConversationMessage) []string {
	var out []string
	for _, m := range messages {
		if (m.Kind == MessageEngineer || m.Kind == MessageModel) && m.Text != "" {
			out = append(out, string(m.Kind)+": "+m.Text)
		}
	}
	return out
}

func TestConversationHistoryReopensPersistedMessages(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Text("The opening case summary."),
		modelfixture.Text("The follow-up answer."),
	))
	ctx := t.Context()
	if _, err := s.Orient(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "What should I inspect next?", nil); err != nil {
		t.Fatal(err)
	}
	before, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls := len(f.Requests())
	s = reopenHistorySession(t, s)
	after, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(before, after); d != "" {
		t.Errorf("history changed after reopening (-before +after):\n%s", d)
	}
	want := []string{
		"model: The opening case summary.",
		"engineer: What should I inspect next?",
		"model: The follow-up answer.",
	}
	if d := cmp.Diff(want, historyDialogue(after)); d != "" {
		t.Errorf("persisted dialogue (-want +got):\n%s", d)
	}
	var caseBlock, instruction, results bool
	for _, m := range after {
		caseBlock = caseBlock || m.Kind == MessageCase
		instruction = instruction || m.Kind == MessageEngine
		results = results || m.Kind == MessageResults
		if m.CreatedAt.IsZero() {
			t.Error("a stored message lost its creation time")
		}
		if m.Kind == MessageModel && m.Text != "" && m.Invocation == "" {
			t.Error("a stored model answer lost its invocation")
		}
	}
	if !caseBlock || !instruction || !results {
		t.Errorf("history filtered internal kinds: case %t, engine %t, results %t", caseBlock, instruction, results)
	}
	if got := len(f.Requests()); got != calls {
		t.Errorf("reading history made %d model calls", got-calls)
	}
}

func TestConversationHistoryExcludesConsultMessages(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "consult", map[string]any{
			"expert": "openshift.platform", "question": "A private consult question.",
		})),
		modelfixture.Text("A private consult answer."),
		modelfixture.Text("The main conversation's answer."),
	))
	ctx := t.Context()
	if _, err := s.Talk(ctx, "Ask the platform expert.", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(f.Requests()); got != 3 {
		t.Fatalf("%d model calls, want main, consult and main continuation", got)
	}
	messages, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"engineer: Ask the platform expert.", "model: The main conversation's answer."}
	if d := cmp.Diff(want, historyDialogue(messages)); d != "" {
		t.Errorf("main dialogue includes consult internals (-want +got):\n%s", d)
	}
	var consultResult bool
	for _, m := range messages {
		if m.Kind == MessageConsult {
			t.Error("history includes the consult's internal prompt")
		}
		if m.Kind == MessageResults {
			for _, r := range m.Results {
				consultResult = consultResult || strings.Contains(r.Text, "A private consult answer.")
			}
		}
	}
	if !consultResult {
		t.Error("history dropped the main conversation's consult result instead of leaving display filtering to its consumer")
	}
	if got := len(f.Requests()); got != 3 {
		t.Errorf("reading history made %d model calls", got-3)
	}
}

func TestConversationHistoryAcrossCompactions(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Text("The original case summary."),
		modelfixture.Text("The first compaction handoff."),
		modelfixture.Text("The first continuation answer."),
		modelfixture.Text("The second compaction handoff."),
		modelfixture.Text("The latest continuation answer."),
	))
	ctx := t.Context()
	if _, err := s.Orient(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Continue after the first compaction.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Continue after the second compaction.", nil); err != nil {
		t.Fatal(err)
	}
	calls := len(f.Requests())
	s = reopenHistorySession(t, s)
	messages, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"model: The original case summary.",
		"model: The first compaction handoff.",
		"engineer: Continue after the first compaction.",
		"model: The first continuation answer.",
		"model: The second compaction handoff.",
		"engineer: Continue after the second compaction.",
		"model: The latest continuation answer.",
	}
	if d := cmp.Diff(want, historyDialogue(messages)); d != "" {
		t.Errorf("compaction lineage dialogue (-want +got):\n%s", d)
	}
	if got := len(f.Requests()); got != calls {
		t.Errorf("reading history made %d model calls", got-calls)
	}
}

func TestConversationHistoryEmptyIsReadOnly(t *testing.T) {
	s, f := companionSession(t, nil)
	for range 2 {
		messages, err := s.ConversationHistory(t.Context())
		if err != nil || len(messages) != 0 {
			t.Fatalf("empty history = %v, %v", messages, err)
		}
	}
	var conversations, messages, invocations int
	if err := s.c.st.DB().QueryRowContext(t.Context(), `SELECT
		(SELECT count(*) FROM companion_conversations),
		(SELECT count(*) FROM companion_messages),
		(SELECT count(*) FROM invocations)`).Scan(&conversations, &messages, &invocations); err != nil {
		t.Fatal(err)
	}
	if conversations != 0 || messages != 0 || invocations != 0 {
		t.Errorf("reading empty history created %d conversations, %d messages and %d invocations", conversations, messages, invocations)
	}
	if got := len(f.Requests()); got != 0 {
		t.Errorf("reading empty history made %d model calls", got)
	}
}

func TestConversationHistoryReportsStoreErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*testing.T, *Session)
	}{
		{
			name: "closed store",
			change: func(t *testing.T, s *Session) {
				t.Helper()
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing ancestor",
			change: func(t *testing.T, s *Session) {
				t.Helper()
				if _, err := s.c.st.DB().ExecContext(t.Context(), `UPDATE companion_conversations SET compacted_from = 'missing-conversation'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "cyclic lineage",
			change: func(t *testing.T, s *Session) {
				t.Helper()
				if _, err := s.c.st.DB().ExecContext(t.Context(), `UPDATE companion_conversations SET compacted_from = id`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable message",
			change: func(t *testing.T, s *Session) {
				t.Helper()
				if _, err := s.c.st.DB().ExecContext(t.Context(), `UPDATE companion_messages SET calls = 'invalid-json' WHERE kind = 'model'`); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, f := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
				return modelfixture.Text("A stored answer."), nil
			})
			if _, err := s.Orient(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			tt.change(t, s)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			messages, err := s.ConversationHistory(ctx)
			if err == nil || len(messages) != 0 {
				t.Errorf("history = %v, %v; want no partial history and an error", messages, err)
			}
			if got := len(f.Requests()); got != 1 {
				t.Errorf("reading broken history made %d model calls", got-1)
			}
		})
	}
}

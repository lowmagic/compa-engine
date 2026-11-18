package chat

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/engine/enginetest"
)

func TestReopenRestoresConversation(t *testing.T) {
	e := enginetest.EngineWith(t, chatConfig(t, "case.json"))
	s, err := e.StartSession(t.Context(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := start(t, s, Options{})
	first.say("which model?")
	before, err := s.CompanionUsage()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = e.StartSession(t.Context(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	reopened := start(t, s, Options{NoOpen: true})
	out := flat(reopened.out())
	for _, want := range []string{"The OVN controller cannot sync worker-1", "› which model?", "The first model answers."} {
		if n := strings.Count(out, want); n != 1 {
			t.Errorf("reopened conversation has %d occurrences of %q, want 1:\n%s", n, want, out)
		}
	}
	if strings.Index(out, "cannot sync") > strings.Index(out, "which model?") ||
		strings.Index(out, "which model?") > strings.Index(out, "The first model answers.") {
		t.Errorf("reopened messages are out of order:\n%s", out)
	}
	if strings.Contains(out, "Orient me") || strings.Contains(out, "# The case") {
		t.Errorf("reopening exposed internal instructions or case context:\n%s", out)
	}
	if !strings.Contains(out, "[e999:2] ⚠") || !strings.Contains(out, "effort high") {
		t.Errorf("restored answer lost its citation flag or original effort:\n%s", out)
	}
	after, err := s.CompanionUsage()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("reopening called the model (-before +after):\n%s", diff)
	}
	if len(reopened.m.answers) != 2 {
		t.Fatalf("reopened chat has %d answers, want 2", len(reopened.m.answers))
	}
	reopened.say("which model now?")
	if out := flat(reopened.out()); !strings.Contains(out, "a3 · fixture-model effort high") {
		t.Errorf("new answer did not continue restored answer labels:\n%s", out)
	}
}

func TestReopenRestoresEvidenceNavigation(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	h := start(t, s, Options{NoOpen: true})
	h.say("/evidence")
	if b := h.m.browser; b == nil || b.open == nil || b.open.name != "e1" || b.open.cur != 1 {
		t.Fatalf("reopened /evidence did not use the restored answer's first citation:\n%s", h.view())
	}
	h.press("esc")
	h.settle()
	mark := len(h.printed)
	h.press("ctrl+o")
	h.settle()
	if out := flat(h.since(mark)); !strings.Contains(out, "error syncing node worker-1: failed to get chassis") {
		t.Errorf("restored ctrl+o lost the last turn's tool output:\n%s", out)
	}
}

// TestReopenNamesToolEvidence: a reopened chat's tool line names the
// evidence the tool stored, as the live line did.
func TestReopenNamesToolEvidence(t *testing.T) {
	s := session(t, "case.json")
	toolLine := regexp.MustCompile(`▸ grep[^\n]*→ (e[0-9]+)`)
	live := toolLine.FindStringSubmatch(flat(start(t, s, Options{}).out()))
	if live == nil {
		t.Fatal("the live tool line names no evidence")
	}
	out := flat(start(t, s, Options{NoOpen: true}).out())
	if got := toolLine.FindStringSubmatch(out); got == nil || got[1] != live[1] {
		t.Errorf("the reopened tool line names %v, want %s:\n%s", got, live[1], out)
	}
}

func TestReopenChatRestoresConversation(t *testing.T) {
	e := enginetest.EngineWith(t, chatConfig(t, "case.json"))
	s, err := e.StartChat(t.Context(), "synthetic-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, s, Options{})
	h.say("which model?")
	before, err := s.CompanionUsage()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = e.StartChat(t.Context(), "synthetic-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	reopened := start(t, s, Options{})
	if out := flat(reopened.out()); !strings.Contains(out, "› which model?") ||
		strings.Count(out, "The first model answers.") != 1 {
		t.Errorf("reopened troubleshooting chat lost its conversation:\n%s", out)
	}
	after, err := s.CompanionUsage()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("reopening a troubleshooting chat called the model (-before +after):\n%s", diff)
	}
}

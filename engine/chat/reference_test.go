package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

func TestReadReferenceLabels(t *testing.T) {
	args := map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"}
	r := toolRun{id: "c1", conv: "synthetic", name: "read_reference", args: args}
	if got := runningLabel(r); got != "reading openshift.networking references/ovn.md" {
		t.Errorf("runningLabel = %q", got)
	}
	r.done = true
	if got := toolOutcome(r); got != "read openshift.networking references/ovn.md" {
		t.Errorf("toolOutcome (ok) = %q", got)
	}
	r.isError, r.result = true, "not declared"
	if got := toolOutcome(r); got != "read_reference openshift.networking references/ovn.md ✗ not declared" {
		t.Errorf("toolOutcome (err) = %q", got)
	}

	// Verify live event updates status bar while running and prints tool row once finished.
	h := start(t, session(t, "question.json"), Options{NoOpen: true})
	h.update(tea.WindowSizeMsg{Width: 120, Height: 45})
	h.settle()
	mark := len(h.printed)
	h.m.turn = &turnState{kind: kindAnswer, model: h.m.current}
	h.m.spinning = true
	call := enginetest.ToolCall{ID: "r1", Name: "read_reference", Args: args}
	h.update(eventMsg{engine.TalkEvent{Kind: engine.TalkToolStart, Conversation: "synthetic", Call: &call}})
	h.settle()
	if h.m.turn.status != "reading openshift.networking references/ovn.md" {
		t.Errorf("status = %q", h.m.turn.status)
	}
	h.update(eventMsg{engine.TalkEvent{Kind: engine.TalkToolDone, Conversation: "synthetic", Call: &call, Result: &enginetest.ToolResult{Text: "# Reference openshift.networking 0.1.0 references/ovn.md\n\nOK."}}})
	h.update(eventMsg{engine.TalkEvent{Kind: engine.TalkThinking, Conversation: "synthetic", Text: "thinking"}})
	h.settle()
	rows := batchRows(h.since(mark))
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != "▸ read openshift.networking references/ovn.md" {
		t.Errorf("rows = %q", rows)
	}
}

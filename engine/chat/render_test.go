package chat

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
)

// The model harness checks the text presented to the engineer and the physical
// row bound required by Bubble Tea v2.0.10's insertAbove. Actual native terminal
// scrollback, asynchronous redraws, and resize/reflow need the PTY smoke test.
func TestLongStreamCommitsOnceInBoundedRows(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	mark := len(h.printed)
	var lines []string
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("synthetic-answer-row-%03d", i))
	}
	h.m.turn = &turnState{kind: kindAnswer, model: h.m.current}
	// This synthetic turn has no work goroutine; its clock is test-owned.
	h.m.spinning = true
	for start := 0; start < len(lines); start += 10 {
		end := start + 10
		h.update(eventMsg{engine.TalkEvent{Kind: engine.TalkText, Conversation: "synthetic",
			Text: strings.Join(lines[start:end], "\n") + "\n"}})
		h.settle()
		view := h.view()
		if !strings.Contains(view, lines[end-1]) {
			t.Fatalf("streaming did not show its newest text:\n%s", view)
		}
		if n := strings.Count(view, h.m.input.Placeholder); n != 1 {
			t.Fatalf("streaming shows %d placeholder prompts, want one:\n%s", n, view)
		}
		if rows := strings.Count(view, "\n") + 1; rows >= h.m.height {
			t.Fatalf("streaming uses %d rows of a %d-row terminal, no insertion row remains", rows, h.m.height)
		}
	}
	h.update(turnDone{kind: kindAnswer, text: strings.Join(lines, "\n")})
	h.settle()
	out := h.since(mark)
	for _, line := range lines {
		if n := strings.Count(out, line); n != 1 {
			t.Errorf("%q was printed %d times, want once", line, n)
		}
	}
	for _, printed := range h.printed[mark:] {
		if strings.Contains(printed, "\n") || ansi.StringWidth(printed) > h.m.width {
			t.Fatalf("scrollback insertion exceeds one physical terminal row: %q", printed)
		}
	}
	if strings.Contains(out, h.m.input.Placeholder) || strings.Contains(out, "waiting for you") {
		t.Errorf("a transient prompt/status was queued into scrollback:\n%s", out)
	}
	if view := h.view(); strings.Contains(view, "synthetic-answer-row-") || !strings.Contains(view, "waiting for you") {
		t.Errorf("the completed answer remained in the live view:\n%s", view)
	}
}

func TestMainChatLeavesWheelToTerminal(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.update(tea.WindowSizeMsg{Width: 24, Height: 10})
	var draft []string
	for i := range 20 {
		draft = append(draft, fmt.Sprintf("draft-row-%02d", i))
	}
	h.m.input.SetValue(strings.Join(draft, "\n"))
	h.press("up", "up", "up")
	h.settle()
	before := h.m.View()
	value := h.m.input.Value()
	if before.MouseMode != tea.MouseModeNone || before.AltScreen {
		t.Fatalf("main chat captures terminal scrollback: mouse=%v alternate=%v", before.MouseMode, before.AltScreen)
	}
	for range 12 {
		h.update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		if after := h.m.View(); before.Content != after.Content || *before.Cursor != *after.Cursor {
			t.Fatal("a delivered wheel event moved the main chat's input or layout")
		}
		h.update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	h.settle()
	if h.m.input.Value() != value {
		t.Error("wheel events changed the draft")
	}
}

// Exercise the bound using the actual rendered footer and when the terminal is
// too small for both the normal textarea and the status line.
func TestInlineViewReservesInsertionRow(t *testing.T) {
	m := newModel(session(t, "case.json"), Options{NoOpen: true}, nil)
	m.input.SetValue(strings.Repeat("synthetic input\n", 7))
	m.turn = &turnState{}
	m.turn.live.WriteString(strings.Repeat("synthetic streamed row\n", 100))
	for _, size := range [][2]int{{80, 24}, {24, 10}, {5, 4}, {2, 2}, {1, 1}, {80, 30}} {
		m.update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		v := m.View()
		rows := strings.Split(v.Content, "\n")
		if v.Content == "" {
			rows = nil
		}
		if len(rows) > max(size[1]-1, 0) {
			t.Errorf("%dx%d view has %d rows, no insertion row remains", size[0], size[1], len(rows))
		}
		for _, row := range rows {
			// No row fills the terminal: one that did would wrap on any
			// narrowing, and a full-width print loses its last cell.
			if w := ansi.StringWidth(row); w > max(size[0]-1, 1) {
				t.Errorf("%dx%d row is %d columns, filling or passing the terminal", size[0], size[1], w)
			}
		}
		if c := v.Cursor; c != nil && c.X >= max(size[0]-1, 1) {
			t.Errorf("%dx%d cursor at column %d, in or past the last column", size[0], size[1], c.X)
		}
	}
}

// TestWidthChangeReprintsTheTranscript: once a width change settles, the
// chat clears the terminal and prints what it printed before again, at the
// new width, once; nothing is printed before the clear, and a resize that
// keeps the width reprints nothing. A terminal rewrapping on a narrowing
// leaves the inline frame's top behind otherwise
// (charmbracelet/bubbletea#1867), which only a real terminal shows.
func TestWidthChangeReprintsTheTranscript(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.m.print("synthetic first line", strings.Repeat("synthetic-word ", 12))
	h.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.settle()
	before := len(h.printed)
	h.update(tea.WindowSizeMsg{Width: 100, Height: 20})
	h.settle()
	if len(h.printed) != before || slices.ContainsFunc(h.screen, func(s string) bool { return strings.HasPrefix(s, "raw ") }) {
		t.Fatalf("a height change reprinted:\n%s", strings.Join(h.screen, "\n"))
	}

	mark := len(h.screen)
	for _, w := range []int{90, 70, 40} {
		h.update(tea.WindowSizeMsg{Width: w, Height: 20})
	}
	h.settle()
	got := h.screen[mark:]
	if len(got) == 0 || !strings.HasPrefix(got[0], "raw ") {
		t.Fatalf("the reprint did not begin with the clear:\n%s", strings.Join(got, "\n"))
	}
	clears, firsts := 0, 0
	for _, s := range got {
		if strings.HasPrefix(s, "raw ") {
			clears++
		}
		if s == "print synthetic first line" {
			firsts++
		}
		if strings.HasPrefix(s, "print ") && ansi.StringWidth(strings.TrimPrefix(s, "print ")) > 39 {
			t.Errorf("a reprinted row is wider than the new chat width: %q", s)
		}
	}
	if clears != 1 || firsts != 1 {
		t.Errorf("three width steps made %d clears and printed the first line %d times, want 1 and 1:\n%s",
			clears, firsts, strings.Join(got, "\n"))
	}
}

func TestStreamRenderingResetsBetweenCalls(t *testing.T) {
	turn := &turnState{}
	turn.live.WriteString("synthetic first")
	first := turn.rendered(80)
	turn.resetLive()
	turn.live.WriteString("synthetic other")
	if got := turn.rendered(80); got == first || !strings.Contains(ansi.Strip(got), "synthetic other") {
		t.Errorf("the next call reused the previous call's rendering: %q", got)
	}
}

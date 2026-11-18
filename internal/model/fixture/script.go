package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
)

// Script is what a scripted fixture answers: for each turn, the answers to
// its calls in order. A turn is chosen by the newest message with text in
// the request, the engineer's words or the engine's instruction, as the
// first turn whose When it contains; an empty When matches any.
type Script struct {
	Turns []ScriptTurn `json:"turns"`
	// Pace is the pause between streamed pieces, for watching a run.
	Pace string `json:"pace,omitempty"`
}

// ScriptTurn is one turn's answers: the first call of the turn gets the
// first, and each call after one that ran tools the next.
type ScriptTurn struct {
	When    string         `json:"when"`
	Answers []ScriptAnswer `json:"answers"`
}

// ScriptAnswer is one answer: text, streamed word by word, or tool calls.
type ScriptAnswer struct {
	Text  string       `json:"text,omitempty"`
	Calls []ScriptCall `json:"calls,omitempty"`
}

// ScriptCall is one tool call of a scripted answer.
type ScriptCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Scripted is a fixture that answers each call from the JSON script at the
// path the call names as its model: how configuration drives a
// deployment's binary with no model (provider fixture, its model the
// script's path), for the chat's end-to-end check. It sends nothing anywhere.
func Scripted() *Provider {
	p := New()
	p.Respond = func(_ context.Context, r model.Request) (*Answer, error) {
		data, err := os.ReadFile(r.Model)
		if err != nil {
			return nil, fmt.Errorf("fixture script: %w", err)
		}
		var s Script
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("fixture script %s: %w", r.Model, err)
		}
		return s.answer(r)
	}
	return p
}

func (s Script) answer(r model.Request) (*Answer, error) {
	last, after := -1, 0
	for i, m := range r.Messages {
		if m.Role == model.RoleUser && m.Text != "" && len(m.Results) == 0 {
			last, after = i, 0
		}
		if m.Role == model.RoleModel && i > last {
			after++
		}
	}
	asked := ""
	if last >= 0 {
		asked = r.Messages[last].Text
	}
	for ti, t := range s.Turns {
		if !strings.Contains(asked, t.When) {
			continue
		}
		if after >= len(t.Answers) {
			return nil, fmt.Errorf("fixture script: turn %q has no answer %d", t.When, after+1)
		}
		a := t.Answers[after]
		out := &Answer{}
		for ci, c := range a.Calls {
			out.Calls = append(out.Calls, model.ToolCall{ID: fmt.Sprintf("s%d-%d-%d", ti+1, after+1, ci+1), Name: c.Name, Args: c.Args})
		}
		if a.Text != "" {
			out.Pace, _ = time.ParseDuration(s.Pace)
			out.Pieces = strings.SplitAfter(a.Text, " ")
		}
		return out, nil
	}
	return nil, fmt.Errorf("fixture script: no turn matches %q", firstLine(asked))
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

package chat

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// said is one turn as the browser's turns tab lists it: what the engineer
// sent or what the model wrote, as stored, its Markdown unrendered.
type said struct {
	// label is a1, a2, … for an answer, as the chat names it; you for the
	// engineer; model for the model's words beside its tool calls.
	label string
	text  string
	at    time.Time
	// preview is the text on one line as the chat shows it, its Markdown's
	// markers gone, for the list.
	preview string
}

// turnsOf are the conversation's turns, oldest first, from its stored
// messages: the engineer's and the model's with text, the reviewer's
// reviews and the engineer's verdicts the model reads. An answer, the
// model's text with no tool call beside it, is named as the chat names it
// on reopening (engine.AnswerViews), a draft said so with its intention
// and its citations taken out as it was shown, and listed as the engineer
// was shown it. With pipeline, the answer as written is listed instead,
// then under its name the editor's correction of it, the engine's ask for
// its revision, the revision, taken or not, and what was shown after one
// not taken, each as stored; a review by what it is of, as stored.
func turnsOf(messages []engine.ConversationMessage, pipeline bool) []said {
	owner := map[int]engine.AnswerView{}
	for _, v := range engine.AnswerViews(messages) {
		for i := v.Original; i <= v.At; i++ {
			owner[i] = v
		}
	}
	var out []said
	for i, msg := range messages {
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		s := said{label: "you", text: msg.Text, at: msg.CreatedAt}
		v, owned := owner[i]
		draft := ""
		if owned && v.Draft {
			draft = " · draft"
			if v.DraftIntention != "" {
				draft += ": " + v.DraftIntention
			}
		}
		switch {
		case msg.Kind == engine.MessageEngineer:
		case owned && !pipeline:
			if i != v.At {
				continue
			}
			s.label, s.text = v.Label+draft, v.Text
			if v.Draft {
				s.text = engine.StripDraftCites(v.Text)
			}
		case owned && i == v.Original:
			s.label = v.Label + draft
		case owned && msg.Kind == engine.MessageCorrection:
			s.label = v.Label + " corrected by the editor"
		case owned && msg.Kind == engine.MessageShown:
			s.label = v.Label + " as shown, its revision not taken"
		case owned && msg.Kind == engine.MessageRevise:
			s.label = v.Label + " sent back for revision"
		case owned && msg.Kind == engine.MessageModel && i == v.At:
			s.label = v.Label + " revised"
		case owned && msg.Kind == engine.MessageModel:
			s.label = v.Label + " revision not taken"
		case msg.Kind == engine.MessageModel:
			s.label = "model"
		case msg.Kind == engine.MessageReview && msg.Review != nil:
			s.label = "review of " + msg.Review.Of
		case msg.Kind == engine.MessageReview:
			s.label = "review"
		case msg.Kind == engine.MessageVerdict:
			s.label = "your verdict"
		default:
			continue
		}
		s.preview = firstLine(ansi.Strip(markdown(text.Safe(msg.Text), 0, nil)))
		out = append(out, s)
	}
	return out
}

// saidLabel is a turn as the turns tab lists it.
func saidLabel(s said) string {
	return s.label + " · " + s.at.UTC().Format("2006-01-02 15:04") + " · " + s.preview
}

// turnsKey is a key on the turns tab: the cursor moves, enter opens one
// whole, y copies it, o lists the editor's steps or hides them; on a turn
// open, w wraps it with no numbers, for selecting with the mouse.
func (m *model) turnsKey(key string) tea.Cmd {
	b := m.browser
	if o := b.saidOpen; o != nil {
		switch key {
		case "up", "k":
			o.cur--
		case "down", "j":
			o.cur++
		case "pgup":
			o.cur -= 20
		case "pgdown", "space":
			o.cur += 20
		case "home", "g":
			o.cur = 0
		case "end", "G":
			o.cur = len(o.lines) - 1
		case "y":
			return m.copySaid()
		case "w":
			o.raw = !o.raw
		case "left", "right":
			o.scroll(key, m.width)
			return nil
		}
		o.cur = min(max(o.cur, 0), max(len(o.lines)-1, 0))
		return nil
	}
	switch key {
	case "o":
		b.pipeline = !b.pipeline
		b.said = turnsOf(b.history, b.pipeline)
		b.saidCur = max(len(b.said)-1, 0)
		return nil
	case "up", "k":
		b.saidCur--
	case "down", "j":
		b.saidCur++
	case "pgup":
		b.saidCur -= 10
	case "pgdown":
		b.saidCur += 10
	case "home", "g":
		b.saidCur = 0
	case "end", "G":
		b.saidCur = len(b.said) - 1
	case "enter":
		if b.saidCur < len(b.said) {
			s := b.said[b.saidCur]
			head := []string{text.Safe(s.label + " · " + s.at.UTC().Format("2006-01-02 15:04") + " · its Markdown unrendered")}
			b.saidOpen = &opened{name: s.label, head: head, lines: contentLines(s.text), top: -1}
		}
	case "y":
		return m.copySaid()
	}
	b.saidCur = min(max(b.saidCur, 0), max(len(b.said)-1, 0))
	return nil
}

// copySaid copies the turn under the cursor, as stored, said in the
// browser and printed once the chat has its screen back.
func (m *model) copySaid() tea.Cmd {
	b := m.browser
	if b.saidCur >= len(b.said) {
		b.note = "the conversation has no turns yet"
		return nil
	}
	s := b.said[b.saidCur]
	b.note = "asked the terminal to copy " + s.label + " to the clipboard (OSC 52)"
	m.print(noteLine("asked the terminal to copy " + s.label + " to the clipboard (OSC 52); a terminal that does not take it copies nothing"))
	return tea.SetClipboard(s.text)
}

// saidRows are the turns tab's rows, the cursor's in them.
func (b *browser) saidRows(w, room int) []string {
	if len(b.said) == 0 {
		return []string{faintW.Render("no turns yet: what you send and what the model writes are listed here")}
	}
	first := max(0, min(b.saidCur-room/2, len(b.said)-room))
	var out []string
	for i := first; i < len(b.said) && len(out) < room; i++ {
		row := ansi.Truncate(text.Safe(saidLabel(b.said[i])), w-2, "…")
		if i == b.saidCur {
			out = append(out, cursorW.Render("› "+row))
			continue
		}
		out = append(out, "  "+row)
	}
	return out
}

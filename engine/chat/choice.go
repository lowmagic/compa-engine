package chat

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// choicePanel is a choice the chat offers above the status bar, as the
// slash popover is drawn: its rows bounded, never printed into the
// scrollback, its whole area cleared when it closes. While it is open
// the keys are its own, but ctrl+c; nothing it offers runs until chosen.
type choicePanel struct {
	// head is what the panel says above its options, each line wrapped,
	// and every line kept at least one row; short says why nothing is
	// chosen while the room cannot show them all, and shown whether the
	// last view could.
	head     []string
	short    string
	shown    bool
	options  []choiceOption
	selected int
	hint     string
	// dismiss is what esc does once the panel is closed.
	dismiss func(m *model)
}

// choiceOption is one choice, chosen by its key or by enter on it; one
// with no label is taken by its key, or by enter as the first, and is not
// listed.
type choiceOption struct {
	key, label string
	run        func(m *model)
}

// listed are the options the panel lists, those with a label.
func (p *choicePanel) listed() []choiceOption {
	var out []choiceOption
	for _, o := range p.options {
		if o.label != "" {
			out = append(out, o)
		}
	}
	return out
}

// rows are the panel's rows within width and room: each head line, wrapped
// and kept at least its first row, a row cut short ending in …, then its
// options, then the hint. Where room cannot hold every head line, an
// option and the hint, it draws nothing but, with a head, the one row
// saying why (short), and is not shown.
func (p *choicePanel) rows(width, room int) []string {
	listed := p.listed()
	need := len(p.head) + min(len(listed), 1) + 1
	p.shown = len(p.options) > 0 && width >= 1 && room >= need
	if !p.shown {
		if len(p.head) > 0 && room >= 1 && width >= 1 {
			return []string{warnW.Render(ansi.Truncate(p.short, width, "…"))}
		}
		return nil
	}
	free := room - need
	var rows []string
	for _, h := range p.head {
		wrapped := strings.Split(ansi.Wrap(h, width, ""), "\n")
		keep := min(len(wrapped), 1+free)
		free -= keep - 1
		if keep < len(wrapped) {
			wrapped[keep-1] = ansi.Truncate(wrapped[keep-1], width-1, "") + "…"
		}
		rows = append(rows, wrapped[:keep]...)
	}
	head := len(rows)
	if len(listed) > 0 {
		p.selected = min(max(p.selected, 0), len(listed)-1)
		count := min(len(listed), room-1-head, 6)
		first := max(0, min(p.selected-count/2, len(listed)-count))
		for i := first; i < first+count; i++ {
			mark := "  "
			if i == p.selected {
				mark = "› "
			}
			row := ansi.Truncate(mark+listed[i].key+"  "+listed[i].label, width, "…")
			if i == p.selected {
				row = cursorW.Render(row)
			} else {
				row = faintW.Render(row)
			}
			rows = append(rows, row)
		}
	}
	return append(rows, faintW.Render(ansi.Truncate(p.hint, width, "…")))
}

// choiceKey takes a key while a panel is open: an option's key or enter
// chooses, the arrows select, esc closes it; it reports false only for
// ctrl+c, which the chat handles as always. A panel whose head the room
// cannot show takes no choice and says why.
func (m *model) choiceKey(k string) bool {
	p := m.choice
	listed := p.listed()
	if len(p.head) > 0 && !p.shown && k != "esc" && k != "ctrl+c" {
		m.print(warnLine(p.short))
		return true
	}
	switch k {
	case "ctrl+c":
		return false
	case "up":
		p.selected = max(p.selected-1, 0)
	case "down":
		p.selected = max(min(p.selected+1, len(listed)-1), 0)
	case "enter":
		m.choice = nil
		if len(listed) == 0 {
			p.options[0].run(m)
			break
		}
		listed[min(max(p.selected, 0), len(listed)-1)].run(m)
	case "esc":
		m.choice = nil
		if p.dismiss != nil {
			p.dismiss(m)
		}
	default:
		for _, o := range p.options {
			if k == o.key {
				m.choice = nil
				o.run(m)
				break
			}
		}
	}
	return true
}

// offerReopen offers, on a reopened case, the ways on where the
// provider's cache has expired on a large conversation, then, once that
// is chosen or put off, or at once where the cache is fine, the
// catch-up where the case gained comments or attachments.
func (m *model) offerReopen(r engine.ReopenState, offer engine.ReloadOffer) {
	m.news = nil
	if r.Fresh() {
		m.news = &r
	}
	if !r.CacheExpired || !r.Large {
		m.offerCatchUp()
		return
	}
	m.choice = &choicePanel{
		options: []choiceOption{
			{key: "1", label: "full context — resend about " + engine.Thousands(offer.Estimate) + " tokens without the cache",
				run: func(m *model) { m.reload(engine.ReloadFull) }},
			{key: "2", label: "suggested context — the case's state, the ledger, " + text.Plural(offer.Ranked, "ranked item") + " and the last exchange",
				run: func(m *model) { m.reload(engine.ReloadSuggested) }},
			{key: "3", label: "suggested context and a summary — one model call over the earlier conversation",
				run: func(m *model) { m.reload(engine.ReloadSummary) }},
		},
		hint: "1 2 3 or ↑↓ enter choose · esc decide later (/reload)",
		dismiss: func(m *model) {
			m.print(noteLine("decided later: /reload offers the three again; a message goes on in the conversation as it is"))
			m.offerCatchUp()
		},
	}
}

// offerCatchUp offers the catch-up the reopening found news for, once.
func (m *model) offerCatchUp() {
	if m.news == nil {
		return
	}
	r := *m.news
	m.news = nil
	m.choice = &choicePanel{
		options: []choiceOption{{key: "c", label: "catch up on what's new (" + newsShown(r) + ")",
			run: func(m *model) { m.catchUp() }}},
		hint: "c or enter catch up · esc skip (/catchup later)",
		dismiss: func(m *model) {
			m.print(noteLine("skipped: /catchup catches up later; your next message is given what is new"))
		},
	}
}

// newsShown counts what a reopened case gained: its comments and
// attachments.
func newsShown(r engine.ReopenState) string {
	parts := []string{text.Plural(r.NewComments, "comment")}
	switch {
	case len(r.NewAttachments) > 0:
		parts = append(parts, text.Plural(len(r.NewAttachments), "attachment"))
	case r.Attachments:
		parts = append(parts, "the attachments changed")
	default:
		parts = append(parts, "0 attachments")
	}
	return strings.Join(parts, ", ")
}

// reload goes on with the conversation in mode, as a turn: only the
// summary calls the model.
func (m *model) reload(mode engine.ReloadMode) {
	status := map[engine.ReloadMode]string{
		engine.ReloadFull:      "keeping the conversation",
		engine.ReloadSuggested: "beginning a new conversation from the case's state",
		engine.ReloadSummary:   "calling the model for a handoff",
	}[mode]
	if status == "" {
		m.print(errorLine("/reload takes full, suggested or summary"))
		return
	}
	s := m.s
	m.startTurn(kindReload, status, func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
		r, err := s.Reload(ctx, mode, on)
		return turnDone{reloaded: &r, text: r.Handoff, err: err}
	})
}

// reloadChoices lists the ways on with what each costs now.
func reloadChoices(ctx context.Context, s *engine.Session) jobDone {
	o, err := s.ReloadOffer(ctx)
	if err != nil {
		return jobDone{err: err}
	}
	return jobDone{lines: []string{
		noteLine("/reload full — the conversation as it is: its next call resends about " + engine.Thousands(o.Estimate) + " tokens, without the cache once it has expired"),
		noteLine("/reload suggested — a new conversation from the case's state, the ledger, " + text.Plural(o.Ranked, "ranked item") + ", the pinned items and the last exchange; no model call"),
		noteLine("/reload summary — the same and a handoff the model writes: one model call over the earlier conversation"),
	}}
}

// reloaded says what a reload did; a summary's handoff prints as an
// answer.
func (m *model) reloaded(d turnDone, model engine.CompanionModel) {
	r := d.reloaded
	switch r.Mode {
	case engine.ReloadFull:
		m.print(noteLine("full context: the conversation goes on as it is; its next call resends about " + engine.Thousands(r.Estimate) + " tokens"))
	case engine.ReloadSuggested:
		m.print(noteLine("suggested context: a new conversation begun from the case as it is now, the ledger, " + text.Plural(r.Ranked, "ranked item") +
			", the pinned items and the last exchange, about " + engine.Thousands(r.Estimate) + " tokens; the earlier one is kept"))
	case engine.ReloadSummary:
		m.printAnswer(d, model, false)
	}
}

// catchUp asks the conversation what changed since the case was last
// worked, as a turn: the ledger reads the new comments first; with
// nothing new no model is called.
func (m *model) catchUp() {
	if m.s.IsChat() {
		m.print(errorLine("a chat has no case to catch up on"))
		return
	}
	s := m.s
	m.startTurn(kindCatchUp, "catching up on what's new", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
		reply, err := s.CatchUp(ctx, on)
		if errors.Is(err, engine.ErrNothingNew) {
			return turnDone{note: err.Error()}
		}
		return turnDone{reply: reply, text: reply.Text, check: reply.Check, err: err}
	})
}

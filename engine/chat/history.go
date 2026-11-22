package chat

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

type historyEntry struct {
	message engine.ConversationMessage
	model   engine.CompanionModel
	usage   string
	cites   []engine.Cite
	checks  []engine.CiteCheck
	flags   []engine.AnswerFlag
	turn    int
	conv    string
	// check is what the editor made of the answer, and verdicts the
	// engineer's on its span flags; shown says the entry is an answer as
	// it was printed, and draft says it is a comment draft.
	check          engine.AnswerCheck
	verdicts       []engine.SpanVerdict
	shown          bool
	draft          bool
	draftIntention string
}

type historyMsg struct {
	entries  []historyEntry
	checkErr error
	flagErr  error
}

// shownAnswer says whether an entry is an answer as the chat prints it:
// the model's, its revision, the editor's correction of it, or the record
// of what was shown after a revision not taken (engine.AnswerViews).
func (e historyEntry) shownAnswer() bool { return e.shown }

// readHistory reads only the local store. In particular, --no-open must
// recover the engineer's answers even when no provider is available. An
// answer the editor corrected, or the model revised, is restored as it was
// printed (engine.AnswerViews): what was shown in its place, under its
// label, with what the check made of it; the answer it replaced, and a
// revision not taken with its calls, are not. A review is restored as it
// was printed, where it came, and is no answer.
func (m *model) readHistory(ctx context.Context, send func(tea.Msg)) error {
	total, usageErr := m.s.CompanionTotalUsage()
	send(usageMsg{total: total, err: usageErr})
	messages, err := m.s.ConversationHistory(ctx)
	if err != nil {
		return err
	}
	views := map[int]engine.AnswerView{}
	hidden := map[int]bool{}
	for _, v := range engine.AnswerViews(messages) {
		views[v.At] = v
		for i := v.Original; i < v.At; i++ {
			hidden[i] = true
		}
	}
	h := historyMsg{}
	for i, message := range messages {
		v, shown := views[i]
		switch {
		case hidden[i]:
			continue
		case shown:
		case message.Kind == engine.MessageEngineer, message.Kind == engine.MessageModel, message.Kind == engine.MessageResults, message.Kind == engine.MessageReview:
		default:
			continue
		}
		e := historyEntry{message: message, shown: shown}
		// An answer's model and usage are the call that wrote what is
		// shown, the original's where the engine wrote it.
		from := message
		if message.Kind != engine.MessageModel && shown {
			from = messages[v.Original]
		}
		if rec := from.Call; rec != nil {
			e.model = engine.CompanionModel{Name: rec.Model.ModelID, Effort: rec.Model.Effort}
			e.usage = engine.UsageFidelity(rec.Usage)
			e.turn, e.conv = rec.Turn, from.ConversationID
		}
		if shown {
			e.message.Text = v.Text
			e.draft = v.Draft
			e.draftIntention = v.DraftIntention
			if c := messages[v.Original].Check; c != nil {
				e.check = *c
			}
			e.verdicts = messages[v.Original].Verdicts
		}
		h.entries = append(h.entries, e)
	}
	var cites []engine.Cite
	for i, e := range h.entries {
		if (e.message.Kind == engine.MessageModel || e.shown) && strings.TrimSpace(e.message.Text) != "" {
			h.entries[i].cites = engine.Cites(e.message.Text)
			cites = append(cites, h.entries[i].cites...)
		}
	}
	if len(cites) > 0 {
		checks, err := m.s.CheckCites(ctx, cites)
		h.checkErr = err
		if err == nil {
			at := 0
			for i := range h.entries {
				n := len(h.entries[i].cites)
				h.entries[i].checks = checks[at : at+n]
				at += n
			}
		}
	}
	// An answer's own checks, as a turn's end runs them: every item the
	// conversation saw read once for all the answers.
	var answers []int
	var texts []string
	for i, e := range h.entries {
		if e.shownAnswer() {
			answers = append(answers, i)
			texts = append(texts, e.message.Text)
		}
	}
	if len(texts) > 0 {
		flags, err := m.s.AnswerFlags(ctx, texts)
		h.flagErr = err
		if len(flags) == len(texts) {
			for j, i := range answers {
				h.entries[i].flags = flags[j]
			}
		}
	}
	if len(h.entries) > 0 {
		send(h)
	}
	return nil
}

func (m *model) restoreHistory(h historyMsg) {
	var tools []toolRun
	var turn int
	var conv string
	flush := func() {
		if len(tools) > 0 {
			m.print(toolRows(tools, m.cols())...)
			m.lastTools = append(m.lastTools, tools...)
			tools = nil
		}
	}
	for _, entry := range h.entries {
		message := entry.message
		switch message.Kind {
		case engine.MessageEngineer:
			flush()
			m.lastTools = nil
			m.print("", youLine(message.Text))
		case engine.MessageReview:
			flush()
			// A review whose check was not kept is printed as stored.
			if r := message.Review; r != nil {
				m.printReview(*r)
			} else {
				m.print("", markdown(text.Safe(message.Text), m.cols(), nil))
			}
		case engine.MessageModel, engine.MessageCorrection, engine.MessageShown:
			flush()
			if entry.conv != "" && (entry.conv != conv || entry.turn != turn) {
				m.lastTools = nil
				conv, turn = entry.conv, entry.turn
			}
			switch {
			case entry.shownAnswer():
				kind := kindAnswer
				if entry.draft {
					kind = kindDraft
				}
				m.printAnswer(turnDone{kind: kind, draftIntention: entry.draftIntention, text: message.Text, cites: entry.cites, checks: entry.checks,
					checkErr: h.checkErr, flags: entry.flags, flagErr: h.flagErr, verdicts: entry.verdicts, usage: entry.usage, check: entry.check}, entry.model)
			case len(message.Calls) > 0 && strings.TrimSpace(message.Text) != "":
				m.print(markdown(text.Safe(message.Text), m.cols(), marksOf(entry.cites, entry.checks)))
			}
			for _, call := range message.Calls {
				tools = append(tools, toolRun{id: call.ID, name: call.Name, args: call.Args})
			}
		case engine.MessageResults:
			for _, result := range message.Results {
				for i := range tools {
					if tools[i].id == result.CallID {
						tools[i].done, tools[i].result, tools[i].isError = true, result.Text, result.IsError
						// The stored result keeps its text only; the
						// evidence it names is in the head the engine wrote.
						if !result.IsError {
							tools[i].evidence = engine.ExcerptEvidence(result.Text)
						}
					}
				}
			}
		}
	}
	flush()
}

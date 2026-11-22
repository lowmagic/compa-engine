package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// turnKind says what a turn is for, and so what its end prints.
type turnKind int

const (
	// kindOpening reads the case and runs the opening turn.
	kindOpening turnKind = iota
	kindAnswer
	kindDraft
	kindCompact
	kindLoad
	// kindLedger reads the comments the ledger has not, if any, and shows
	// the ledger.
	kindLedger
	// kindReload goes on in a new conversation from the case's state and
	// a handoff, /compact in a case.
	kindReload
	// kindReview has the reviewer review the last answer or an item.
	kindReview
)

// turnState is the turn running.
type turnState struct {
	kind   turnKind
	cancel context.CancelFunc
	// model is the model and effort the turn began on.
	model engine.CompanionModel
	// status is what the turn is doing now, for the status line.
	status string
	// conv is the turn's main conversation, from its first event; a
	// consult's events carry their own.
	conv string
	// live is the main conversation's text streamed by the call running,
	// and shown its rendering at a width.
	live              strings.Builder
	shown             string
	shownLen, shownAt int
	// mark is what the live view says above the answer streaming, where
	// the editor checks it before it is printed; checking says the
	// editor's check, and any revision, has begun.
	mark     string
	checking bool
	// held are lines said while the live view shows an answer the editor
	// has yet to settle, printed once the turn has printed what it ends
	// with (say).
	held []string
	// batch is the tool calls since the model last called; tools all the
	// turn's.
	batch, tools []toolRun
	// made are the IDs of the calls the model of each conversation made
	// since the batch was last printed, in its order: the calls run
	// together, so they start and end in any.
	made map[string][]string
}

// rendered is the streamed text's rendering at width, made again only
// when either has changed.
func (t *turnState) rendered(width int) string {
	if t.live.Len() != t.shownLen || width != t.shownAt {
		raw := t.live.String()
		if t.kind == kindDraft {
			raw = engine.StripDraftCites(raw)
		}
		t.shown = markdown(text.Safe(raw), width, nil)
		t.shownLen, t.shownAt = t.live.Len(), width
	}
	return t.shown
}

// resetLive also invalidates the rendering cache: a later model call can
// stream a different text of exactly the same byte length.
func (t *turnState) resetLive() {
	t.live.Reset()
	t.shown, t.shownLen, t.shownAt = "", 0, 0
}

// toolRun is one tool call of a turn and, once done, its result.
type toolRun struct {
	id, conv, name string
	args           map[string]any
	done           bool
	result         string
	isError        bool
	evidence       []string
}

// answer is an answer printed this run: its label and the evidence it
// cites, for the browser; its span flags and the engineer's verdicts on
// them by span, its findings that take no verdict, and the anchors asked
// of its revision, which are no finding, for the checks tab.
type answer struct {
	label    string
	cites    []engine.Cite
	flags    []engine.AnswerFlag
	verdicts map[string]engine.SpanVerdict
	reads    []reading
	asks     []reading
}

// Messages a turn sends as it runs, and its end.
type (
	eventMsg struct{ ev engine.TalkEvent }
	stateMsg struct{ state string }
	noteMsg  struct{ line string }
	// ledgerCallMsg is a call of the ledger begun by the opening, during
	// it or after; ledgerRunningMsg its run going on past the opening.
	ledgerCallMsg    struct{ ev engine.TalkEvent }
	ledgerRunningMsg struct{ job *engine.LedgerJob }
	// recordMsg keeps this process's record and a cumulative stored usage
	// snapshot, read independently so either failure leaves the other useful.
	recordMsg struct {
		rec      engine.InvocationRecord
		err      error
		total    engine.UsageAggregate
		totalErr error
	}
	turnDone struct {
		kind  turnKind
		reply engine.TalkReply
		// text is what the turn ends with to print: an answer, a draft or
		// a handoff; item a draft's item; draftIntention a restored draft's
		// intention; loaded the expert loaded.
		text           string
		item           engine.CompanionItem
		draftIntention string
		loaded         string
		// ledger is the case ledger as /ledger shows it.
		ledger string
		// reloaded is what a reload did; note a line of the chat's own
		// the turn ends with in place of an answer.
		reloaded *engine.Reloaded
		note     string
		// cites are text's citations, checks their checks, or checkErr why
		// there are none; flags what the deterministic checks show beneath
		// it beside them, or flagErr why there are none, and verdicts the
		// engineer's on its span flags, a reopened answer's; usage how the
		// last call's usage was measured; check what the editor made of
		// it.
		cites    []engine.Cite
		checks   []engine.CiteCheck
		checkErr error
		flags    []engine.AnswerFlag
		flagErr  error
		verdicts []engine.SpanVerdict
		usage    string
		check    engine.AnswerCheck
		// review is the reviewer's review a kindReview turn ends with.
		review *engine.Review
		err    error
	}
)

// turnWork is what a turn does: it calls the session with on for its
// events and may send the chat its state and notes.
type turnWork func(ctx context.Context, on func(engine.TalkEvent), send func(tea.Msg)) turnDone

// startTurn runs work as the turn, which the caller has checked is the
// only one; its end checks the citations of what it ends with, runs the
// deterministic checks shown beneath it, and reads how its last call's
// usage was measured. A review or the checks tab offered is let go.
func (m *model) startTurn(kind turnKind, status string, work turnWork) {
	s := m.s
	t := &turnState{kind: kind, status: status, model: m.current}
	if s.EditsAnswers() && kind != kindCompact && kind != kindLoad && kind != kindLedger && kind != kindReload && kind != kindReview {
		t.mark = markChecking
	}
	t.cancel = m.spawn(func(ctx context.Context, send func(tea.Msg)) tea.Msg {
		var mu sync.Mutex
		last, editing := "", false
		on := func(ev engine.TalkEvent) {
			mu.Lock()
			switch {
			case ev.Kind == engine.TalkStage:
				// The editor's own calls are not the answer's.
				editing = ev.Text == engine.StageChecking
			case ev.Kind == engine.TalkCall && ev.Invocation.Parent == "" && !editing:
				last = ev.Invocation.ID
			}
			mu.Unlock()
			send(eventMsg{ev})
		}
		d := work(ctx, on, send)
		d.kind = kind
		if d.text != "" {
			// An answer the editor finished with after a cancel is
			// printed, so its checks run past the cancel, within a bound.
			cctx := ctx
			if ctx.Err() != nil {
				var done context.CancelFunc
				cctx, done = context.WithTimeout(context.WithoutCancel(ctx), checkBound)
				defer done()
			}
			if d.cites = engine.Cites(d.text); len(d.cites) > 0 {
				d.checks, d.checkErr = s.CheckCites(cctx, d.cites)
			}
			// A reload's text is a handoff, or nothing to show: its own
			// checks are not run.
			if kind != kindReload {
				var flags [][]engine.AnswerFlag
				if flags, d.flagErr = s.AnswerFlags(cctx, []string{d.text}); len(flags) > 0 {
					d.flags = flags[0]
				}
			}
		}
		mu.Lock()
		id := last
		mu.Unlock()
		if id != "" {
			rec, err := s.Invocation(id)
			if err != nil {
				d.usage = "its usage could not be read: " + err.Error()
			} else {
				d.usage = engine.UsageFidelity(rec.Usage)
			}
		}
		return d
	})
	m.turn = t
}

// opening restores the local conversation, then prepares the case: its
// comments, counts and inventory, and, for a case new to the companion
// unless --no-open, the ledger beside them, waited for up to its bound
// before the opening turn. A case reopened makes no model call: the chat
// says what is new since it was last worked instead, and what the next
// call resends where the cache has expired, offering nothing to choose.
// --no-open skips only model calls, never the saved history.
func (m *model) opening() {
	s, open := m.s, !m.noOpen
	m.startTurn(kindOpening, "reading the saved conversation", func(ctx context.Context, on func(engine.TalkEvent), send func(tea.Msg)) turnDone {
		if err := m.readHistory(ctx, send); err != nil {
			return turnDone{err: err}
		}
		if s.IsChat() || ctx.Err() != nil {
			return turnDone{err: ctx.Err()}
		}
		reopened, err := s.HasConversation(ctx)
		if err != nil {
			return turnDone{err: err}
		}
		p, err := s.Prepare(ctx, engine.PrepareOptions{
			Call:     open && !reopened,
			Progress: func(state string) { send(stateMsg{state}) },
			// The ledger's calls count in the status bar, during the
			// opening and after it.
			On: func(ev engine.TalkEvent) {
				if ev.Kind == engine.TalkCall {
					send(ledgerCallMsg{ev})
				}
			},
		})
		if err != nil {
			return turnDone{err: err}
		}
		for _, err := range p.Problems {
			send(noteMsg{warnLine(err.Error())})
		}
		send(noteMsg{noteLine(inventoryLine(p.Inventory))})
		why := "--no-open makes no model call"
		if reopened {
			why = "a reopened case makes no model call"
		}
		send(noteMsg{ledgerStatusLine(p.Ledger, why)})
		if p.Ledger.Running {
			send(ledgerRunningMsg{p.Ledger.Job})
		}
		if reopened {
			r, _, err := s.ReopenState(ctx)
			if err != nil {
				return turnDone{err: err}
			}
			for _, l := range reopenLines(r) {
				send(noteMsg{noteLine(l)})
			}
			return turnDone{}
		}
		if !open {
			return turnDone{}
		}
		send(stateMsg{"calling the model"})
		reply, err := s.Orient(ctx, on)
		return turnDone{reply: reply, text: reply.Text, check: reply.Check, err: err}
	})
}

// inventoryLine is the attachments' inventory in one line: how many
// attachments, their captures by currency, and those with none.
func inventoryLine(inv engine.Inventory) string {
	n := len(inv.Attachments) + inv.Omitted
	if n == 0 {
		return "inventory: no attachments"
	}
	counts := map[string]int{}
	none := 0
	for _, a := range inv.Attachments {
		if len(a.Captures) == 0 {
			none++
		}
		for _, c := range a.Captures {
			counts[c.Currency]++
		}
	}
	parts := []string{text.Plural(n, "attachment")}
	var captures []string
	for _, c := range []string{engine.CaptureCurrent, engine.CaptureOutdated, engine.CaptureDuplicate, engine.CaptureUncertain} {
		if counts[c] > 0 {
			captures = append(captures, strconv.Itoa(counts[c])+" "+c)
		}
	}
	if len(captures) > 0 {
		parts = append(parts, "captures "+strings.Join(captures, ", "))
	}
	if none > 0 {
		parts = append(parts, strconv.Itoa(none)+" with no capture identified")
	}
	if inv.Omitted > 0 {
		parts = append(parts, strconv.Itoa(inv.Omitted)+" not inventoried")
	}
	return "inventory: " + strings.Join(parts, " · ")
}

// ledgerStatusLine is the ledger as preparation left it, in one line:
// what it holds and has yet to read, still reading, or why it did not run,
// why being the chat's reason where no model call was asked for.
func ledgerStatusLine(l engine.LedgerStatus, why string) string {
	held := "ledger: " + text.Plural(l.Items, "item")
	unread := ""
	if l.Unread > 0 {
		unread = ", " + text.Plural(l.Unread, "comment") + " unread"
	}
	switch {
	case l.Blocked != "":
		return warnLine("ledger not run: " + l.Blocked)
	case l.NotAsked:
		return noteLine(held + unread + "; not run: " + why + "; /ledger reads the comments")
	case l.Running:
		return noteLine("ledger: still reading the comments; the opening goes on without it, and the conversation is given it at your next message")
	case l.Err != nil:
		return warnLine("ledger: " + l.Err.Error() + "; " + strings.TrimPrefix(held, "ledger: ") + unread)
	}
	line := held + unread + ", read " + text.Plural(l.Run.Read, "comment") + " now"
	if l.Run.NotShown > 0 {
		line += ", " + text.Plural(l.Run.NotShown, "comment") + " too long for it to read"
	}
	return noteLine(line)
}

// reopenLines say what the conversation reopened has yet to be shown and,
// where the provider's cache has expired on a large conversation, what
// the next call resends.
func reopenLines(r engine.ReopenState) []string {
	since := r.Since.Local().Format("2006-01-02 15:04")
	var news []string
	if r.NewComments > 0 {
		news = append(news, text.Plural(r.NewComments, "new comment"))
	}
	switch {
	case len(r.NewAttachments) > 0:
		news = append(news, text.Plural(len(r.NewAttachments), "new attachment"))
	case r.Attachments:
		news = append(news, "the attachments' inventory or listing changed")
	}
	lines := []string{"nothing new since you last worked this case (" + since + ")"}
	if len(news) > 0 {
		lines[0] = "since you last worked this case (" + since + "): " + strings.Join(news, ", ") + "; your next message gives them to the conversation, and /ledger reads the comments"
	}
	if r.CacheExpired && r.Large {
		lines = append(lines, "the provider's cache has expired since the last call ("+r.LastCall.Local().Format("2006-01-02 15:04")+
			"): your next message resends the conversation, about "+engine.Thousands(r.Estimate)+" tokens, without it; /compact goes on in a new one from the case's state and a handoff")
	}
	return lines
}

// ledgerWatch prints the end of a ledger still reading when the opening
// went on without it.
func (m *model) ledgerWatch(j *engine.LedgerJob) {
	m.spawn(func(ctx context.Context, _ func(tea.Msg)) tea.Msg {
		select {
		case <-j.Done():
		case <-ctx.Done():
			return noteMsg{}
		}
		run, err := j.Result()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return noteMsg{}
			}
			return noteMsg{warnLine("ledger: " + err.Error())}
		}
		line := "ledger: read " + text.Plural(run.Read, "comment") + ", " + text.Plural(run.Added, "new item")
		if run.NotShown > 0 {
			line += ", " + text.Plural(run.NotShown, "comment") + " too long for it to read"
		}
		return noteMsg{noteLine(line + "; the conversation is given it at your next message")}
	})
}

// event is one of the turn's events.
func (m *model) event(ev engine.TalkEvent) {
	t := m.turn
	if t == nil {
		return
	}
	// A call's end does not name the turn's conversation: a catch-up's
	// ledger calls, in a conversation of the ledger's own, come first. A
	// main call's end that comes before its conversation's other events
	// streamed nothing to print.
	if t.conv == "" && ev.Kind != engine.TalkCompacted && ev.Kind != engine.TalkCall {
		t.conv = ev.Conversation
	}
	main := ev.Conversation == t.conv
	switch ev.Kind {
	case engine.TalkText:
		if main {
			m.flushTools()
			t.live.WriteString(ev.Text)
			t.status = "the model is writing"
		}
	case engine.TalkThinking:
		if main {
			m.flushTools()
			t.status = "the model is thinking"
		}
	case engine.TalkToolStart:
		t.batch = append(t.batch, toolRun{id: ev.Call.ID, conv: ev.Conversation, name: ev.Call.Name, args: ev.Call.Args})
		t.status = running(t.batch)
	case engine.TalkToolDone:
		for i := range t.batch {
			r := &t.batch[i]
			if r.id == ev.Call.ID && r.conv == ev.Conversation {
				r.done, r.result, r.isError, r.evidence = true, ev.Result.Text, ev.Result.IsError, ev.Result.Evidence
			}
		}
		t.status = running(t.batch)
	case engine.TalkCall:
		m.called(ev)
		if main {
			m.flushTools()
		}
		if t.made == nil {
			t.made = map[string][]string{}
		}
		for _, c := range ev.Invocation.Calls {
			t.made[ev.Conversation] = append(t.made[ev.Conversation], c.ID)
		}
		if !main {
			return
		}
		if len(ev.Invocation.Calls) == 0 {
			t.status = "checking the answer's citations"
			return
		}
		// What the model wrote before calling tools is its words too; a
		// revision's, whose calls are refused, is not taken, and only what
		// the editor's check ends with is printed.
		if s := strings.TrimSpace(t.live.String()); s != "" && t.mark != markRevising {
			m.print(markdown(text.Safe(s), m.cols(), nil))
		}
		t.resetLive()
		// The answer they streamed beside is printed, so what was held
		// while it showed goes after it.
		if !t.checking {
			m.print(t.held...)
			t.held = nil
		}
		t.status = "running tools"
	case engine.TalkCompacted:
		m.flushTools()
		t.resetLive()
		t.conv = ev.Conversation
		if t.kind != kindCompact {
			m.print(noteLine("the conversation passed its context limit: a model wrote a handoff and the turn goes on in a new conversation"))
		}
	case engine.TalkWarning:
		m.say(warnLine(ev.Text))
	case engine.TalkStage:
		switch ev.Text {
		case engine.StageChecking:
			t.checking = true
			t.status = "the editor is checking the answer"
		case engine.StageRevising:
			// The revision streams in the answer's place.
			t.resetLive()
			t.mark = markRevising
			t.status = "the model is revising the answer for the editor's anchors"
		}
	case engine.TalkReplaced:
		t.resetLive()
		t.live.WriteString(ev.Text)
		if t.mark == markChecking {
			t.mark = markCorrected
		}
	}
}

// What the live view says above an answer the editor checks: that it is
// being checked, as it streams and while the editor reads it; that the
// editor corrected it, once its correction replaces it; that the model is
// revising it.
const (
	markChecking  = "being checked: the editor reads it before it is printed"
	markCorrected = "corrected by the editor"
	markRevising  = "being revised for the editor's anchors"
)

// running says which tools are running, or that the model is called
// again once none is.
func running(batch []toolRun) string {
	var names []string
	for _, r := range batch {
		if !r.done {
			names = append(names, runningLabel(r))
		}
	}
	if len(names) == 0 {
		return "calling the model"
	}
	return strings.Join(names, ", ")
}

// runningLabel says a running tool call precisely.
func runningLabel(r toolRun) string {
	switch r.name {
	case "load_expert":
		return "loading " + argString(r.args, "id")
	case "consult":
		return "consulting " + argString(r.args, "expert")
	}
	return "running " + r.name
}

// called takes a model call's end: a debug line where asked, and its
// record read for the usage.
func (m *model) called(ev engine.TalkEvent) {
	if m.debug {
		m.say(noteLine(debugLine(ev)))
	}
	s, id := m.s, ev.Invocation.ID
	m.spawn(func(context.Context, func(tea.Msg)) tea.Msg {
		rec, err := s.Invocation(id)
		total, totalErr := s.CompanionTotalUsage()
		return recordMsg{rec: rec, err: err, total: total, totalErr: totalErr}
	})
}

// say prints lines said during the turn, a warning or a debug line; while
// the live view shows an answer the editor has yet to settle — the answer
// streaming under its mark, then from the editor's check on to the turn's
// end — it holds them until the turn has printed what it ends with. A
// print while that view is drawn can leave the view's rows in the
// terminal's scrollback, the answer as first written and its mark among
// them, before the answer as shown.
func (m *model) say(lines ...string) {
	if t := m.turn; t != nil && (t.checking || t.mark != "" && t.live.Len() > 0) {
		t.held = append(t.held, lines...)
		return
	}
	m.print(lines...)
}

// flushTools prints the rows for the tool calls since the model last
// called, if any, in the order they were made, and under /debug each
// call's question whole and the answers each gave, as the engine checked
// them.
func (m *model) flushTools() {
	t := m.turn
	if t == nil {
		return
	}
	made := t.made
	t.made = nil
	if len(t.batch) == 0 {
		return
	}
	inOrder(t.batch, made)
	m.print(toolRows(t.batch, m.cols())...)
	if m.debug {
		m.print(debugQuestions(t.batch)...)
		m.debugAnswers(t.batch)
	}
	t.tools = append(t.tools, t.batch...)
	t.batch = nil
}

// inOrder puts the calls of each conversation in a batch in the order its
// model made them (made), each conversation keeping the places its calls
// hold; a call made does not name is put after those it does.
func inOrder(batch []toolRun, made map[string][]string) {
	slots := map[string][]int{}
	for i, r := range batch {
		slots[r.conv] = append(slots[r.conv], i)
	}
	for conv, at := range slots {
		ids := made[conv]
		place := func(r toolRun) int {
			if i := slices.Index(ids, r.id); i >= 0 {
				return i
			}
			return len(ids)
		}
		runs := make([]toolRun, len(at))
		for k, i := range at {
			runs[k] = batch[i]
		}
		slices.SortStableFunc(runs, func(a, b toolRun) int { return place(a) - place(b) })
		for k, i := range at {
			batch[i] = runs[k]
		}
	}
}

// debugQuestions are a batch's questions as /debug shows them: each call
// to a tool that takes one, with its question whole, or none.
func debugQuestions(batch []toolRun) []string {
	var out []string
	for _, r := range batch {
		if !engine.TakesQuestion(r.name) {
			continue
		}
		q := engine.QuestionOf(r.name, r.args)
		if q == "" {
			q = "none"
		}
		out = append(out, noteLine("question · "+r.name+": "+q))
	}
	return out
}

// debugAnswers prints, once the engine has checked them, the answers the
// calls of a batch gave, each whole with what was flagged.
func (m *model) debugAnswers(batch []toolRun) {
	var runs []toolRun
	for _, r := range batch {
		if engine.HasAnswers(r.args) {
			runs = append(runs, r)
		}
	}
	if len(runs) == 0 {
		return
	}
	s := m.s
	m.spawn(func(ctx context.Context, _ func(tea.Msg)) tea.Msg {
		var lines []string
		for _, r := range runs {
			answers, err := s.CallAnswers(ctx, r.conv, r.id)
			if err != nil {
				lines = append(lines, warnLine("the answers of "+r.name+" could not be checked: "+err.Error()))
				continue
			}
			for _, a := range answers {
				line := "answer · " + a.Item + " on " + a.Tool + ": " + a.Answer
				if len(a.Flags) > 0 {
					line += " ⚠ " + strings.Join(a.Flags, "; ")
				}
				lines = append(lines, noteLine(line))
			}
		}
		return noteMsg{line: strings.Join(lines, "\n")}
	})
}

// toolRows are the dim rows naming the tool calls of a batch, what each
// stored and each refusal or failure, each followed by its own question:
// one row, the calls apart by callSep, where it fits the terminal's
// width; otherwise a row for each call, the first under the batch's mark,
// each cut to the width, so every call keeps its question beside it.
func toolRows(batch []toolRun, width int) []string {
	calls := make([]string, len(batch))
	for i, r := range batch {
		calls[i] = text.Safe(firstLine(toolOutcome(r)))
		if q := engine.QuestionOf(r.name, r.args); q != "" {
			calls[i] += " · " + text.Safe(q)
		}
	}
	if s := toolMark + strings.Join(calls, callSep); width <= 0 || ansi.StringWidth(s) <= width {
		return []string{faintW.Render(s)}
	}
	rows := make([]string, len(calls))
	for i, c := range calls {
		lead := toolMark
		if i > 0 {
			lead = strings.Repeat(" ", ansi.StringWidth(toolMark))
		}
		rows[i] = faintW.Render(ansi.Truncate(lead+c, width, "…"))
	}
	return rows
}

// toolMark leads a batch's tool row, and callSep parts its calls on one.
const (
	toolMark = "▸ "
	callSep  = " │ "
)

// toolOutcome says what one tool call did.
func toolOutcome(r toolRun) string {
	s := toolLabel(r.name, r.args)
	switch {
	case !r.done:
		return s + " (cancelled)"
	case r.isError:
		return s + " ✗ " + text.Ellipsis(firstLine(r.result), 60)
	case r.name == "load_expert":
		s = "loaded " + argString(r.args, "id")
	case r.name == "consult":
		s = "consulted " + argString(r.args, "expert")
	}
	if len(r.evidence) > 0 {
		s += " → " + strings.Join(r.evidence, ", ")
	}
	return s
}

// toolLabel is a tool call by its name and the parameter that says most
// about it.
func toolLabel(name string, args map[string]any) string {
	switch name {
	case "run_tool", "describe_tool", "load_expert":
		return name + " " + argString(args, "id")
	case "consult":
		return name + " " + argString(args, "expert")
	case "command":
		if argv, ok := args["argv"].([]any); ok {
			words := make([]string, 0, len(argv))
			for _, w := range argv {
				words = append(words, fmt.Sprint(w))
			}
			return name + " " + strings.Join(words, " ")
		}
	}
	for _, k := range []string{"pattern", "filter", "query"} {
		if v := argString(args, k); v != "" {
			return name + " " + strconv.Quote(v)
		}
	}
	for _, k := range []string{"path", "id", "a"} {
		if v := argString(args, k); v != "" {
			return name + " " + v
		}
	}
	if ids, ok := args["ids"].([]any); ok {
		words := make([]string, 0, len(ids))
		for _, w := range ids {
			words = append(words, fmt.Sprint(w))
		}
		return name + " " + strings.Join(words, ", ")
	}
	return name
}

func argString(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return s
}

// printTools prints the tool calls and results of the last turn, or of
// the turn running, in full.
func (m *model) printTools() {
	tools := m.lastTools
	if t := m.turn; t != nil {
		tools = append(append([]toolRun{}, t.tools...), t.batch...)
	}
	if len(tools) == 0 {
		m.print(noteLine("the last turn called no tools"))
		return
	}
	for _, r := range tools {
		args, err := json.Marshal(r.args)
		if err != nil {
			args = []byte(fmt.Sprint(r.args))
		}
		head := "▸ " + r.name + " " + string(args)
		switch {
		case !r.done:
			head += " (not finished)"
		case r.isError:
			head += " ✗"
		case len(r.evidence) > 0:
			head += " → " + strings.Join(r.evidence, ", ")
		}
		m.print(stateW.Render(text.Safe(head)))
		if r.done {
			m.print(text.Safe(strings.TrimRight(r.result, "\n")))
		}
	}
}

// done is the turn's end: what it ends with printed, its citations
// marked, with the model that wrote it, then what was held while the
// editor's check ran (say).
func (m *model) done(d turnDone) tea.Cmd {
	m.flushTools()
	t := m.turn
	m.turn, m.quitArmed = nil, false
	release := func() {
		if t != nil {
			m.print(t.held...)
			t.held = nil
		}
	}
	defer release()
	if t != nil && d.kind != kindLedger && d.kind != kindReview && (d.kind != kindOpening || d.reply.Conversation != "" || d.err != nil) {
		m.lastTools = t.tools
	}
	switch {
	case errors.Is(d.err, context.Canceled):
		// An answer the editor had checked or was checking is kept as
		// shown, so it is printed as reopening prints it.
		if d.check.Ran && strings.TrimSpace(d.text) != "" && t != nil {
			m.printAnswer(d, t.model)
		}
		release()
		m.print(noteLine("cancelled"))
		return nil
	case d.err != nil:
		if t != nil {
			if s := strings.TrimSpace(t.live.String()); s != "" {
				m.print(markdown(text.Safe(s), m.cols(), nil))
			}
		}
		release()
		m.print(errorLine(d.err.Error()))
		return nil
	}
	if d.kind == kindLoad {
		m.print(noteLine("loaded " + d.loaded + " into the conversation"))
		return nil
	}
	if d.kind == kindLedger {
		m.print(ledgerView(text.Safe(d.ledger), m.cols()))
		return nil
	}
	if d.note != "" {
		m.print(noteLine(d.note))
		return nil
	}
	model := engine.CompanionModel{}
	if t != nil {
		model = t.model
	}
	if d.kind == kindReview {
		m.printReview(*d.review)
		return nil
	}
	m.printAnswer(d, model)
	return nil
}

// review has the reviewer review what ask names, an item or the last
// answer as shown, as a turn of its own, and prints what it found beneath.
func (m *model) review(ask engine.ReviewAsk) {
	if m.s.ReviewerModel() == "" {
		m.print(errorLine(engine.ErrNoReviewer.Error()))
		return
	}
	what := ask.Item
	if what == "" {
		what = "the last answer"
	}
	if ask.Ranked {
		what += ", given the evidence used most and not cited"
	}
	s := m.s
	m.startTurn(kindReview, "the reviewer is reviewing "+what, func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
		r, err := s.Review(ctx, ask, on)
		return turnDone{review: &r, err: err}
	})
}

// reviewAsk is what /review's arguments ask: --ranked, anywhere, and at
// most one item; anything else is refused with the command's form.
func reviewAsk(arg string) (engine.ReviewAsk, error) {
	var ask engine.ReviewAsk
	for _, f := range strings.Fields(arg) {
		switch {
		case f == "--ranked":
			ask.Ranked = true
		case strings.HasPrefix(f, "-") || ask.Item != "":
			return engine.ReviewAsk{}, errors.New("/review takes " + commandNamed("review").args)
		default:
			ask.Item = f
		}
	}
	return ask, nil
}

// printReview prints a review as the turn that ran it ends with it and
// as reopening restores it: each finding flagged, with its support, the
// strongest alternative and the discriminator, and the most the lines
// justify faint beneath it; then what it is of, its figures, the model
// and its usage.
func (m *model) printReview(r engine.Review) {
	lines := []string{""}
	for _, n := range r.Notes() {
		l := flagW.Render("⚠ "+text.Safe(n.Text)) + faintW.Render(" · "+text.Safe(n.Why))
		if n.Alternative != "" {
			l += "\n" + faintW.Render(text.Safe("  "+n.Alternative))
		}
		if n.AtMost != "" {
			l += "\n" + faintW.Render(text.Safe("  at most: "+n.AtMost))
		}
		lines = append(lines, l)
	}
	m.print(append(lines, noteLine(strings.Join(r.Footer(), " · ")))...)
}

// printAnswer displays a completed answer and its browser citations, without
// changing the turn running. Restored answers use the same presentation.
// Its findings are in the checks tab, and beneath it one line says how
// many, one notes the citations resolved on a showing not the
// conversation's own (viaLine), and one quiet footer names the answer,
// model, usage and check summary (/debug adds the anchor breakdown).
func (m *model) printAnswer(d turnDone, model engine.CompanionModel) {
	if strings.TrimSpace(d.text) == "" {
		// An opening skipped, --no-open, has nothing to say.
		if d.kind != kindOpening || d.reply.Conversation != "" {
			m.print(warnLine("the model's answer holds no text"))
		}
		return
	}
	if d.kind != kindDraft && d.kind != kindCompact && d.kind != kindReload {
		m.hasNext = hasNextSteps(d.text)
		m.refreshPlaceholder()
	}
	// A blank row between the turn's tool lines, or the engineer's words,
	// and the answer. Drafts strip [eN] citations for display after checks run.
	shownText := strings.TrimSpace(d.text)
	var marks citeMarks
	if d.kind == kindDraft {
		shownText = strings.TrimSpace(engine.StripDraftCites(d.text))
	} else {
		marks = marksOf(d.cites, d.checks)
	}
	m.print("", markdown(text.Safe(shownText), m.cols(), marks))
	if d.checkErr != nil {
		m.print(warnLine("the citations could not be checked: " + d.checkErr.Error()))
	}
	if d.flagErr != nil {
		m.print(warnLine("the answer's own checks could not run: " + d.flagErr.Error()))
	}
	a := answer{label: "a" + strconv.Itoa(len(m.answers)+1), flags: d.flags, verdicts: map[string]engine.SpanVerdict{},
		reads: readings(d.cites, d.checks, d.flags, d.check), asks: asked(d.check)}
	for _, c := range d.cites {
		if c.Name != "" {
			a.cites = append(a.cites, c)
		}
	}
	for _, v := range d.verdicts {
		a.verdicts[v.Span] = v
	}
	if l := findingsLine(a); l != "" {
		m.print(l)
	}
	if l := viaLine(d.cites, d.checks); l != "" {
		m.print(noteLine(l))
	}
	m.print(checkNotes(d.check)...)
	if m.debug {
		if l := asksLine(a); l != "" {
			m.print(noteLine(l))
		}
	}
	foot := []string{a.label}
	if model.Name != "" {
		foot = append(foot, model.Name+" "+engine.EffortShown(model.Effort))
	}
	if d.usage != "" {
		foot = append(foot, d.usage)
	}
	foot = append(foot, d.check.Words()...)
	if !m.debug && len(a.asks) > 0 {
		if !d.check.Revised {
			foot = append(foot, "asked "+text.Plural(len(a.asks), "anchor"))
		}
		foot = append(foot, "/check "+a.label+" lists anchors")
	}
	if d.check.Changed() {
		foot = append(foot, "the original in the turns tab")
	}
	switch d.kind {
	case kindDraft:
		switch {
		case d.item.Seq != 0:
			foot = append(foot, "draft: "+d.item.Intention, "/copy "+a.label+" copies it", "/review "+a.label+" for a second opinion")
		case errors.Is(d.err, context.Canceled):
			foot = append(foot, "the draft was not kept: its turn was cancelled")
		case d.draftIntention != "":
			foot = append(foot, "draft: "+d.draftIntention)
		}
	case kindCompact:
		foot = append(foot, "a handoff: the conversation goes on in a new one, begun from it and the drafts kept")
	case kindReload:
		foot = append(foot, "a handoff: the conversation goes on in a new one, begun from the case as it is now, the ledger, the handoff, the drafts kept, "+
			text.Plural(d.reloaded.Ranked, "ranked item")+" and the last exchange")
	}
	m.print(noteLine(strings.Join(foot, " · ")))
	m.answers = append(m.answers, a)
	if d.reply.Compact {
		m.print(noteLine("the conversation is long: /compact writes a handoff and goes on in a new one"))
	}
}

// checkNotes are the lines beneath an answer saying what of the editor's
// check is not in its text and is no finding (engine.AnswerCheck.Notes),
// styled: a revision or a follow-up not taken. Its findings are in the
// checks tab (readings), as are the anchors asked of the revision
// (asked).
func checkNotes(c engine.AnswerCheck) []string {
	var out []string
	for _, n := range c.Notes() {
		if n.Kind == engine.NoteWarning {
			out = append(out, warnLine(n.Text))
		}
	}
	return out
}

// viaPlaces is how many citations viaLine names for one showing; the rest
// are counted.
const viaPlaces = 3

// viaLine is the one line beneath an answer saying which of its
// citations resolve on a showing not the conversation's own
// (engine.CiteCheck.Via) — a consult's read, or names alone where the
// exposure is unknown — grouped by that showing, each group its
// citations by place, viaPlaces at most and the rest counted, then the
// showing; empty where none does. A note, not a finding: each citation
// resolves. One line, not one a citation, since every citation of a
// conversation whose exposure is unknown says the same.
func viaLine(cites []engine.Cite, checks []engine.CiteCheck) string {
	var vias []string
	places := map[string][]string{}
	for i, c := range cites {
		if i >= len(checks) || !checks[i].OK || checks[i].Via == "" {
			continue
		}
		via, place := checks[i].Via, "["+citeName(c)+"]"
		if !slices.Contains(vias, via) {
			vias = append(vias, via)
		}
		if !slices.Contains(places[via], place) {
			places[via] = append(places[via], place)
		}
	}
	groups := make([]string, len(vias))
	for i, via := range vias {
		ps := places[via]
		named := strings.Join(ps[:min(len(ps), viaPlaces)], ", ")
		if len(ps) > viaPlaces {
			named += " and " + strconv.Itoa(len(ps)-viaPlaces) + " more"
		}
		groups[i] = named + " · " + via
	}
	return strings.Join(groups, "; ")
}

// debugLine is one model call as /debug shows it: the model, its input,
// the share read from cache, its output, and its time to the first token
// and in all; a count the provider did not report is said so.
func debugLine(ev engine.TalkEvent) string {
	inv := ev.Invocation
	u := inv.Usage
	count := func(v int64, known bool) string {
		if !known {
			return "unknown"
		}
		return engine.Thousands(v)
	}
	parts := []string{"call " + inv.Model + " " + engine.EffortShown(inv.Effort)}
	in := u.InputTokens.Value + u.CacheReadTokens.Value + u.CacheWriteTokens.Value
	if u.InputTokens.Known && u.CacheReadTokens.Known && u.CacheWriteTokens.Known {
		parts = append(parts, "in "+engine.Thousands(in))
		if in > 0 {
			parts = append(parts, percent(u.CacheReadTokens.Value, in)+" cached")
		}
	} else {
		parts = append(parts, "in "+count(u.InputTokens.Value, u.InputTokens.Known)+" uncached, "+
			count(u.CacheReadTokens.Value, u.CacheReadTokens.Known)+" cached")
	}
	parts = append(parts, "out "+count(u.OutputTokens.Value, u.OutputTokens.Known))
	if !inv.First.IsZero() {
		parts = append(parts, "first token "+inv.First.Sub(inv.Started).Round(time.Millisecond).String())
	}
	parts = append(parts, "total "+inv.Completed.Sub(inv.Started).Round(time.Millisecond).String())
	return strings.Join(parts, " · ")
}

// record keeps this process's call record without rebuilding stored totals.
func (m *model) record(r recordMsg) {
	if r.err != nil {
		m.print(warnLine("a model call's record could not be read: " + r.err.Error()))
	} else {
		m.records = append(m.records, r.rec)
		sort.SliceStable(m.records, func(i, j int) bool { return m.records[i].StartedAt.Before(m.records[j].StartedAt) })
	}
	m.applyUsage(usageMsg{total: r.total, err: r.totalErr})
}

// checkBound bounds the deterministic checks of an answer printed after
// its turn was cancelled.
const checkBound = 10 * time.Second

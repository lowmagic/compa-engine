package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// The chat's tests drive its model as Bubble Tea would, message by
// message, on the stub case 00000001 with the fixture must-gather, the
// companion's model the scripted fixture provider answering from the
// synthetic scripts in testdata, which send nothing anywhere.

// chatConfig is the configuration the tests add: two fixture models, the
// companion and the case ledger on the first at effort high, and one
// intention to draft.
func chatConfig(t *testing.T, script string) string {
	t.Helper()
	path := func(name string) string {
		p, err := filepath.Abs(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	return fmt.Sprintf(`reasoning:
  maxEgressClass: local_only
  models:
    fixture-model: {provider: fixture, model: %q, efforts: [low, high]}
    other-model: {provider: fixture, model: %q, efforts: [medium]}
  profiles:
    companion: {model: fixture-model, effort: high}
    ledger: {model: fixture-model, effort: high}
comments:
  style: Plain and direct.
  intentions:
    - {id: customer-update, label: Customer update, audience: customer, purpose: Tell the customer what was found.}
`, path(script), path("other.json"))
}

// session opens the stub case on an engine so configured.
func session(t *testing.T, script string) *engine.Session {
	t.Helper()
	s, err := enginetest.EngineWith(t, chatConfig(t, script)).StartSession(t.Context(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// harness runs the chat's model as Bubble Tea's program does: commands
// in goroutines of their own, what they return fed back to Update one at
// a time, and the work the model spawns likewise. It keeps what the chat
// printed and the commands Bubble Tea would carry out itself.
type harness struct {
	t  *testing.T
	m  *model
	wg sync.WaitGroup
	// msgs carries what commands and work send; running counts what has
	// yet to send its last message.
	msgs    chan delivery
	running int
	printed []string
	// screen is what reached the terminal, in order: each print, and each
	// switch of the view to or from the alternate screen, as Bubble Tea
	// renders the view after each Update; alt is the view's screen now.
	screen []string
	alt    bool
	// teaMsgs are the messages Bubble Tea handles itself, such as the
	// clipboard's; quit is whether the chat quit.
	teaMsgs []tea.Msg
	quit    bool
}

type delivery struct {
	msg  tea.Msg
	last bool
}

// start runs the chat on s, a terminal of 100 by 30.
func start(t *testing.T, s *engine.Session, o Options) *harness {
	t.Helper()
	h := &harness{t: t, msgs: make(chan delivery)}
	// The test's context ends as it does, and with it the chat's work,
	// which ends before the session closes.
	t.Cleanup(h.wg.Wait)
	h.m = newModel(s, o, h.spawn)
	h.m.println = func(s string) tea.Cmd {
		h.printed = append(h.printed, s)
		h.screen = append(h.screen, "print "+ansi.Strip(s))
		return nil
	}
	h.update(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.exec(h.m.Init())
	h.settle()
	return h
}

func (h *harness) send(d delivery) {
	select {
	case h.msgs <- d:
	case <-h.t.Context().Done():
	}
}

func (h *harness) spawn(work func(context.Context, func(tea.Msg)) tea.Msg) context.CancelFunc {
	ctx, cancel := context.WithCancel(h.t.Context())
	h.running++
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer cancel()
		msg := work(ctx, func(m tea.Msg) { h.send(delivery{msg: m}) })
		h.send(delivery{msg: msg, last: true})
	}()
	return cancel
}

func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.running++
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.send(delivery{msg: cmd(), last: true})
	}()
}

func (h *harness) update(msg tea.Msg) {
	_, cmd := h.m.Update(msg)
	if alt := h.m.View().AltScreen; alt != h.alt {
		h.alt = alt
		h.screen = append(h.screen, fmt.Sprintf("alternate screen %v", alt))
	}
	h.exec(cmd)
}

// step takes one message.
func (h *harness) step() {
	h.t.Helper()
	select {
	case d := <-h.msgs:
		if d.last {
			h.running--
		}
		switch msg := d.msg.(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range msg {
				h.exec(c)
			}
		case tea.QuitMsg:
			h.quit = true
		case resumeMsg:
			h.screen = append(h.screen, "frames waited")
			h.update(msg)
		case printedMsg, eventMsg, stateMsg, noteMsg, ledgerCallMsg, ledgerRunningMsg, recordMsg, turnDone, jobDone, shellDoneMsg, itemsMsg, historyMsg, usageMsg, verdictMsg, spinner.TickMsg:
			h.update(msg)
		case tea.RawMsg:
			// Bubble Tea writes it to the terminal when its loop takes it.
			h.screen = append(h.screen, fmt.Sprintf("raw %q", msg.Msg))
			h.teaMsgs = append(h.teaMsgs, msg)
			h.update(msg)
		default:
			h.teaMsgs = append(h.teaMsgs, msg)
			h.update(msg)
		}
	case <-time.After(30 * time.Second):
		h.t.Fatal("the chat sent nothing for 30s")
	}
}

// settle runs until nothing is running.
func (h *harness) settle() {
	h.t.Helper()
	for h.running > 0 {
		h.step()
	}
}

// until runs until ok holds.
func (h *harness) until(what string, ok func() bool) {
	h.t.Helper()
	for !ok() {
		if h.running == 0 {
			h.t.Fatalf("nothing runs, and still not %s", what)
		}
		h.step()
	}
}

// keyMsg is a key by its name as Bubble Tea writes it, or a character.
func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "alt+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	}
	r := []rune(k)[0]
	return tea.KeyPressMsg{Code: r, Text: k}
}

// press presses keys.
func (h *harness) press(keys ...string) {
	for _, k := range keys {
		h.update(keyMsg(k))
	}
}

// typed types s into the chat.
func (h *harness) typed(s string) {
	for _, r := range s {
		h.update(keyMsg(string(r)))
	}
}

// say types a line, sends it, and waits for what it began.
func (h *harness) say(s string) {
	h.t.Helper()
	h.typed(s)
	h.press("enter")
	h.settle()
}

// out is what the chat printed, its styles stripped.
func (h *harness) out() string { return ansi.Strip(strings.Join(h.printed, "\n")) }

// view is what the chat's view shows, its styles stripped.
func (h *harness) view() string { return ansi.Strip(h.m.View().Content) }

// newOutput is what was printed since mark, a count of h.printed.
func (h *harness) since(mark int) string {
	return ansi.Strip(strings.Join(h.printed[mark:], "\n"))
}

// flat is s with its runs of space and its line breaks as one space, so
// a check holds wherever the chat wrapped.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// toolLines are the lines printed naming tool calls.
func toolLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "▸") {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestOpeningTurn: a case new to the companion opens with its preparation
// said in a line each for the inventory and the ledger, the ledger read
// first, then the opening turn: the model's grep prints as one dim line
// naming what ran and the evidence it stored, the answer prints rendered,
// its Markdown markers gone, with the model and effort that wrote it, and
// the chat then waits for the engineer, its status line saying so with
// the run's tokens, the ledger's call among them.
func TestOpeningTurn(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{})
	out := h.out()
	flatOut := flat(out)
	tools := toolLines(out)
	if len(tools) != 1 || !regexp.MustCompile(`^▸ grep "error syncing node" → e\d+$`).MatchString(tools[0]) {
		t.Errorf("the tool lines: %q, want one naming the grep and its evidence\n%s", tools, out)
	}
	for _, want := range []string{"inventory: 2 attachments · 2 with no capture identified", "ledger: 2 items, read 3 comments now",
		"The case The OVN controller cannot sync worker-1", "a1 · fixture-model effort high · usage reported"} {
		if !strings.Contains(flatOut, want) {
			t.Errorf("the chat printed no %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "ledger: 2 items") > strings.Index(out, "▸ grep") {
		t.Errorf("the ledger's line printed after the opening's calls:\n%s", out)
	}
	if strings.Contains(out, "**") || strings.Contains(out, "## ") {
		t.Errorf("the answer printed with its Markdown markers:\n%s", out)
	}
	if strings.Index(out, "▸ grep") > strings.Index(out, "The OVN controller") {
		t.Errorf("the tool line printed after the answer:\n%s", out)
	}
	view := h.view()
	if !strings.Contains(view, "case 00000001 · fixture-model effort high · waiting for you") {
		t.Errorf("the status line after the opening:\n%s", view)
	}
	// Three calls of the fixture's 100 input and 20 output tokens, the
	// ledger's and the opening's two; no fixture model is priced.
	if !strings.Contains(view, "360 tok") {
		t.Errorf("the status line lacks the run's tokens:\n%s", view)
	}
	// ctrl+o prints the last turn's calls and results in full.
	mark := len(h.printed)
	h.press("ctrl+o")
	h.settle()
	full := flat(h.since(mark))
	if !strings.Contains(full, `▸ grep {"path":"namespaces/openshift-ovn-kubernetes","pattern":"error syncing node"} → e`) ||
		!strings.Contains(full, "error syncing node worker-1: failed to get chassis") {
		t.Errorf("ctrl+o printed:\n%s", full)
	}
}

// TestNoOpen: --no-open reads the case, says what the inventory and the
// ledger hold and why the ledger did not run, and makes no model call,
// the ledger's included.
func TestNoOpen(t *testing.T) {
	s := session(t, "case.json")
	h := start(t, s, Options{NoOpen: true})
	if out := flat(h.out()); out != "inventory: 2 attachments · 2 with no capture identified ledger: 0 items, 3 comments unread; not run: --no-open makes no model call; /ledger reads the comments" {
		t.Errorf("--no-open printed:\n%s", out)
	}
	aggs, err := s.CompanionUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(aggs) != 0 {
		t.Errorf("--no-open called the model: %+v", aggs)
	}
}

// TestReopenMakesNoCall: a case reopened on its conversation makes no
// model call, the ledger's included, and the chat says what is new since
// it was last worked.
func TestReopenMakesNoCall(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	before, err := s.CompanionTotalUsage()
	if err != nil {
		t.Fatal(err)
	}
	h := start(t, s, Options{})
	out := flat(h.out())
	for _, want := range []string{"ledger: 2 items; not run: a reopened case makes no model call", "nothing new since you last worked this case ("} {
		if !strings.Contains(out, want) {
			t.Errorf("the reopened chat printed no %q:\n%s", want, h.out())
		}
	}
	if after, err := s.CompanionTotalUsage(); err != nil || after.Invocations != before.Invocations {
		t.Errorf("reopening made %d model calls, %v", after.Invocations-before.Invocations, err)
	}
}

// TestCitationsFlagged: [C5] an answer's citation to evidence that does
// not exist, or to lines past an item's end, is shown flagged, never
// dropped, and listed with why in the checks tab, one line beneath the
// answer saying how many findings it has; one that resolves is shown as a
// citation and not listed.
func TestCitationsFlagged(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{})
	out := flat(h.out())
	for _, want := range []string{"say [e1:2].", "[e999:2] ⚠", "[e1:100000] ⚠", "⚠ 2 findings · /check a1 opens them"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	raw := strings.Join(h.printed, "\n")
	if !strings.Contains(raw, citeW.Render("[e1:2]")) || !strings.Contains(raw, flagW.Render("[e999:2] ⚠")) {
		t.Error("the citations are not styled as citations and flags")
	}
	openChecks(t, h)
	if got := tabbed(h, "a1"); len(got) != 2 || got[0] != "· e999:2 no evidence e999" || !strings.HasPrefix(got[1], "· e1:100000 e1 has ") {
		t.Errorf("the tab lists %q; want e999:2 and e1:100000 flagged with why, and not e1:2", got)
	}
}

// TestLedgerItemCitedFlagged: a case-ledger item an answer cites, alone or
// beside evidence names in one bracket, is shown flagged, the bracket
// whole, and listed in the checks tab with why — the ledger's reading of
// a comment, to cite in its place, or naming nothing — while the evidence
// name beside it resolves and is not listed; reopened, the same.
func TestLedgerItemCitedFlagged(t *testing.T) {
	s := session(t, "case.json")
	h := start(t, s, Options{NoOpen: true})
	h.say("/ledger")
	h.say("cite the ledger")
	for _, run := range []struct {
		name string
		h    *harness
	}{{"the turn", h}, {"the reopened chat", start(t, s, Options{NoOpen: true})}} {
		out := flat(run.h.out())
		for _, want := range []string{"It began after the upgrade [l1, e1:2] ⚠.", "is owed [l2] ⚠, as [l9] ⚠ says."} {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", run.name, want, out)
			}
		}
		checkAt(t, run.h, "a1")
		got := tabbed(run.h, "a1")
		want := []*regexp.Regexp{
			regexp.MustCompile(`^· l1 l1 is the case ledger's reading of comment c#\d+ \(e\d+\), not evidence: cite that comment's lines instead$`),
			regexp.MustCompile(`^· l2 l2 is the case ledger's reading of comment c#\d+ \(e\d+\), not evidence: cite that comment's lines instead$`),
			regexp.MustCompile(`^· l9 l9 names nothing: the case ledger has no item l9$`),
		}
		if len(got) != len(want) {
			t.Fatalf("%s: the tab lists %q", run.name, got)
		}
		for i, w := range want {
			if !w.MatchString(got[i]) {
				t.Errorf("%s: the tab lists %q; want %s", run.name, got[i], w)
			}
		}
	}
}

// TestAnswerFlagsShown: the checks tab lists, after an answer's span
// flags and its citation flags, a citation not in the form read —
// without brackets, in parentheses — each with why, and a code span in no
// evidence the conversation saw; a span the evidence holds and a
// well-formed citation are not flagged, and an unresolved citation is
// flagged once. Causal wording in the model's own voice and a command
// found in no evidence or guidance are listed for reading, each with the
// statement holding it, and a question is none. Beneath each answer with
// findings one line says how many, and beneath one with nothing to flag
// there is none. A reopened chat shows the same beneath the restored
// answers, naming /check.
func TestAnswerFlagsShown(t *testing.T) {
	s := session(t, "case.json")
	h := start(t, s, Options{})
	h.say("check the forms")
	h.say("which model?")
	h.say("check the reading")
	want := []string{
		"? `no such marker 7f3a` in no evidence this conversation saw",
		"· e999:3 no evidence e999",
		"· e1:2 not read as a citation without brackets: write [e1:2]",
		"· (e1) not read as a citation in parentheses: write [e1]",
	}
	reading := []string{
		`· "likely", "due to" states a cause or a likelihood in the model's own voice: offer it as a lead to explore, with what would test it, or attribute it`,
		"· `journalctl -u ovn-controller --no-pager` `journalctl` is on no line of the evidence this conversation saw or of the guidance of an expert it loaded",
	}
	claims := []string{
		engine.FindingCausal + ": The sync likely failed due to the upgrade [e1:2].",
		engine.FindingCommand + ": Run `journalctl -u ovn-controller --no-pager` on worker-1.",
	}
	clean := regexp.MustCompile(`The first model answers\. (.*?)a\d+ · `)
	for _, run := range []struct {
		name  string
		h     *harness
		lines []string
	}{
		{"the turn", h, []string{"⚠ 2 findings · /check a1 opens them", "⚠ 4 findings · /check a2 opens them", "⚠ 2 findings · /check a4 opens them"}},
		{"the reopened chat", start(t, s, Options{NoOpen: true}), []string{"⚠ 2 findings · /check a1 opens them", "⚠ 4 findings · /check a2 opens them", "⚠ 2 findings · /check a4 opens them"}},
	} {
		out := run.h.out()
		if got := findingLines(out); !slices.Equal(got, run.lines) {
			t.Errorf("%s shows the finding lines %q; want %q:\n%s", run.name, got, run.lines, out)
		}
		if m := clean.FindStringSubmatch(flat(out)); m == nil || strings.Contains(m[1], "⚠") {
			t.Errorf("%s shows a finding line beneath an answer with nothing to flag:\n%s", run.name, out)
		}
		checkAt(t, run.h, "a2")
		if got := tabbed(run.h, "a2"); !slices.Equal(got, want) {
			t.Errorf("%s lists a2's findings %q; want %q", run.name, got, want)
		}
		run.h.press("esc")
		checkAt(t, run.h, "a4")
		if got := tabbed(run.h, "a4"); !slices.Equal(got, reading) {
			t.Errorf("%s lists a4's findings %q; want %q", run.name, got, reading)
		}
		var got []string
		for _, v := range run.h.m.browser.checks.values {
			if v.label == "a4" && v.read != nil {
				got = append(got, v.read.about+": "+v.read.claim)
			}
		}
		if !slices.Equal(got, claims) {
			t.Errorf("%s gives a4's findings the statements %q; want %q", run.name, got, claims)
		}
		run.h.press("esc")
	}
}

// TestCitationOfAConsultsReadNoted: an answer citing lines only its
// consult read shows the citation resolved, not flagged, with one note
// beneath it saying whose read it rests on, live and on reopening; no
// finding is counted for it.
func TestCitationOfAConsultsReadNoted(t *testing.T) {
	s := session(t, "consult.json")
	h := start(t, s, Options{NoOpen: true})
	h.say("ask the platform expert")
	want := "[e6:1] · read in the consult of openshift.platform, not in this conversation"
	for _, run := range []struct {
		name string
		h    *harness
	}{
		{"the turn", h},
		{"the reopened chat", start(t, s, Options{NoOpen: true})},
	} {
		out := run.h.out()
		if n := strings.Count(out, want); n != 1 {
			t.Errorf("%s shows the consult's note %d times; want once:\n%s", run.name, n, out)
		}
		if got := findingLines(out); len(got) != 0 || strings.Contains(out, "⚠ e6") {
			t.Errorf("%s flags the consult's citation %q:\n%s", run.name, got, out)
		}
	}
}

// TestViaLineSummarises: the note beneath an answer is one line however
// many of its citations rest on another showing: grouped by it, each
// citation once, three named and the rest counted; a flagged citation and
// one the conversation was shown itself are not in it.
func TestViaLineSummarises(t *testing.T) {
	const consult = "read in the consult of openshift.platform, not in this conversation"
	const unknown = "exposure unknown: synthetic"
	cites := engine.Cites("[e4:1] [e4:2] [e5] [e4:1] [e6:3] [e7:9] [e8:1] [e9:2] [e10:4]")
	checks := []engine.CiteCheck{
		{OK: true, Via: consult}, {OK: true, Via: unknown}, {OK: true, Via: unknown}, {OK: true, Via: consult},
		{OK: true, Via: unknown}, {OK: true, Via: unknown}, {OK: true, Via: unknown}, {OK: true}, {Why: "flagged", Via: consult},
	}
	want := "[e4:1] · " + consult + "; [e4:2], [e5], [e6:3] and 2 more · " + unknown
	if got := viaLine(cites, checks); got != want {
		t.Errorf("the note %q; want %q", got, want)
	}
	if got := viaLine(cites, checks[7:8]); got != "" {
		t.Errorf("a note %q where no citation rests on another showing", got)
	}
}

// TestDraftKeptAndCopied: /draft lists the modes without one and writes
// one, its footer naming the answer to copy or review; /copy and ctrl+y
// ask the terminal to put its text on the clipboard, saying they asked,
// and /copy of no answer is refused naming those there are.
func TestDraftKeptAndCopied(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.say("/draft")
	if out := h.out(); !strings.Contains(out, "customer-update — Customer update, for the customer") {
		t.Errorf("/draft with no intention:\n%s", out)
	}
	mark := len(h.printed)
	h.say("/draft customer-update")
	draft := "Hello, the OVN controller on worker-1 cannot sync; we are reading its logs."
	out := h.since(mark)
	if !strings.Contains(out, draft) || !strings.Contains(flat(out), "/copy a1 copies it · /review a1 for a second opinion") {
		t.Errorf("/draft printed:\n%s", out)
	}
	h.say("/copy")
	want := []tea.Msg{tea.SetClipboard(draft)()}
	if diff := cmp.Diff(want, h.teaMsgs); diff != "" {
		t.Errorf("/copy asked of the terminal (-want +got):\n%s", diff)
	}
	if out := h.out(); !strings.Contains(out, "asked the terminal to copy a1") {
		t.Errorf("/copy did not say it asked:\n%s", out)
	}
	h.press("ctrl+y")
	h.settle()
	if diff := cmp.Diff(append(want, want...), h.teaMsgs); diff != "" {
		t.Errorf("ctrl+y asked of the terminal (-want +got):\n%s", diff)
	}
	h.say("/copy A9")
	if out := h.out(); !strings.Contains(out, "there is no answer a9; the answers are a1 to a1") {
		t.Errorf("/copy of no answer:\n%s", out)
	}
	if len(h.teaMsgs) != 2 {
		t.Errorf("/copy of no answer asked the terminal: %v", h.teaMsgs)
	}
}

// TestMarksPointTheNextMessage: lines marked in the browser begin the
// engineer's next message, shown in the status line until it is sent; the
// browser takes the screen while open and gives it back, and the model
// answers the message as sent.
func TestMarksPointTheNextMessage(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{})
	before := len(h.printed)
	h.say("/evidence e1:2")
	if !h.m.View().AltScreen {
		t.Fatalf("/evidence e1:2 did not open the browser:\n%s", h.out())
	}
	if v := h.view(); !strings.Contains(v, "e1 · ") {
		t.Errorf("the browser does not show e1:\n%s", v)
	}
	h.press("v", "down", "down", "enter", "esc")
	if h.m.View().AltScreen {
		t.Fatal("esc left the browser open")
	}
	if v := h.view(); !strings.Contains(v, "pointing at e1:2-4") {
		t.Errorf("the status line does not show the mark:\n%s", v)
	}
	h.say("what do these say?")
	out := h.since(before)
	for _, want := range []string{"› the engineer points at e1:2-4\n  \n  what do these say?", "Those lines are the case's details."} {
		if !strings.Contains(out, want) {
			t.Errorf("the chat printed no %q:\n%s", want, out)
		}
	}
	if v := h.view(); strings.Contains(v, "pointing at") {
		t.Errorf("the mark outlived the message:\n%s", v)
	}
}

// TestPrintsOutsideTheBrowser: nothing is printed while the browser holds
// the alternate screen, whose lines never reach the scrollback: the
// command opening it is printed before the view switches to it, and a
// line queued while it is open is printed only once the view has left it
// and the frames for the renderer to draw that have passed.
func TestPrintsOutsideTheBrowser(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	mark := len(h.screen)
	h.say("/evidence")
	h.update(noteMsg{line: noteLine("a line queued meanwhile")})
	h.settle()
	if !h.m.View().AltScreen {
		t.Fatal("/evidence did not open the browser")
	}
	h.press("esc")
	h.settle()
	want := []string{
		"print › /evidence",
		"alternate screen true",
		"alternate screen false",
		"frames waited",
		"print a line queued meanwhile",
	}
	if diff := cmp.Diff(want, h.screen[mark:]); diff != "" {
		t.Errorf("what reached the terminal, in order (-want +got):\n%s", diff)
	}
}

// TestBrowserSteps: /evidence with no argument opens at the last answer's
// first citation; n steps to the next, which names no evidence and says
// so; l goes to the list, which, filtered as the engineer types, shows
// which answer cites each item; esc returns to the chat; navigating
// prints nothing.
func TestBrowserSteps(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{})
	before := len(h.printed)
	h.say("/evidence")
	b := h.m.browser
	if b == nil || b.open == nil || b.open.name != "e1" || b.open.cur != 1 {
		t.Fatalf("/evidence opened %+v, want e1 at its line 2", b)
	}
	h.press("n")
	if v := h.view(); !strings.Contains(v, "citation 2 of 3: e999:2 — e999:") {
		t.Errorf("n to a citation naming no evidence:\n%s", v)
	}
	h.press("l")
	h.typed("e1 ")
	v := h.view()
	if !strings.Contains(v, "› e1 ") || !strings.Contains(v, "cited by a1") {
		t.Errorf("the list filtered to e1:\n%s", v)
	}
	if strings.Contains(v, "e2 ") {
		t.Errorf("the filter kept e2:\n%s", v)
	}
	h.press("esc")
	if h.m.browser != nil {
		t.Fatal("esc on the list did not return to the chat")
	}
	if got, want := h.since(before), "› /evidence"; got != want {
		t.Errorf("the browser printed %q, want only %q", got, want)
	}
}

// TestModelSwitch: [C5] /model shows the model and the choices, switches
// model and effort for the session, each later call records the
// engineer's choice, and the conversation goes on.
func TestModelSwitch(t *testing.T) {
	s := session(t, "case.json")
	h := start(t, s, Options{NoOpen: true})
	h.say("which model?")
	h.say("/model")
	if out := h.out(); !strings.Contains(out, "the model: fixture-model effort high") || !strings.Contains(out, "other-model — efforts medium") {
		t.Errorf("/model with nothing given:\n%s", out)
	}
	h.say("/model other-model low")
	if out := h.out(); !strings.Contains(out, "✗ other-model takes the efforts medium, not low") {
		t.Errorf("/model at an effort it does not take:\n%s", out)
	}
	h.say("/model other-model medium")
	if v := h.view(); !strings.Contains(v, "other-model effort medium") {
		t.Errorf("the status line after /model:\n%s", v)
	}
	mark := len(h.printed)
	h.say("which model now?")
	out := h.since(mark)
	if !strings.Contains(out, "The other model answers.") || !strings.Contains(out, "a2 · other-model effort medium") {
		t.Errorf("the turn after /model:\n%s", out)
	}
	if len(h.m.records) != 2 {
		t.Fatalf("%d calls recorded, want 2", len(h.m.records))
	}
	type call struct{ Model, Effort, Source, Conversation string }
	var got []call
	for _, r := range h.m.records {
		got = append(got, call{filepath.Base(r.Model.ModelID), r.Model.Effort, r.ModelProfile.Source, r.ConversationID})
	}
	conv := got[0].Conversation
	want := []call{{"case.json", "high", "configuration", conv}, {"other.json", "medium", "engineer", conv}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the calls' records (-want +got):\n%s", diff)
	}
}

// TestCancelThenQuit: ctrl+c cancels a running turn and leaves the chat
// open; pressed again with no turn running, it quits.
func TestCancelThenQuit(t *testing.T) {
	h := start(t, session(t, "slow.json"), Options{NoOpen: true})
	h.typed("go slowly")
	h.press("enter")
	h.until("streaming", func() bool { return h.m.turn != nil && h.m.turn.live.Len() > 0 })
	if v := h.view(); !strings.Contains(v, "one") || !strings.Contains(v, "the model is writing") {
		t.Errorf("the view while the answer streams:\n%s", v)
	}
	h.press("ctrl+c")
	h.settle()
	if h.quit {
		t.Fatal("ctrl+c during a turn quit")
	}
	if out := h.out(); !strings.Contains(out, "cancelled") || strings.Contains(out, "twenty") {
		t.Errorf("after ctrl+c during the turn:\n%s", out)
	}
	if v := h.view(); !strings.Contains(v, "waiting for you") {
		t.Errorf("the status line after cancelling:\n%s", v)
	}
	h.press("ctrl+c")
	h.settle()
	if !h.quit {
		t.Error("ctrl+c with no turn running did not quit")
	}
}

// TestRefusedWhileRunning: a message or a command needing the model,
// given while a turn runs, is refused and stays in the input.
func TestRefusedWhileRunning(t *testing.T) {
	h := start(t, session(t, "slow.json"), Options{NoOpen: true})
	h.typed("go slowly")
	h.press("enter")
	h.typed("/compact")
	h.press("enter")
	if got := h.m.input.Value(); got != "/compact" {
		t.Errorf("the input after a refused /compact: %q", got)
	}
	h.until("the refusal printed", func() bool { return strings.Contains(h.out(), "/compact waits for the turn running") })
	h.press("ctrl+c")
	h.settle()
}

// TestShellOpensWithCompaIn: ! opens the engineer's shell in the case's
// folder under a banner saying how to keep what it shows, COMPA_CASE
// naming the case and COMPA_INGEST the socket the chat serves; text after
// ! is refused, saying how. Back from the shell, what compa in kept is
// listed, the status line says the next message names it, and it does.
func TestShellOpensWithCompaIn(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s := session(t, "case.json")
	h := start(t, s, Options{NoOpen: true})
	srv, err := s.ServeIngest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	h.m.ingest = srv

	h.typed("!oc get co")
	h.press("enter")
	if got := h.m.input.Value(); got != "!oc get co" || !strings.Contains(h.out(), `! alone opens your shell, where oc get co | compa in "what does this answer?" keeps its output`) {
		t.Errorf("!oc get co: input %q, printed:\n%s", got, h.out())
	}

	sub := h.m.subshellFor()
	var term strings.Builder
	sub.SetStdin(strings.NewReader("echo \"case=$COMPA_CASE\"\npwd\ntest -S \"$COMPA_INGEST\" && echo socket-ok\nexit\n"))
	sub.SetStdout(&term)
	sub.SetStderr(&term)
	if err := sub.Run(); err != nil {
		t.Fatal(err)
	}
	got := ansi.Strip(term.String())
	root, err := filepath.EvalSymlinks(s.Store().ArtifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"━━ compa shell · " + s.CaseID(), `in "what does this answer?" [-t tag,tag]`, "Back to chat:   exit (or ctrl+d)",
		"case=" + s.CaseID(), root, "socket-ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("the shell shows no %q:\n%s", want, got)
		}
	}

	item, err := engine.SendIngest(t.Context(), srv.Socket(), strings.NewReader("NAME   READY\nnetwork  False\n"), "stdin", "what does this answer? · tags: openshift")
	if err != nil {
		t.Fatal(err)
	}
	mark := len(h.printed)
	h.update(shellDoneMsg{})
	h.settle()
	kept := "kept " + item.Name + " (stdin, 2 lines: what does this answer? · tags: openshift)"
	if out := flat(h.since(mark)); !strings.Contains(out, kept) || !strings.Contains(out, "back from the shell: 1 item kept, which your next message names") {
		t.Errorf("back from the shell:\n%s", out)
	}
	if v := h.view(); !strings.Contains(v, "naming 1 item kept") {
		t.Errorf("the status line does not say the item is named:\n%s", v)
	}
	h.m.input.Reset()
	h.say("what does it show?")
	history, err := s.ConversationHistory(t.Context())
	if err != nil || len(history) == 0 {
		t.Fatal(history, err)
	}
	sent := ""
	for _, msg := range history {
		if msg.Kind == engine.MessageEngineer {
			sent = msg.Text
		}
	}
	if !strings.HasPrefix(sent, "the engineer kept with compa in "+item.Name+" (stdin, 2 lines: ") || !strings.HasSuffix(sent, "what does it show?") {
		t.Errorf("the message sent: %q", sent)
	}
	if strings.Contains(h.view(), "naming 1 item") {
		t.Error("the status line still names the item after the message")
	}
}

// TestDebugLines: /debug on prints each model call's input, the share read
// from cache, its output and its timing, from then on only.
func TestDebugLines(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.say("which model?")
	if strings.Contains(h.out(), "call ") {
		t.Errorf("a call printed before /debug:\n%s", h.out())
	}
	h.say("/debug on")
	mark := len(h.printed)
	h.say("which model again?")
	if out := flat(h.since(mark)); !regexp.MustCompile(`call \S+case\.json effort high · in 100 · 0% cached · out 20 · first token \S+ · total \S+`).MatchString(out) {
		t.Errorf("the turn under /debug:\n%s", out)
	}
}

// TestComments: the browser's comments tab lists the case's comments
// newest first, and enter opens the newest, before any conversation has
// named them; tab on to the turns and back keeps each tab's list.
func TestComments(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.say("/evidence")
	h.press("tab")
	v := h.view()
	first, last := strings.Index(v, "comment c3 · support-engineer"), strings.Index(v, "comment c1 · customer")
	if first < 0 || last < 0 || first > last || !strings.Contains(v, "[comments]") {
		t.Errorf("the comments tab lists:\n%s", v)
	}
	h.press("enter")
	b := h.m.browser
	if b == nil || b.open == nil {
		t.Fatalf("enter on the newest comment opened nothing:\n%s", h.view())
	}
	if body := strings.Join(b.open.lines, "\n"); !strings.Contains(body, "Requested a fresh must-gather") {
		t.Errorf("enter opened:\n%s", body)
	}
	h.press("tab", "tab", "tab")
	if v := h.view(); !strings.Contains(v, "[evidence]") || strings.Contains(v, "comment c3") {
		t.Errorf("back on the evidence tab:\n%s", v)
	}
	h.press("tab")
	if b.open == nil || !strings.Contains(h.view(), "Requested a fresh must-gather") {
		t.Errorf("the comments tab lost the comment open:\n%s", h.view())
	}
}

// TestUnknownCommand: an unknown command says so and lists the commands.
func TestUnknownCommand(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.say("/nonsense")
	if out := h.out(); !strings.Contains(out, "there is no command /nonsense; the commands are /draft") {
		t.Errorf("an unknown command:\n%s", out)
	}
}

// TestReopenLines: the reopened case's lines say what is new, or that
// nothing is, and the expired cache only on a large conversation.
func TestReopenLines(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.Local)
	r := engine.ReopenState{Since: at, LastCall: at, NewComments: 2, NewAttachments: []string{"0030-sosreport"}, Attachments: true,
		CacheLifetime: 10 * time.Minute, CacheExpired: true, Estimate: 45_000, Large: true}
	got := reopenLines(r)
	want := []string{"since you last worked this case (2026-11-06 12:00): 2 new comments, 1 new attachment; your next message gives them to the conversation, and /ledger reads the comments",
		"the provider's cache has expired since the last call (2026-11-06 12:00): your next message resends the conversation, about 45,000 tokens, without it; /compact goes on in a new one from the case's state and a handoff"}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("the lines (-want +got):\n%s", d)
	}
	r = engine.ReopenState{Since: at, CacheExpired: true, Estimate: 20_000}
	if got := reopenLines(r); len(got) != 1 || got[0] != "nothing new since you last worked this case (2026-11-06 12:00)" {
		t.Errorf("the lines for a small conversation with nothing new: %q", got)
	}
}

// TestShellBannerFits: the shell's banner keeps each line within the
// width, a long folder shortened from its start, and says why compa in
// cannot reach the chat where it cannot.
func TestShellBannerFits(t *testing.T) {
	dir := "/" + strings.Repeat("deep/", 40) + "case-folder"
	got := ansi.Strip(shellBanner("compa", "00000001", dir, "", 100))
	for _, l := range strings.Split(strings.TrimRight(got, "\r\n"), "\r\n") {
		if n := ansi.StringWidth(l); n > 80 {
			t.Errorf("a line %d wide: %q", n, l)
		}
	}
	if !strings.Contains(got, "━━ compa shell · 00000001 · …") || !strings.Contains(got, "case-folder ━━") {
		t.Errorf("the banner:\n%s", got)
	}
	if got := ansi.Strip(shellBanner("compa", "00000001", "", "no socket", 80)); !strings.Contains(got, "compa in cannot reach this chat: no socket") {
		t.Errorf("the banner where compa in cannot reach the chat:\n%s", got)
	}
}

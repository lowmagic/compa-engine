package chat

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// The editor's chat tests: the companion answers from the synthetic
// testdata/edited.json and the editor from testdata/editor.json, its
// findings synthetic.

const (
	wordingAnswer    = "The upgrade confirms that the OVN controller failures were caused by worker-1 [e1:2]."
	wordingCorrected = "The OVN controller failures began after the upgrade, on worker-1"
	anchorRevised    = "The customer reports that the operator restarted ovnkube-node at 09:00"
)

// editedSession opens the stub case on an engine whose companion answers
// from edited.json and whose editor, on a model of its own, from
// editor.json.
func editedSession(t *testing.T) *engine.Session {
	t.Helper()
	return editedSessionWith(t, "editor.json")
}

// turnsTab opens the browser's turns tab and returns its view.
func (h *harness) turnsTab() string {
	h.t.Helper()
	h.say("/evidence")
	h.settle()
	h.press("tab", "tab")
	return h.view()
}

// TestEditorCorrectionShown: [E2] an answer the editor corrects streams
// marked as being checked, the status bar saying the editor runs, and is
// replaced in the live view by the correction before anything is printed;
// the correction alone is printed, its footer saying so and where the
// original is, and the editor's call counts in the total. A reopened chat
// shows the same, and the turns tab lists the answer as written and the
// correction, each opened as stored.
func TestEditorCorrectionShown(t *testing.T) {
	s := editedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.typed("wording please")
	h.press("enter")
	h.until("the editor checks", func() bool { return h.m.turn != nil && h.m.turn.status == "the editor is checking the answer" })
	if v := flat(h.view()); !strings.Contains(v, "⋯ being checked: the editor reads it before it is printed") || !strings.Contains(v, "confirms that the OVN controller failures") ||
		!strings.Contains(v, "the editor is checking the answer") {
		t.Errorf("the view while the editor checks:\n%s", h.view())
	}
	h.until("the correction replaces it", func() bool { return h.m.turn != nil && h.m.turn.mark == markCorrected })
	if v := flat(h.view()); !strings.Contains(v, "⋯ corrected by the editor") || !strings.Contains(v, wordingCorrected) || strings.Contains(v, "confirms that") {
		t.Errorf("the view once corrected:\n%s", h.view())
	}
	if out := flat(h.out()); strings.Contains(out, "OVN controller failures") {
		t.Errorf("printed before the turn ended:\n%s", out)
	}
	h.settle()
	foot := "· corrected by the editor: 1 statement · the original in the turns tab"
	for _, run := range []struct {
		name string
		h    *harness
	}{{"the turn", h}, {"the reopened chat", start(t, s, Options{NoOpen: true})}} {
		out := flat(run.h.out())
		if !strings.Contains(out, wordingCorrected) || strings.Contains(out, "were caused by") || !strings.Contains(out, foot) || strings.Count(out, "a1 ·") != 1 {
			t.Errorf("%s printed:\n%s", run.name, out)
		}
	}
	if h.m.usage.Invocations != 2 {
		t.Errorf("the total counts %d calls; want the answer's and the editor's", h.m.usage.Invocations)
	}
	r := start(t, s, Options{NoOpen: true})
	v := r.turnsTab()
	original, correction := strings.Index(v, "  a1 · "), strings.Index(v, "› a1 corrected by the editor · ")
	if original < 0 || correction < original {
		t.Fatalf("the turns tab:\n%s", v)
	}
	r.press("up", "enter")
	if v := r.view(); !strings.Contains(flat(v), wordingAnswer) {
		t.Errorf("the original opened is not as stored:\n%s", v)
	}
}

// TestEditorAnchorShown: [E2] an answer with an anchor is printed as the
// model revised it, its single quiet footer saying it was revised and where
// the anchors and original are, the anchors themselves never printed inline
// (with /debug adding the per-kind anchor breakdown). The checks tab lists
// the anchor whole, for reading, live and reopened. The turns tab lists the
// answer, the engine's ask and the revision under the answer's name, and a
// reopened chat prints the revision once.
func TestEditorAnchorShown(t *testing.T) {
	s := editedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("anchor please")
	for _, run := range []struct {
		name string
		h    *harness
	}{{"the turn", h}, {"the reopened chat", start(t, s, Options{NoOpen: true})}} {
		out := flat(run.h.out())
		for _, want := range []string{anchorRevised,
			"· revised for 1 anchor · /check a1 lists anchors · the original in the turns tab"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", run.name, want, out)
			}
		}
		if strings.Count(out, "a1 ·") != 1 || strings.Contains(out, "a2 ·") || strings.Contains(out, "asked of the revision: s1") {
			t.Errorf("%s does not print the revision once, as a1, with its anchor summed up:\n%s", run.name, out)
		}
		checkAt(t, run.h, "a1")
		want := []string{`· s1 misattributed · "The operator restarted ovnkube-node" who saw the restart is not in the lines given`}
		if got := tabbed(run.h, "a1"); !slices.Equal(got, want) {
			t.Errorf("%s: the tab lists %q; want %q", run.name, got, want)
		}
		v := flat(run.h.view())
		for _, w := range []string{"Finding: an anchor asked of the revision", "Statement: s1 · misattributed", "Asked: name who said it, or mark it inferred",
			"Verdict: · for reading: it takes no verdict"} {
			if !strings.Contains(v, w) {
				t.Errorf("%s: the anchor's detail lacks %q:\n%s", run.name, w, run.h.view())
			}
		}
		run.h.press("esc")
		run.h.settle()
	}
	v := start(t, s, Options{NoOpen: true}).turnsTab()
	a1, ask, revised := strings.Index(v, "  a1 · "), strings.Index(v, "  a1 sent back for revision · "), strings.Index(v, "› a1 revised · ")
	if a1 < 0 || ask < a1 || revised < ask {
		t.Errorf("the turns tab:\n%s", v)
	}
}

// TestAsksLineSumsUp: the line beneath a revised answer counts its
// anchors by kind, most first, in one line however many there are, as
// 13 printed one a line did not in live use.
func TestAsksLineSumsUp(t *testing.T) {
	var asks []reading
	for i := range 13 {
		kind := "misattributed"
		if i%3 == 1 {
			kind = "unsupported"
		}
		asks = append(asks, reading{about: aboutAnchor, kind: kind})
	}
	a := answer{label: "a2", asks: asks}
	if got, want := asksLine(a), "asked of the revision: 13 anchors: 9 misattributed, 4 unsupported · /check a2 lists them"; got != want {
		t.Errorf("many anchors: %q; want %q", got, want)
	}
	if got, want := asksLine(answer{label: "a2", asks: asks[:1]}), "asked of the revision: 1 anchor, misattributed · /check a2 lists them"; got != want {
		t.Errorf("one anchor: %q; want %q", got, want)
	}
	if got := asksLine(answer{label: "a2"}); got != "" {
		t.Errorf("no anchors: %q", got)
	}
}

// TestFindingsInChecksTab: beneath an answer with a citation flagged, one
// not in the form read, a span in no evidence, a finding of the editor's
// it wrote no correction for and a citation the revision added, one line
// says how many findings there are, f opening the checks tab, and none of
// them is printed; the anchor asked of the revision, which is no finding,
// is summed up in a line of its own. The tab lists each: the span first,
// to judge, then the rest, for reading, · their state, the anchor last,
// their verdict keys keeping nothing and saying so, in the tab and on the
// lines m opens. Reopened, the same line names /check, and the tab lists
// the same.
func TestFindingsInChecksTab(t *testing.T) {
	s := editedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("findings please")
	out := h.out()
	if got, want := findingLines(out), []string{"⚠ 5 findings · /check a1 opens them"}; !slices.Equal(got, want) {
		t.Errorf("the finding lines %q; want %q:\n%s", got, want, out)
	}
	for _, not := range []string{"no evidence e999", "without brackets", "in no evidence this conversation saw", "no line given holds it", "added by the revision, naming"} {
		if strings.Contains(flat(out), not) {
			t.Errorf("the answer is printed with %q beneath it:\n%s", not, out)
		}
	}
	if !strings.Contains(flat(out), "revised for 1 anchor") || !strings.Contains(flat(out), "/check a1 lists anchors") || strings.Contains(out, "asked of the revision: s2") {
		t.Errorf("the anchor asked is not summed up in the footer:\n%s", out)
	}
	want := []string{
		"? `no such marker 7f3a` in no evidence this conversation saw",
		"· e999:2 no evidence e999",
		"· e1:2 not read as a citation without brackets: write [e1:2]",
		"· s1 unsupported · \"logged `no such marker 7f3a`\" no line given holds it",
		"· [e1:2-3] added by the revision, naming lines no tool result showed it",
		"· s2 misattributed · \"The operator restarted ovnkube-pods\" who saw the restart is not in the lines given",
	}
	mark := len(h.printed)
	p := openChecks(t, h)
	if got := tabbed(h, "a1"); !slices.Equal(got, want) {
		t.Errorf("the tab lists %q; want %q", got, want)
	}
	if p.cur != 0 {
		t.Errorf("f opened the tab at %d; want the span to judge", p.cur)
	}
	for range 4 {
		h.press("down")
	}
	v := flat(h.view())
	for _, w := range []string{"· a1 [e1:2-3] · added by the revision", "Finding: a citation the revision added", "Citation: [e1:2-3]",
		"Verdict: · for reading: it takes no verdict", "Evidence: e1:2-3 · the lines it cites", "for reading: it takes no verdict · m opens the lines it cites"} {
		if !strings.Contains(v, w) {
			t.Errorf("the added citation's detail lacks %q:\n%s", w, h.view())
		}
	}
	refused := "a1 [e1:2-3] " + readWords
	for _, k := range []string{"y", "i", "u", "x", "enter", "e", "d"} {
		h.press(k)
		h.settle()
		if b := h.m.browser; b.note != refused || p.judging || p.listing {
			t.Errorf("%s on a finding for reading: the note %q, judging %v, listing %v; want %q", k, b.note, p.judging, p.listing, refused)
		}
	}
	h.press("m")
	if o := p.view; o == nil || o.name != "e1" || o.from != 2 || o.to != 3 {
		t.Fatalf("m opened %+v; want e1 at lines 2 to 3", p.view)
	}
	h.m.browser.note = ""
	h.press("y", "v", "enter")
	h.settle()
	if p.view == nil || h.m.browser.note != refused || len(p.values[p.cur].marks) > 0 {
		t.Errorf("keys on the lines it cites: open %v, the note %q, marks %v", p.view != nil, h.m.browser.note, p.values[p.cur].marks)
	}
	h.press("esc", "esc")
	h.settle()
	if len(h.m.answers[0].verdicts) > 0 || len(verdictMessages(t, s)) > 0 || h.m.browser != nil {
		t.Errorf("a verdict was kept: %+v; the browser open %v", h.m.answers[0].verdicts, h.m.browser != nil)
	}
	if out := strings.TrimSpace(h.since(mark)); out != "› /check a1" {
		t.Errorf("the tab printed %q; want only the command", out)
	}

	r := start(t, s, Options{NoOpen: true})
	if got, w := findingLines(r.out()), []string{"⚠ 5 findings · /check a1 opens them"}; !slices.Equal(got, w) {
		t.Errorf("reopened, the finding lines %q; want %q:\n%s", got, w, r.out())
	}
	checkAt(t, r, "a1")
	if got := tabbed(r, "a1"); !slices.Equal(got, want) {
		t.Errorf("reopened, the tab lists %q; want %q", got, want)
	}
	for range 3 {
		r.press("down")
	}
	v = flat(r.view())
	for _, w := range []string{"Finding: the editor's finding, not corrected", "Statement: s1 · unsupported", "Words: \"logged `no such marker 7f3a`\"",
		"Why: no line given holds it", "At most: the controller logged errors", "Evidence: it cites no item's lines"} {
		if !strings.Contains(v, w) {
			t.Errorf("the editor's finding's detail lacks %q:\n%s", w, r.view())
		}
	}
}

// TestEditorUncheckedShown: [E2] an editor that fails leaves the answer
// printed as written, its footer saying it went unchecked and why.
func TestEditorUncheckedShown(t *testing.T) {
	h := start(t, editedSession(t), Options{NoOpen: true})
	h.say("fail please")
	out := flat(h.out())
	if !strings.Contains(out, "The OVN controller logs nothing new") || !strings.Contains(out, "· unchecked: the editor failed: ") || strings.Contains(out, "the original in the turns tab") {
		t.Errorf("printed:\n%s", out)
	}
}

// TestEditorDraftCorrected: [E2] a draft is printed and kept as the editor
// corrected it, and /copy copies that text.
func TestEditorDraftCorrected(t *testing.T) {
	h := start(t, editedSession(t), Options{NoOpen: true})
	h.say("/draft customer-update")
	draft := "Hello, the OVN controller on worker-1 cannot sync; we are reading its logs."
	if out := flat(h.out()); !strings.Contains(out, draft) || strings.Contains(out, "investigation confirms") || !strings.Contains(out, "corrected by the editor: 1 statement") || !strings.Contains(out, "kept as i1") {
		t.Errorf("/draft printed:\n%s", out)
	}
	h.say("/copy")
	if d := cmp.Diff([]tea.Msg{tea.SetClipboard(draft)()}, h.teaMsgs); d != "" {
		t.Errorf("/copy asked of the terminal (-want +got):\n%s", d)
	}
}

// editedSessionWith is editedSession with the editor answering from the
// script given.
func editedSessionWith(t *testing.T, editorScript string) *engine.Session {
	t.Helper()
	editor, err := filepath.Abs(filepath.Join("testdata", editorScript))
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(chatConfig(t, "edited.json"), "  profiles:\n",
		"    editor-model: {provider: fixture, model: "+editor+"}\n  profiles:\n    editor: {model: editor-model}\n", 1)
	s, err := enginetest.EngineWith(t, cfg).StartSession(t.Context(), "00000001", nil)
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

// TestEditorRevisionNotTakenShown: [E2] a revision that is not taken — a
// tool called, a transport failure, an empty answer — leaves the answer
// as written printed with the failure and the anchor asked, the
// revision's own text never printed, even where the model wrote some
// before calling a tool; reopening prints the same under the same names,
// and /pin keeps what was shown.
func TestEditorRevisionNotTakenShown(t *testing.T) {
	s := editedSession(t)
	h := start(t, s, Options{NoOpen: true})
	kinds := []struct{ ask, answer string }{
		{"tool revision please", "The operator restarted ovnkube-master at 10:00"},
		{"transport revision please", "The operator restarted ovnkube-control at 10:00"},
		{"empty revision please", "The operator restarted ovnkube-identity at 10:00"},
	}
	for _, k := range kinds {
		h.say(k.ask)
	}
	h.say("/pin")
	for _, run := range []struct {
		name string
		h    *harness
	}{{"the turns", h}, {"the reopened chat", start(t, s, Options{NoOpen: true})}} {
		out := flat(run.h.out())
		if strings.Contains(out, "REJECTED REVISION") {
			t.Errorf("%s printed the revision not taken:\n%s", run.name, out)
		}
		for i, k := range kinds {
			label := "a" + string(rune('1'+i)) + " ·"
			if strings.Count(out, k.answer) != 1 || strings.Count(out, label) != 1 {
				t.Errorf("%s shows %q or %q other than once:\n%s", run.name, k.answer, label, out)
			}
		}
		if strings.Count(out, "! the revision failed: ") != 3 || strings.Count(out, "asked 1 anchor · /check a") != 3 || strings.Contains(out, "a4 ·") {
			t.Errorf("%s:\n%s", run.name, out)
		}
	}
	items, err := s.Items(t.Context())
	if err != nil || len(items) != 1 || !strings.HasPrefix(items[0].Text, kinds[2].answer) {
		t.Errorf("/pin kept %+v, %v", items, err)
	}
}

// TestEditorCancelledShown: [E2] a turn cancelled while the editor checks
// prints its answer as written, said unchecked, then that it was
// cancelled; the next answer is numbered after it, and reopening prints
// both under the same names.
func TestEditorCancelledShown(t *testing.T) {
	s := editedSessionWith(t, "editor-slow.json")
	h := start(t, s, Options{NoOpen: true})
	h.typed("wording please")
	h.press("enter")
	h.until("the editor checks", func() bool { return h.m.turn != nil && h.m.turn.status == "the editor is checking the answer" })
	h.press("ctrl+c")
	h.settle()
	h.say("fail please")
	for _, run := range []struct {
		name string
		h    *harness
	}{{"the turns", h}, {"the reopened chat", start(t, s, Options{NoOpen: true})}} {
		out := flat(run.h.out())
		first, second := strings.Index(out, wordingAnswer), strings.Index(out, "The OVN controller logs nothing new")
		if first < 0 || second < first || strings.Count(out, "a1 ·") != 1 || strings.Count(out, "a2 ·") != 1 || !strings.Contains(out, "· unchecked: cancelled") {
			t.Errorf("%s:\n%s", run.name, out)
		}
	}
	if out := flat(h.out()); !strings.Contains(out, "unchecked: cancelled cancelled") {
		t.Errorf("the cancelled turn does not say so after its answer:\n%s", out)
	}
}

// TestEditorNotesWaitForAnswer: [E2] a warning said while the editor
// checks an answer, and another while the model revises it, wait until
// the revision is printed: nothing is printed while the live view shows
// the answer under its mark, so neither the answer as first written nor
// the mark can reach the scrollback, and the revision is printed once,
// the warnings after it.
func TestEditorNotesWaitForAnswer(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	mark := len(h.printed)
	// What each print could leave in the scrollback besides itself: the
	// frame drawn when it is made.
	var frames []string
	write := h.m.println
	h.m.println = func(s string) tea.Cmd {
		frames = append(frames, h.view())
		return write(s)
	}
	const (
		original = "The operator restarted the synthetic DNS pods at 09:00 [e1]."
		revised  = "The customer reports that the operator restarted the synthetic DNS pods at 09:00 [e1]."
	)
	h.m.turn = &turnState{kind: kindAnswer, model: h.m.current, mark: markChecking}
	// This synthetic turn has no work goroutine; its clock is test-owned.
	h.m.spinning = true
	for _, ev := range []engine.TalkEvent{
		{Kind: engine.TalkText, Text: original},
		{Kind: engine.TalkStage, Text: engine.StageChecking},
		{Kind: engine.TalkWarning, Text: "a synthetic warning during the check"},
		{Kind: engine.TalkStage, Text: engine.StageRevising},
		{Kind: engine.TalkText, Text: revised},
		{Kind: engine.TalkWarning, Text: "the editor's check was not kept: a synthetic failure"},
		{Kind: engine.TalkReplaced, Text: revised},
	} {
		ev.Conversation = "synthetic"
		h.update(eventMsg{ev})
		h.settle()
	}
	if out := h.since(mark); out != "" {
		t.Errorf("printed while the editor checked:\n%s", out)
	}
	h.update(turnDone{kind: kindAnswer, text: revised, check: engine.AnswerCheck{Ran: true, Revised: true}})
	h.settle()
	out := flat(h.since(mark))
	if strings.Count(out, revised) != 1 || strings.Contains(out, "The operator restarted") {
		t.Errorf("printed:\n%s", out)
	}
	answer, check, kept := strings.Index(out, revised), strings.Index(out, "a synthetic warning during the check"), strings.Index(out, "check was not kept")
	if check < answer || kept < check {
		t.Errorf("the warnings are not printed after the answer, in order:\n%s", out)
	}
	for _, f := range frames {
		if strings.Contains(f, "being checked") || strings.Contains(f, "being revised") || strings.Contains(f, "restarted the synthetic DNS pods") {
			t.Errorf("a print was made while the view showed the answer under its mark:\n%s", f)
		}
	}
}

// TestRevisionHoldsLiveRows: a revision taking a tall answer's place does
// not shrink the view before a print, which would leave the answer and its
// mark on screen (chat.go, menuHeight); the rows it gave up are blank, and
// the print at the turn's end gives them back.
func TestRevisionHoldsLiveRows(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	mark := len(h.printed)
	var tall strings.Builder
	tall.WriteString("The operator restarted the synthetic DNS pods at 09:00 [e1].\n")
	for n := range 20 {
		fmt.Fprintf(&tall, "\n- synthetic point %d holds nothing new.", n+1)
	}
	const revised = "The customer reports that the operator restarted the synthetic DNS pods at 09:00 [e1]."
	h.m.turn = &turnState{kind: kindAnswer, model: h.m.current, mark: markChecking}
	// This synthetic turn has no work goroutine; its clock is test-owned.
	h.m.spinning = true
	rows := func() int { return strings.Count(h.view(), "\n") + 1 }
	var answer int
	for _, ev := range []engine.TalkEvent{
		{Kind: engine.TalkText, Text: tall.String()},
		{Kind: engine.TalkStage, Text: engine.StageChecking},
		{Kind: engine.TalkStage, Text: engine.StageRevising},
		{Kind: engine.TalkText, Text: revised},
	} {
		ev.Conversation = "synthetic"
		h.update(eventMsg{ev})
		h.settle()
		if answer == 0 {
			answer = rows()
		}
		if r := rows(); r < answer {
			t.Fatalf("the view shrank from %d to %d rows with nothing printed, after event %v %q:\n%s", answer, r, ev.Kind, ev.Text, h.view())
		}
	}
	if out := h.since(mark); out != "" {
		t.Errorf("printed during the check:\n%s", out)
	}
	h.update(turnDone{kind: kindAnswer, text: revised, check: engine.AnswerCheck{Ran: true, Revised: true}})
	h.settle()
	if out := flat(h.since(mark)); strings.Count(out, revised) != 1 || strings.Contains(out, "point 20") {
		t.Errorf("printed:\n%s", out)
	}
	if r := rows(); r >= answer {
		t.Errorf("the view kept %d rows after the turn's prints:\n%s", r, h.view())
	}
}

package chat

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// The checks tab's tests: the companion answers from the synthetic
// testdata/verdict.json, its opening grepping the fixture must-gather and
// stating two values the evidence does not hold as written, each with a
// line close to it, one sharing no word with any line, arithmetic that
// does not hold and arithmetic that does.

// spanSession opens the stub case on an engine whose companion answers
// from verdict.json, with the reviewer of reviewedSession where reviewer.
func spanSession(t *testing.T, reviewer bool) *engine.Session {
	t.Helper()
	cfg := chatConfig(t, "verdict.json")
	if reviewer {
		path, err := filepath.Abs(filepath.Join("testdata", "reviewer.json"))
		if err != nil {
			t.Fatal(err)
		}
		cfg = strings.Replace(cfg, "  profiles:\n", "    reviewer-model: {provider: fixture, model: "+path+"}\n  profiles:\n    reviewer: {model: reviewer-model}\n", 1)
	}
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

// The opening's flagged spans, in the answer's order.
const (
	syncSpan    = "error syncing node worker-9"
	chassisSpan = "failed to get chassis 0000"
	sumSpan     = "16 GiB / 4 = 5 GiB"
	noneSpan    = "zqxj vbnm"
)

// flagOf is the flag of span among the answer's.
func flagOf(t *testing.T, a answer, span string) engine.AnswerFlag {
	t.Helper()
	for _, f := range a.flags {
		if f.Span == span {
			return f
		}
	}
	t.Fatalf("%s has no flag of %q: %+v", a.label, span, a.flags)
	return engine.AnswerFlag{}
}

// verdictMessages are the verdicts the model reads in the conversation.
func verdictMessages(t *testing.T, s *engine.Session) []string {
	t.Helper()
	history, err := s.ConversationHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range history {
		if m.Kind == engine.MessageVerdict {
			out = append(out, m.Text)
		}
	}
	return out
}

// openChecks takes the offer after the opening with f and waits for the
// checks tab.
func openChecks(t *testing.T, h *harness) *checksPane {
	t.Helper()
	h.press("f")
	h.until("the browser opens", func() bool { return h.m.browser != nil })
	h.settle()
	if h.m.browser.tab != tabChecks {
		t.Fatalf("f opened the browser on tab %d; want the checks tab:\n%s", h.m.browser.tab, h.view())
	}
	return h.m.browser.checks
}

// checkAt opens the checks tab with /check label and waits for it.
func checkAt(t *testing.T, h *harness, label string) *checksPane {
	t.Helper()
	h.say("/check " + label)
	h.until("the browser opens", func() bool { return h.m.browser != nil })
	h.settle()
	return h.m.browser.checks
}

// tabbed are the findings the checks tab lists of the answer labelled,
// in order, each as its state, the finding as listed and why.
func tabbed(h *harness, label string) []string {
	var out []string
	for _, v := range h.m.browser.checks.values {
		if v.label != label {
			continue
		}
		why := v.flag.Why
		if v.read != nil {
			why = v.read.why
		}
		out = append(out, h.m.stateOf(v)+" "+v.shown()+" "+why)
	}
	return out
}

// findingLines are the lines printed that begin ⚠, the findings' line
// beneath each answer.
func findingLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "⚠ ") {
			lines = append(lines, l)
		}
	}
	return lines
}

// at moves the tab's cursor to the value of span.
func at(t *testing.T, h *harness, span string) {
	t.Helper()
	p := h.m.browser.checks
	want := slices.IndexFunc(p.values, func(v checkValue) bool { return v.flag.Span == span })
	if want < 0 {
		t.Fatalf("the tab lists no %q", span)
	}
	for p.cur < want {
		h.press("down")
	}
	for p.cur > want {
		h.press("up")
	}
}

// judge presses key on the value under the cursor and waits for its
// verdict.
func judge(h *harness, key string) {
	h.press(key)
	h.settle()
}

// TestChecksTabListsEveryFlag: [E4] after the opening the chat offers its
// findings on f, in one line beneath it, none of them printed; f opens the
// browser's checks tab at them, printing nothing more: every span flagged,
// those with no close line and the engine's arithmetic among them, each
// with its state — ? unjudged, ✗ the arithmetic that does not hold — and
// its complaint, and arithmetic that holds not at all; a value with no
// close line shows no evidence and is judged as any.
func TestChecksTabListsEveryFlag(t *testing.T) {
	s := spanSession(t, false)
	h := start(t, s, Options{})
	out := h.out()
	if got, want := findingLines(out), []string{"⚠ 4 findings to review · f opens the checks tab"}; !slices.Equal(got, want) {
		t.Errorf("the opening shows the finding lines %q; want %q:\n%s", got, want, out)
	}
	if strings.Contains(flat(out), "in no evidence this conversation saw") {
		t.Errorf("the opening prints a span's complaint:\n%s", out)
	}
	mark := len(h.printed)
	p := openChecks(t, h)
	var spans []string
	for _, v := range p.values {
		spans = append(spans, v.label+" "+v.flag.Span)
	}
	if want := []string{"a1 " + syncSpan, "a1 " + chassisSpan, "a1 " + sumSpan, "a1 " + noneSpan}; !slices.Equal(spans, want) {
		t.Errorf("the tab lists %q; want %q", spans, want)
	}
	view := h.view()
	for _, want := range []string{"[checks]", "? a1 `" + syncSpan + "`", "? a1 `" + chassisSpan + "`", "✗ a1 `" + sumSpan + "`", "? a1 `" + noneSpan + "`"} {
		if !strings.Contains(view, want) {
			t.Errorf("the tab shows no %q:\n%s", want, view)
		}
	}
	if out := h.since(mark); out != "› /check a1" {
		t.Errorf("opening the tab printed %q; want only the command", out)
	}
	at(t, h, sumSpan)
	if v := flat(h.view()); !strings.Contains(v, "Verdict: ✗ incorrect, by the engine: the arithmetic does not hold") ||
		!strings.Contains(v, "Complaint: `"+sumSpan+"` incorrect: the arithmetic does not hold") {
		t.Errorf("the arithmetic's complaint and verdict:\n%s", h.view())
	}
	at(t, h, noneSpan)
	if v := flat(h.view()); !strings.Contains(v, "Evidence: none close to it") || !strings.Contains(v, "Claim: Nothing logged `zqxj vbnm`.") ||
		!strings.Contains(v, "Complaint: `"+noneSpan+"` in no evidence this conversation saw · "+unsupportedWords) {
		t.Errorf("a value with no close line:\n%s", h.view())
	}
	judge(h, "u")
	if got, ok := h.m.answers[0].verdicts[noneSpan]; !ok || got.Kind != engine.VerdictUnsupported {
		t.Errorf("u on a value with no close line kept %+v, %v", got, ok)
	}
}

// TestChecksContext: [E4] the evidence shown for a value is its line with
// two lines before and five after, or a range with one before and two
// after, within the item, numbered as stored and shown as the browser
// shows the item.
func TestChecksContext(t *testing.T) {
	for _, c := range []struct{ from, to, n, a, z int }{
		{10, 10, 100, 8, 15}, {10, 12, 100, 9, 14}, {1, 1, 3, 1, 3}, {2, 3, 3, 1, 3},
	} {
		if a, z := contextOf(c.from, c.to, c.n); a != c.a || z != c.z {
			t.Errorf("contextOf(%d, %d, %d) = %d, %d; want %d, %d", c.from, c.to, c.n, a, z, c.a, c.z)
		}
	}
	s := spanSession(t, false)
	h := start(t, s, Options{})
	sync := flagOf(t, h.m.answers[0], syncSpan)
	openChecks(t, h)
	it, err := s.EvidenceNamed(sync.Closest.Name)
	if err != nil {
		t.Fatal(err)
	}
	lines := withoutSource(contentLines(string(it.Content)), it.Location)
	a, z := contextOf(sync.Closest.Line, sync.Closest.Line, len(lines))
	var shown []string
	numbered := regexp.MustCompile(`^  +(\d+) (.*)$`)
	rows := strings.Split(h.view(), "\n")
	start := slices.IndexFunc(rows, func(r string) bool { return strings.HasPrefix(r, "Evidence:") })
	for _, r := range rows[start+1:] {
		mm := numbered.FindStringSubmatch(r)
		if mm == nil {
			break
		}
		n, _ := strconv.Atoi(mm[1])
		if !strings.HasPrefix(lines[n-1], strings.TrimSuffix(mm[2], "…")) {
			t.Errorf("line %d shown as %q; want it as the browser shows it, %q", n, mm[2], lines[n-1])
		}
		shown = append(shown, mm[1])
	}
	var want []string
	for n := a; n <= z; n++ {
		want = append(want, strconv.Itoa(n))
	}
	if !slices.Equal(shown, want) {
		t.Errorf("the evidence shows lines %v; want %v:\n%s", shown, want, h.view())
	}
	if strings.Contains(h.view(), "namespaces/openshift-ovn-kubernetes") {
		t.Errorf("the tab shows the evidence with the search's file name:\n%s", h.view())
	}
}

// TestChecksMarkThenSupported: [E4] m opens the evidence shown at the
// closest line; v and enter mark a range, esc returns to the value, and y
// keeps it supported resting on that range, not the closest line; i with
// nothing marked is refused, saying why, and keeps nothing.
func TestChecksMarkThenSupported(t *testing.T) {
	s := spanSession(t, false)
	h := start(t, s, Options{})
	sync := flagOf(t, h.m.answers[0], syncSpan)
	p := openChecks(t, h)
	at(t, h, chassisSpan)
	h.press("i")
	if note := h.m.browser.note; !strings.Contains(note, engine.ErrInferredUnmarked.Error()) || p.judging {
		t.Errorf("i with nothing marked: %q, judging %v", note, p.judging)
	}
	h.settle()
	if _, ok := h.m.answers[0].verdicts[chassisSpan]; ok {
		t.Error("i with nothing marked kept a verdict")
	}
	at(t, h, syncSpan)
	h.press("m")
	if p.view == nil || p.view.name != sync.Closest.Name || p.view.cur != sync.Closest.Line-1 {
		t.Fatalf("m opened %+v; want %s", p.view, sync.Closest.Place())
	}
	from := sync.Closest.Line
	if from > 1 {
		h.press("up")
		from--
	}
	h.press("v", "down", "enter", "esc")
	want := engine.EvidenceRange{Name: sync.Closest.Name, From: from, To: from + 1}
	if p.view != nil || len(p.values[p.cur].marks) != 1 || p.values[p.cur].marks[0].Place() != want.Place() {
		t.Fatalf("the value's marks %+v; want %s", p.values[p.cur].marks, want.Place())
	}
	judge(h, "y")
	got := h.m.answers[0].verdicts[syncSpan]
	if got.Kind != engine.VerdictSupported || len(got.Evidence) != 1 || got.Evidence[0].Place() != want.Place() {
		t.Errorf("y kept %+v; want supported resting on %s", got, want.Place())
	}
	history, err := s.ConversationHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kept := history[engine.AnswerViews(history)[0].Original].Verdicts
	if len(kept) != 1 || kept[0].Evidence[0].Place() != want.Place() {
		t.Errorf("the verdict as stored: %+v", kept)
	}
}

// TestChecksVerdictFromMarkView: on the evidence m opened, a verdict's key
// judges the value on the lines just marked and returns to the tab, with
// no esc first.
func TestChecksVerdictFromMarkView(t *testing.T) {
	s := spanSession(t, false)
	h := start(t, s, Options{})
	sync := flagOf(t, h.m.answers[0], syncSpan)
	p := openChecks(t, h)
	at(t, h, syncSpan)
	h.press("m", "v", "enter")
	want := engine.EvidenceRange{Name: sync.Closest.Name, From: sync.Closest.Line, To: sync.Closest.Line}
	judge(h, "x")
	got := h.m.answers[0].verdicts[syncSpan]
	if p.view != nil || got.Kind != engine.VerdictIncorrect || len(got.Evidence) != 1 || got.Evidence[0].Place() != want.Place() {
		t.Errorf("x on the open item: view %v, kept %+v; want incorrect resting on %s, back in the tab", p.view != nil, got, want.Place())
	}
}

// TestChecksAnyItem: [E4] e opens the evidence list; an item opened from
// it is marked for the value, esc returns to the list and again to the
// same value, which keeps the lines marked in that item beside those of
// the evidence shown.
func TestChecksAnyItem(t *testing.T) {
	s := spanSession(t, false)
	h := start(t, s, Options{})
	chassis := flagOf(t, h.m.answers[0], chassisSpan)
	p := openChecks(t, h)
	at(t, h, chassisSpan)
	cur := p.cur
	h.press("m", "v", "enter", "esc")
	h.press("e")
	if !p.listing {
		t.Fatalf("e opened no list:\n%s", h.view())
	}
	b := h.m.browser
	other := slices.IndexFunc(b.shown(), func(l listed) bool { return l.name != chassis.Closest.Name })
	if other < 0 {
		t.Fatalf("no other item to open: %+v", b.shown())
	}
	for b.cur < other {
		h.press("down")
	}
	name := b.shown()[other].name
	h.press("enter")
	if p.view == nil || p.view.name != name {
		t.Fatalf("enter on the list opened %+v; want %s", p.view, name)
	}
	h.press("v", "enter", "esc")
	if p.view != nil || !p.listing {
		t.Errorf("esc in the item did not return to the list")
	}
	h.press("esc")
	if p.listing || p.cur != cur || h.m.browser == nil {
		t.Fatalf("esc on the list did not return to the value: listing %v, at %d, want %d", p.listing, p.cur, cur)
	}
	want := []engine.EvidenceRange{{Name: chassis.Closest.Name, From: chassis.Closest.Line, To: chassis.Closest.Line}, {Name: name, From: 1, To: 1}}
	if d := cmp.Diff(want, p.values[p.cur].marks); d != "" {
		t.Errorf("the value's marks (-want +got):\n%s", d)
	}
	judge(h, "i")
	if got := h.m.answers[0].verdicts[chassisSpan]; got.Kind != engine.VerdictInferred || places(got.Evidence) != places(want) {
		t.Errorf("i kept %+v; want inferred from %s", got, places(want))
	}
}

// TestChecksSummary: [E4] nothing is printed while the engineer judges;
// esc back to the chat prints one line for the answer judged, how many of
// each kind, and that the model reads the incorrect and unsupported, which
// are kept as messages worded as the engineer's, the supported and
// inferred not; reopened, the line beneath the answer says how many are
// judged, nothing is offered, and the tab shows each verdict.
func TestChecksSummary(t *testing.T) {
	s := spanSession(t, false)
	h := start(t, s, Options{})
	p := openChecks(t, h)
	mark := len(h.printed)
	at(t, h, syncSpan)
	judge(h, "y")
	at(t, h, chassisSpan)
	h.press("m", "v", "enter", "esc")
	judge(h, "i")
	at(t, h, sumSpan)
	judge(h, "x")
	at(t, h, noneSpan)
	judge(h, "u")
	if out := h.since(mark); out != "" || p.judging {
		t.Errorf("judging printed %q", out)
	}
	h.press("esc")
	h.settle()
	want := "a1: 1 supported, 1 inferred, 1 incorrect, 1 unsupported; the model reads the incorrect and unsupported at its next call"
	if out := flat(h.since(mark)); strings.Count(out, "a1:") != 1 || !strings.Contains(out, want) || h.m.browser != nil {
		t.Errorf("esc printed %q; want %q once", out, want)
	}
	wantTold := []string{"The engineer checked a1's `" + sumSpan + "`: it is incorrect.",
		"The engineer found no evidence for a1's `" + noneSpan + "`: treat it as unsupported."}
	if got := verdictMessages(t, s); !slices.Equal(got, wantTold) {
		t.Errorf("the verdicts the model reads %q; want %q", got, wantTold)
	}
	again := len(h.printed)
	h.say("/evidence")
	h.until("the browser opens", func() bool { return h.m.browser != nil })
	h.press("esc")
	h.settle()
	if out := h.since(again); strings.Contains(out, "a1:") {
		t.Errorf("a browser closed with nothing judged printed a summary again:\n%s", out)
	}

	r := start(t, s, Options{NoOpen: true})
	if got, want := findingLines(r.out()), []string{"⚠ 4 findings, 4 judged · /check a1 opens them"}; !slices.Equal(got, want) {
		t.Errorf("the reopened chat shows the finding lines %q; want %q:\n%s", got, want, r.out())
	}
	if strings.Contains(flat(r.out()), "f opens the checks tab") {
		t.Errorf("the reopened chat offers the check:\n%s", r.out())
	}
	checkAt(t, r, "a1")
	view := r.view()
	for _, want := range []string{"✓ a1 `" + syncSpan + "`", "~ a1 `" + chassisSpan + "`", "✗ a1 `" + sumSpan + "`", "⚠ a1 `" + noneSpan + "`"} {
		if !strings.Contains(view, want) {
			t.Errorf("the reopened tab shows no %q:\n%s", want, view)
		}
	}
	for span, want := range map[string]string{syncSpan: "Verdict: ✓ supported, by the engineer against", chassisSpan: "Verdict: ~ inferred, by the engineer against",
		sumSpan: "Verdict: ✗ incorrect, by the engineer; the model is told", noneSpan: "Verdict: ⚠ unsupported, by the engineer; the model is told"} {
		at(t, r, span)
		if v := flat(r.view()); !strings.Contains(v, want) {
			t.Errorf("the reopened tab at %q lacks %q:\n%s", span, want, r.view())
		}
	}
}

// TestSpanCheckOfferEndsOnOtherInput: [E4] any key but f as the first
// ends the offer, so f in a message is a letter; after a draft, the review
// and the checks tab are offered together, and f takes the tab, opening
// it at the draft's value.
func TestSpanCheckOfferEndsOnOtherInput(t *testing.T) {
	h := start(t, spanSession(t, false), Options{})
	h.press("x", "backspace", "f")
	h.settle()
	if h.m.browser != nil || h.m.pending != nil || h.m.input.Value() != "f" {
		t.Errorf("f after another key: the input %q, the browser %v", h.m.input.Value(), h.m.browser != nil)
	}

	d := start(t, spanSession(t, true), Options{})
	d.press("esc")
	mark := len(d.printed)
	d.say("/draft customer-update")
	out := flat(d.since(mark))
	if offer, review := strings.Index(out, "⚠ 1 finding to review · f opens the checks tab"), strings.Index(out, "r reviews it with "); offer < 0 || review < offer ||
		len(findingLines(d.since(mark))) != 1 {
		t.Fatalf("the draft's offers:\n%s", out)
	}
	p := openChecks(t, d)
	if v := p.values[p.cur]; v.label != d.m.answers[len(d.m.answers)-1].label || v.flag.Span != "error syncing node worker-7" || d.m.turn != nil {
		t.Errorf("f after a draft opened the tab at %+v", v)
	}
}

// TestChecksMarkLimitAndDrop: [E4] marks past the ranges a verdict may
// rest on are refused, saying so, and d drops the last mark, on the item
// open and in the tab, so the value stays judgeable on as many as allowed.
func TestChecksMarkLimitAndDrop(t *testing.T) {
	s := spanSession(t, false)
	h := start(t, s, Options{})
	p := openChecks(t, h)
	at(t, h, chassisSpan)
	h.press("e")
	b := h.m.browser
	long := slices.IndexFunc(b.shown(), func(l listed) bool {
		it, err := s.EvidenceNamed(l.name)
		return err == nil && len(contentLines(string(it.Content))) > engine.VerdictRanges
	})
	if long < 0 {
		t.Fatalf("no item of more than %d lines: %+v", engine.VerdictRanges, b.shown())
	}
	for b.cur < long {
		h.press("down")
	}
	h.press("enter")
	for i := range engine.VerdictRanges + 1 {
		if i > 0 {
			h.press("down")
		}
		h.press("v", "enter")
	}
	v := &p.values[p.cur]
	if len(v.marks) != engine.VerdictRanges || !strings.Contains(b.note, "at most "+strconv.Itoa(engine.VerdictRanges)+" ranges") {
		t.Fatalf("%d marks, the note %q; want %d and the limit said", len(v.marks), b.note, engine.VerdictRanges)
	}
	h.press("d")
	if len(v.marks) != engine.VerdictRanges-1 || !strings.Contains(b.note, "dropped") {
		t.Errorf("d on the item: %d marks, the note %q", len(v.marks), b.note)
	}
	h.press("esc", "esc", "d")
	if _, keys := h.m.checksView(100, 28); p.view != nil || p.listing || len(v.marks) != engine.VerdictRanges-2 || !strings.Contains(keys, "d unmarks") {
		t.Errorf("d in the tab: %d marks, the keys %q:\n%s", len(v.marks), keys, h.view())
	}
	// m opens the item at the first mark, line 1; lines 7 and 8 again.
	h.press("m")
	for range engine.VerdictRanges - 2 {
		h.press("down")
	}
	h.press("v", "enter", "down", "v", "enter", "esc")
	judge(h, "x")
	if got := h.m.answers[0].verdicts[chassisSpan]; got.Kind != engine.VerdictIncorrect || len(got.Evidence) != engine.VerdictRanges {
		t.Errorf("x on %d marks kept %+v; note %q", engine.VerdictRanges, got, b.note)
	}
}

// TestChecksEscWhileJudging: [E4] esc while a verdict is being kept waits
// for it, so the summary, printed once, counts it.
func TestChecksEscWhileJudging(t *testing.T) {
	h := start(t, spanSession(t, false), Options{})
	openChecks(t, h)
	mark := len(h.printed)
	at(t, h, syncSpan)
	judge(h, "y")
	at(t, h, noneSpan)
	h.press("u", "esc")
	h.settle()
	if h.m.browser == nil {
		t.Fatalf("esc while judging closed the browser:\n%s", h.since(mark))
	}
	h.press("esc")
	h.settle()
	want := "a1: 1 supported, 1 unsupported; the model reads the unsupported at its next call"
	if out := flat(h.since(mark)); strings.Count(out, "a1:") != 1 || !strings.Contains(out, want) {
		t.Errorf("the summary %q; want %q once", out, want)
	}
}

package chat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// The reviewer's chat tests: the companion answers from the synthetic
// testdata/edited.json, with no editor, and the reviewer, on a model of
// its own, from testdata/reviewer.json, its findings synthetic.

// reviewedSession opens the stub case on an engine whose companion
// answers from edited.json and whose reviewer from reviewer.json; it
// returns the reviewer's model as the chat names it.
func reviewedSession(t *testing.T) (*engine.Session, string) {
	t.Helper()
	reviewer, err := filepath.Abs(filepath.Join("testdata", "reviewer.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(chatConfig(t, "edited.json"), "  profiles:\n",
		"    reviewer-model: {provider: fixture, model: "+reviewer+"}\n  profiles:\n    reviewer: {model: reviewer-model}\n", 1)
	s, err := enginetest.EngineWith(t, cfg).StartSession(t.Context(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, reviewer
}

// draftReview is the review of the draft a2 as printed.
var draftReview = []string{
	`⚠ s1 certainty · "the investigation confirms the OVN controller on worker-1 cannot sync" · e1:2 shows one sync error on worker-1, not a cause (e1:2)`,
	"alternative: a transient API outage during the sync · discriminator: the controller's log after the error, not given",
	"at most: the OVN controller on worker-1 reported a sync error",
	"review of a2 · 1 statement: 0 supported, 1 finding, 0 skipped · ",
}

// unwrapped is s with every space gone, so a check of a long path holds
// wherever the chat wrapped it.
func unwrapped(s string) string { return strings.Join(strings.Fields(s), "") }

// TestLeadingRTypesNormallyAfterDraft: [E3] a draft prints no single-key r
// offer, and typing a message beginning with r on the empty input after a
// draft types into the input without calling the reviewer; /review runs only
// when asked explicitly.
func TestLeadingRTypesNormallyAfterDraft(t *testing.T) {
	s, _ := reviewedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("fail please")
	h.say("/draft customer-update")
	if out := flat(h.out()); !strings.Contains(out, "kept as i1") || strings.Contains(out, "r reviews it with") {
		t.Fatalf("/draft printed:\n%s", out)
	}
	was := calls(t, s)
	h.typed("read the controller log")
	if h.m.input.Value() != "read the controller log" || h.m.turn != nil || calls(t, s) != was {
		t.Errorf("typing r-message after draft: input %q, turn %v, calls %d (was %d)", h.m.input.Value(), h.m.turn != nil, calls(t, s), was)
	}
}

// TestReviewCommand: [E3] /review reviews the last answer and /review a2
// the draft, each printed beneath with its findings and footer,
// neither an answer: the next answer is a3, after a1 and the draft. The
// turns tab lists each review by what it is of, and a reopened chat
// prints them the same, in place.
func TestReviewCommand(t *testing.T) {
	s, reviewer := reviewedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("fail please")
	h.typed("/review")
	h.press("enter")
	if h.m.turn == nil || h.m.turn.status != "the reviewer is reviewing the last answer" {
		t.Fatal("/review began no review")
	}
	h.settle()
	h.say("/draft customer-update")
	h.say("/review a2")
	h.say("anchor please")
	answerReview := []string{
		`⚠ s1 overstated · "logs nothing new" · e1:2 is one line of the log, not the whole of it (e1:2)`,
		"alternative: new errors past the lines given · discriminator: the controller's whole log since, not given",
		"at most: the lines given show no new error",
		"review of a1 · 1 statement: 0 supported, 1 finding, 0 skipped · ",
	}
	reopened := start(t, s, Options{NoOpen: true})
	for _, run := range []struct {
		name string
		h    *harness
	}{{"the chat", h}, {"the reopened chat", reopened}} {
		out := flat(run.h.out())
		block := strings.Join(answerReview, " ")
		first, draft, second := strings.Index(out, "The OVN controller logs nothing new"), strings.Index(out, "Hello, the investigation"), strings.Index(out, "The operator restarted ovnkube-node")
		at, atDraft := strings.Index(out, block), strings.Index(out, strings.Join(draftReview, " "))
		if at < first || draft < at || atDraft < draft || second < atDraft {
			t.Errorf("%s does not print each review beneath what it reviewed, in order:\n%s", run.name, out)
		}
		if strings.Count(out, "a1 ·")-strings.Count(out, "review of a1 ·") != 1 || strings.Count(out, "a3 ·") != 1 || strings.Contains(out, "a4 ·") {
			t.Errorf("%s numbers a review as an answer:\n%s", run.name, out)
		}
		if !strings.Contains(unwrapped(out), unwrapped("skipped · "+reviewer)) {
			t.Errorf("%s does not name the reviewer's model:\n%s", run.name, out)
		}
	}
	v := reopened.turnsTab()
	if a1, r1, ra2 := strings.Index(v, "  a1 · "), strings.Index(v, "  review of a1 · "), strings.Index(v, "  review of a2 · "); a1 < 0 || r1 < a1 || ra2 < r1 {
		t.Errorf("the turns tab:\n%s", v)
	}
}

// TestReviewRanked: /review --ranked reviews the last answer and /review
// --ranked a2 the draft, each given the evidence used most and not cited,
// the status bar saying so, the scripted reviewer answering a request
// that carries it; /review alone does not carry it, and arguments other
// than --ranked and one target are refused with the command's form.
func TestReviewRanked(t *testing.T) {
	s, _ := reviewedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("fail please")
	h.typed("/review --ranked")
	h.press("enter")
	if h.m.turn == nil || h.m.turn.status != "the reviewer is reviewing the last answer, given the evidence used most and not cited" {
		t.Fatal("/review --ranked began no review given the ranked evidence")
	}
	h.settle()
	ranked := `⚠ s1 undetermined · "OVN controller" · e1:2 is one line, and no evidence the text does not cite settles it (e1:2)`
	if out := flat(h.out()); !strings.Contains(out, ranked) || !strings.Contains(out, "review of a1 · 1 statement: 0 supported, 1 finding, 0 skipped · ") {
		t.Errorf("/review --ranked printed:\n%s", out)
	}
	h.say("/review")
	if out := flat(h.out()); strings.Count(out, ranked) != 1 || !strings.Contains(out, `⚠ s1 overstated · "logs nothing new"`) {
		t.Errorf("/review after /review --ranked printed:\n%s", out)
	}
	h.say("/draft customer-update")
	h.typed("/review a2 --ranked")
	h.press("enter")
	if h.m.turn == nil || h.m.turn.status != "the reviewer is reviewing a2, given the evidence used most and not cited" {
		t.Fatal("/review a2 --ranked began no review given the ranked evidence")
	}
	h.settle()
	if out := flat(h.out()); !strings.Contains(out, "review of a2 · 1 statement: 0 supported, 1 finding, 0 skipped · ") || strings.Count(out, ranked) != 2 {
		t.Errorf("/review a2 --ranked printed:\n%s", out)
	}
	was := calls(t, s)
	for _, bad := range []string{"/review --rank", "/review a1 a2"} {
		h.say(bad)
		if out := h.out(); !strings.Contains(out, "✗ /review takes [--ranked] [aN]") || h.m.turn != nil {
			t.Errorf("%s was not refused:\n%s", bad, out)
		}
	}
	if calls(t, s) != was {
		t.Errorf("a refused /review made %d calls", calls(t, s)-was)
	}
}

// TestReviewRefusedWhileTurnRuns: [E3] /review waits for the turn running,
// said so, and stays in the input.
func TestReviewRefusedWhileTurnRuns(t *testing.T) {
	s, _ := reviewedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("fail please")
	h.typed("wording please")
	h.press("enter")
	h.typed("/review")
	h.press("enter")
	h.settle()
	if out := h.out(); !strings.Contains(out, "✗ /review waits for the turn running") || h.m.input.Value() != "/review" {
		t.Errorf("/review during a turn: the input %q\n%s", h.m.input.Value(), out)
	}
	if strings.Contains(flat(h.out()), "review of") {
		t.Errorf("a review ran:\n%s", h.out())
	}
}

// TestReviewNotConfigured: [E3] with no reviewer profile a draft is
// followed by no offer, r is typed, and /review says none is configured.
func TestReviewNotConfigured(t *testing.T) {
	h := start(t, session(t, "edited.json"), Options{NoOpen: true})
	h.say("/draft customer-update")
	h.press("r")
	if out := flat(h.out()); strings.Contains(out, "reviews it with") || h.m.input.Value() != "r" || h.m.turn != nil {
		t.Errorf("with no reviewer: the input %q\n%s", h.m.input.Value(), out)
	}
	h.press("backspace")
	h.say("/review")
	if out := h.out(); !strings.Contains(out, "✗ no reviewer is configured (reasoning.profiles.reviewer)") {
		t.Errorf("/review with no reviewer:\n%s", out)
	}
}

// TestReviewFailureShown: [E3] a review that fails is said, and nothing
// is kept of it in the conversation: neither a reopened chat nor the
// turns tab shows a review.
func TestReviewFailureShown(t *testing.T) {
	s, _ := reviewedSession(t)
	h := start(t, s, Options{NoOpen: true})
	h.say("anchor please")
	h.say("/review")
	if out := flat(h.out()); !strings.Contains(out, "✗ the reviewer failed: ") || strings.Contains(out, "review of a1") {
		t.Errorf("a failed review:\n%s", out)
	}
	r := start(t, s, Options{NoOpen: true})
	if out := flat(r.out()); strings.Contains(out, "review of") || strings.Contains(out, "⚠") {
		t.Errorf("the reopened chat shows a failed review:\n%s", out)
	}
	if v := r.turnsTab(); strings.Contains(v, "review") {
		t.Errorf("the turns tab lists a failed review:\n%s", v)
	}
}

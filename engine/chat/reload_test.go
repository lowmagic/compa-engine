package chat

import (
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
)

// calls is how many model calls the case has made, as stored.
func calls(t *testing.T, s *engine.Session) int {
	t.Helper()
	u, err := s.CompanionTotalUsage()
	if err != nil {
		t.Fatal(err)
	}
	return u.Invocations
}

// expired is a reopened case's state whose cache has expired on a large
// conversation, with a new comment, as the opening would offer it.
var expired = engine.ReopenState{NewComments: 1, CacheExpired: true, Large: true, Estimate: 45_000}

// TestReopenChoicePanel: [C11] the choices a reopened case offers show in
// a panel above the status bar and never in the transcript; while it is
// open its keys are its own and nothing calls a model; 2 starts the
// suggested context with no call, then the catch-up is offered, and c,
// finding nothing new, says so with no call. Reopened again, the history
// still shows the earlier answers, and a message goes on in the new
// conversation.
func TestReopenChoicePanel(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	h := start(t, s, Options{NoOpen: true})
	before, transcript := calls(t, s), len(h.printed)
	h.m.offerReopen(expired, engine.ReloadOffer{Estimate: 45_000, Ranked: 3})
	h.update(nil)
	view := h.view()
	for _, want := range []string{"› 1  full context — resend about 45,000 tokens without the cache",
		"2  suggested context — the case's state, the ledger, 3 ranked items and the last exchange",
		"3  suggested context and a summary — one model call over the earlier conversation", "esc decide later (/reload)"} {
		if !strings.Contains(view, want) {
			t.Errorf("the panel lacks %q:\n%s", want, view)
		}
	}
	h.typed("hello")
	h.press("down")
	if h.m.input.Value() != "" || !strings.Contains(h.view(), "› 2  suggested context") {
		t.Fatalf("the panel let keys through or did not select:\n%s", h.view())
	}
	h.press("up")
	if !strings.Contains(h.view(), "› 1  full context") {
		t.Fatalf("up did not select:\n%s", h.view())
	}
	if calls(t, s) != before || h.m.turn != nil {
		t.Fatal("the panel called a model before a choice")
	}
	h.press("2")
	h.settle()
	out := flat(h.since(transcript))
	if !strings.Contains(out, "suggested context: a new conversation begun from the case as it is now, the ledger, ") || calls(t, s) != before {
		t.Fatalf("2 printed %q, %d calls", out, calls(t, s)-before)
	}
	if !strings.Contains(h.view(), "c  catch up on what's new (1 comment, 0 attachments)") {
		t.Fatalf("no catch-up offered after the choice:\n%s", h.view())
	}
	h.press("c")
	h.settle()
	out = flat(h.since(transcript))
	if !strings.Contains(out, "nothing new since you last worked this case") || calls(t, s) != before {
		t.Errorf("c with nothing new printed %q, %d calls", out, calls(t, s)-before)
	}
	if strings.Contains(out, "full context —") || strings.Contains(out, "catch up on what's new") || strings.Contains(h.view(), "catch up on") {
		t.Errorf("the panel's rows entered the transcript or stayed:\n%s\n%s", out, h.view())
	}

	again := start(t, s, Options{NoOpen: true})
	if out := flat(again.out()); !strings.Contains(out, "The OVN controller cannot sync worker-1") {
		t.Errorf("after the reload the history lost the earlier answer:\n%s", out)
	}
	again.say("which model?")
	history, err := s.ConversationHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, last := history[0].ConversationID, history[len(history)-1].ConversationID
	if first == last || history[len(history)-1].Text != "The first model answers." {
		t.Errorf("the message did not go on in the new conversation: %s, %s", first, last)
	}
}

// TestReopenChoicePanelLater: esc puts the reload off, saying /reload,
// and offers the catch-up; esc skips that, saying /catchup; no call is
// made, the panel's rows never enter the transcript, and its area is
// cleared once it closes.
func TestReopenChoicePanelLater(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	h := start(t, s, Options{NoOpen: true})
	before, transcript := calls(t, s), len(h.printed)
	r := expired
	r.NewAttachments = []string{"0030-synthetic"}
	h.m.offerReopen(r, engine.ReloadOffer{Estimate: 45_000, Ranked: 3})
	h.update(nil)
	height := strings.Count(h.view(), "\n")
	h.press("esc")
	if view := h.view(); !strings.Contains(view, "catch up on what's new (1 comment, 1 attachment)") || strings.Count(view, "\n") != height {
		t.Fatalf("esc did not offer the catch-up in the panel's area:\n%s", view)
	}
	h.settle()
	h.press("esc")
	h.settle()
	out := flat(h.since(transcript))
	for _, want := range []string{"decided later: /reload offers the three again", "skipped: /catchup catches up later"} {
		if !strings.Contains(out, want) {
			t.Errorf("the chat printed no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "full context") || strings.Contains(out, "catch up on what's new") {
		t.Errorf("the panel's rows entered the transcript:\n%s", out)
	}
	if strings.Count(h.view(), "\n") >= height || h.m.choice != nil {
		t.Errorf("the closed panel left its area:\n%s", h.view())
	}
	if calls(t, s) != before {
		t.Error("putting the choices off called a model")
	}
}

// TestReopenChoiceSummary: [C11] 3 makes one model call, the handoff,
// which prints as its answer, and the conversation goes on in a new one.
func TestReopenChoiceSummary(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	h := start(t, s, Options{NoOpen: true})
	before, transcript := calls(t, s), len(h.printed)
	h.m.offerReopen(engine.ReopenState{CacheExpired: true, Large: true}, engine.ReloadOffer{Estimate: 45_000})
	h.update(nil)
	h.press("3")
	h.settle()
	out := flat(h.since(transcript))
	if calls(t, s) != before+1 || !strings.Contains(out, "A synthetic handoff: the OVN controller cannot sync worker-1") ||
		!strings.Contains(out, "a handoff: the conversation goes on in a new one, begun from the case as it is now") {
		t.Errorf("3 made %d calls and printed:\n%s", calls(t, s)-before, out)
	}
	if h.m.choice != nil {
		t.Error("a catch-up was offered with nothing new")
	}
}

// TestReloadCommands: [C11] /reload lists the three ways on with the
// estimate, /reload full keeps the conversation, an unknown way is
// refused, /ranked lists the evidence ranked, /catchup with nothing new
// says so; none calls a model.
func TestReloadCommands(t *testing.T) {
	s := session(t, "case.json")
	h := start(t, s, Options{})
	before, transcript := calls(t, s), len(h.printed)
	for _, c := range []string{"/reload", "/reload full", "/reload other", "/ranked", "/catchup"} {
		h.say(c)
	}
	out := flat(h.since(transcript))
	for _, want := range []string{
		"/reload full — the conversation as it is: its next call resends about ",
		"/reload suggested — a new conversation from the case's state, the ledger, ",
		"/reload summary — the same and a handoff the model writes",
		"full context: the conversation goes on as it is; its next call resends about ",
		"/reload takes full, suggested or summary",
		"· returned by grep · ",
		"nothing new since you last worked this case",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the commands printed no %q:\n%s", want, out)
		}
	}
	if calls(t, s) != before {
		t.Errorf("the commands made %d model calls", calls(t, s)-before)
	}
}

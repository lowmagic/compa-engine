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

// TestReopenIsQuiet: [C11] a reopened case says what is new and offers
// nothing to choose: no model is called and the keys go to the input.
// Where the case gained comments and the cache has expired on a large
// conversation, its lines say the next message gives the news to the
// conversation, /ledger reads the comments, what the next message
// resends, and that /compact goes on in a new conversation instead.
func TestReopenIsQuiet(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	before := calls(t, s)
	h := start(t, s, Options{})
	if out := flat(h.out()); !strings.Contains(out, "nothing new since you last worked this case") {
		t.Errorf("the reopened case printed:\n%s", out)
	}
	h.typed("2")
	if got := h.m.input.Value(); got != "2" || h.m.turn != nil || calls(t, s) != before {
		t.Errorf("the reopened case took the key, %q in the input, or called a model (%d calls)", got, calls(t, s)-before)
	}
	lines := strings.Join(reopenLines(engine.ReopenState{NewComments: 1, CacheExpired: true, Large: true, Estimate: 45_000}), "\n")
	for _, want := range []string{
		"1 new comment; your next message gives them to the conversation, and /ledger reads the comments",
		"your next message resends the conversation, about 45,000 tokens, without it; /compact goes on in a new one from the case's state and a handoff",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("the reopening's lines lack %q:\n%s", want, lines)
		}
	}
}

// TestCompactInCase: [C11] /compact in a case makes one model call, the
// handoff, which prints as its answer, and the conversation goes on in a
// new one begun from the case as it is now; reopened, the history still
// shows the earlier answers.
func TestCompactInCase(t *testing.T) {
	s := session(t, "case.json")
	start(t, s, Options{})
	h := start(t, s, Options{NoOpen: true})
	before, transcript := calls(t, s), len(h.printed)
	h.say("/compact")
	out := flat(h.since(transcript))
	if calls(t, s) != before+1 || !strings.Contains(out, "A synthetic handoff: the OVN controller cannot sync worker-1") ||
		!strings.Contains(out, "a handoff: the conversation goes on in a new one, begun from the case as it is now") {
		t.Errorf("/compact made %d calls and printed:\n%s", calls(t, s)-before, out)
	}
	again := start(t, s, Options{NoOpen: true})
	if out := flat(again.out()); !strings.Contains(out, "The OVN controller cannot sync worker-1") {
		t.Errorf("after /compact the history lost the earlier answer:\n%s", out)
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

// TestDebugCommands: /debug lists its arguments and says whether it is
// on; /debug ranked lists the evidence ranked, /debug usage the usage,
// and an unknown argument or a load naming no expert is refused; none
// calls a model.
func TestDebugCommands(t *testing.T) {
	s := session(t, "case.json")
	h := start(t, s, Options{})
	before, transcript := calls(t, s), len(h.printed)
	for _, c := range []string{"/debug", "/debug ranked", "/debug usage", "/debug other", "/debug load"} {
		h.say(c)
	}
	out := flat(h.since(transcript))
	for _, want := range []string{
		"debug is off; /debug takes one of:",
		"load <expert> — load an installed expert into the conversation",
		"· returned by grep · ",
		"total: ",
		"/debug takes on, off, usage, experts, load <expert> or ranked",
		"/debug load names an expert; /debug experts lists them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the commands printed no %q:\n%s", want, out)
		}
	}
	if calls(t, s) != before || h.m.debug {
		t.Errorf("the commands made %d model calls, or turned debug on", calls(t, s)-before)
	}
}

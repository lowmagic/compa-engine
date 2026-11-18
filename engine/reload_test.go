package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// Reloading a conversation on the companion's stub case, whose comments
// and items are synthetic.

// reloadSession is the stub case whose companion answers from a queue and
// whose ledger reads its comments as the fixture ledger model does.
func reloadSession(t *testing.T) (*Session, *modelfixture.Provider, *rankedQueue) {
	t.Helper()
	q := &rankedQueue{}
	led := updating(reader)
	s, f := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isLedgerCall(r) {
			return led(ctx, r)
		}
		return q.respond(ctx, r)
	})
	return s, f, q
}

// numberedItem is a synthetic item the engineer brought in of n lines,
// each saying its number, named.
func numberedItem(t *testing.T, s *Session, location string, n int, width int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		line := fmt.Sprintf("synthetic line %d", i)
		if width > len(line) {
			line += strings.Repeat(".", width-len(line))
		}
		b.WriteString(line + "\n")
	}
	it, _, err := s.Ingest(strings.NewReader(b.String()), location, "")
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.NameEvidence(it.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func latestConversation(t *testing.T, s *Session) companion.Conversation {
	t.Helper()
	conv, err := companion.Store{DB: s.c.st.DB()}.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

func conversationMessages(t *testing.T, s *Session, id string) []companion.Message {
	t.Helper()
	msgs, err := companion.Store{DB: s.c.st.DB()}.Messages(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// TestReloadSuggested: [C11] a suggested reload makes no model call and
// starts a main conversation linked to the old one as a compaction's is,
// holding the case block as it is now, the whole ledger, and what the
// engine carries: the pinned item, the ranked items best first with the
// lines pointed at or cited numbered as stored, an item cited whole by its
// first lines, and the last exchange verbatim; the same inputs carry the
// same bytes; the next turn continues in it, and the history still holds
// the earlier conversation's answers.
func TestReloadSuggested(t *testing.T) {
	s, f, q := reloadSession(t)
	ctx := t.Context()
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	pointed, cited, whole := numberedItem(t, s, "pointed.txt", 30, 0), numberedItem(t, s, "cited.txt", 30, 0), numberedItem(t, s, "whole.txt", 40, 0)
	q.add(modelfixture.Text("Those lines show it, with [" + cited + ":7-8] and [" + whole + "]."))
	rankedTalk(t, s, pointsAt+pointed+":3-4\n\nWhat do these lines mean?")
	if _, err := s.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	q.add(modelfixture.Text("The last answer, synthetic."))
	rankedTalk(t, s, "And what now?")
	old := latestConversation(t, s).ID
	calls := len(f.Requests())

	r, err := s.Reload(ctx, ReloadSuggested, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.Requests()); n != calls {
		t.Fatalf("a suggested reload made %d model calls", n-calls)
	}
	conv := latestConversation(t, s)
	if r.Conversation == old || conv.ID != r.Conversation || conv.CompactedFrom != old || conv.Kind != "main" || r.Ranked != 3 || r.Estimate <= 0 {
		t.Fatalf("reloaded %+v into %+v, from %s", r, conv, old)
	}
	msgs := conversationMessages(t, s, r.Conversation)
	block, _, err := caseBlock(s.c, s.inventory(), s.e.config.SupportName())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Kind != companion.KindCase || msgs[0].Text != block ||
		msgs[1].Kind != companion.KindUpdate || !strings.HasPrefix(msgs[1].Text, ledgerHeading+"\n") ||
		msgs[2].Kind != companion.KindHandoff || !strings.HasPrefix(msgs[2].Text, carriedHeading+"\n") {
		t.Fatalf("the new conversation begins %+v", msgs)
	}
	carry := msgs[2].Text
	for _, want := range []string{
		"## i1 · pinned answer",
		"## " + pointed + " · ingested · pointed at by the engineer\n\n3  synthetic line 3\n4  synthetic line 4\n",
		"## " + cited + " · ingested · cited in i1\n\n7  synthetic line 7\n8  synthetic line 8\n",
		"## " + whole + " · ingested · cited in i1\n\nIts first lines, of 40 lines.\n 1  synthetic line 1\n",
		"20  synthetic line 20\n",
		"# The last exchange",
		"## The engineer\n\nAnd what now?",
		"## The model\n\nThe last answer, synthetic.",
	} {
		if !strings.Contains(carry, want) {
			t.Errorf("what is carried lacks %q:\n%s", want, carry)
		}
	}
	if strings.Contains(carry, "21  synthetic line 21") || strings.Contains(carry, "\n5  synthetic line 5") || strings.Contains(carry, "\n9  synthetic line 9") {
		t.Errorf("what is carried holds lines beyond those named or the first:\n%s", carry)
	}
	pointedAt, citedAt, wholeAt := strings.Index(carry, "## "+pointed+" ·"), strings.Index(carry, "## "+cited+" ·"), strings.Index(carry, "## "+whole+" ·")
	if pointedAt >= citedAt || citedAt >= wholeAt {
		t.Errorf("the ranked items are not best first:\n%s", carry)
	}

	again, err := s.Reload(ctx, ReloadSuggested, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := conversationMessages(t, s, again.Conversation)[2].Text; got != carry {
		t.Errorf("the same inputs carried other bytes:\n%s\n---\n%s", carry, got)
	}

	q.add(modelfixture.Text("Continued."))
	reply, err := s.Talk(ctx, "Go on.", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != calls+1 || reply.Conversation != again.Conversation {
		t.Fatalf("%d calls after the reload, the turn in %s; want 1, in %s", len(reqs)-calls, reply.Conversation, again.Conversation)
	}
	last := reqs[len(reqs)-1]
	if last.Messages[0].Text != block || messageStarting(last, carriedHeading) != 2 {
		t.Errorf("the next turn's call:\n%s", texts(last))
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var earlier bool
	for _, m := range history {
		earlier = earlier || (m.Kind == MessageModel && m.Text == "The last answer, synthetic." && m.ConversationID == old)
	}
	if !earlier {
		t.Error("the history lost the earlier conversation's answer")
	}
}

// TestReloadSummary: [C11] a summary reload makes exactly one model call,
// the handoff over the old conversation, and begins the new one with the
// handoff, said to be a model's, then what a suggested reload carries,
// whose last exchange is the engineer's, not the handoff's.
func TestReloadSummary(t *testing.T) {
	s, f, q := reloadSession(t)
	ctx := t.Context()
	q.add(modelfixture.Text("The DNS pods restart."))
	rankedTalk(t, s, "What is wrong?")
	old := latestConversation(t, s).ID
	calls := len(f.Requests())
	q.add(modelfixture.Text("Handoff: the DNS pods restart."))
	r, err := s.Reload(ctx, ReloadSummary, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != calls+1 || !strings.Contains(reqs[len(reqs)-1].Messages[len(reqs[len(reqs)-1].Messages)-1].Text, "Write a handoff") {
		t.Fatalf("a summary reload made %d calls, want the handoff's", len(reqs)-calls)
	}
	if r.Handoff != "Handoff: the DNS pods restart." || latestConversation(t, s).CompactedFrom != old {
		t.Fatalf("reloaded %+v", r)
	}
	msgs := conversationMessages(t, s, r.Conversation)
	text := msgs[len(msgs)-1].Text
	if msgs[len(msgs)-1].Kind != companion.KindHandoff || !strings.HasPrefix(text, companion.HandoffText(r.Handoff)) ||
		strings.Index(text, r.Handoff) > strings.Index(text, carriedHeading) {
		t.Fatalf("the new conversation ends %+v", msgs[len(msgs)-1])
	}
	if !strings.Contains(text, "## The engineer\n\nWhat is wrong?") || !strings.Contains(text, "## The model\n\nThe DNS pods restart.") ||
		strings.Contains(text, "Write a handoff") {
		t.Errorf("the summary carried another exchange:\n%s", text)
	}
}

// TestReloadFull: [C11] a full reload changes nothing and calls no model:
// the conversation stays the latest, and its estimate is what its next
// call resends.
func TestReloadFull(t *testing.T) {
	s, f, _ := reloadSession(t)
	ctx := t.Context()
	rankedTalk(t, s, "Where does it stand?")
	conv := latestConversation(t, s)
	n, calls := len(conversationMessages(t, s, conv.ID)), len(f.Requests())
	r, err := s.Reload(ctx, ReloadFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.ReopenState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Conversation != conv.ID || latestConversation(t, s).ID != conv.ID || len(conversationMessages(t, s, conv.ID)) != n ||
		len(f.Requests()) != calls || r.Estimate != state.Estimate {
		t.Errorf("a full reload: %+v, estimate %d", r, state.Estimate)
	}
	if _, err := s.Reload(ctx, "other", nil); err == nil {
		t.Error("an unknown reload was taken")
	}
	offer, err := s.ReloadOffer(ctx)
	if err != nil || offer.Estimate != state.Estimate || len(f.Requests()) != calls {
		t.Errorf("the offer %+v, %v", offer, err)
	}
}

// TestReloadCarriesBounded: a restart carries at most 12 ranked items, and
// no more than 24 KiB of their lines, the item reaching the bound cut and
// said so.
func TestReloadCarriesBounded(t *testing.T) {
	s, _, q := reloadSession(t)
	ctx := t.Context()
	var cites []string
	for i := range 14 {
		cites = append(cites, "["+numberedItem(t, s, fmt.Sprintf("item-%d.txt", i), 5, 0)+":2]")
	}
	q.add(modelfixture.Text("All of them: " + strings.Join(cites, ", ") + "."))
	rankedTalk(t, s, "Read them all.")
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	text, _, n, err := s.rankedLines(ctx, history)
	if err != nil {
		t.Fatal(err)
	}
	if n != carryRanked || strings.Count(text, "\n## e") != carryRanked {
		t.Errorf("%d items carried, want %d:\n%s", n, carryRanked, text)
	}

	big := numberedItem(t, s, "big.txt", 400, 200)
	q.add(modelfixture.Text("The whole of it: [" + big + ":1-400]."))
	rankedTalk(t, s, "And this one?")
	if history, err = s.ConversationHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if text, _, n, err = s.rankedLines(ctx, history); err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(text, rankedIntro+"\n")
	if !strings.HasPrefix(body, "\n## "+big+" ·") || n != 1 || len(body) > carryBytes+200 ||
		!strings.Contains(body, "… cut here at the bound on what is carried; read on with evidence_read "+big+" from line ") {
		t.Errorf("%d items, %d bytes carried:\n%s", n, len(body), body[max(0, len(body)-400):])
	}
}

// TestCatchUp: [C11] with nothing new a catch-up calls no model; with a
// new comment the ledger reads it first, then one turn begins with the
// case update and the ledger, and the engine's words name the comment.
func TestCatchUp(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	s.e.cases = fixedCases{companionComments}
	ctx := t.Context()
	rankedTalk(t, s, "Where does it stand?")
	calls := len(f.Requests())
	if _, err := s.CatchUp(ctx, nil); !errors.Is(err, ErrNothingNew) || len(f.Requests()) != calls {
		t.Fatalf("with nothing new: %v, %d calls", err, len(f.Requests())-calls)
	}
	s.e.cases = fixedCases{append(append([]Comment(nil), companionComments...),
		Comment{ID: "c3", Author: Party{Name: "customer"}, Created: s.c.now(), Body: "Still failing after the reboot."})}
	if err := s.ReadComments(ctx); err != nil {
		t.Fatal(err)
	}
	ledgerCalls := 0
	if _, err := s.CatchUp(ctx, func(ev TalkEvent) {
		if ev.Kind == TalkCall && strings.HasPrefix(ev.Conversation, ledgerConversation) {
			ledgerCalls++
		}
	}); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()[calls:]
	if len(reqs) < 2 || isLedgerCall(reqs[len(reqs)-1]) || len(companionRequests(f)) != 2 || ledgerCalls != len(reqs)-1 {
		t.Fatalf("the catch-up's calls: %d, %d of them the ledger's told", len(reqs), ledgerCalls)
	}
	for _, r := range reqs[:len(reqs)-1] {
		if !isLedgerCall(r) {
			t.Error("a companion call came before the ledger's")
		}
	}
	last := reqs[len(reqs)-1]
	ask := last.Messages[len(last.Messages)-1].Text
	update, led := messageStarting(last, "# Case update"), messageStarting(last, ledgerHeading+"\n")
	if update < 0 || led < update || led != len(last.Messages)-2 ||
		!strings.Contains(last.Messages[update].Text, "Still failing after the reboot.") {
		t.Errorf("the catch-up's call:\n%s", texts(last))
	}
	if !strings.HasPrefix(ask, "Catch the engineer up. Since they last worked this case (2026-11-06 12:00 UTC), it gained 1 new comment, e") ||
		!strings.Contains(ask, "with the case ledger's reading of the comments") {
		t.Errorf("the engine's words: %q", ask)
	}
	calls = len(f.Requests())
	if _, err := s.CatchUp(ctx, nil); !errors.Is(err, ErrNothingNew) || len(f.Requests()) != calls {
		t.Errorf("caught up, again: %v, %d calls", err, len(f.Requests())-calls)
	}
}

// TestCatchUpAfterReload: a conversation a suggested reload began, before
// its first turn, is judged by the one it continues: the comment that came
// after the engineer last worked the case is still new, and the catch-up
// names it, the new conversation's case block already holding it.
func TestCatchUpAfterReload(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := t.Context()
	rankedTalk(t, s, "Where does it stand?")
	s.e.cases = fixedCases{append(append([]Comment(nil), companionComments...),
		Comment{ID: "c3", Author: Party{Name: "customer"}, Created: s.c.now(), Body: "Still failing after the reboot."})}
	if err := s.ReadComments(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reload(ctx, ReloadSuggested, nil); err != nil {
		t.Fatal(err)
	}
	r, _, err := s.ReopenState(ctx)
	if err != nil || r.NewComments != 1 || !r.Fresh() || r.CacheExpired {
		t.Fatalf("after the reload: %+v, %v", r, err)
	}
	if _, err := s.CatchUp(ctx, nil); err != nil {
		t.Fatal(err)
	}
	reqs := companionRequests(f)
	last := reqs[len(reqs)-1]
	if !strings.Contains(last.Messages[0].Text, "Still failing after the reboot.") || messageStarting(last, "# Case update") >= 0 ||
		!strings.Contains(last.Messages[len(last.Messages)-1].Text, "1 new comment, e") {
		t.Errorf("the catch-up after the reload:\n%s", texts(last))
	}
	if r, _, _ = s.ReopenState(ctx); r.Fresh() {
		t.Errorf("caught up, still new: %+v", r)
	}
}

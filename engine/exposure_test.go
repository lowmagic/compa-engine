package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The exposure tests run on the companion's stub case with a synthetic
// item of thirty numbered lines, stored and named but shown to no
// conversation until a tool reads it.

// thirtyLines stores a synthetic item of thirty lines, each saying what
// and its number, and returns its short name.
func thirtyLines(t *testing.T, s *Session, what string) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "synthetic %s line %d\n", what, i)
	}
	it, _, err := s.Ingest(strings.NewReader(b.String()), "synthetic numbered lines", "")
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.NameEvidence(it.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// citeChecks are each citation of text, as written, with ok or why it is
// flagged, and whose showing it rests on where not the conversation's own.
func citeChecks(t *testing.T, s *Session, text string) []string {
	t.Helper()
	cites := Cites(text)
	checks, err := s.CheckCites(context.Background(), cites)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i, c := range checks {
		got := fmt.Sprintf("%s:%d-%d ", cites[i].Name, cites[i].From, cites[i].To)
		switch {
		case !c.OK:
			got += c.Why
		case c.Via != "":
			got += "ok, " + c.Via
		default:
			got += "ok"
		}
		out = append(out, got)
	}
	return out
}

// TestCiteOutsideAPartialReadFlagged: a citation is checked against the
// lines the conversation was shown, not the item's name: lines an
// evidence_read gave and an evidence_grep's match with its context
// resolve, a line outside them is flagged saying which lines were shown,
// and the item cited whole resolves on any of them.
func TestCiteOutsideAPartialReadFlagged(t *testing.T) {
	var name atomic.Pointer[string]
	var grep atomic.Bool
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		if isLedgerCall(r) || n == nil || len(r.Messages[len(r.Messages)-1].Results) > 0 {
			return modelfixture.Text("done"), nil
		}
		if grep.Load() {
			return modelfixture.Calling(call("g1", "evidence_grep", map[string]any{"pattern": "line 20$", "ids": *n, "context": 1})), nil
		}
		return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "from": 3, "lines": 4})), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	n := thirtyLines(t, s, "numbered")
	name.Store(&n)
	if _, err := s.Talk(ctx, "Read the numbered lines.", nil); err != nil {
		t.Fatal(err)
	}
	text := "[" + n + ":4] [" + n + ":3-6] [" + n + ":10] [" + n + ":5-8] [" + n + "]"
	want := []string{
		n + ":4-0 ok",
		n + ":3-6 ok",
		n + ":10-0 " + n + "'s line 10 was never shown in this conversation, only lines 3–6",
		n + ":5-8 " + n + "'s lines 5–8 were never shown in this conversation, only lines 3–6",
		n + ":0-0 ok",
	}
	if d := cmp.Diff(want, citeChecks(t, s, text)); d != "" {
		t.Errorf("after reading lines 3–6 (-want +got):\n%s", d)
	}
	grep.Store(true)
	if _, err := s.Talk(ctx, "Find line 20.", nil); err != nil {
		t.Fatal(err)
	}
	want = []string{
		n + ":19-21 ok",
		n + ":25-0 " + n + "'s line 25 was never shown in this conversation, only lines 3–6, 19–21",
	}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":19-21] ["+n+":25]")); d != "" {
		t.Errorf("after the grep (-want +got):\n%s", d)
	}
}

// TestCiteOfAConsultsReadSaysSo: lines a consult read resolve a citation
// in the conversation that consulted, said to be the consult's read, not
// its own; lines the consult did not read stay flagged.
func TestCiteOfAConsultsReadSaysSo(t *testing.T) {
	var name atomic.Pointer[string]
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		last := r.Messages[len(r.Messages)-1]
		consulted := slices.ContainsFunc(r.Messages, func(m model.Message) bool { return strings.HasPrefix(m.Text, "You are consulted") })
		switch {
		case isLedgerCall(r) || n == nil:
			return modelfixture.Text("done"), nil
		case consulted && len(last.Results) == 0:
			return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "lines": 5})), nil
		case consulted:
			return modelfixture.Text("Line 2 says so [" + *n + ":2]."), nil
		case len(last.Results) == 0:
			return modelfixture.Calling(call("c1", "consult", map[string]any{"expert": "openshift.platform", "question": "What do the numbered lines say?"})), nil
		}
		return modelfixture.Text("The consult says so [" + *n + ":2]."), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	n := thirtyLines(t, s, "numbered")
	name.Store(&n)
	if _, err := s.Talk(ctx, "Ask the platform expert.", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		n + ":2-0 ok, read in the consult of openshift.platform, not in this conversation",
		n + ":9-0 " + n + "'s line 9 was never shown in this conversation, only lines 1–5",
	}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":2] ["+n+":9]")); d != "" {
		t.Errorf("(-want +got):\n%s", d)
	}
}

// TestCheckersReadShowsTheAnswerNothing: the editor is quoted an item the
// answer cites whole, and its conversation's record holds it, but what a
// checker was shown never resolves the answer's citation: a line the
// conversation was not shown stays flagged after the check.
func TestCheckersReadShowsTheAnswerNothing(t *testing.T) {
	var name atomic.Pointer[string]
	main := func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		switch {
		case isLedgerCall(r) || n == nil:
			return modelfixture.Text("done"), nil
		case len(r.Messages[len(r.Messages)-1].Results) == 0:
			return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "lines": 3})), nil
		}
		return modelfixture.Text("Line 2 holds it [" + *n + ":2], and line 20 too [" + *n + ":20]."), nil
	}
	s, _ := editorSession(t, main, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1", "s2"}, ""), nil
	})
	ctx := context.Background()
	n := thirtyLines(t, s, "numbered")
	name.Store(&n)
	if _, err := s.Talk(ctx, "Read the numbered lines.", nil); err != nil {
		t.Fatal(err)
	}
	var editor string
	if err := s.c.st.DB().QueryRow(`SELECT id FROM companion_conversations WHERE kind = 'editor'`).Scan(&editor); err != nil {
		t.Fatal(err)
	}
	read, err := companion.Store{DB: s.c.st.DB()}.Exposure(ctx, editor)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]companion.Shown{{Name: n, From: 1, To: 30}}, read); d != "" {
		t.Errorf("what the editor was shown (-want +got):\n%s", d)
	}
	want := []string{
		n + ":2-0 ok",
		n + ":20-0 " + n + "'s line 20 was never shown in this conversation, only lines 1–3",
	}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":2] ["+n+":20]")); d != "" {
		t.Errorf("after the editor's check (-want +got):\n%s", d)
	}
}

// TestRankedReviewShowsTheAnswerNothing: a review of an answer citing
// another item, given the ranked evidence, is quoted the lines an earlier
// answer cited, line 20 among them, which the conversation was never
// shown; the reviewer's conversation's record holds them, but the
// conversation's citation of line 20 stays flagged after the review.
func TestRankedReviewShowsTheAnswerNothing(t *testing.T) {
	var name atomic.Pointer[string]
	var other string
	main := func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		last := r.Messages[len(r.Messages)-1]
		switch {
		case isLedgerCall(r) || n == nil:
			return modelfixture.Text("done"), nil
		case strings.Contains(last.Text, "Anything else?"):
			return modelfixture.Text("The other item's first line [" + other + ":1]."), nil
		case len(last.Results) == 0:
			return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "lines": 3})), nil
		}
		return modelfixture.Text("Line 2 holds it [" + *n + ":2], and line 20 too [" + *n + ":20]."), nil
	}
	s, f := reviewerSession(t, main, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	ctx := context.Background()
	n := thirtyLines(t, s, "numbered")
	other = thirtyLines(t, s, "other")
	name.Store(&n)
	for _, ask := range []string{"Read the numbered lines.", "Anything else?"} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Review(ctx, ReviewAsk{Ranked: true}, nil); err != nil {
		t.Fatal(err)
	}
	if reviewer, _ := reviewerCalls(f); len(reviewer) != 1 || !strings.Contains(reviewer[0].Messages[len(reviewer[0].Messages)-1].Text, "synthetic numbered line 20") {
		t.Fatalf("the ranked review was not quoted line 20: %d calls", len(reviewer))
	}
	var review string
	if err := s.c.st.DB().QueryRow(`SELECT id FROM companion_conversations WHERE kind = 'review'`).Scan(&review); err != nil {
		t.Fatal(err)
	}
	read, err := companion.Store{DB: s.c.st.DB()}.Exposure(ctx, review)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(read, companion.Shown{Name: n, From: 20, To: 20}) {
		t.Errorf("what the reviewer was shown: %v; want %s:20 among it", read, n)
	}
	want := []string{n + ":20-0 " + n + "'s line 20 was never shown in this conversation, only lines 1–3"}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":20]")); d != "" {
		t.Errorf("after the ranked review (-want +got):\n%s", d)
	}
}

// TestCompactionCarriesExposure: the conversation a compaction begins is
// judged with the lines the one it continues was shown, and no more.
func TestCompactionCarriesExposure(t *testing.T) {
	var name atomic.Pointer[string]
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		last := r.Messages[len(r.Messages)-1]
		if isLedgerCall(r) || n == nil || len(last.Results) > 0 || strings.HasPrefix(last.Text, "Write a handoff") || strings.Contains(last.Text, "Carry on") {
			return modelfixture.Text("A synthetic handoff or answer."), nil
		}
		return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "lines": 5})), nil
	})
	ctx := context.Background()
	n := thirtyLines(t, s, "numbered")
	name.Store(&n)
	if _, err := s.Talk(ctx, "Read the numbered lines.", nil); err != nil {
		t.Fatal(err)
	}
	next, err := s.Compact(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Carry on.", nil); err != nil {
		t.Fatal(err)
	}
	st := companion.Store{DB: s.c.st.DB()}
	own, err := st.Exposure(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range own {
		if sh.Name == n {
			t.Fatalf("the new conversation was itself shown %s: %v", n, own)
		}
	}
	want := []string{
		n + ":3-0 ok",
		n + ":12-0 " + n + "'s line 12 was never shown in this conversation, only lines 1–5",
	}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":3] ["+n+":12]")); d != "" {
		t.Errorf("after the compaction (-want +got):\n%s", d)
	}
}

// TestUnrecordedConversationExposureUnknown: a conversation stored before
// the engine recorded what each call carried is judged by the names it
// holds, never by lines inferred for it, and a citation says its exposure
// is unknown whether it resolves or not.
func TestUnrecordedConversationExposureUnknown(t *testing.T) {
	var name atomic.Pointer[string]
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		if isLedgerCall(r) || n == nil || len(r.Messages[len(r.Messages)-1].Results) > 0 {
			return modelfixture.Text("done"), nil
		}
		return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "lines": 5})), nil
	})
	ctx := context.Background()
	n := thirtyLines(t, s, "numbered")
	hidden := thirtyLines(t, s, "hidden")
	name.Store(&n)
	if _, err := s.Talk(ctx, "Read the numbered lines.", nil); err != nil {
		t.Fatal(err)
	}
	// The store as migration 45 leaves one made before it.
	if _, err := s.c.st.DB().Exec(`UPDATE companion_conversations SET exposure = 0; DELETE FROM companion_exposures; DELETE FROM companion_requests`); err != nil {
		t.Fatal(err)
	}
	want := []string{
		n + ":20-0 ok, " + exposureUnknown,
		hidden + ":2-0 " + hidden + " was never shown in this conversation (" + exposureUnknown + ")",
	}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":20] ["+hidden+":2]")); d != "" {
		t.Errorf("(-want +got):\n%s", d)
	}
}

// readsLines3To6 is a session whose model, asked to read, reads lines 3–6
// of the item named, then answers with answer, {n} its name; every other
// call answers "done".
func readsLines3To6(t *testing.T, answer string) (*Session, string) {
	t.Helper()
	var name atomic.Pointer[string]
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		n := name.Load()
		last := r.Messages[len(r.Messages)-1]
		switch {
		case isLedgerCall(r) || n == nil:
			return modelfixture.Text("done"), nil
		case len(last.Results) > 0:
			return modelfixture.Text(strings.ReplaceAll(answer, "{n}", *n)), nil
		case strings.Contains(last.Text, "Read the numbered lines"):
			return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *n, "from": 3, "lines": 4})), nil
		}
		return modelfixture.Text("done"), nil
	})
	n := thirtyLines(t, s, "numbered")
	name.Store(&n)
	if _, err := s.Talk(context.Background(), "Read the numbered lines.", nil); err != nil {
		t.Fatal(err)
	}
	return s, n
}

// forgetExposure leaves the store as migration 45 leaves one made before
// it: every conversation's exposure unknown.
func forgetExposure(t *testing.T, s *Session) {
	t.Helper()
	if _, err := s.c.st.DB().Exec(`UPDATE companion_conversations SET exposure = 0; DELETE FROM companion_exposures; DELETE FROM companion_requests`); err != nil {
		t.Fatal(err)
	}
}

// TestSpanSearchReadsOnlyLinesShown: a code span is looked for in the
// lines the conversation was shown, not the whole of an item it was shown
// some of: one only in a line never shown is flagged as in no evidence
// the conversation saw, its closest line one it was shown; one in a line
// shown is not flagged. A conversation whose exposure is unknown keeps
// the name rule, the item searched whole.
func TestSpanSearchReadsOnlyLinesShown(t *testing.T) {
	answer := "It logged `synthetic numbered line 4` and `synthetic numbered line 20` [{n}:4]."
	s, n := readsLines3To6(t, answer)
	text := strings.ReplaceAll(answer, "{n}", n)
	flags := spanFlags(t, s, text)
	unseen, ok := flags["synthetic numbered line 20"]
	if len(flags) != 1 || !ok {
		t.Fatalf("span flags %+v; want the unshown line's only", flags)
	}
	if want := (EvidenceLine{Name: n, Line: 3, Text: "synthetic numbered line 3"}); unseen.Why != "in no evidence this conversation saw" || unseen.Closest == nil || *unseen.Closest != want {
		t.Errorf("the unshown line's flag %+v, closest %+v; want closest %+v", unseen, unseen.Closest, want)
	}
	forgetExposure(t, s)
	if flags := spanFlags(t, s, text); len(flags) != 0 {
		t.Errorf("exposure unknown, span flags %+v; want none, the item searched whole", flags)
	}
}

// TestVerdictRestsOnLinesShown: a verdict may rest only on lines the
// conversation was shown, as the span search reads them, a line outside
// refused saying which were shown; one resting on lines shown is kept,
// and the message the model is told records the lines it quotes as what
// it shows. A conversation whose exposure is unknown keeps the name rule.
func TestVerdictRestsOnLinesShown(t *testing.T) {
	answer := "It logged `synthetic numbered line 20` and `synthetic numbered line 25` [{n}:4]."
	s, n := readsLines3To6(t, answer)
	ctx := context.Background()
	flags := spanFlags(t, s, strings.ReplaceAll(answer, "{n}", n))
	twenty, twentyFive := flags["synthetic numbered line 20"], flags["synthetic numbered line 25"]
	if twenty.Span == "" || twentyFive.Span == "" {
		t.Fatalf("span flags %+v; want both spans flagged", flags)
	}
	_, err := s.JudgeSpan(ctx, "a1", twenty, VerdictSupported, []EvidenceRange{{Name: n, From: 20, To: 20}})
	if want := n + "'s line 20 was never shown in this conversation, only lines 3–6"; err == nil || err.Error() != want {
		t.Errorf("a verdict on a line never shown: %v; want %q", err, want)
	}
	if _, err := s.JudgeSpan(ctx, "a1", twenty, VerdictIncorrect, []EvidenceRange{{Name: n, From: 4, To: 5}}); err != nil {
		t.Fatalf("a verdict on lines shown: %v", err)
	}
	msgs := mainMessages(t, s)
	told := msgs[len(msgs)-1]
	if told.Kind != companion.KindVerdict {
		t.Fatalf("the last message is a %s; want the verdict", told.Kind)
	}
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shows, err := st.Shows(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]companion.Shown{{Name: n, From: 4, To: 5}}, shows[told.Seq]); d != "" {
		t.Errorf("what the verdict shows (-want +got):\n%s", d)
	}
	forgetExposure(t, s)
	if _, err := s.JudgeSpan(ctx, "a1", twentyFive, VerdictSupported, []EvidenceRange{{Name: n, From: 25, To: 25}}); err != nil {
		t.Errorf("exposure unknown, a verdict on a line of an item named: %v", err)
	}
}

// TestReloadCarriesRankedLines: the lines a reload carries of the ranked
// evidence are what its handoff shows, recorded with it and carried by
// the next call: lines the engineer pointed at, never shown before, then
// resolve a citation, and a line outside them does not.
func TestReloadCarriesRankedLines(t *testing.T) {
	s, _ := companionSession(t, script(modelfixture.Text("A synthetic answer.")))
	ctx := context.Background()
	n := thirtyLines(t, s, "numbered")
	if _, err := s.Talk(ctx, pointsAt+n+":10-11\n\nWhat do these lines say?", nil); err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]string{n + ":10-0 " + n + " was never shown in this conversation"}, citeChecks(t, s, "["+n+":10]")); d != "" {
		t.Errorf("before the reload, the pointer alone (-want +got):\n%s", d)
	}
	r, err := s.Reload(ctx, ReloadSuggested, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Go on.", nil); err != nil {
		t.Fatal(err)
	}
	st := companion.Store{DB: s.c.st.DB()}
	own, err := st.Exposure(ctx, r.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(own, companion.Shown{Name: n, From: 10, To: 11}) {
		t.Errorf("the reloaded conversation was shown %v; want %s's lines 10–11", own, n)
	}
	want := []string{
		n + ":10-11 ok",
		n + ":12-0 " + n + "'s line 12 was never shown in this conversation, only lines 10–11",
	}
	if d := cmp.Diff(want, citeChecks(t, s, "["+n+":10-11] ["+n+":12]")); d != "" {
		t.Errorf("after the reload (-want +got):\n%s", d)
	}
}

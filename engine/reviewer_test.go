package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The reviewer's tests run on the companion's stub case, its answers and
// the reviewer's findings synthetic, as the editor's do.

// isReviewerCall reports whether a request is the reviewer's.
func isReviewerCall(r model.Request) bool {
	return r.System == check.ReviewerPrompt() && len(r.Tools) == 1 && r.Tools[0].Name == check.ToolName
}

// reviewerSession is the companion's stub case with the reviewer
// configured on the fixture model, and no editor: main answers the
// companion's calls and review the reviewer's.
func reviewerSession(t *testing.T, main, review func(context.Context, model.Request) (*modelfixture.Answer, error)) (*Session, *modelfixture.Provider) {
	t.Helper()
	s, f := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isReviewerCall(r) {
			return review(ctx, r)
		}
		return main(ctx, r)
	})
	s.e.config.Reasoning.Profiles["reviewer"] = config.Profile{Model: "fixture-model"}
	return s, f
}

func reviewerCalls(f *modelfixture.Provider) (reviewer, main []model.Request) {
	for _, r := range f.Requests() {
		if isReviewerCall(r) {
			reviewer = append(reviewer, r)
		} else {
			main = append(main, r)
		}
	}
	return reviewer, main
}

// reviewFinding is the reviewer's finding on certainAnswer's s1, with an
// alternative and a discriminator.
var reviewFinding = map[string]any{"statement": "s1", "kind": "certainty", "quote": "confirms that the DNS timeouts were caused by worker-1",
	"why": "e1:2 says the timeouts began after the upgrade, not what caused them", "evidence": []any{"e1:2"},
	"alternative": "a fault of the DNS operator on both workers, worker-1 incidental", "discriminator": "the DNS operator's logs at the time, not given",
	"suggested": "the DNS timeouts began after the upgrade"}

// reviews answers the reviewer's calls with the finding on s1, s2
// supported.
func reviewsWith(findings ...any) func(context.Context, model.Request) (*modelfixture.Answer, error) {
	return func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, "", findings...), nil
	}
}

// reviewState is what of the case a review must leave as it was: the
// answers as shown, the last exchange a reload carries, the ranked
// evidence's uses and the items.
type reviewState struct {
	views    []AnswerView
	exchange string
	uses     []string
	items    []CompanionItem
}

func stateOf(t *testing.T, s *Session) reviewState {
	t.Helper()
	ctx := context.Background()
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := reviewState{views: AnswerViews(history), exchange: lastExchange(history), items: items}
	for _, u := range rankUses(history, items) {
		st.uses = append(st.uses, fmtUse(u))
	}
	return st
}

// TestReviewLastAnswer: [E3] /review of the last answer makes one call of
// the reviewer, in a conversation of its own with its prompt and one tool,
// recorded as answers.review, given the answer as shown; its findings,
// the alternative and the discriminator kept, are appended once to the
// main conversation after every earlier message, each byte for byte as it
// was, headed as a model's reading; the main model's next call is given
// it. It is no answer: the answers as shown, /pin, a reload's last
// exchange and the ranked evidence are as they were, and the next answer
// is a2.
func TestReviewLastAnswer(t *testing.T) {
	s, f := reviewerSession(t, answers(certainAnswer, ""), reviewsWith(reviewFinding))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	before, was := mainMessages(t, s), stateOf(t, s)
	r, err := s.Review(ctx, ReviewAsk{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _ := reviewerCalls(f)
	if len(reviewer) != 1 {
		t.Fatalf("the reviewer called %d times; want once", len(reviewer))
	}
	if req := reviewer[0].Messages; len(req) != 1 || !strings.HasPrefix(req[0].Text, "# Review this text\n\nThe text is the companion's answer a1,") || !strings.Contains(req[0].Text, "s1: The upgrade confirms") {
		t.Errorf("the reviewer's request: %+v", req)
	}
	var task string
	var convs int
	if err := s.c.st.DB().QueryRowContext(ctx, `SELECT task_type FROM invocations ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&task); err != nil || task != ReviewTask {
		t.Errorf("the last call recorded as %q, %v", task, err)
	}
	if err := s.c.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM companion_conversations WHERE kind = 'review'`).Scan(&convs); err != nil || convs != 1 {
		t.Errorf("%d review conversations, %v", convs, err)
	}
	if r.Of != "a1" || r.Model != "fixture-model" || len(r.Findings) != 1 || r.Findings[0].Alternative == "" || r.Findings[0].Discriminator == "" ||
		r.Counts() != "2 statements: 1 supported, 1 finding, 0 skipped" {
		t.Errorf("the review: %+v", r)
	}
	after := mainMessages(t, s)
	if len(after) != len(before)+1 {
		t.Fatalf("%d messages after the review, %d before; want one more", len(after), len(before))
	}
	if d := cmp.Diff(before, after[:len(before)]); d != "" {
		t.Errorf("the earlier messages changed (-before +after):\n%s", d)
	}
	text := after[len(after)-1].Text
	for _, want := range []string{ReviewHeading("a1", "fixture-model") + "\n\n",
		`- s1 certainty · "confirms that the DNS timeouts were caused by worker-1" · e1:2 says the timeouts began after the upgrade, not what caused them (e1:2)`,
		"  alternative: a fault of the DNS operator on both workers, worker-1 incidental · discriminator: the DNS operator's logs at the time, not given",
		"  at most: the DNS timeouts began after the upgrade", "2 statements: 1 supported, 1 finding, 0 skipped."} {
		if !strings.Contains(text, want) {
			t.Errorf("the review kept lacks %q:\n%s", want, text)
		}
	}
	if last := after[len(after)-1]; last.Kind != companion.KindReview || last.Role != model.RoleUser {
		t.Errorf("the review is kept as a %s message of role %s", last.Kind, last.Role)
	}
	if d := cmp.Diff(was, stateOf(t, s), cmp.AllowUnexported(reviewState{})); d != "" {
		t.Errorf("the review changed the answers, the last exchange, the ranked uses or the items (-before +after):\n%s", d)
	}
	if it, err := s.Pin(ctx); err != nil || it.Text != certainAnswer {
		t.Errorf("/pin kept %q, %v", it.Text, err)
	}
	if d := cmp.Diff([]string{checkReviewed}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
	var name, checked string
	if err := s.c.st.DB().QueryRowContext(ctx, `SELECT checked_name, checked_invocation_id FROM checks`).Scan(&name, &checked); err != nil || name != "a1" || checked != before[len(before)-1].Invocation || checked == "" {
		t.Errorf("the check kept of %q, the call %q, %v", name, checked, err)
	}
	if _, err := s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	_, main := reviewerCalls(f)
	if next := main[len(main)-1].Messages; !slices.ContainsFunc(next, func(m model.Message) bool { return m.Text == text && m.Role == model.RoleUser }) {
		t.Error("the main model's next call is not given the review")
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	views := AnswerViews(history)
	if len(views) != 2 || views[1].Label != "a2" {
		t.Errorf("the answers after the review and another turn: %+v", views)
	}
	i := slices.IndexFunc(history, func(m ConversationMessage) bool { return m.Kind == MessageReview })
	if i < 0 || history[i].Review == nil || cmp.Diff(r, *history[i].Review, cmp.AllowUnexported(UsageAggregate{})) != "" {
		t.Errorf("the review as history reads it: %+v, want %+v", history[i].Review, r)
	}
}

// TestReviewItem: [E3] /review i1 reviews the item as kept, a draft, its
// text unchanged by the review, and appends the review of i1 once.
func TestReviewItem(t *testing.T) {
	s, f := reviewerSession(t, answers(certainAnswer, ""), reviewsWith(reviewFinding))
	s.e.config.Comments.Intentions = []config.Intention{barredIntention}
	ctx := context.Background()
	if _, _, err := s.Draft(ctx, barredIntention.ID, nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := mainMessages(t, s)
	r, err := s.Review(ctx, ReviewAsk{Item: "i1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _ := reviewerCalls(f)
	if len(reviewer) != 1 || !strings.Contains(reviewer[0].Messages[0].Text, "The text is the draft i1 for update, for the customer,") {
		t.Fatalf("the reviewer's requests: %d", len(reviewer))
	}
	after := mainMessages(t, s)
	if len(after) != len(before)+1 || !strings.HasPrefix(after[len(after)-1].Text, ReviewHeading("i1", "fixture-model")) || r.Of != "i1" {
		t.Errorf("the conversation ends %q; the review %+v", after[len(after)-1].Text, r)
	}
	if d := cmp.Diff(before, after[:len(before)]); d != "" {
		t.Errorf("the earlier messages changed (-before +after):\n%s", d)
	}
	if now, err := s.Items(ctx); err != nil || cmp.Diff(items, now) != "" {
		t.Errorf("the items after the review: %+v, %v; before %+v", now, err, items)
	}
	if _, err := s.Review(ctx, ReviewAsk{Item: "i9"}, nil); err == nil {
		t.Error("a review of no item ran")
	}
}

// TestReviewBarredMakesNoCall: [E3] a case barred by the time a review is
// asked makes no reviewer call, its refusal recorded as barred_account
// under answers.review with no record holding the account; the review is
// said to have failed and nothing is appended.
func TestReviewBarredMakesNoCall(t *testing.T) {
	_, s, f, _ := barredCase(t, "1000000001", func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isReviewerCall(r) {
			return finding([]any{"s1", "s2"}, ""), nil
		}
		return modelfixture.Text(certainAnswer), nil
	}, "# nobody yet")
	s.e.config.Reasoning.Profiles["reviewer"] = config.Profile{Model: "fixture-model"}
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.e.config.Reasoning.BarredAccounts.List, []byte("1000000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := mainMessages(t, s)
	if _, err := s.Review(ctx, ReviewAsk{}, nil); err == nil || !strings.Contains(err.Error(), "barred") {
		t.Errorf("the review of a barred case: %v", err)
	}
	if reviewer, _ := reviewerCalls(f); len(reviewer) != 0 {
		t.Errorf("the reviewer called %d times", len(reviewer))
	}
	var outcome string
	if err := s.c.st.DB().QueryRowContext(ctx, `SELECT outcome FROM invocations WHERE task_type = ?`, ReviewTask).Scan(&outcome); err != nil || outcome != companion.OutcomeBarred {
		t.Errorf("the reviewer's record %q, %v", outcome, err)
	}
	if _, holds := invocationOutcomes(t, s.c, "1000000001"); holds {
		t.Error("an invocation record holds the account number")
	}
	if after := mainMessages(t, s); len(after) != len(before) {
		t.Errorf("%d messages after the refused review, %d before", len(after), len(before))
	}
	if d := cmp.Diff([]string{checkFailed}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
}

// TestReviewRequestRedacted: [E3] a synthetic token in a line the answer
// cites is redacted in the reviewer's request as sent, by the provider's
// egress policy.
func TestReviewRequestRedacted(t *testing.T) {
	const token = "synthetic0token0abcdefghijkl"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.log"), []byte("kubelet started\nAuthorization: Bearer "+token+"\nkubelet ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var name atomic.Value
	s, f := reviewerSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if calls.Add(1) == 1 {
			return modelfixture.Calling(call("r1", "read", map[string]any{"path": "node.log"})), nil
		}
		for _, res := range lastResults(r) {
			if ev := ExcerptEvidence(res.Text); len(ev) == 1 {
				name.Store(ev[0])
			}
		}
		n, _ := name.Load().(string)
		return modelfixture.Text("The kubelet was given a bearer token [" + n + ":2]."), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	s.c.root = dir
	s.e.config.Reasoning.MaxEgressClass = "internal_network"
	f.Egress = "internal_network"
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What does node.log say?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, ReviewAsk{}, nil); err != nil {
		t.Fatal(err)
	}
	reviewer, _ := reviewerCalls(f)
	if len(reviewer) != 1 {
		t.Fatalf("the reviewer called %d times", len(reviewer))
	}
	if req := reviewer[0].Messages[0].Text; strings.Contains(req, token) || !strings.Contains(req, "Authorization: Bearer [REDACTED:token]") {
		t.Errorf("the reviewer's request as sent:\n%s", req)
	}
}

// TestReviewLeavesOutExcludedComment: [E3] the lines of a comment the
// engineer leaves out of what models read are not quoted to the reviewer,
// though the answer cites them.
func TestReviewLeavesOutExcludedComment(t *testing.T) {
	var cite atomic.Pointer[string]
	none := ""
	cite.Store(&none)
	s, f := reviewerSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return modelfixture.Text("Support asked for a fresh must-gather " + *cite.Load() + "."), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	ctx := context.Background()
	comments, err := ledgerComments(s.c)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(comments, func(cm CaseComment) bool { return cm.ID == "c2" })
	if i < 0 {
		t.Fatal("no comment c2")
	}
	n, err := evidenceName(s.c, comments[i].EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	cited := "[" + n + ":1]"
	cite.Store(&cited)
	if _, err := s.Talk(ctx, "What was asked?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := excludeComments(s.c, ExcludeComment, "c2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, ReviewAsk{}, nil); err != nil {
		t.Fatal(err)
	}
	reviewer, _ := reviewerCalls(f)
	if len(reviewer) != 1 {
		t.Fatalf("the reviewer called %d times", len(reviewer))
	}
	if req := reviewer[0].Messages[0].Text; strings.Contains(req, "DNS operator looks healthy") || !strings.Contains(req, "not quoted: the engineer's rules leave it out of what models read") {
		t.Errorf("the reviewer's request:\n%s", req)
	}
}

// TestReviewCountsInTotalUsage: [E3] the reviewer's calls count in the
// case's total, as the status bar and /usage show it, and the review
// carries its own.
func TestReviewCountsInTotalUsage(t *testing.T) {
	s, _ := reviewerSession(t, answers(certainAnswer, ""), reviewsWith(reviewFinding))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.Review(ctx, ReviewAsk{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	total, err := s.CompanionTotalUsage()
	if err != nil || total.Invocations != 2 || r.Usage.Invocations != 1 {
		t.Errorf("the total counts %d calls, %v, and the review %d; want the turn's and the reviewer's, and the reviewer's", total.Invocations, err, r.Usage.Invocations)
	}
}

// TestReviewNotConfigured: [E3] a deployment with no reviewer profile
// offers no review: asked for one, it says so and calls nothing.
func TestReviewNotConfigured(t *testing.T) {
	s, f := companionSession(t, answers(certainAnswer, ""))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	before := mainMessages(t, s)
	_, err := s.Review(ctx, ReviewAsk{}, nil)
	if err == nil || err.Error() != "no reviewer is configured (reasoning.profiles.reviewer)" || s.ReviewerModel() != "" {
		t.Errorf("a review with no reviewer: %v, the model %q", err, s.ReviewerModel())
	}
	if len(f.Requests()) != 1 || len(mainMessages(t, s)) != len(before) {
		t.Errorf("%d calls, %d messages after; want the turn's alone", len(f.Requests()), len(mainMessages(t, s)))
	}
}

// TestReviewTwoCallsInAll: [E3] the reviewer makes at most two model calls
// in all, whatever it answers — text and then findings refused whole,
// unknown tools, text twice — and the review fails, appending nothing.
func TestReviewTwoCallsInAll(t *testing.T) {
	invalid := modelfixture.Calling(call("f1", check.ToolName, map[string]any{"findings": []any{}}))
	unknown := modelfixture.Calling(call("u1", "verdict", map[string]any{"fine": true}))
	for _, c := range []struct {
		name    string
		answers []*modelfixture.Answer
	}{
		{"text, then refused", []*modelfixture.Answer{modelfixture.Text("It reads fine."), invalid, invalid}},
		{"unknown tools", []*modelfixture.Answer{unknown, unknown, unknown}},
		{"text twice", []*modelfixture.Answer{modelfixture.Text("It reads fine."), modelfixture.Text("Still fine."), modelfixture.Text("Fine.")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f := reviewerSession(t, answers(certainAnswer, ""), script(c.answers...))
			ctx := context.Background()
			if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
				t.Fatal(err)
			}
			before := mainMessages(t, s)
			_, err := s.Review(ctx, ReviewAsk{}, nil)
			if reviewer, _ := reviewerCalls(f); len(reviewer) != 2 {
				t.Errorf("the reviewer called %d times; want 2", len(reviewer))
			}
			if err == nil || !strings.HasPrefix(err.Error(), "the reviewer failed: ") {
				t.Errorf("the review: %v", err)
			}
			if len(mainMessages(t, s)) != len(before) {
				t.Error("a failed review appended a message")
			}
		})
	}
}

// TestReviewFailsAppendsNothing: [E3] a reviewer that fails, or passes its
// bound, says so; nothing is appended to the conversation, and the check
// is kept as failed with why.
func TestReviewFailsAppendsNothing(t *testing.T) {
	for _, c := range []struct {
		name, why string
		review    func(context.Context, model.Request) (*modelfixture.Answer, error)
	}{
		{"failing", "the reviewer failed: ", func(context.Context, model.Request) (*modelfixture.Answer, error) {
			return nil, errors.New("a synthetic transport failure")
		}},
		{"slow", "the reviewer passed its 100ms bound", func(ctx context.Context, _ model.Request) (*modelfixture.Answer, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func(was time.Duration) { reviewerBound = was }(reviewerBound)
			reviewerBound = 100 * time.Millisecond
			s, _ := reviewerSession(t, answers(certainAnswer, ""), c.review)
			ctx := context.Background()
			if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
				t.Fatal(err)
			}
			before := mainMessages(t, s)
			if _, err := s.Review(ctx, ReviewAsk{}, nil); err == nil || !strings.HasPrefix(err.Error(), c.why) {
				t.Errorf("the review: %v, want %q", err, c.why)
			}
			if after := mainMessages(t, s); cmp.Diff(before, after) != "" {
				t.Error("a failed review changed the conversation")
			}
			var outcome, why string
			if err := s.c.st.DB().QueryRowContext(ctx, `SELECT outcome, error FROM checks`).Scan(&outcome, &why); err != nil || outcome != checkFailed || !strings.HasPrefix(why, c.why) {
				t.Errorf("the check kept: %q %q, %v", outcome, why, err)
			}
		})
	}
}

// TestReviewKeepsItsFields: [E3] the reviewer's alternative and
// discriminator are kept, and what is the editor's alone — an anchor, a
// corrected text — is dropped with its reason: the review is appended
// with no anchor and no correction.
func TestReviewKeepsItsFields(t *testing.T) {
	anchored := map[string]any{}
	for k, v := range reviewFinding {
		anchored[k] = v
	}
	anchored["anchor"] = true
	s, _ := reviewerSession(t, answers(certainAnswer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, "A SYNTHETIC CORRECTION.", anchored), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.Review(ctx, ReviewAsk{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 1 || r.Findings[0].Anchor || r.Findings[0].Alternative == "" || r.Findings[0].Discriminator == "" {
		t.Errorf("the review's findings: %+v", r.Findings)
	}
	msgs := mainMessages(t, s)
	if text := msgs[len(msgs)-1].Text; strings.Contains(text, "A SYNTHETIC CORRECTION") || !strings.Contains(text, "discriminator: the DNS operator's logs") {
		t.Errorf("the review kept:\n%s", text)
	}
	var dropped string
	if err := s.c.st.DB().QueryRowContext(ctx, `SELECT dropped FROM checks`).Scan(&dropped); err != nil || !strings.Contains(dropped, "findings[0].anchor") || !strings.Contains(dropped, `"corrected"`) {
		t.Errorf("the dropped elements kept: %s, %v", dropped, err)
	}
}

// TestReviewerBoundHoldsMeasured: the reviewer's bound holds the slowest
// review the live benchmark measured on claude-sonnet-4-6, 88 s with
// its retry, twice over, so the default reviewer is not cut off where it
// answers.
func TestReviewerBoundHoldsMeasured(t *testing.T) {
	if slowest := 88 * time.Second; reviewerBound < 2*slowest {
		t.Errorf("the reviewer's bound is %s; the slowest review measured took %s", reviewerBound, slowest)
	}
}

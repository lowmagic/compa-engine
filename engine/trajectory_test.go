package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// TestTrajectoryCountsTheOpening: an opening that makes two reads, repeats
// the first, makes a third, calls a tool there is none of, reads on in the
// first read's item, consults an expert whose consult repeats the first
// read, and answers citing the second's item is reported as exactly that:
// seven calls, one refused, one repeated; of the three items returned, one
// read on, one cited and the third neither; the case routing key's expert loaded
// by the engine; the consult's one call repeating the main conversation's,
// the last the opening's budget allows; and each model call's input as the
// fixture reported it. Synthetic.
func TestTrajectoryCountsTheOpening(t *testing.T) {
	grep := map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"}
	other := map[string]any{"pattern": "(?i)timeout", "path": "namespaces/openshift-ovn-kubernetes"}
	var n atomic.Int64
	var first, second string
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Calling(call("c1", "grep", grep), call("c2", "run_tool", map[string]any{"id": "networking.ovn_errors", "params": map[string]any{}})), nil
		case 2:
			res := lastResults(r)
			first, _, _ = strings.Cut(res["c1"].Text, " ")
			second, _, _ = strings.Cut(res["c2"].Text, " ")
			return modelfixture.Calling(call("c3", "grep", grep), call("c4", "grep", other), call("c5", "no_such_tool", map[string]any{})), nil
		case 3:
			return modelfixture.Calling(call("c6", "evidence_read", map[string]any{"id": first})), nil
		case 4:
			return modelfixture.Calling(call("c7", "consult", map[string]any{"expert": "openshift.platform", "question": "Are the workers Ready?"})), nil
		case 5: // the consult
			return modelfixture.Calling(call("k1", "grep", grep)), nil
		case 6:
			return modelfixture.Text("Both workers are Ready."), nil
		}
		return modelfixture.Text("The OVN controller logs errors [" + second + ":1]."), nil
	})
	ctx := context.Background()
	if _, err := s.Orient(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || len(got.Conversations) != 1 {
		t.Fatalf("the trajectory: %+v", got)
	}
	turn := got.Turns[0]
	if len(turn.Consults) != 1 || turn.Consults[0].Conversation == "" {
		t.Fatalf("the consults: %+v", turn.Consults)
	}
	third := turn.Evidence.NeitherNames
	if len(third) != 1 || third[0] == first || third[0] == second {
		t.Fatalf("the item no one read on or cited: %v, beside %s and %s", third, first, second)
	}
	fixture := model.Reported(100)
	var calls []ModelCallTokens
	for range 7 {
		calls = append(calls, ModelCallTokens{Input: fixture, CacheRead: model.Reported(0)})
	}
	args, _ := json.Marshal(grep)
	want := TrajectoryTurn{Number: 1, Conversation: got.Conversations[0], Kind: "opening", Calls: 7, Refused: 1, BudgetSpent: true,
		Evidence:   TrajectoryEvidence{Returned: 3, ReadOn: 1, Cited: 1, Neither: 1, NeitherNames: third, None: 1, NoneNames: third},
		Repeated:   []RepeatedCall{{Tool: "grep", Args: string(args), FirstTurn: 1}},
		Experts:    []ExpertLoad{{ID: "openshift.networking", ByEngine: true}},
		Consults:   []ConsultRun{{Expert: "openshift.platform", Conversation: turn.Consults[0].Conversation, Calls: 1, Repeats: 1}},
		ModelCalls: calls, Input: TokenSum{Value: 700}, CacheRead: TokenSum{Value: 0},
		// The calls ask nothing; no_such_tool and consult take no question.
		Asked: []CallQuestion{{Tool: "grep", Missing: true}, {Tool: "run_tool", Missing: true}, {Tool: "grep", Missing: true},
			{Tool: "grep", Missing: true}, {Tool: "evidence_read", Missing: true}},
		Questions: QuestionCounts{Calls: 5, None: 5}}
	ignoreIDs := cmp.Transformer("calls", func(c ModelCallTokens) ModelCallTokens {
		return ModelCallTokens{Input: c.Input, CacheRead: c.CacheRead}
	})
	// The guidance's figures are TestTrajectoryGuidance's.
	noGuidance := cmp.Options{cmpopts.IgnoreFields(TrajectoryTurn{}, "Guidance"), cmpopts.IgnoreFields(ConsultRun{}, "Guidance"),
		cmpopts.IgnoreFields(TrajectoryTotals{}, "Guidance", "ConsultGuidance")}
	if d := cmp.Diff(want, turn, ignoreIDs, noGuidance); d != "" {
		t.Errorf("the opening (-want +got):\n%s", d)
	}
	wantTotals := TrajectoryTotals{Turns: 1, Calls: 7, Refused: 1, BudgetSpent: 1, Evidence: want.Evidence, Repeated: 1, Loaded: 1, ByEngine: 1,
		Consults: 1, ConsultCalls: 1, ConsultRepeats: 1, ModelCalls: 7, Input: TokenSum{Value: 700}, Questions: want.Questions}
	if d := cmp.Diff(wantTotals, got.Totals, noGuidance); d != "" {
		t.Errorf("the totals (-want +got):\n%s", d)
	}
	for _, c := range turn.ModelCalls[4:6] {
		if c.Conversation != turn.Consults[0].Conversation {
			t.Errorf("the consult's calls are not the fifth and sixth: %+v", turn.ModelCalls)
		}
	}
	out := RenderTrajectory(got)
	for _, line := range []string{"── turn 1 · opening · ", "  7 calls, 1 refused, 1 repeated; the turn's budget was spent\n",
		"  3 items returned: 1 read on, 1 cited, 0 both, 1 neither; 0 answered, 1 with no answer, citation or read-on (" + third[0] + ")\n", "  expert openshift.networking loaded by the engine\n",
		"  consulted openshift.platform: 1 call, 1 repeating the main conversation's\n",
		"  7 model calls, input tokens 100 100 100 100 100 100 100: 700; cache read 0\n",
		"── totals\n  1 turn, 7 calls, 1 refused, 1 repeated; 1 over budget\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("the rendering lacks %q:\n%s", line, out)
		}
	}
}

// TestTrajectoryCitedByPath: an answer citing a file by path credits the
// item that read that file alone, and a search's item only where its
// result names the file, not every file it opened; an evidence_read given
// an item's full ID reads on in it as its short name would. Synthetic.
func TestTrajectoryCitedByPath(t *testing.T) {
	const (
		operators = "cluster-scoped-resources/config.openshift.io/clusteroperators.yaml"
		master    = "cluster-scoped-resources/core/nodes/master-0.yaml"
		worker    = "cluster-scoped-resources/core/nodes/worker-1.yaml"
	)
	var s *Session
	var n atomic.Int64
	var names [3]string
	s, _ = companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Calling(
				call("c1", "read", map[string]any{"path": operators}),
				call("c2", "grep", map[string]any{"pattern": "KubeletNotReady", "path": "cluster-scoped-resources/core/nodes"}),
				call("c3", "read", map[string]any{"path": worker})), nil
		case 2:
			res := lastResults(r)
			for i, id := range []string{"c1", "c2", "c3"} {
				names[i], _, _ = strings.Cut(res[id].Text, " ")
			}
			full, err := evidenceByName(s.c, names[2])
			if err != nil {
				return nil, err
			}
			return modelfixture.Calling(call("c4", "evidence_read", map[string]any{"id": full})), nil
		}
		return modelfixture.Text("The operators are listed at `" + operators + ":1`, and master-0 at `" + master + ":1`."), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What do the nodes say?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := TrajectoryEvidence{Returned: 3, ReadOn: 1, Cited: 1, Neither: 1, NeitherNames: []string{names[1]}, None: 1, NoneNames: []string{names[1]}}
	if len(got.Turns) != 1 {
		t.Fatalf("turns: %+v", got.Turns)
	}
	if d := cmp.Diff(want, got.Turns[0].Evidence); d != "" {
		t.Errorf("the evidence of %v (-want +got):\n%s", names, d)
	}
}

// TestTrajectoryOfNoConversation: a case with no conversation has a
// trajectory with no turns, said so, and no error.
func TestTrajectoryOfNoConversation(t *testing.T) {
	s, _ := companionSession(t, script())
	got, err := s.Trajectory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"conversations":[],"turns":[]`) || got.Totals.Turns != 0 {
		t.Errorf("the trajectory of no conversation: %s", b)
	}
	if out := RenderTrajectory(got); out != "no turns: the case has no conversation yet\n" {
		t.Errorf("its rendering: %q", out)
	}
}

// TestTrajectoryKeepsTheCheck: a turn's answer the editor had revised
// reports its anchor and the citations the revision added: one naming
// lines, as the check found it, and one naming a whole item the answer did
// not cite. Synthetic.
func TestTrajectoryKeepsTheCheck(t *testing.T) {
	answer := "The operator restarted the DNS pods at 09:00 [e1:2]."
	revised := "The customer reports that the operator restarted the DNS pods at 09:00 [e1:2], after the node rebooted [e2:5] [e3]."
	anchor := map[string]any{"statement": "s1", "kind": "misattributed", "quote": "The operator restarted the DNS pods", "why": "who saw the restart is not in the lines given",
		"suggested": "name who said it, or mark it inferred", "anchor": true}
	s, _ := editorSession(t, answers(answer, revised), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{}, "", anchor), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Who restarted them?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || got.Turns[0].Kind != "talk" {
		t.Fatalf("the trajectory: %+v", got)
	}
	want := &TurnCheck{Answer: "a1", Anchors: 1, Revised: true, AddedLines: []string{"[e2:5]"}, AddedWhole: []string{"[e3]"},
		Calls: []CheckCall{{Kind: CallFirst, Outcome: "called_tools"}, {Kind: CallRevision, Outcome: "answered"}}}
	if d := cmp.Diff(want, got.Turns[0].Check); d != "" {
		t.Errorf("the check (-want +got):\n%s", d)
	}
	if got.Totals.Checked != 1 || got.Totals.Revised != 1 || got.Totals.AddedLines != 1 || got.Totals.AddedWhole != 1 {
		t.Errorf("the totals: %+v", got.Totals)
	}
	if out := RenderTrajectory(got); !strings.Contains(out, "  check of a1: revised for 1 anchor; added with lines [e2:5]; added whole [e3]\n") {
		t.Errorf("the rendering:\n%s", out)
	}
}

// TestTrajectoryCheckCalls: each check's model calls are its own, by kind:
// a call after an answer in text is a retry, and one after the ask about
// statements left unaccounted a follow-up; the totals count each kind.
// Synthetic.
func TestTrajectoryCheckCalls(t *testing.T) {
	var n atomic.Int64
	s, _ := editorSession(t, answers("The DNS pods restarted at 09:00 [e1:2]. Two workers are affected [e1:3].", ""), func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Text("All supported."), nil
		case 2:
			return finding([]any{"s1", "s2"}, ""), nil
		case 3:
			return finding([]any{"s1"}, ""), nil
		}
		if _, ok := followUpOf(r); !ok {
			return nil, errors.New("want the follow-up")
		}
		return finding([]any{"s2"}, ""), nil
	})
	ctx := context.Background()
	for _, ask := range []string{"What restarted?", "And then?"} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, u := range got.Turns {
		for _, c := range u.Check.Calls {
			kinds = append(kinds, u.Check.Answer+" "+c.Kind+" "+c.Outcome)
		}
	}
	want := []string{"a1 first answered", "a1 retry called_tools", "a2 first called_tools", "a2 follow-up called_tools"}
	if d := cmp.Diff(want, kinds); d != "" {
		t.Errorf("the checks' calls (-want +got):\n%s", d)
	}
	if d := cmp.Diff(CheckTimes{CallFirst: {Calls: 2}, CallRetry: {Calls: 1}, CallFollowUp: {Calls: 1}}, got.Totals.CheckTime); d != "" {
		t.Errorf("the totals' check calls (-want +got):\n%s", d)
	}
	out := RenderTrajectory(got)
	for _, line := range []string{"    first 0.0 s answered\n    retry 0.0 s called_tools — answered in text\n", "    first 0.0 s called_tools\n    follow-up 0.0 s called_tools\n",
		"  check calls: first 2, longest 0.0 s; retry 1, longest 0.0 s; follow-up 1, longest 0.0 s\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("the rendering lacks %q:\n%s", line, out)
		}
	}
}

// TestTrajectoryReviews: a review the engineer asked for is listed by what
// it reviewed, with its outcome and its calls, and counted. Synthetic.
func TestTrajectoryReviews(t *testing.T) {
	s, _ := reviewerSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return modelfixture.Text("The DNS pods restarted at 09:00 [e1:2]."), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What restarted?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, ReviewAsk{}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewRun{{Of: "a1", Outcome: checkReviewed, Calls: []CheckCall{{Kind: CallFirst, Outcome: "called_tools"}}}}
	if d := cmp.Diff(want, got.Reviews); d != "" || got.Totals.Reviews != 1 {
		t.Errorf("the reviews (-want +got):\n%s; counted %d", d, got.Totals.Reviews)
	}
	if out := RenderTrajectory(got); !strings.Contains(out, "── reviews\n  review of a1: "+checkReviewed+"\n    first 0.0 s called_tools\n") {
		t.Errorf("the rendering:\n%s", out)
	}
}

// retryCauses answers a check's calls in pairs, one retry from each cause
// a check has: an answer in text, findings the schema refuses whole, and a
// call to a tool there is none of; each pair's second call gives valid
// findings. Synthetic.
func retryCauses() func(context.Context, model.Request) (*modelfixture.Answer, error) {
	var n atomic.Int64
	return func(context.Context, model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Text("All supported."), nil
		case 3:
			return findingsCall(map[string]any{"findings": []any{map[string]any{"statement": "s1", "kind": "certainty", "quote": "restarted"}}, "supported": []any{}}), nil
		case 5:
			return modelfixture.Calling(call("x1", "evidence_read", map[string]any{"name": "e1"})), nil
		}
		return finding([]any{"s1"}, ""), nil
	}
}

// retryReasons are retryCauses's retries' reasons, in order, and
// retryFirst the outcomes of the first calls they follow.
var (
	retryReasons = []string{"answered in text", "findings refused: /findings/0: missing property 'why'", `call refused: There is no tool "evidence_read".`}
	retryFirst   = []string{"answered", "called_tools", "called_tools"}
)

// checkReasons are calls's kinds, outcomes and reasons, one a line.
func checkReasons(calls []CheckCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Kind+" "+c.Outcome+" "+c.Reason)
	}
	return out
}

// TestTrajectoryRetryReasons: each retry of the editor's checks gives why
// it was asked, read from the check's conversation — the first call's
// answer in text, its findings refused whole with the schema's problem, or
// its call to a tool there is none of — in the text, cut to a line, and in
// the JSON; a first call gives none. Synthetic.
func TestTrajectoryRetryReasons(t *testing.T) {
	s, _ := editorSession(t, answers("The DNS pods restarted at 09:00 [e1:2].", ""), retryCauses())
	ctx := context.Background()
	for _, ask := range []string{"What restarted?", "And then?", "And after?"} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 3 {
		t.Fatalf("the turns: %+v", got.Turns)
	}
	out := RenderTrajectory(got)
	for i, u := range got.Turns {
		if d := cmp.Diff([]string{"first " + retryFirst[i] + " ", "retry called_tools " + retryReasons[i]}, checkReasons(u.Check.Calls)); d != "" {
			t.Errorf("%s's calls (-want +got):\n%s", u.Check.Answer, d)
		}
		lines := "\n    first 0.0 s " + retryFirst[i] + "\n    retry 0.0 s called_tools — " + retryReasons[i] + "\n"
		if !strings.Contains(out, lines) {
			t.Errorf("the rendering lacks %q:\n%s", lines, out)
		}
	}
	b, err := json.Marshal(got.Turns[0].Check.Calls)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"kind":"first","outcome":"answered","duration_ns":0},{"kind":"retry","outcome":"called_tools","duration_ns":0,"reason":"answered in text"}]`; string(b) != want {
		t.Errorf("the JSON:\n%s\nwant\n%s", b, want)
	}
	got.Turns[0].Check.Calls[1].Reason = "findings refused: " + strings.Repeat("/findings/0: missing property 'why'; ", 10)
	var cut string
	for l := range strings.Lines(RenderTrajectory(got)) {
		if strings.HasPrefix(l, "    retry 0.0 s called_tools — findings refused: /findings/0: missing property 'why'; ") {
			cut = l
		}
	}
	if !strings.HasSuffix(cut, "…\n") || len([]rune(cut)) > 160 {
		t.Errorf("a long reason is not cut to a line: %q", cut)
	}
}

// TestTrajectoryReviewRetryReasons: each retry of a review gives why it was
// asked, as a check's does, in the text and the JSON; a first call gives
// none. Synthetic.
func TestTrajectoryReviewRetryReasons(t *testing.T) {
	s, _ := reviewerSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return modelfixture.Text("The DNS pods restarted at 09:00 [e1:2]."), nil
	}, retryCauses())
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What restarted?", nil); err != nil {
		t.Fatal(err)
	}
	for range retryReasons {
		if _, err := s.Review(ctx, ReviewAsk{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reviews) != len(retryReasons) {
		t.Fatalf("the reviews: %+v", got.Reviews)
	}
	out := RenderTrajectory(got)
	for i, r := range got.Reviews {
		if d := cmp.Diff([]string{"first " + retryFirst[i] + " ", "retry called_tools " + retryReasons[i]}, checkReasons(r.Calls)); d != "" {
			t.Errorf("review %d's calls (-want +got):\n%s", i+1, d)
		}
		lines := "  review of a1: " + checkReviewed + "\n    first 0.0 s " + retryFirst[i] + "\n    retry 0.0 s called_tools — " + retryReasons[i] + "\n"
		if !strings.Contains(out, lines) {
			t.Errorf("the rendering lacks %q:\n%s", lines, out)
		}
	}
	b, err := json.Marshal(got.Reviews[1].Calls)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"kind":"first","outcome":"called_tools","duration_ns":0},{"kind":"retry","outcome":"called_tools","duration_ns":0,"reason":"findings refused: /findings/0: missing property 'why'"}]`; string(b) != want {
		t.Errorf("the JSON:\n%s\nwant\n%s", b, want)
	}
}

// cachedUsage is a call's usage with most of its input read from the
// provider's cache, all of it reported. Synthetic.
var cachedUsage = model.Usage{InputTokens: model.Reported(1000), CacheReadTokens: model.Reported(4000), CacheWriteTokens: model.Reported(0),
	OutputTokens: model.Reported(20), ReasoningTokens: model.Unavailable, Normalization: model.NormalizationNativeDisjoint}

// TestTrajectoryGuidance: a conversation with the case routing key's expert
// preloaded, a second loaded by the model in its first turn and consulted
// there, and a second turn, gives each turn the guidance the conversation
// held by ID and version, with the tokens the engine's estimator gives each
// load, and what its calls sent of it, each call counting what was loaded
// before it, as a share of their input as reported — uncached and cache
// read — labelled with the estimator; the consult's guidance, its ask's,
// apart; and the totals over both. Synthetic.
func TestTrajectoryGuidance(t *testing.T) {
	var n atomic.Int64
	var asked string
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		var a *modelfixture.Answer
		switch n.Add(1) {
		case 1:
			a = modelfixture.Calling(call("c1", "load_expert", map[string]any{"id": "openshift.platform"}))
		case 2:
			a = modelfixture.Calling(call("c2", "consult", map[string]any{"expert": "openshift.platform", "question": "Are the workers Ready?"}))
		case 3: // the consult
			for _, m := range r.Messages {
				if i := strings.Index(m.Text, "# Expert openshift.platform "); i >= 0 {
					asked = m.Text[i:strings.LastIndex(m.Text, "\n\n## The question")]
				}
			}
			a = modelfixture.Text("Both workers are Ready.")
		case 4:
			a = modelfixture.Text("Both workers are Ready, by the consult.")
		default:
			a = modelfixture.Text("The DNS pods were not checked.")
		}
		a.Usage = cachedUsage
		return a, nil
	})
	ctx := context.Background()
	for _, q := range []string{"Are the workers Ready?", "And the DNS pods?"} {
		if _, err := s.Talk(ctx, q, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var loads []string
	for _, m := range history {
		for _, r := range m.Results {
			if r.Name == "load_expert" {
				loads = append(loads, r.Text)
			}
		}
	}
	if len(loads) != 2 || asked == "" || len(got.Turns) != 2 || len(got.Turns[0].Consults) != 1 {
		t.Fatalf("loads %d, the consult's guidance %q, the trajectory %+v", len(loads), asked, got)
	}
	est := func(v int64) model.Count { return model.Estimated(v, model.EstimatorVersion) }
	share := func(sent, prompt int64) Share {
		return Share{Value: float64(sent) / float64(prompt), Known: true, Estimator: model.EstimatorVersion}
	}
	net, plat, ask := model.EstimateTokens(loads[0]), model.EstimateTokens(loads[1]), model.EstimateTokens(asked)
	held := []GuidanceLoad{{ID: "openshift.networking", Version: "0.1.0", Tokens: est(net)}, {ID: "openshift.platform", Version: "0.1.0", Tokens: est(plat)}}
	const prompt = 5000 // a call's uncached and cache-read input
	// The first call held the preload alone; the second and the answer
	// both.
	first := net + 2*(net+plat)
	want := []GuidanceShare{
		{Experts: held, Tokens: est(net + plat), Calls: 3, Sent: est(first), Prompt: model.Reported(3 * prompt), Share: share(first, 3*prompt)},
		{Experts: held, Tokens: est(net + plat), Calls: 1, Sent: est(net + plat), Prompt: model.Reported(prompt), Share: share(net+plat, prompt)},
	}
	for i, w := range want {
		if d := cmp.Diff(w, got.Turns[i].Guidance); d != "" {
			t.Errorf("turn %d's guidance (-want +got):\n%s", i+1, d)
		}
	}
	consulted := GuidanceShare{Experts: []GuidanceLoad{{ID: "openshift.platform", Version: "0.1.0", Tokens: est(ask)}}, Tokens: est(ask), Calls: 1,
		Sent: est(ask), Prompt: model.Reported(prompt), Share: share(ask, prompt)}
	if d := cmp.Diff(&consulted, got.Turns[0].Consults[0].Guidance); d != "" {
		t.Errorf("the consult's guidance (-want +got):\n%s", d)
	}
	sent := first + net + plat
	wantTotal := GuidanceShare{Experts: held, Tokens: est(net + plat), Calls: 4, Sent: est(sent), Prompt: model.Reported(4 * prompt), Share: share(sent, 4*prompt)}
	if d := cmp.Diff(wantTotal, got.Totals.Guidance); d != "" {
		t.Errorf("the totals' guidance (-want +got):\n%s", d)
	}
	if d := cmp.Diff(consulted, got.Totals.ConsultGuidance); d != "" {
		t.Errorf("the totals' consult guidance (-want +got):\n%s", d)
	}
	b, _ := json.Marshal(got.Turns[0].Guidance)
	if !strings.Contains(string(b), fmt.Sprintf(`"share_of_prompt":{"value":%v,"estimator_version":"%s"}`, share(first, 3*prompt).Value, model.EstimatorVersion)) {
		t.Errorf("the first turn's guidance as JSON: %s", b)
	}
	both := fmt.Sprintf("openshift.networking 0.1.0 %d, openshift.platform 0.1.0 %d: %d tokens; ", net, plat, net+plat)
	label := " of their input (estimated, " + model.EstimatorVersion + ")\n"
	out := RenderTrajectory(got)
	for _, line := range []string{
		"  guidance " + both + fmt.Sprintf("%d sent over 3 model calls, %.1f%%", first, 100*share(first, 3*prompt).Value) + label,
		fmt.Sprintf("    guidance openshift.platform 0.1.0 %d: %d tokens; %d sent over 1 model call, %.1f%%", ask, ask, ask, 100*share(ask, prompt).Value) + label,
		"  guidance " + both + fmt.Sprintf("%d sent over 1 model call, %.1f%%", net+plat, 100*share(net+plat, prompt).Value) + label,
		"── totals\n", "  guidance " + both + fmt.Sprintf("%d sent over 4 model calls, %.1f%%", sent, 100*share(sent, 4*prompt).Value) + label,
		fmt.Sprintf("  consults' guidance openshift.platform 0.1.0 %d: %d tokens; %d sent over 1 model call", ask, ask, ask),
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the rendering lacks %q:\n%s", line, out)
		}
	}
}

// TestTrajectoryGuidanceUnreported: a turn one of whose calls has no
// usage reported gives the guidance it sent and no share of the input,
// unavailable and never zero, nor of the calls whose input was reported;
// the totals too. Synthetic.
func TestTrajectoryGuidanceUnreported(t *testing.T) {
	unreported := model.Usage{InputTokens: model.Unavailable, CacheReadTokens: model.Unavailable, CacheWriteTokens: model.Unavailable,
		OutputTokens: model.Unavailable, ReasoningTokens: model.Unavailable, Normalization: model.NormalizationAmbiguous}
	reported := modelfixture.Calling(call("c1", "grep", map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"}))
	reported.Usage = cachedUsage
	answer := modelfixture.Text("The OVN controller logs errors.")
	answer.Usage = unreported
	s, _ := companionSession(t, script(reported, answer))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 {
		t.Fatalf("the trajectory: %+v", got)
	}
	for name, g := range map[string]GuidanceShare{"the turn's": got.Turns[0].Guidance, "the totals'": got.Totals.Guidance} {
		if len(g.Experts) != 1 || g.Experts[0].ID != "openshift.networking" || g.Calls != 2 || g.Sent.Value != 2*g.Tokens.Value ||
			g.Prompt.Known || g.Unavailable != 1 || g.Share.Known {
			t.Errorf("%s guidance: %+v", name, g)
		}
		b, _ := json.Marshal(g)
		if !strings.Contains(string(b), `"prompt_tokens":"unavailable","unavailable_calls":1,"share_of_prompt":"unavailable"`) {
			t.Errorf("%s guidance as JSON: %s", name, b)
		}
	}
	if out := RenderTrajectory(got); strings.Count(out, " sent over 2 model calls, share of their input unavailable, 1 call unreported (estimated, "+model.EstimatorVersion+")\n") != 2 {
		t.Errorf("the rendering:\n%s", out)
	}
}

// TestTrajectoryCacheOutcomes: each model call gives what its provider did
// with a cache of its own as its record holds it — made, a make refused
// with the provider's error and the older cache read, or none recorded —
// in the JSON whole and in the text by the error's first line. Synthetic.
func TestTrajectoryCacheOutcomes(t *testing.T) {
	tokens := model.Estimated(5000, model.EstimatorVersion)
	made := modelfixture.Calling(call("c1", "grep", map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"}))
	made.Cache = &model.Cache{Outcome: model.CacheMade, Name: "caches/c1", Messages: 1, Tokens: &tokens}
	refused := modelfixture.Calling(call("c2", "grep", map[string]any{"pattern": "(?i)timeout", "path": "namespaces/openshift-ovn-kubernetes"}))
	refused.Cache = &model.Cache{Outcome: model.CacheRefused, Name: "caches/c1", Messages: 1, Tokens: &tokens,
		Error: "Error 400, Message: Requests ending with a model turn are not supported.\n=== Source Location Trace: ==="}
	s, _ := companionSession(t, script(made, refused, modelfixture.Text("The OVN controller logs errors.")))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || len(got.Turns[0].ModelCalls) != 3 {
		t.Fatalf("the trajectory: %+v", got)
	}
	calls := got.Turns[0].ModelCalls
	if d := cmp.Diff([]*model.Cache{made.Cache, refused.Cache, nil}, []*model.Cache{calls[0].Cache, calls[1].Cache, calls[2].Cache}); d != "" {
		t.Errorf("the calls' caches (-want +got):\n%s", d)
	}
	b, _ := json.Marshal(calls[1])
	if !strings.Contains(string(b), `"cache":{"outcome":"refused","name":"caches/c1","messages":1,"tokens":{"value":5000,"fidelity":"estimated","estimator_version":"`+model.EstimatorVersion+`"},"error":"Error 400, Message: Requests ending with a model turn are not supported.\n=== Source Location Trace: ==="}`) {
		t.Errorf("the refused call as JSON: %s", b)
	}
	want := "  cache by call: made (1 message); create refused: Error 400, Message: Requests ending with a model turn are not supported., reused (1 message); none recorded\n"
	if out := RenderTrajectory(got); !strings.Contains(out, want) {
		t.Errorf("the rendering:\n%s", out)
	}
}

// TestTrajectoryGuidanceAfterCompaction: the conversation a compaction
// starts holds none of the guidance its ancestor loaded until it is loaded
// again, so its turn's calls send none of it. Synthetic.
func TestTrajectoryGuidanceAfterCompaction(t *testing.T) {
	s, _ := companionSession(t, script(modelfixture.Text("The workers are Ready."), modelfixture.Text("Handoff: the workers are Ready."),
		modelfixture.Text("Nothing else is known.")))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Are the workers Ready?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "What else?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conversations) != 2 || len(got.Turns) < 2 {
		t.Fatalf("the trajectory: %+v", got)
	}
	before, after := got.Turns[0].Guidance, got.Turns[len(got.Turns)-1].Guidance
	if len(before.Experts) != 1 || before.Sent.Value != before.Tokens.Value || before.Tokens.Value == 0 {
		t.Errorf("before the compaction: %+v", before)
	}
	if len(after.Experts) != 0 || after.Calls != 1 || after.Sent.Value != 0 || !after.Share.Known || after.Share.Value != 0 {
		t.Errorf("after the compaction: %+v", after)
	}
}

// TestTurnKindReadsBothAnalyzeAsks: a stored /analyze turn reads as one
// whether it was asked for a structured analysis, as before the
// observations report, or for the report.
func TestTurnKindReadsBothAnalyzeAsks(t *testing.T) {
	for _, ask := range []string{analyzeAsk, analyzeAskBefore} {
		m := ConversationMessage{}
		m.Kind, m.Text = MessageEngine, ask
		if got := turnKind(m); got != "analyze" {
			t.Errorf("%q reads as %q", ask, got)
		}
	}
}

// TestTrajectoryReferences: the trajectory records read_reference calls
// (read and refused, in the main conversation and in consults), counts
// non-refused reads in Totals.References, renders them, and includes read
// reference tokens in GuidanceShare without double-counting repeat reads.
func TestTrajectoryReferences(t *testing.T) {
	td := t.TempDir()
	writeRefExpert(t, td, "refs", "fixture.refs", "0.1.0",
		map[string]string{"references/guide.md": "# Guide\n\nSynthetic guide body for tokens.\n"},
		map[string]string{"references/guide.md": "Guide"},
		"Read `references/guide.md`.\n",
	)
	var step atomic.Int64
	s, _ := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		var a *modelfixture.Answer
		switch step.Add(1) {
		case 1:
			a = modelfixture.Calling(
				call("c1", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"}),
				call("c2", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/missing.md"}),
			)
		case 2:
			// Repeat read: should record in References, not double-count GuidanceLoad.
			a = modelfixture.Calling(
				call("c3", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"}),
				call("c4", "consult", map[string]any{"expert": "fixture.refs", "question": "Check the guide."}),
			)
		case 3: // consult
			a = modelfixture.Calling(call("k1", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/guide.md"}))
		case 4: // consult answer
			a = modelfixture.Text("Consult done.")
		default:
			a = modelfixture.Text("Main done.")
		}
		a.Usage = cachedUsage
		return a, nil
	}, td)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Read references.", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || len(got.Turns[0].Consults) != 1 {
		t.Fatalf("unexpected trajectory: %+v", got)
	}
	turn := got.Turns[0]
	wantRefs := []ReferenceRead{
		{Expert: "openshift.networking", Path: "references/ovn.md"},
		{Expert: "openshift.networking", Path: "references/missing.md", Refused: true},
		{Expert: "openshift.networking", Path: "references/ovn.md", Repeated: true},
	}
	if d := cmp.Diff(wantRefs, turn.References); d != "" {
		t.Errorf("turn references (-want +got):\n%s", d)
	}
	wantConsultRefs := []ReferenceRead{{Expert: "fixture.refs", Path: "references/guide.md"}}
	if d := cmp.Diff(wantConsultRefs, turn.Consults[0].References); d != "" {
		t.Errorf("consult references (-want +got):\n%s", d)
	}
	if got.Totals.References != 1 {
		t.Errorf("Totals.References = %d, want 1", got.Totals.References)
	}
	// GuidanceLoad should include openshift.networking (load) + references/ovn.md once (not repeat).
	if len(turn.Guidance.Experts) != 2 || turn.Guidance.Experts[1].Path != "references/ovn.md" {
		t.Errorf("turn guidance experts = %+v, want preload + one reference", turn.Guidance.Experts)
	}
	if cg := turn.Consults[0].Guidance; cg == nil || len(cg.Experts) != 2 || cg.Experts[1].Path != "references/guide.md" {
		t.Errorf("consult guidance experts = %+v, want consult ask + reference", cg)
	}
	out := RenderTrajectory(got)
	for _, want := range []string{
		"  reference openshift.networking references/ovn.md read\n",
		"  reference openshift.networking references/missing.md refused\n",
		"  reference openshift.networking references/ovn.md already read\n",
		"    reference fixture.refs references/guide.md read\n",
		"  1 reference read\n",
		"openshift.networking 0.1.0 references/ovn.md ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trajectory output lacks %q:\n%s", want, out)
		}
	}
}

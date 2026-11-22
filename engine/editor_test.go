package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// The editor's tests run on the companion's stub case, its answers and the
// editor's findings synthetic: e1 is the case's details, whose second line
// the answers cite.

// isEditorCall reports whether a request is the editor's.
func isEditorCall(r model.Request) bool {
	return r.System == check.EditorPrompt() && len(r.Tools) == 1 && r.Tools[0].Name == check.ToolName
}

// isRevision reports whether a request asks the main model for a revision.
func isRevision(r model.Request) bool {
	return len(r.Messages) > 0 && strings.HasPrefix(r.Messages[len(r.Messages)-1].Text, "# Revise your answer")
}

// finding is a check_findings call's arguments; supported and corrected
// as given, the rest the findings.
func finding(supported []any, corrected string, findings ...any) *modelfixture.Answer {
	args := map[string]any{"findings": append([]any{}, findings...), "supported": supported}
	if corrected != "" {
		args["corrected"] = corrected
	}
	return modelfixture.Calling(call("f1", check.ToolName, args))
}

// editorSession is the companion's stub case with the editor configured on
// the fixture model: main answers the companion's calls and edit the
// editor's.
func editorSession(t *testing.T, main, edit func(context.Context, model.Request) (*modelfixture.Answer, error)) (*Session, *modelfixture.Provider) {
	t.Helper()
	s, f := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isEditorCall(r) {
			return edit(ctx, r)
		}
		return main(ctx, r)
	})
	s.e.config.Reasoning.Profiles["editor"] = config.Profile{Model: "fixture-model"}
	return s, f
}

// answers answers the companion's calls with text: the turn's answer, and
// a revision asked for with revised.
func answers(answer, revised string) func(context.Context, model.Request) (*modelfixture.Answer, error) {
	return func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isRevision(r) {
			return modelfixture.Text(revised), nil
		}
		return modelfixture.Text(answer), nil
	}
}

func editorCalls(f *modelfixture.Provider) (editor, main []model.Request) {
	for _, r := range f.Requests() {
		if isEditorCall(r) {
			editor = append(editor, r)
		} else {
			main = append(main, r)
		}
	}
	return editor, main
}

// mainMessages are the stored messages of the case's main conversation.
func mainMessages(t *testing.T, s *Session) []companion.Message {
	t.Helper()
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Messages(context.Background(), conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// checkOutcomes are the kept checks' outcomes, in order.
func checkOutcomes(t *testing.T, s *Session) []string {
	t.Helper()
	rows, err := s.c.st.DB().QueryContext(t.Context(), `SELECT outcome FROM checks ORDER BY created_at, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

const (
	certainAnswer    = "The upgrade confirms that the DNS timeouts were caused by worker-1 [e1:2]. Pods on two workers time out resolving service names [e1:2]."
	certainCorrected = "The DNS timeouts began after the upgrade, on worker-1 [e1:2]. Pods on two workers time out resolving service names [e1:2]."
)

// certainFinding is the editor's finding on certainAnswer's s1.
var certainFinding = map[string]any{"statement": "s1", "kind": "certainty", "quote": "confirms that the DNS timeouts were caused by worker-1",
	"why": "e1:2 says the timeouts began after the upgrade, not what caused them", "evidence": []any{"e1:2"}, "suggested": "the DNS timeouts began after the upgrade"}

// TestEditorCorrectsWording: [E2] an answer needing wording only is
// printed as the editor corrected it, with no call of the main model: the
// editor is called once in a conversation of its own, with its prompt and
// one tool, given the certainty candidate, and recorded as answers.edit;
// the answer stays stored byte for byte, the correction after it as a
// message of its own, which the main model is given at its next call.
func TestEditorCorrectsWording(t *testing.T) {
	s, f := editorSession(t, answers(certainAnswer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, certainCorrected, certainFinding), nil
	})
	ctx := context.Background()
	reply, err := s.Talk(ctx, "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != certainCorrected || !slices.Equal(reply.Check.Words(), []string{"corrected by the editor: 1 statement"}) || !reply.Check.Changed() {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	editor, main := editorCalls(f)
	if len(editor) != 1 || len(main) != 1 {
		t.Fatalf("the editor called %d times and the main model %d; want once each", len(editor), len(main))
	}
	if req := editor[0].Messages[0].Text; !strings.Contains(req, `s1 holds "confirms", "caused": whether in its own voice`) || !strings.Contains(req, "## The answer as written\n\n"+certainAnswer) {
		t.Errorf("the editor's request:\n%s", req)
	}
	msgs := mainMessages(t, s)
	n := len(msgs)
	if msgs[n-2].Kind != companion.KindModel || msgs[n-2].Text != certainAnswer || msgs[n-1].Kind != companion.KindCorrection || msgs[n-1].Text != companion.CorrectionText(certainCorrected) {
		t.Errorf("the conversation ends %q %q, %q %q", msgs[n-2].Kind, msgs[n-2].Text, msgs[n-1].Kind, msgs[n-1].Text)
	}
	var task string
	if err := s.c.st.DB().QueryRow(`SELECT task_type FROM invocations ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&task); err != nil || task != EditorTask {
		t.Errorf("the last call recorded as %q, %v", task, err)
	}
	if d := cmp.Diff([]string{checkCorrected}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
	if _, err := s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	_, main = editorCalls(f)
	next := main[len(main)-1].Messages
	if !slices.ContainsFunc(next, func(m model.Message) bool { return m.Text == companion.CorrectionText(certainCorrected) }) {
		t.Error("the main model's next call is not given the correction")
	}
}

// TestEditorAnchorRevises: [E2] an anchor makes exactly one call of the
// main model, which offers no tool and is given the editor's corrected
// text and the anchor; its revision is printed, stored after the engine's
// ask, the answer byte for byte before both, and named as the answer.
func TestEditorAnchorRevises(t *testing.T) {
	answer := "The operator restarted the DNS pods at 09:00 [e1:2]. The upgrade confirms that the DNS timeouts were caused by worker-1 [e1:2]."
	corrected := "The operator restarted the DNS pods at 09:00 [e1:2]. The DNS timeouts began after the upgrade, on worker-1 [e1:2]."
	revised := "The customer reports that the operator restarted the DNS pods at 09:00 [e1:2]. The DNS timeouts began after the upgrade, on worker-1 [e1:2]."
	anchor := map[string]any{"statement": "s1", "kind": "misattributed", "quote": "The operator restarted the DNS pods", "why": "who saw the restart is not in the lines given",
		"suggested": "name who said it, or mark it inferred", "anchor": true}
	fix := map[string]any{"statement": "s2", "kind": "certainty", "quote": "confirms that the DNS timeouts were caused by worker-1", "why": "e1:2 gives no cause", "suggested": "the DNS timeouts began after the upgrade"}
	s, f := editorSession(t, answers(answer, revised), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{}, corrected, anchor, fix), nil
	})
	reply, err := s.Talk(context.Background(), "Who restarted them?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != revised || !reply.Check.Revised || len(reply.Check.Anchors) != 1 ||
		!slices.Equal(reply.Check.Words(), []string{"corrected by the editor: 1 statement", "revised for 1 anchor"}) {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	editor, main := editorCalls(f)
	if len(editor) != 1 || len(main) != 2 {
		t.Fatalf("the editor called %d times and the main model %d; want once and twice", len(editor), len(main))
	}
	rev := main[1]
	ask := rev.Messages[len(rev.Messages)-1].Text
	if !rev.NoCalls || !strings.Contains(ask, AnchorLine(CheckFinding{Statement: "s1", Kind: "misattributed", Quote: "The operator restarted the DNS pods", Suggested: "name who said it, or mark it inferred"})) || !strings.Contains(ask, corrected) {
		t.Errorf("the revision's request, no calls %v:\n%s", rev.NoCalls, ask)
	}
	msgs := mainMessages(t, s)
	n := len(msgs)
	if msgs[n-3].Kind != companion.KindModel || msgs[n-3].Text != answer || msgs[n-2].Kind != companion.KindRevise || msgs[n-1].Kind != companion.KindModel || msgs[n-1].Text != revised {
		t.Errorf("the conversation ends %q, %q, %q", msgs[n-3].Kind, msgs[n-2].Kind, msgs[n-1].Kind)
	}
	history, err := s.ConversationHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := answersOf(history); len(got) != 1 || got[0].label != "a1" || got[0].text != revised {
		t.Errorf("the answers as shown: %+v", got)
	}
	if d := cmp.Diff([]string{checkRevised}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
}

// TestEditorNoSecondRound: [E2] a revision still failing the deterministic
// checks — a citation that resolves to nothing — is what is printed, with
// its anchors, and nothing checks it again; a revision that fails leaves
// the editor's corrected text, its anchors listed.
func TestEditorNoSecondRound(t *testing.T) {
	answer := "The alert fired with a 300 s threshold [e1:2]."
	anchor := map[string]any{"statement": "s1", "kind": "unsupported", "quote": "fired with a 300 s threshold", "why": "no line given holds the rule",
		"suggested": "cite the lines that hold the rule, or say no evidence holds it", "anchor": true}
	revised := "The alert fired with a 300 s threshold [e999:2]."
	edit := func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{}, "", anchor), nil
	}
	s, f := editorSession(t, answers(answer, revised), edit)
	ctx := context.Background()
	reply, err := s.Talk(ctx, "What fired?", nil)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := s.CheckCites(ctx, Cites(reply.Text))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != revised || len(checks) != 1 || checks[0].OK || !reply.Check.Revised || len(reply.Check.Anchors) != 1 {
		t.Errorf("the reply %q, its citation %+v, %+v", reply.Text, checks, reply.Check)
	}
	if editor, main := editorCalls(f); len(editor) != 1 || len(main) != 2 {
		t.Errorf("the editor called %d times and the main model %d; want once and twice", len(editor), len(main))
	}

	s, f = editorSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isRevision(r) {
			return nil, errors.New("a synthetic transport failure")
		}
		return modelfixture.Text(certainAnswer), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		anchored := map[string]any{"statement": "s2", "kind": "unsupported", "quote": "time out resolving service names", "why": "who saw it is not given", "suggested": "name who saw it", "anchor": true}
		return finding([]any{}, certainCorrected, certainFinding, anchored), nil
	})
	reply, err = s.Talk(ctx, "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != certainCorrected || reply.Check.Revised || reply.Check.RevisionFailed == "" || len(reply.Check.Anchors) != 1 {
		t.Errorf("a failed revision: %q, %+v", reply.Text, reply.Check)
	}
	if editor, main := editorCalls(f); len(editor) != 1 || len(main) != 2 {
		t.Errorf("the editor called %d times and the main model %d; want once and twice", len(editor), len(main))
	}
	if d := cmp.Diff([]string{checkRevisionFailed}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
}

// TestEditorCleanAnswer: [E2] an answer the editor finds supported is
// printed as written, with no correction stored and no main-model call.
func TestEditorCleanAnswer(t *testing.T) {
	answer := "Pods on two workers time out resolving service names [e1:2]."
	s, f := editorSession(t, answers(answer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	reply, err := s.Talk(context.Background(), "What fails?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != answer || !reply.Check.Ran || reply.Check.Changed() || len(reply.Check.Words()) != 0 {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	if msgs := mainMessages(t, s); msgs[len(msgs)-1].Kind != companion.KindModel || msgs[len(msgs)-1].Text != answer {
		t.Errorf("the conversation ends with a %s message: %q", msgs[len(msgs)-1].Kind, msgs[len(msgs)-1].Text)
	}
	if editor, main := editorCalls(f); len(editor) != 1 || len(main) != 1 {
		t.Errorf("the editor called %d times and the main model %d; want once each", len(editor), len(main))
	}
	if d := cmp.Diff([]string{checkClean}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
}

// TestEditorFailsOpen: [E2] an editor that fails, or passes its bound,
// leaves the answer printed as written and said unchecked; the turn goes
// on, and nothing is added to the conversation.
func TestEditorFailsOpen(t *testing.T) {
	for _, c := range []struct {
		name, why string
		edit      func(context.Context, model.Request) (*modelfixture.Answer, error)
	}{
		{"failing", "the editor failed: ", func(context.Context, model.Request) (*modelfixture.Answer, error) {
			return nil, errors.New("a synthetic transport failure")
		}},
		{"slow", "the editor passed its 100ms bound", func(ctx context.Context, _ model.Request) (*modelfixture.Answer, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func(was time.Duration) { editorBound = was }(editorBound)
			editorBound = 100 * time.Millisecond
			s, _ := editorSession(t, answers(certainAnswer, ""), c.edit)
			reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Text != certainAnswer || !strings.HasPrefix(reply.Check.Unchecked, c.why) || reply.Check.Words()[0] != "unchecked: "+reply.Check.Unchecked {
				t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
			}
			if msgs := mainMessages(t, s); msgs[len(msgs)-1].Text != certainAnswer {
				t.Errorf("the conversation ends with %q", msgs[len(msgs)-1].Text)
			}
			if d := cmp.Diff([]string{checkUnchecked}, checkOutcomes(t, s)); d != "" {
				t.Errorf("checks kept (-want +got):\n%s", d)
			}
		})
	}
}

// TestEditorDraftKeepsCorrection: [E2] a draft is kept as the engineer is
// shown it — corrected, with [eN] citations stripped outside fenced blocks
// and inline code — while the turn's reply and the stored messages retain
// [eN:lines] citations for checking, ranking and review.
func TestEditorDraftKeepsCorrection(t *testing.T) {
	s, _ := editorSession(t, answers(certainAnswer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, certainCorrected, certainFinding), nil
	})
	s.e.config.Comments.Intentions = []config.Intention{barredIntention}
	it, chk, err := s.Draft(context.Background(), barredIntention.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	stripped := StripDraftCites(certainCorrected)
	if chk.Text != certainCorrected || it.Text != stripped || strings.Contains(it.Text, "[e1:2]") || len(chk.Check.Corrected) != 1 {
		t.Errorf("the draft kept %q, reply %q, %+v", it.Text, chk.Text, chk)
	}
	items, err := s.Items(context.Background())
	if err != nil || len(items) != 1 || items[0].Text != stripped {
		t.Errorf("the items %+v, %v", items, err)
	}
	if msgs := mainMessages(t, s); !slices.ContainsFunc(msgs, func(m companion.Message) bool { return m.Kind == companion.KindModel && m.Text == certainAnswer }) {
		t.Error("the model's draft is not stored as written")
	}
}

func TestStripDraftCitesAndCustomIntention(t *testing.T) {
	raw := strings.Join([]string{
		"Observed in `/var/log/messages` (`[e1:2]` is literal code) on worker-1 [e1:2]:",
		"",
		"- Sync timed out ([e1:2], [e4:8]).",
		"- Next read [e1:2, 5–7] and [link](https://example.com) or [e1](https://example.com).",
		"  ```",
		"  Oct 29 11:27 worker-1 ovnkube[12]: [e1:2] sync error",
		"  ```",
	}, "\n")
	want := strings.Join([]string{
		"Observed in `/var/log/messages` (`[e1:2]` is literal code) on worker-1:",
		"",
		"- Sync timed out.",
		"- Next read and [link](https://example.com) or [e1](https://example.com).",
		"  ```",
		"  Oct 29 11:27 worker-1 ovnkube[12]: [e1:2] sync error",
		"  ```",
	}, "\n")
	if got := StripDraftCites(raw); got != want {
		t.Errorf("StripDraftCites (-want +got):\n%s", cmp.Diff(want, got))
	}

	citedDraft := "Observed in `details.txt` on worker-1 [e1:2]:\n\n```\nPods on two workers time out resolving service names.\n```"
	s, f := reviewerSession(t, answers(citedDraft, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "none"))
	baseEng, err := New(Options{Cases: StubCases("../testdata/cases"), Artifacts: LocalDir(home), Home: home})
	if err != nil {
		t.Fatal(err)
	}
	s.e.config.Comments = baseEng.config.Comments
	ctx := context.Background()
	it, reply, err := s.Draft(ctx, "internal focus on worker-1 DNS timeouts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Text, "[e1:2]") || strings.Contains(it.Text, "[e1:2]") || !strings.Contains(it.Text, "```\nPods on two workers") {
		t.Errorf("draft reply %q, item %q", reply.Text, it.Text)
	}
	_, main := reviewerCalls(f)
	internalAsk := main[len(main)-1].Messages[len(main[len(main)-1].Messages)-1].Text
	if !strings.Contains(internalAsk, "Draft Internal note (internal)") ||
		!strings.Contains(internalAsk, "Engineer's instructions for this draft: focus on worker-1 DNS timeouts") ||
		strings.Contains(internalAsk, "smoking gun") {
		t.Errorf("internal draft request:\n%s", internalAsk)
	}
	if _, _, err := s.Draft(ctx, "update ask for ovnkube logs", nil); err != nil {
		t.Fatal(err)
	}
	_, main = reviewerCalls(f)
	updateAsk := main[len(main)-1].Messages[len(main[len(main)-1].Messages)-1].Text
	if !strings.Contains(updateAsk, "Draft Customer update (update)") ||
		!strings.Contains(updateAsk, "Engineer's instructions for this draft: ask for ovnkube logs") ||
		!strings.Contains(updateAsk, "Never state a cause as found in your own voice") {
		t.Errorf("customer update draft request:\n%s", updateAsk)
	}
	rA1, err := s.Review(ctx, ReviewAsk{Item: "a1"}, nil)
	if err != nil || rA1.Of != "a1" {
		t.Fatalf("/review a1 on draft: %+v, %v", rA1, err)
	}
	reviewer, _ := reviewerCalls(f)
	if req := reviewer[len(reviewer)-1].Messages[0].Text; !strings.Contains(req, "the draft a1 for internal") || !strings.Contains(req, "[e1:2]") || !strings.Contains(req, "### e1") {
		t.Errorf("/review a1 did not quote cited evidence from stored draft:\n%s", req)
	}
}

// TestEditorBarredMakesNoCall: [E2] a case barred by the time its answer is
// checked makes no editor call, its refusal recorded as barred_account
// under answers.edit with no record holding the account; the answer is
// printed as written, said unchecked.
func TestEditorBarredMakesNoCall(t *testing.T) {
	var list string
	_, s, f, _ := barredCase(t, "1000000001", func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isEditorCall(r) {
			return finding([]any{"s1", "s2"}, ""), nil
		}
		if err := os.WriteFile(list, []byte("1000000001\n"), 0o600); err != nil {
			return nil, err
		}
		return modelfixture.Text(certainAnswer), nil
	}, "# nobody yet")
	list = s.e.config.Reasoning.BarredAccounts.List
	s.e.config.Reasoning.Profiles["editor"] = config.Profile{Model: "fixture-model"}
	reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if editor, main := editorCalls(f); len(editor) != 0 || len(main) != 1 {
		t.Errorf("the editor called %d times and the main model %d; want none and once", len(editor), len(main))
	}
	if reply.Text != certainAnswer || !strings.Contains(reply.Check.Unchecked, "barred") {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	var task, outcome string
	if err := s.c.st.DB().QueryRow(`SELECT task_type, outcome FROM invocations WHERE task_type = ?`, EditorTask).Scan(&task, &outcome); err != nil || outcome != companion.OutcomeBarred {
		t.Errorf("the editor's record %q %q, %v", task, outcome, err)
	}
	if _, holds := invocationOutcomes(t, s.c, "1000000001"); holds {
		t.Error("an invocation record holds the account number")
	}
}

// TestEditorRequestRedacted: [E2] a synthetic token in a line the answer
// cites is redacted in the editor's request as sent, by the provider's
// egress policy.
func TestEditorRequestRedacted(t *testing.T) {
	const token = "synthetic0token0abcdefghijkl"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.log"), []byte("kubelet started\nAuthorization: Bearer "+token+"\nkubelet ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var name atomic.Value
	s, f := editorSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
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
	if _, err := s.Talk(context.Background(), "What does node.log say?", nil); err != nil {
		t.Fatal(err)
	}
	editor, _ := editorCalls(f)
	if len(editor) != 1 {
		t.Fatalf("the editor called %d times", len(editor))
	}
	req := editor[0].Messages[0].Text
	if strings.Contains(req, token) || !strings.Contains(req, "Authorization: Bearer [REDACTED:token]") {
		t.Errorf("the editor's request as sent:\n%s", req)
	}
}

// TestEditorLeavesOutExcludedComment: [E2] the lines of a comment the
// engineer leaves out of what models read are not quoted to the editor,
// though the answer cites them.
func TestEditorLeavesOutExcludedComment(t *testing.T) {
	var cite atomic.Pointer[string]
	none := ""
	cite.Store(&none)
	s, f := editorSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return modelfixture.Text("Support asked for a fresh must-gather " + *cite.Load() + "."), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, ""), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What was asked?", nil); err != nil {
		t.Fatal(err)
	}
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
	if _, err := excludeComments(s.c, ExcludeComment, "c2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Again?", nil); err != nil {
		t.Fatal(err)
	}
	editor, _ := editorCalls(f)
	req := editor[len(editor)-1].Messages[0].Text
	if strings.Contains(req, "DNS operator looks healthy") || !strings.Contains(req, "not quoted: the engineer's rules leave it out of what models read") {
		t.Errorf("the editor's request:\n%s", req)
	}
}

// TestEditorCountsInTotalUsage: [E2] the editor's calls count in the
// case's total, as the status bar and /usage show it, beside the turn's.
func TestEditorCountsInTotalUsage(t *testing.T) {
	s, _ := editorSession(t, answers(certainAnswer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, certainCorrected, certainFinding), nil
	})
	if _, err := s.Talk(context.Background(), "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	total, err := s.CompanionTotalUsage()
	if err != nil || total.Invocations != 2 {
		t.Errorf("the total counts %d calls, %v; want the turn's and the editor's", total.Invocations, err)
	}
}

// TestEditorNotConfigured: a deployment with no editor profile runs no
// editor: the answer is printed as written, said nothing of.
func TestEditorNotConfigured(t *testing.T) {
	s, f := companionSession(t, answers(certainAnswer, ""))
	reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != certainAnswer || reply.Check.Ran || s.EditsAnswers() || len(f.Requests()) != 1 {
		t.Errorf("with no editor: %q, %+v, %d calls", reply.Text, reply.Check, len(f.Requests()))
	}
}

// partial is the fixture provider but for a revision, which it answers
// with text cut at the output limit: what a failed call leaves stored.
type partial struct{ *modelfixture.Provider }

func (p partial) Call(ctx context.Context, r model.Request, stream func(model.Delta)) (*model.Response, error) {
	if isRevision(r) {
		return &model.Response{Text: "The customer reports that the operator", Usage: modelfixture.Usage()}, model.ErrOutputLimit
	}
	return p.Provider.Call(ctx, r, stream)
}

// anchorFinding is the editor's anchor on certainAnswer's s2.
var anchorFinding = map[string]any{"statement": "s2", "kind": "unsupported", "quote": "time out resolving service names", "why": "who saw it is not given", "suggested": "name who saw it", "anchor": true}

// TestEditorRevisionNotTakenIsKept: [E2] whatever way a revision fails — a
// transport failure, an empty answer, an answer cut short, a tool called —
// what the engineer was shown, the editor's corrected text, is kept after
// it as a message of its own: reopening, /pin, the answers' numbering and
// a reload's last exchange all read it, and the next call gives it to
// the model.
func TestEditorRevisionNotTakenIsKept(t *testing.T) {
	for _, c := range []struct {
		name   string
		revise func() (*modelfixture.Answer, error)
	}{
		{"transport", func() (*modelfixture.Answer, error) { return nil, errors.New("a synthetic transport failure") }},
		{"empty", func() (*modelfixture.Answer, error) { return modelfixture.Text(""), nil }},
		{"partial", nil},
		{"tool", func() (*modelfixture.Answer, error) {
			return &modelfixture.Answer{Pieces: []string{"A revision with a call."}, Calls: []model.ToolCall{call("g1", "grep", map[string]any{"pattern": "x", "path": "."})}}, nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f := editorSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
				if isRevision(r) {
					return c.revise()
				}
				return modelfixture.Text(certainAnswer), nil
			}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
				return finding([]any{}, certainCorrected, certainFinding, anchorFinding), nil
			})
			if c.revise == nil {
				s.e.models["fixture"] = partial{f}
			}
			ctx := context.Background()
			reply, err := s.Talk(ctx, "Why do the pods time out?", nil)
			if err != nil {
				t.Fatal(err)
			}
			if reply.Text != certainCorrected || reply.Check.RevisionFailed == "" {
				t.Fatalf("the reply %q, %+v", reply.Text, reply.Check)
			}
			msgs := mainMessages(t, s)
			if last := msgs[len(msgs)-1]; last.Kind != companion.KindShown || last.Text != companion.ShownText(certainCorrected) {
				t.Errorf("the conversation ends with %s %q", last.Kind, last.Text)
			}
			history, err := s.ConversationHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			views := AnswerViews(history)
			if len(views) != 1 || views[0].Text != certainCorrected || !views[0].Corrected || history[views[0].Original].Check == nil {
				t.Errorf("the answers as shown: %+v", views)
			}
			if ex := lastExchange(history); !strings.Contains(ex, certainCorrected) || strings.Contains(ex, "were caused by") {
				t.Errorf("the last exchange:\n%s", ex)
			}
			it, err := s.Pin(ctx)
			if err != nil || it.Text != certainCorrected {
				t.Errorf("/pin kept %q, %v", it.Text, err)
			}
			if _, err := s.Talk(ctx, "And now?", nil); err != nil {
				t.Fatal(err)
			}
			_, main := editorCalls(f)
			next := main[len(main)-1].Messages
			if !slices.ContainsFunc(next, func(m model.Message) bool { return m.Text == companion.ShownText(certainCorrected) }) {
				t.Error("the main model's next call is not given what was shown")
			}
			if history, _ = s.ConversationHistory(ctx); len(AnswerViews(history)) != 2 {
				t.Errorf("after another turn the answers are %+v", AnswerViews(history))
			}
		})
	}
}

// TestEditorCancelled: [E2] a turn cancelled while the editor checks, or
// while the model revises, keeps how its answer was shown — as written,
// unchecked; or the editor's corrected text, the revision not taken — and
// returns it with the cancellation, so the chat prints what reopening
// restores; the next turn's answer is numbered after it.
func TestEditorCancelled(t *testing.T) {
	for _, c := range []struct {
		name, shown, outcome string
	}{{"editor", certainAnswer, checkUnchecked}, {"revision", certainCorrected, checkRevisionFailed}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var cancelled atomic.Bool
			stop := func(ctx context.Context) (*modelfixture.Answer, error) {
				cancelled.Store(true)
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			s, _ := editorSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
				if isRevision(r) && !cancelled.Load() {
					return stop(ctx)
				}
				if isRevision(r) {
					return modelfixture.Text("A revision on the next turn [e1:2]."), nil
				}
				return modelfixture.Text(certainAnswer), nil
			}, func(ctx context.Context, _ model.Request) (*modelfixture.Answer, error) {
				if c.name == "editor" && !cancelled.Load() {
					return stop(ctx)
				}
				return finding([]any{}, certainCorrected, certainFinding, anchorFinding), nil
			})
			reply, err := s.Talk(ctx, "Why do the pods time out?", nil)
			if !errors.Is(err, context.Canceled) || reply.Text != c.shown || !reply.Check.Ran {
				t.Fatalf("the cancelled turn: %q, %+v, %v", reply.Text, reply.Check, err)
			}
			if d := cmp.Diff([]string{c.outcome}, checkOutcomes(t, s)); d != "" {
				t.Errorf("checks kept (-want +got):\n%s", d)
			}
			bg := context.Background()
			if _, err := s.Talk(bg, "And now?", nil); err != nil {
				t.Fatal(err)
			}
			history, err := s.ConversationHistory(bg)
			if err != nil {
				t.Fatal(err)
			}
			views := AnswerViews(history)
			if len(views) != 2 || views[0].Text != c.shown || views[1].Label != "a2" {
				t.Errorf("the answers as shown: %+v", views)
			}
			chk := history[views[0].Original].Check
			if chk == nil || (c.name == "editor") != (chk.Unchecked == "cancelled") || (c.name == "revision") != (chk.RevisionFailed == "cancelled") {
				t.Errorf("the first answer's check as reopened: %+v", chk)
			}
		})
	}
}

// TestEditorTwoCallsInAll: [E2] the editor makes at most two model calls
// in all, whatever it answers — text and then findings refused whole,
// unknown tools, text twice — and the answer goes on unchecked.
func TestEditorTwoCallsInAll(t *testing.T) {
	invalid := modelfixture.Calling(call("f1", check.ToolName, map[string]any{"findings": []any{}}))
	unknown := modelfixture.Calling(call("u1", "verdict", map[string]any{"fine": true}))
	for _, c := range []struct {
		name    string
		answers []*modelfixture.Answer
	}{
		{"text, then refused", []*modelfixture.Answer{modelfixture.Text("It reads fine."), invalid, invalid}},
		{"unknown tools", []*modelfixture.Answer{unknown, unknown, unknown}},
		{"text twice", []*modelfixture.Answer{modelfixture.Text("It reads fine."), modelfixture.Text("Still fine."), modelfixture.Text("Fine.")}},
		{"refused twice", []*modelfixture.Answer{invalid, invalid, invalid}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f := editorSession(t, answers(certainAnswer, ""), script(c.answers...))
			reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
			if err != nil {
				t.Fatal(err)
			}
			if editor, _ := editorCalls(f); len(editor) != 2 {
				t.Errorf("the editor called %d times; want 2", len(editor))
			}
			if reply.Text != certainAnswer || !strings.HasPrefix(reply.Check.Unchecked, "the editor failed: ") {
				t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
			}
		})
	}
}

// TestEditorCorrectionInReloadAndRanking: [E2] a reload's last exchange
// carries the answer as the engineer was shown it, the editor's
// correction, said so; and the ranked evidence counts the correction's
// citations as the answer's, said corrected, and not the lines it
// dropped, nor an answer and its revision twice.
func TestEditorCorrectionInReloadAndRanking(t *testing.T) {
	answer := "The DNS timeouts were caused by worker-1 [e1:3]. Pods time out resolving service names [e1:2]."
	corrected := "The DNS timeouts began on worker-1. Pods time out resolving service names [e1:2]."
	fix := map[string]any{"statement": "s1", "kind": "certainty", "quote": "were caused by worker-1", "why": "no line gives a cause", "suggested": "began on worker-1"}
	s, _ := editorSession(t, answers(answer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, corrected, fix), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why?", nil); err != nil {
		t.Fatal(err)
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ex := lastExchange(history); !strings.Contains(ex, "## The model, as the engine's editor corrected its wording\n\n"+corrected) || strings.Contains(ex, "caused by") {
		t.Errorf("the last exchange:\n%s", ex)
	}
	var whys []string
	for _, u := range rankUses(history, nil) {
		if strings.HasPrefix(u.why, "cited in a") {
			whys = append(whys, fmtUse(u))
		}
	}
	if d := cmp.Diff([]string{"e1 2-2 cited in a1, as the editor corrected it"}, whys); d != "" {
		t.Errorf("the answer's uses (-want +got):\n%s", d)
	}

	s, _ = editorSession(t, answers(certainAnswer, "Pods on two workers time out resolving service names [e1:2]."), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1"}, "", anchorFinding), nil
	})
	if _, err := s.Talk(ctx, "Why?", nil); err != nil {
		t.Fatal(err)
	}
	if history, err = s.ConversationHistory(ctx); err != nil {
		t.Fatal(err)
	}
	whys = nil
	for _, u := range rankUses(history, nil) {
		if strings.HasPrefix(u.why, "cited in a") {
			whys = append(whys, fmtUse(u))
		}
	}
	if d := cmp.Diff([]string{"e1 2-2 cited in a1"}, whys); d != "" {
		t.Errorf("a revised answer's uses (-want +got):\n%s", d)
	}
}

// fmtUse is a use as the tests compare it: its name, ranges and why.
func fmtUse(u rankUse) string {
	out := u.name
	for _, r := range u.ranges {
		out += " " + strconv.Itoa(r.From) + "-" + strconv.Itoa(r.To)
	}
	return out + " " + u.why
}

// TestEditorCountsChangedStatements: the statements an answer's footer
// says the editor corrected are those its corrected text changes — one a
// finding validation dropped called for included, one a kept finding
// named but left as written flagged instead — live and on reopening.
func TestEditorCountsChangedStatements(t *testing.T) {
	answer := "The DNS pods restarted at 09:00 [e1:2]. The upgrade confirms that the DNS timeouts were caused by worker-1 [e1:2]."
	kept := map[string]any{"statement": "s2", "kind": "certainty", "quote": "confirms that the DNS timeouts were caused by worker-1",
		"why": "e1:2 gives no cause", "evidence": []any{"e1:2"}, "suggested": "the DNS timeouts began after the upgrade"}
	dropped := map[string]any{"statement": "s1", "kind": "overstated", "quote": "the pods crashed", "why": "not in s1"}
	for _, c := range []struct {
		name, corrected, footer string
		want                    []string
		flagged                 int
	}{
		{"both changed, one finding dropped", "The DNS pods were restarted around 09:00 [e1:2]. The DNS timeouts began after the upgrade, on worker-1 [e1:2].",
			"corrected by the editor: 2 statements", []string{"s1", "s2"}, 0},
		{"the finding's statement left as written", "The DNS pods were restarted around 09:00 [e1:2]. The upgrade confirms that the DNS timeouts were caused by worker-1 [e1:2].",
			"corrected by the editor: 1 statement", []string{"s1"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := editorSession(t, answers(answer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
				return finding([]any{}, c.corrected, kept, dropped), nil
			})
			reply, err := s.Talk(context.Background(), "Who restarted them?", nil)
			if err != nil {
				t.Fatal(err)
			}
			// s1's only finding is dropped, so s1 is left unaccounted.
			want := []string{"checked 1 of 2 statements", c.footer}
			if !slices.Equal(reply.Check.Corrected, c.want) || len(reply.Check.Flagged) != c.flagged || !slices.Equal(reply.Check.Words(), want) {
				t.Errorf("the check %+v, footer %q", reply.Check, reply.Check.Words())
			}
			history, err := s.ConversationHistory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(history, func(m ConversationMessage) bool { return m.Check != nil })
			if i < 0 {
				t.Fatal("on reopening, no answer holds its check")
			}
			if got := history[i].Check; !slices.Equal(got.Corrected, c.want) || len(got.Flagged) != c.flagged {
				t.Errorf("on reopening, the check %+v", *got)
			}
		})
	}
}

// TestCheckStoresNoBudgetNote: a check whose findings are refused whole
// and then taken ends on its tool's result; the engine's word that the
// turn's tool budget is spent, said to a model before its next call, is
// not stored, since no call follows.
func TestCheckStoresNoBudgetNote(t *testing.T) {
	invalid := modelfixture.Calling(call("f1", check.ToolName, map[string]any{"findings": []any{}}))
	s, _ := editorSession(t, answers(certainAnswer, ""), script(invalid, finding([]any{"s2"}, certainCorrected, certainFinding)))
	reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != certainCorrected {
		t.Fatalf("the reply %q, %+v", reply.Text, reply.Check)
	}
	var conv string
	if err := s.c.st.DB().QueryRow(`SELECT check_conversation_id FROM checks`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	msgs, err := (companion.Store{DB: s.c.st.DB()}).Messages(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "tool budget") {
			t.Errorf("the check's conversation stores %q", m.Text)
		}
	}
}

// TestEditorWholeItemCitation: an answer citing a whole item, [e1], is
// given to the editor with the item quoted from its first line, said to be
// cited whole, a valid citation, the claim's line among those quoted; the
// editor's prompt says a citation naming no lines is never anchored or
// flagged for that alone. An editor finding it supported leaves the
// answer as written, with no revision asked.
func TestEditorWholeItemCitation(t *testing.T) {
	answer := "Pods on worker-1 and worker-2 time out resolving service names [e1]."
	var sent atomic.Value
	s, f := editorSession(t, answers(answer, ""), func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		sent.Store(r)
		return finding([]any{"s1"}, ""), nil
	})
	reply, err := s.Talk(context.Background(), "What fails?", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := sent.Load().(model.Request)
	if len(r.Messages) == 0 {
		t.Fatal("the editor was not called")
	}
	text := r.Messages[len(r.Messages)-1].Text
	heading := strings.Index(text, "\n### e1 · ")
	whole := -1
	if loc := regexp.MustCompile(`\n\[cited whole, a valid citation: every line of it is cited\]\n +1  case: 00000001\n`).FindStringIndex(text); loc != nil {
		whole = loc[0]
	}
	claim := regexp.MustCompile(`\n *\d+  [^\n]*pods on worker-1 and worker-2 time out resolving service names`).FindStringIndex(text)
	if heading < 0 || whole < heading || claim == nil || claim[0] < whole {
		t.Errorf("the editor's request does not quote e1 whole, said so, with the claim's line:\n%s", text)
	}
	if !strings.Contains(r.System, "Never anchor or flag a statement only because its citation names no lines") {
		t.Error("the editor's prompt does not say a whole-item citation is valid")
	}
	if reply.Text != answer || reply.Check.Changed() || len(reply.Check.Anchors) != 0 {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	if _, main := editorCalls(f); len(main) != 1 {
		t.Errorf("the main model was called %d times; want once, no revision", len(main))
	}
}

// TestEditorRevisionAddedCitation: the revision is told it reads no
// evidence — citations kept, only lines the conversation showed cited —
// and a citation naming lines it wrote that the answer as written did not
// have, [e1:2-3] where the answer cited [e1], is flagged beneath it and
// counted in its footer, live and on reopening; a citation the answer had,
// [e1:2], is not.
func TestEditorRevisionAddedCitation(t *testing.T) {
	answer := "The operator restarted the DNS pods at 09:00 [e1]. Pods on two workers time out resolving service names [e1:2]."
	revised := "The customer reports that the operator restarted the DNS pods at 09:00 [e1:2-3]. Pods on two workers time out resolving service names [e1:2]."
	anchor := map[string]any{"statement": "s1", "kind": "misattributed", "quote": "The operator restarted the DNS pods", "why": "who saw the restart is not in the lines given",
		"suggested": "name who said it, or mark it inferred", "anchor": true}
	s, f := editorSession(t, answers(answer, revised), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, "", anchor), nil
	})
	reply, err := s.Talk(context.Background(), "Who restarted them?", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, main := editorCalls(f)
	if len(main) != 2 {
		t.Fatalf("the main model was called %d times; want twice", len(main))
	}
	if ask := main[1].Messages[len(main[1].Messages)-1].Text; !strings.Contains(ask, "You read no evidence in this call: keep each citation as it is, cite only lines this conversation has already shown you") {
		t.Errorf("the revision's ask does not say it reads no evidence:\n%s", ask)
	}
	want := []CheckNote{{Kind: NoteFlag, Text: "[e1:2-3]", Why: revisionAddedWhy}}
	notes := func(c AnswerCheck) []CheckNote {
		var out []CheckNote
		for _, n := range c.Notes() {
			if n.Kind == NoteFlag {
				out = append(out, n)
			}
		}
		return out
	}
	if d := cmp.Diff(want, notes(reply.Check)); d != "" || reply.Text != revised || !slices.Contains(reply.Check.Words(), "1 citation added by the revision") {
		t.Errorf("the reply %q, footer %q, flags (-want +got):\n%s", reply.Text, reply.Check.Words(), d)
	}
	history, err := s.ConversationHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(history, func(m ConversationMessage) bool { return m.Check != nil })
	if i < 0 {
		t.Fatal("on reopening, no answer holds its check")
	}
	if d := cmp.Diff(want, notes(*history[i].Check)); d != "" || !slices.Contains(history[i].Check.Words(), "1 citation added by the revision") {
		t.Errorf("on reopening, footer %q, flags (-want +got):\n%s", history[i].Check.Words(), d)
	}
}

// TestEditorRevisionCitesShownLines: a citation the revision adds naming
// lines the conversation's tool results showed it numbered — a tool's
// excerpt, an evidence_read's lines, an evidence_grep's match — is not
// flagged as added by it; one naming lines none showed is, live and on
// reopening.
func TestEditorRevisionCitesShownLines(t *testing.T) {
	var listed atomic.Pointer[string]
	fill := func(s string) string {
		if n := listed.Load(); n != nil {
			return strings.ReplaceAll(s, "{x}", *n)
		}
		return s
	}
	answer := "The operators are listed [{x}]. The case is open [e1]."
	revised := "The customer lists the operators [{x}:1-2]. The case is open [e1:1], its details at [e1:2-3] and [e1:4-5]."
	s, _ := editorSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isRevision(r) {
			return modelfixture.Text(fill(revised)), nil
		}
		res, ok := lastResults(r)["c1"]
		if !ok {
			return modelfixture.Calling(
				call("c1", "read", map[string]any{"path": "cluster-scoped-resources/config.openshift.io/clusteroperators.yaml"}),
				call("c2", "evidence_read", map[string]any{"id": "e1", "from": float64(2), "lines": float64(2)}),
				call("c3", "evidence_grep", map[string]any{"pattern": ".", "ids": "e1", "max": float64(1)}),
			), nil
		}
		if ev := ExcerptEvidence(res.Text); len(ev) == 1 {
			listed.Store(&ev[0])
		}
		return modelfixture.Text(fill(answer)), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s2"}, "", map[string]any{"statement": "s1", "kind": "misattributed", "quote": "The operators are listed",
			"why": "who listed them is not in the lines given", "suggested": "name who listed them", "anchor": true}), nil
	})
	reply, err := s.Talk(context.Background(), "What is listed?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if listed.Load() == nil {
		t.Fatal("the read returned no excerpt")
	}
	want := []CheckNote{{Kind: NoteFlag, Text: "[e1:4-5]", Why: revisionAddedWhy}}
	flags := func(c AnswerCheck) []CheckNote {
		var out []CheckNote
		for _, n := range c.Notes() {
			if n.Kind == NoteFlag {
				out = append(out, n)
			}
		}
		return out
	}
	if d := cmp.Diff(want, flags(reply.Check)); d != "" || reply.Text != fill(revised) {
		t.Errorf("the reply %q, flags (-want +got):\n%s", reply.Text, d)
	}
	history, err := s.ConversationHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(history, func(m ConversationMessage) bool { return m.Check != nil })
	if i < 0 {
		t.Fatal("on reopening, no answer holds its check")
	}
	if d := cmp.Diff(want, flags(*history[i].Check)); d != "" {
		t.Errorf("on reopening, flags (-want +got):\n%s", d)
	}
}

// TestEditorRevisionCitesCarriedLines: the lines the conversation was
// shown numbered are read from the record of what each message showed,
// so lines a reload carried, numbered as stored, are lines the revision
// saw: a citation of them it adds is not flagged, one of lines never
// shown is, live and on reopening. A conversation stored before the
// record reads them from its tool results' text, which a reload's lines
// are not.
func TestEditorRevisionCitesCarriedLines(t *testing.T) {
	var name atomic.Pointer[string]
	fill := func(s string) string { return strings.ReplaceAll(s, "{n}", *name.Load()) }
	answer, revised := "The lines say so [{n}].", "The customer's lines say so [{n}:10-11], and later [{n}:20]."
	s, _ := editorSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch {
		case isRevision(r):
			return modelfixture.Text(fill(revised)), nil
		case strings.Contains(r.Messages[len(r.Messages)-1].Text, "Go on"):
			return modelfixture.Text(fill(answer)), nil
		}
		return modelfixture.Text("Noted."), nil
	}, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if !strings.Contains(r.Messages[0].Text, "The lines say so") {
			return finding([]any{"s1"}, ""), nil
		}
		return finding([]any{}, "", map[string]any{"statement": "s1", "kind": "misattributed", "quote": "The lines say so",
			"why": "whose lines they are is not said", "suggested": "name whose lines they are", "anchor": true}), nil
	})
	ctx := context.Background()
	n := thirtyLines(t, s, "numbered")
	name.Store(&n)
	if _, err := s.Talk(ctx, pointsAt+n+":10-11\n\nWhat do these lines say?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reload(ctx, ReloadSuggested, nil); err != nil {
		t.Fatal(err)
	}
	reply, err := s.Talk(ctx, "Go on.", nil)
	if err != nil {
		t.Fatal(err)
	}
	added := func() []string {
		history, err := s.ConversationHistory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(history) - 1; i >= 0; i-- {
			if c := history[i].Check; c != nil && c.Revised {
				return c.Added
			}
		}
		t.Fatal("on reopening, no answer holds its revised check")
		return nil
	}
	want := []string{"[" + n + ":20]"}
	if d := cmp.Diff(want, reply.Check.Added); d != "" || reply.Text != fill(revised) {
		t.Errorf("the reply %q, the citations added (-want +got):\n%s", reply.Text, d)
	}
	if d := cmp.Diff(want, added()); d != "" {
		t.Errorf("on reopening, the citations added (-want +got):\n%s", d)
	}
	forgetExposure(t, s)
	if d := cmp.Diff([]string{"[" + n + ":10-11]", "[" + n + ":20]"}, added()); d != "" {
		t.Errorf("exposure unknown, the citations added (-want +got):\n%s", d)
	}
}

// TestEditorKeepsCheckAfterSlowRevision: a revision slower than the
// editor's bound, and than the bound each write keeping the check has,
// still leaves the check kept as revised, with no warning that it was not.
func TestEditorKeepsCheckAfterSlowRevision(t *testing.T) {
	defer func(bound, final time.Duration) { editorBound, editorFinalBound = bound, final }(editorBound, editorFinalBound)
	editorBound, editorFinalBound = 300*time.Millisecond, 300*time.Millisecond
	answer := "The operator restarted the DNS pods at 09:00 [e1:2]."
	revised := "The customer reports that the operator restarted the DNS pods at 09:00 [e1:2]."
	anchor := map[string]any{"statement": "s1", "kind": "misattributed", "quote": "The operator restarted the DNS pods", "why": "who saw the restart is not in the lines given",
		"suggested": "name who said it, or mark it inferred", "anchor": true}
	s, _ := editorSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if !isRevision(r) {
			return modelfixture.Text(answer), nil
		}
		select {
		case <-time.After(3 * editorBound):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return modelfixture.Text(revised), nil
	}, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{}, "", anchor), nil
	})
	var warnings []string
	var mu sync.Mutex
	reply, err := s.Talk(context.Background(), "Who restarted them?", func(ev TalkEvent) {
		if ev.Kind == TalkWarning {
			mu.Lock()
			warnings = append(warnings, ev.Text)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != revised || !reply.Check.Revised {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	if d := cmp.Diff([]string{checkRevised}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 0 {
		t.Errorf("warnings: %q", warnings)
	}
}

// claimLine is the stub case's description, e1's last line, 9, which the
// answers below cite at the subject's line, e1:2, instead.
const claimLine = "After upgrading the cluster, pods on worker-1 and worker-2 time out resolving service names. A must-gather is attached."

// citationFix is the editor's finding on statement moving its citation
// of e1:2 to to, the line it says holds the claim.
func citationFix(statement, to string) map[string]any {
	return map[string]any{"statement": statement, "kind": "citation_mismatch", "quote": "[e1:2]", "why": "e1:2 is the case's subject; the description holds the claim",
		"evidence": []any{"e1:9"}, "suggested": "[" + to + "]"}
}

// keptCheck is the editor's check as kept: its footer on reopening, and
// what validation dropped.
func keptCheck(t *testing.T, s *Session) (AnswerCheck, string) {
	t.Helper()
	history, err := s.ConversationHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(history, func(m ConversationMessage) bool { return m.Check != nil })
	if i < 0 {
		t.Fatal("on reopening, no answer holds its check")
	}
	var dropped string
	if err := s.c.st.DB().QueryRowContext(t.Context(), `SELECT dropped FROM checks`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	return *history[i].Check, dropped
}

// TestEditorCorrectsCitation: given the whole item an answer cites, the
// editor settles a claim held by lines other than those cited by
// correcting the citation. A scripted editor that, as one did live,
// anchors the statement undetermined where the line holding it is not
// quoted, and corrects the citation to it where it is: the answer is
// printed corrected, the citation counted in its footer, live and on
// reopening, and no revision is asked.
func TestEditorCorrectsCitation(t *testing.T) {
	answer := "After the upgrade, pods on worker-1 and worker-2 time out resolving service names [e1:2]."
	corrected := "After the upgrade, pods on worker-1 and worker-2 time out resolving service names [e1:9]."
	undetermined := map[string]any{"statement": "s1", "kind": "undetermined", "quote": "time out resolving service names", "why": "the lines given cannot settle it",
		"suggested": "cite the lines that hold it, or say no evidence holds it", "anchor": true}
	held := regexp.MustCompile(`\n(▸ |  )9  ` + regexp.QuoteMeta(claimLine) + `\n`)
	s, f := editorSession(t, answers(answer, answer), func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if held.MatchString(r.Messages[len(r.Messages)-1].Text) {
			return finding([]any{}, corrected, citationFix("s1", "e1:9")), nil
		}
		return finding([]any{}, "", undetermined), nil
	})
	reply, err := s.Talk(context.Background(), "What fails?", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"corrected by the editor: 1 statement, 1 citation"}
	if reply.Text != corrected || !slices.Equal(reply.Check.Words(), want) || len(reply.Check.Anchors) != 0 {
		t.Errorf("the reply %q, footer %q, %+v", reply.Text, reply.Check.Words(), reply.Check)
	}
	if _, main := editorCalls(f); len(main) != 1 {
		t.Errorf("the main model was called %d times; want once, no revision", len(main))
	}
	if d := cmp.Diff([]string{checkCorrected}, checkOutcomes(t, s)); d != "" {
		t.Errorf("checks kept (-want +got):\n%s", d)
	}
	if kept, _ := keptCheck(t, s); !slices.Equal(kept.Words(), want) {
		t.Errorf("on reopening, the footer %q", kept.Words())
	}
}

// TestEditorRefusesCitationNotQuoted: a citation the editor's corrected
// text gives that names lines past its item's end is refused: its
// finding dropped with why, its statement printed as the answer has it,
// and the other statement's correction kept.
func TestEditorRefusesCitationNotQuoted(t *testing.T) {
	answer := "The DNS timeouts were caused by worker-1 [e1:2]. Pods on worker-1 and worker-2 time out resolving service names [e1:2]."
	corrected := "The DNS timeouts began on worker-1 [e1:2]. Pods on worker-1 and worker-2 time out resolving service names [e1:10]."
	shown := "The DNS timeouts began on worker-1 [e1:2]. Pods on worker-1 and worker-2 time out resolving service names [e1:2]."
	fix := map[string]any{"statement": "s1", "kind": "certainty", "quote": "were caused by worker-1", "why": "no line gives a cause", "suggested": "began on worker-1"}
	s, _ := editorSession(t, answers(answer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{}, corrected, fix, citationFix("s2", "e1:10")), nil
	})
	reply, err := s.Talk(context.Background(), "What fails?", nil)
	if err != nil {
		t.Fatal(err)
	}
	// s2's only finding is dropped, so s2 is left unaccounted.
	if reply.Text != shown || !slices.Equal(reply.Check.Words(), []string{"checked 1 of 2 statements", "corrected by the editor: 1 statement"}) {
		t.Errorf("the reply %q, footer %q", reply.Text, reply.Check.Words())
	}
	_, dropped := keptCheck(t, s)
	if want := `"Element":"findings[1]","Reason":"the corrected text cites [e1:10] for s2, and e1 holds 9 lines: s2 is kept as written"`; !strings.Contains(dropped, want) {
		t.Errorf("dropped %s; want %s", dropped, want)
	}
}

// TestEditorRevisionKeepsCorrectedCitation: a citation the editor
// corrected, which the revision keeps from the corrected text, is not
// flagged as added by the revision, live or on reopening; one the
// revision wrote itself still is.
func TestEditorRevisionKeepsCorrectedCitation(t *testing.T) {
	answer := "Pods on worker-1 and worker-2 time out resolving service names [e1:2]. The operator restarted the DNS pods at 09:00 [e1:2]."
	corrected := "Pods on worker-1 and worker-2 time out resolving service names [e1:9]. The operator restarted the DNS pods at 09:00 [e1:2]."
	revised := "Pods on worker-1 and worker-2 time out resolving service names [e1:9]. The customer reports that the operator restarted the DNS pods at 09:00 [e1:2-3]."
	anchor := map[string]any{"statement": "s2", "kind": "misattributed", "quote": "The operator restarted the DNS pods", "why": "who saw the restart is not in the lines given",
		"suggested": "name who said it, or mark it inferred", "anchor": true}
	s, _ := editorSession(t, answers(answer, revised), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{}, corrected, citationFix("s1", "e1:9"), anchor), nil
	})
	reply, err := s.Talk(context.Background(), "Who restarted them?", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"corrected by the editor: 1 statement, 1 citation", "revised for 1 anchor", "1 citation added by the revision"}
	if reply.Text != revised || !slices.Equal(reply.Check.Added, []string{"[e1:2-3]"}) || !slices.Equal(reply.Check.Words(), want) {
		t.Errorf("the reply %q, added %q, footer %q", reply.Text, reply.Check.Added, reply.Check.Words())
	}
	if kept, _ := keptCheck(t, s); !slices.Equal(kept.Added, []string{"[e1:2-3]"}) || !slices.Equal(kept.Words(), want) {
		t.Errorf("on reopening, added %q, footer %q", kept.Added, kept.Words())
	}
}

// followUpOf says whether an editor's request is the follow-up on the
// statements its findings left unaccounted, and gives its text.
func followUpOf(r model.Request) (string, bool) {
	last := r.Messages[len(r.Messages)-1].Text
	return last, strings.HasPrefix(last, "# Statements not accounted for")
}

// findingsCall is a check_findings call with args as they are.
func findingsCall(args map[string]any) *modelfixture.Answer {
	return modelfixture.Calling(call("f1", check.ToolName, args))
}

// TestEditorFollowsUpUnaccounted: findings leaving statements unaccounted
// are followed by exactly one more call in the editor's conversation,
// naming only those statements; what it gives joins the first call's, a
// correction of a statement asked about spliced in, and the check
// accounts for every statement, so its footer says nothing of it, live
// and reopened.
func TestEditorFollowsUpUnaccounted(t *testing.T) {
	answer := certainAnswer + " Send the kubelet log."
	corrected := certainCorrected + " Send the kubelet log."
	var asked atomic.Value
	s, f := editorSession(t, answers(answer, ""), func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if text, ok := followUpOf(r); ok {
			asked.Store(text)
			return findingsCall(map[string]any{"findings": []any{certainFinding}, "supported": []any{}, "corrected": corrected,
				"skipped": []any{map[string]any{"statement": "s3", "why": "request"}}}), nil
		}
		return finding([]any{"s2"}, ""), nil
	})
	reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	editor, main := editorCalls(f)
	text, _ := asked.Load().(string)
	if len(editor) != 2 || len(main) != 1 || !strings.Contains(text, "\ns1: The upgrade confirms") || !strings.Contains(text, "\ns3: Send the kubelet log.\n") || strings.Contains(text, "s2:") {
		t.Errorf("%d editor calls, %d main; the follow-up asked:\n%s", len(editor), len(main), text)
	}
	if len(editor) == 2 && len(editor[1].Messages) <= len(editor[0].Messages) {
		t.Error("the follow-up is not in the editor's conversation")
	}
	want := []string{"corrected by the editor: 1 statement"}
	if reply.Text != corrected || !slices.Equal(reply.Check.Words(), want) || reply.Check.Accounted != 3 {
		t.Errorf("the reply %q, footer %q, %+v", reply.Text, reply.Check.Words(), reply.Check)
	}
	if kept, _ := keptCheck(t, s); !slices.Equal(kept.Words(), want) {
		t.Errorf("on reopening, the footer %q", kept.Words())
	}
}

// TestEditorNoFollowUpWhenAccounted: findings accounting for every
// statement make no second call.
func TestEditorNoFollowUpWhenAccounted(t *testing.T) {
	s, f := editorSession(t, answers(certainAnswer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1", "s2"}, ""), nil
	})
	if _, err := s.Talk(context.Background(), "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	if editor, _ := editorCalls(f); len(editor) != 1 {
		t.Errorf("the editor was called %d times; want once", len(editor))
	}
}

// TestEditorFollowUpCorrectsOnlyAsked: a follow-up's correction that
// changes a statement it was not asked about is dropped, said beneath the
// answer and kept with the check; its finding is kept, shown flagged, and
// the answer printed as written.
func TestEditorFollowUpCorrectsOnlyAsked(t *testing.T) {
	both := "The DNS timeouts began after the upgrade, on worker-1 [e1:2]. Pods on two workers time out [e1:2]."
	s, _ := editorSession(t, answers(certainAnswer, ""), func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if _, ok := followUpOf(r); ok {
			return finding([]any{}, both, certainFinding), nil
		}
		return finding([]any{"s2"}, ""), nil
	})
	reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
	if err != nil {
		t.Fatal(err)
	}
	why := "it changed s2, which it was not asked about; its findings are shown flagged"
	if reply.Text != certainAnswer || len(reply.Check.Flagged) != 1 || reply.Check.Flagged[0].Statement != "s1" || !slices.Equal(reply.Check.FollowUp, []string{why}) {
		t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
	}
	kept, dropped := keptCheck(t, s)
	if !slices.Equal(kept.FollowUp, []string{why}) || !strings.Contains(dropped, `"Element":"follow-up.corrected"`) {
		t.Errorf("on reopening %+v; dropped %s", kept, dropped)
	}
}

// TestEditorFollowUpNotTaken: a follow-up that fails or passes the
// editor's bound leaves the first call's result, printed corrected, said
// beneath the answer, the footer saying how many statements were checked,
// live and reopened.
func TestEditorFollowUpNotTaken(t *testing.T) {
	defer func(bound time.Duration) { editorBound = bound }(editorBound)
	editorBound = 500 * time.Millisecond
	for _, c := range []struct {
		name, why string
		follow    func(context.Context) (*modelfixture.Answer, error)
	}{
		{"failed", "synthetic provider failure", func(context.Context) (*modelfixture.Answer, error) {
			return nil, errors.New("synthetic provider failure")
		}},
		{"past the bound", "it passed the editor's 500ms bound", func(ctx context.Context) (*modelfixture.Answer, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := editorSession(t, answers(certainAnswer+" Send the kubelet log.", ""), func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
				if _, ok := followUpOf(r); ok {
					return c.follow(ctx)
				}
				return finding([]any{}, certainCorrected+" Send the kubelet log.", certainFinding), nil
			})
			reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"checked 1 of 3 statements", "corrected by the editor: 1 statement"}
			if reply.Text != certainCorrected+" Send the kubelet log." || !slices.Equal(reply.Check.Words(), want) ||
				len(reply.Check.FollowUp) != 1 || !strings.Contains(reply.Check.FollowUp[0], c.why) {
				t.Errorf("the reply %q, footer %q, %+v", reply.Text, reply.Check.Words(), reply.Check)
			}
			if d := cmp.Diff([]string{checkCorrected}, checkOutcomes(t, s)); d != "" {
				t.Errorf("checks kept (-want +got):\n%s", d)
			}
			if kept, _ := keptCheck(t, s); !slices.Equal(kept.Words(), want) || len(kept.FollowUp) != 1 {
				t.Errorf("on reopening, the footer %q, %+v", kept.Words(), kept)
			}
		})
	}
}

// TestEditorFooterFromStoredRow: a check kept with statements its
// findings leave unaccounted, as one was live before the follow-up,
// says so in the answer's footer on reopening, from the row as stored.
func TestEditorFooterFromStoredRow(t *testing.T) {
	s, _ := editorSession(t, answers(certainAnswer, ""), func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return finding([]any{"s1", "s2"}, ""), nil
	})
	if _, err := s.Talk(context.Background(), "Why do the pods time out?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.c.st.DB().ExecContext(t.Context(), `UPDATE checks SET statements = 22, findings = '{"findings":[],"supported":["s1","s2"]}'`); err != nil {
		t.Fatal(err)
	}
	if kept, _ := keptCheck(t, s); !slices.Equal(kept.Words(), []string{"checked 2 of 22 statements"}) {
		t.Errorf("on reopening, the footer %q", kept.Words())
	}
}

// TestEditorUncorrectedFindingsSentBack: the editor's findings that name
// wording to correct and give no corrected text are sent back once: the
// second call's correction is what is shown; and where the second call
// gives nothing valid, the first's findings stand, shown flagged, the
// answer as written and not unchecked.
func TestEditorUncorrectedFindingsSentBack(t *testing.T) {
	bare := finding([]any{"s2"}, "", certainFinding)
	for _, c := range []struct {
		name    string
		second  *modelfixture.Answer
		shown   string
		flagged int
	}{
		{"corrected on the second call", finding([]any{"s2"}, certainCorrected, certainFinding), certainCorrected, 0},
		{"refused on the second call", modelfixture.Calling(call("f2", check.ToolName, map[string]any{"findings": []any{}})), certainAnswer, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f := editorSession(t, answers(certainAnswer, ""), script(bare, c.second))
			reply, err := s.Talk(context.Background(), "Why do the pods time out?", nil)
			if err != nil {
				t.Fatal(err)
			}
			editor, _ := editorCalls(f)
			if len(editor) != 2 || !strings.Contains(lastResults(editor[1])["f1"].Text, "give no `corrected` text") {
				t.Fatalf("the editor called %d times; want 2, the second told its findings corrected nothing", len(editor))
			}
			if reply.Text != c.shown || reply.Check.Unchecked != "" || len(reply.Check.Flagged) != c.flagged {
				t.Errorf("the reply %q, %+v", reply.Text, reply.Check)
			}
		})
	}
}

package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// TestToolsDeclareTheQuestion: every tool that returns evidence declares
// a required question, one line bounded; load_expert and describe_tool
// declare none, and consult keeps its own, the consult's; the prompt's
// Tools section says what the question is for.
func TestToolsDeclareTheQuestion(t *testing.T) {
	s, _ := companionSession(t, nil)
	if !strings.Contains(s.e.companionSystem(), "Give each call that reads evidence a `question`") {
		t.Error("the prompt does not ask for each call's question")
	}
	s.e.references = map[string]ReferenceAdapter{"kcs": &fakeReferences{id: "kcs"}}
	for _, tl := range s.e.companionTools(context.Background(), s.c) {
		d := tl.Decl()
		props, _ := d.Parameters["properties"].(map[string]any)
		required, _ := d.Parameters["required"].([]any)
		q, declared := props[questionArg].(map[string]any)
		switch d.Name {
		case "load_expert", readReferenceTool, "describe_tool":
			if declared {
				t.Errorf("%s declares a question", d.Name)
			}
		case consultTool:
			_, bounded := q["maxLength"]
			if !declared || q["type"] != "string" || !slices.Contains(required, any(questionArg)) || bounded {
				t.Errorf("consult question %v, required %v; want its unbounded required handoff context", q, required)
			}
		default:
			if !declared || q["type"] != "string" || q["maxLength"] != questionMax || !slices.Contains(required, any(questionArg)) {
				t.Errorf("%s: question %v, required %v; want a required string of at most %d", d.Name, q, required, questionMax)
			}
		}
	}
}

// TestQuestionTakenOutBeforeTheCallRuns: a call's question is taken out of
// its arguments before the call is validated and run, so a primitive's
// argument rules, which refuse a parameter they do not know, run it, and
// neither the result's heading nor the stored collection holds it; the
// stored call keeps it as the model wrote it; and a call without one runs.
// Synthetic.
func TestQuestionTakenOutBeforeTheCallRuns(t *testing.T) {
	const asked = "Does the OVN controller log a sync error for worker-1?"
	grep := map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes", questionArg: asked}
	s, f := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "grep", grep),
			call("c2", "run_tool", map[string]any{"id": "networking.ovn_errors", "params": map[string]any{}, questionArg: "Which OVN errors does the catalog tool find?"}),
			call("c3", "evidence_read", map[string]any{"id": "e1", questionArg: "What do the case details say of worker-1?"}),
			call("c4", "find", map[string]any{"path": "namespaces"}),
		),
		modelfixture.Text("done"),
	))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	results := lastResults(f.Requests()[1])
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		if r := results[id]; r.IsError || strings.Contains(r.Text, questionArg) {
			t.Errorf("result %s: %+v; want it run, its question nowhere in it", id, r)
		}
	}
	name := ExcerptEvidence(results["c1"].Text)
	if len(name) != 1 {
		t.Fatalf("the grep's result names no evidence: %q", results["c1"].Text)
	}
	id, err := evidenceByName(s.c, name[0])
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.c.ev.Describe(id)
	if err != nil || it.Collection == nil {
		t.Fatalf("the grep's evidence: %+v, %v", it, err)
	}
	ran, _ := json.Marshal(it.Collection.What)
	if strings.Contains(string(ran), questionArg) || strings.Contains(string(ran), asked) {
		t.Errorf("the stored collection holds the question: %s", ran)
	}
	msgs, err := companion.Store{DB: s.c.st.DB()}.Messages(ctx, mustLatest(t, s).ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored model.ToolCall
	for _, m := range msgs {
		for _, c := range m.Calls {
			if c.ID == "c1" {
				stored = c
			}
		}
	}
	if d := cmp.Diff(grep, stored.Args); d != "" {
		t.Errorf("the stored call is not as the model wrote it (-wrote +stored):\n%s", d)
	}
}

func mustLatest(t *testing.T, s *Session) companion.Conversation {
	t.Helper()
	c, err := companion.Store{DB: s.c.st.DB()}.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestVagueQuestion: a question under four words, or one every word of
// which is a function word or a generic one, is vague; one naming a
// component, an identity, a time or a value is not.
func TestVagueQuestion(t *testing.T) {
	for q, want := range map[string]bool{
		"Check the logs":                          true,
		"What is in this file?":                   true,
		"Are there any errors in the logs?":       true,
		"Does the output show anything wrong?":    true,
		"Did osd.3 restart before 08:00?":         false,
		"Does the kubelet report evictions?":      false,
		"Is worker-1 NotReady in the events?":     false,
		"What does the journal say at 02:10?":     false,
		"Which pods are in CrashLoopBackOff now?": false,
	} {
		if got := vagueQuestion(q); got != want {
			t.Errorf("vagueQuestion(%q) = %v, want %v", q, got, want)
		}
	}
	if questionKey("Did osd.3 restart  before 08:00?") != questionKey("did OSD.3 restart before 08:00") {
		t.Error("questions differing in case, spacing and punctuation compare apart")
	}
}

// TestTrajectoryQuestions: the trajectory gives each call's question as
// written, and counts per turn and in all the calls with none, those
// repeating a question of an earlier response, compared case, spacing and
// punctuation aside, the calls of one response sharing theirs, the vague
// and those restating the action; two calls alike but for their questions
// still repeat, since what ran is compared; in the text and the JSON.
// Synthetic.
func TestTrajectoryQuestions(t *testing.T) {
	const (
		restart = "Did osd.3 restart before 08:00?"
		disk    = "Is the OSD's disk full on worker-1?"
		vague   = "check the logs"
		worker  = "cluster-scoped-resources/core/nodes/worker-1.yaml"
	)
	grep := func(q string) map[string]any {
		return map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes", questionArg: q}
	}
	s, _ := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "grep", grep(restart)),
			call("c2", "read", map[string]any{"path": worker, questionArg: restart}),
			call("c3", "find", map[string]any{"path": "namespaces"})),
		modelfixture.Calling(
			call("c4", "grep", grep(disk)),
			call("c5", "evidence_read", map[string]any{"id": "e1", questionArg: vague})),
		modelfixture.Text("Nothing restarted."),
		modelfixture.Calling(call("c6", "grep", map[string]any{"pattern": "(?i)restart", "path": "namespaces", questionArg: "did OSD.3 restart  before 08:00"})),
		modelfixture.Text("Still nothing."),
	))
	ctx := context.Background()
	for _, ask := range []string{"Did it restart?", "Look again."} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 2 {
		t.Fatalf("turns: %+v", got.Turns)
	}
	first := []CallQuestion{{Tool: "grep", Question: restart}, {Tool: "read", Question: restart}, {Tool: "find", Missing: true},
		{Tool: "grep", Question: disk}, {Tool: "evidence_read", Question: vague, Vague: true, Restates: true}}
	second := []CallQuestion{{Tool: "grep", Question: "did OSD.3 restart before 08:00", Repeats: 1, Restates: true}}
	if d := cmp.Diff(first, got.Turns[0].Asked); d != "" {
		t.Errorf("the first turn's questions (-want +got):\n%s", d)
	}
	if d := cmp.Diff(second, got.Turns[1].Asked); d != "" {
		t.Errorf("the second turn's questions (-want +got):\n%s", d)
	}
	for i, want := range []QuestionCounts{{Calls: 5, None: 1, Vague: 1, Restating: 1}, {Calls: 1, Repeated: 1, Restating: 1}} {
		if got.Turns[i].Questions != want {
			t.Errorf("turn %d's counts %+v, want %+v", i+1, got.Turns[i].Questions, want)
		}
	}
	if want := (QuestionCounts{Calls: 6, None: 1, Repeated: 1, Vague: 1, Restating: 2}); got.Totals.Questions != want {
		t.Errorf("the totals' counts %+v, want %+v", got.Totals.Questions, want)
	}
	ran, _ := json.Marshal(map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"})
	if r := got.Turns[0].Repeated; len(r) != 1 || r[0].Args != string(ran) {
		t.Errorf("the repeated calls %+v, want the grep that ran again under another question", r)
	}
	out := RenderTrajectory(got)
	for _, line := range []string{
		"  questions: 4 of 5 calls asked one; 1 with none, 0 repeating an earlier response's, 1 vague, 1 restating the action\n",
		"  ? grep: " + restart + "\n", "  ? read: " + restart + "\n", "  ? find: no question\n",
		"  ? evidence_read: check the logs [vague] [restates the action]\n",
		"  ? grep: did OSD.3 restart before 08:00 [repeats turn 1] [restates the action]\n",
		"── totals\n  2 turns, 6 calls, 0 refused, 1 repeated; 0 over budget\n  questions: 5 of 6 calls asked one; 1 with none, 1 repeating an earlier response's, 1 vague, 2 restating the action\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the rendering lacks %q:\n%s", line, out)
		}
	}
	j, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"call_questions":[{"tool":"grep","question":"Did osd.3 restart before 08:00?"}`, `"repeats_turn":1`,
		`"questions":{"calls":6,"no_question":1,"repeated":1,"vague":1,"restating_action":2}`} {
		if !strings.Contains(string(j), want) {
			t.Errorf("the JSON lacks %s:\n%s", want, j)
		}
	}
}

// TestRestatesAction: a question with no question mark, or one opening
// with an imperative verb, restates its call's action; a question asking
// what the call should settle does not, whatever verb it holds later.
func TestRestatesAction(t *testing.T) {
	for q, want := range map[string]bool{
		"Find ethtool outputs for ens2f0/ens2f1 in sosreport": true,
		"Read uptime output":                                             true,
		"Check dmesg for all mentions of 37:00.1":                        true,
		"osd.3 restarted before 08:00":                                   true,
		"Check whether /var is full on worker-1?":                        true,
		"Search the journal around 18:40 for the NIC reset?":             true,
		"Is /var full on worker-1?":                                      false,
		"Did osd.3 restart before 08:00?":                                false,
		"Which pods show the eviction, and when?":                        false,
		"Does dmesg show 37:00.1 resetting before the bond failed over?": false,
	} {
		if got := restatesAction(q); got != want {
			t.Errorf("restatesAction(%q) = %v, want %v", q, got, want)
		}
	}
}

// TestAnswersDeclaredAndTakenOut: every tool, those taking no question
// included, declares optional answers, each an item and one line; a call
// giving them runs, a primitive's argument rules never seeing them, and
// the stored call keeps them.
func TestAnswersDeclaredAndTakenOut(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "grep", map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes",
			answersArg: []any{map[string]any{"item": "e1", "answer": "the details name worker-1"}}})),
		modelfixture.Text("done"),
	))
	if !strings.Contains(s.e.companionSystem(), "`answers`") {
		t.Error("the prompt does not say what answers are for")
	}
	for _, tl := range s.e.companionTools(context.Background(), s.c) {
		d := tl.Decl()
		props, _ := d.Parameters["properties"].(map[string]any)
		required, _ := d.Parameters["required"].([]any)
		if a, _ := props[answersArg].(map[string]any); a["type"] != "array" || slices.Contains(required, any(answersArg)) {
			t.Errorf("%s: answers %v, required %v; want an optional list", d.Name, a, required)
		}
	}
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	if r := lastResults(f.Requests()[1])["c1"]; r.IsError || strings.Contains(r.Text, answersArg) {
		t.Errorf("the call with answers: %+v; want it run, its answers nowhere in it", r)
	}
	msgs, err := companion.Store{DB: s.c.st.DB()}.Messages(ctx, mustLatest(t, s).ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(msgs, func(m companion.Message) bool {
		return slices.ContainsFunc(m.Calls, func(c model.ToolCall) bool { return c.ID == "c1" && HasAnswers(c.Args) })
	}) {
		t.Error("the stored call lost its answers")
	}
}

// TestExcerptAsksForTheAnswer: an excerpt of a call that asked a question
// gives it back under its head, on a line asking what the item settled,
// naming the item and the answers; the head stays the first line, so
// ExcerptEvidence names the item, and the line is no line shown, read
// from the text or the record. A call without a question, a result that
// is no excerpt and a refusal are left as they were. Synthetic.
func TestExcerptAsksForTheAnswer(t *testing.T) {
	const asked = "Does the OVN controller log a sync error for worker-1?"
	grep := func(q string) map[string]any {
		args := map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"}
		if q != "" {
			args[questionArg] = q
		}
		return args
	}
	refused := grep(asked)
	refused["bogus"] = 1
	s, f := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "grep", grep(asked)),
			call("c2", "grep", grep("")),
			call("c3", "evidence_read", map[string]any{"id": "e1", questionArg: "What do the case details say of worker-1?"}),
			call("c4", "grep", refused),
		),
		modelfixture.Text("done"),
	))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	results := lastResults(f.Requests()[1])
	got, plain := results["c1"], results["c2"]
	name := ExcerptEvidence(got.Text)
	if got.IsError || len(name) != 1 {
		t.Fatalf("the grep that asked: %+v; want an excerpt naming its item", got)
	}
	lines := strings.Split(got.Text, "\n")
	if strings.Contains(lines[0], asked) || !strings.Contains(lines[1], asked) || !strings.Contains(lines[1], name[0]) || !strings.Contains(lines[1], answersArg) {
		t.Errorf("the excerpt's head and next line: %q, %q; want the head alone, then the question asking what %s settled in answers", lines[0], lines[1], name[0])
	}
	without := strings.Join(slices.Delete(slices.Clone(lines), 1, 2), "\n")
	if p := strings.Split(plain.Text, "\n"); len(p) != len(lines)-1 || strings.Contains(plain.Text, asked) || len(ExcerptEvidence(plain.Text)) != 1 {
		t.Errorf("the grep that asked nothing: %q; want the excerpt alone", plain.Text)
	}
	shown := func(text string) map[string][]carriedRead {
		return resultLines([]companion.Message{{Message: model.Message{Results: []model.ToolResult{{CallID: "c1", Name: "grep", Text: text}}}}})
	}
	if l := shown(got.Text); len(l[name[0]]) == 0 || !cmp.Equal(l, shown(without), cmp.AllowUnexported(carriedRead{})) {
		t.Errorf("the lines read from the text %+v, want %+v as without the question", l, shown(without))
	}
	x, err := s.exposure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The excerpt gave the whole item, so the record holds it whole.
	if !x.whole(name[0]) {
		t.Errorf("the exposure record's lines of %s: %+v; want the item whole", name[0], x.lines[name[0]])
	}
	for _, id := range []string{"c3", "c4"} {
		if r := results[id]; strings.Contains(r.Text, "What do the case details") || strings.Contains(r.Text, asked) {
			t.Errorf("result %s: %q; want no question given back", id, r.Text)
		}
	}
	if !results["c4"].IsError {
		t.Errorf("the grep with an unknown parameter ran: %+v", results["c4"])
	}
}

// TestTrajectoryAnswers: answers are checked as statements, the call
// still run: one naming an item no tool of the conversation returned
// before it, the case block's own included, or no item's name, is
// flagged; a code span found in the item's lines shown is not, one in
// none of them is, and arithmetic that does not hold is; a call that
// takes no question may answer. The trajectory gives each answer with its
// flags, counts them, and counts the items answered beside those with no
// answer, citation or read-on, in the text and the JSON; CallAnswers,
// /debug's reading, flags the same. Synthetic.
func TestTrajectoryAnswers(t *testing.T) {
	const worker = "cluster-scoped-resources/core/nodes/worker-1.yaml"
	var n atomic.Int64
	var names [3]string
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Calling(
				call("c1", "grep", map[string]any{"pattern": "error syncing", "path": "namespaces/openshift-ovn-kubernetes", questionArg: "Does OVN fail to sync worker-1?"}),
				call("c2", "read", map[string]any{"path": worker, questionArg: "Is worker-1 Ready?"}),
				call("c3", "find", map[string]any{"path": "namespaces", questionArg: "Which namespaces were collected?",
					answersArg: []any{map[string]any{"item": "e1", "answer": "the details name worker-1"}}})), nil
		case 2:
			res := lastResults(r)
			for i, id := range []string{"c1", "c2", "c3"} {
				names[i] = ExcerptEvidence(res[id].Text)[0]
			}
			return modelfixture.Calling(
				call("c4", "evidence_read", map[string]any{"id": "e1", questionArg: "What do the case details say of worker-1?", answersArg: []any{
					map[string]any{"item": names[0], "answer": "OVN logs `error syncing node worker-1` three times"},
					map[string]any{"item": names[0], "answer": "`2 + 2 = 5` errors"},
					map[string]any{"item": names[1], "answer": "worker-1 is `OOMKilled`"},
					map[string]any{"item": names[1], "answer": "the node says `error syncing node worker-1`"},
				}}),
				call("c5", "load_expert", map[string]any{"id": "openshift.platform", answersArg: []any{
					map[string]any{"item": "e999", "answer": "nothing"},
					map[string]any{"item": "the log", "answer": "nothing"},
				}})), nil
		}
		return modelfixture.Text("OVN cannot sync worker-1."), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Why is worker-1 failing?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 {
		t.Fatalf("turns: %+v", got.Turns)
	}
	turn := got.Turns[0]
	want := []ItemAnswer{
		{Tool: "find", Item: "e1", Answer: "the details name worker-1", Flags: []string{"e1 was returned by no tool of this conversation before this call"}},
		{Tool: "evidence_read", Item: names[0], Answer: "OVN logs `error syncing node worker-1` three times"},
		{Tool: "evidence_read", Item: names[0], Answer: "`2 + 2 = 5` errors", Flags: []string{"`2 + 2 = 5`: " + arithmeticWrong}},
		{Tool: "evidence_read", Item: names[1], Answer: "worker-1 is `OOMKilled`", Flags: []string{"`OOMKilled` is in none of " + names[1] + "'s lines this conversation was shown"}},
		// Found in another item's lines, not the one it names.
		{Tool: "evidence_read", Item: names[1], Answer: "the node says `error syncing node worker-1`",
			Flags: []string{"`error syncing node worker-1` is in none of " + names[1] + "'s lines this conversation was shown"}},
		{Tool: "load_expert", Item: "e999", Answer: "nothing", Flags: []string{"e999 was returned by no tool of this conversation before this call"}},
		{Tool: "load_expert", Item: "the log", Answer: "nothing", Flags: []string{`"the log" is not an item's name, such as e12`}},
	}
	if d := cmp.Diff(want, turn.Answers); d != "" {
		t.Errorf("the answers (-want +got):\n%s", d)
	}
	if turn.FlaggedAnswers != 6 || got.Totals.Answers != 7 || got.Totals.FlaggedAnswers != 6 {
		t.Errorf("flagged %d, totals %d given and %d flagged; want 6, 7 and 6", turn.FlaggedAnswers, got.Totals.Answers, got.Totals.FlaggedAnswers)
	}
	wantEvidence := TrajectoryEvidence{Returned: 3, Neither: 3, NeitherNames: names[:], Answered: 2, None: 1, NoneNames: []string{names[2]}}
	if d := cmp.Diff(wantEvidence, turn.Evidence); d != "" {
		t.Errorf("the evidence (-want +got):\n%s", d)
	}
	for _, c := range []struct {
		id   string
		want []ItemAnswer
	}{{"c3", want[:1]}, {"c4", want[1:5]}, {"c5", want[5:]}} {
		answers, err := s.CallAnswers(ctx, turn.Conversation, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if d := cmp.Diff(c.want, answers); d != "" {
			t.Errorf("CallAnswers of %s (-want +got):\n%s", c.id, d)
		}
	}
	out := RenderTrajectory(got)
	for _, line := range []string{
		"  answers: 7 given, 6 flagged\n",
		"  = " + names[0] + " on evidence_read: OVN logs `error syncing node worker-1` three times\n",
		"  = " + names[1] + " on evidence_read: worker-1 is `OOMKilled` ⚠ `OOMKilled` is in none of " + names[1] + "'s lines this conversation was shown\n",
		"  3 items returned: 0 read on, 0 cited, 0 both, 3 neither; 2 answered, 1 with no answer, citation or read-on (" + names[2] + ")\n",
		"── totals\n", "  answers: 7 given, 6 flagged\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the rendering lacks %q:\n%s", line, out)
		}
	}
	j, err := json.Marshal(got.Totals)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"neither":3`, `"answered":2`, `"none":1`, `"none_names":["` + names[2] + `"]`, `"answers":7`, `"flagged_answers":6`} {
		if !strings.Contains(string(j), field) {
			t.Errorf("the totals' JSON lacks %s:\n%s", field, j)
		}
	}
}

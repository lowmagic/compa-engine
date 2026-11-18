package companion_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/redact"
	"github.com/lowmagic/compa-engine/internal/store"
)

// echo is a tool that returns its argument.
type echo struct{ runs atomic.Int64 }

func (*echo) Decl() model.Tool {
	return model.Tool{Name: "echo", Description: "Echo.", Parameters: map[string]any{"type": "object"}}
}

func (e *echo) Run(_ context.Context, c companion.Call) companion.Result {
	e.runs.Add(1)
	return companion.Result{Text: fmt.Sprint(c.Args["say"])}
}

// loop is a loop over a scratch store, the fixture provider answering with
// respond and recording every invocation.
func loop(t *testing.T, respond func(context.Context, model.Request) (*fixture.Answer, error)) (*companion.Loop, *fixture.Provider, *[]companion.Invocation) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), store.DirName), "00000001", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := fixture.New()
	f.Respond = respond
	var mu sync.Mutex
	var recs []companion.Invocation
	var n atomic.Int64
	return &companion.Loop{
		Store: companion.Store{DB: st.DB()}, Provider: f, Model: "fixture-model", System: "system",
		Tools:     []companion.Tool{&echo{}},
		Policy:    redact.For("external_provider"),
		CaseBlock: func(context.Context) (string, []companion.Shown, error) { return "the case, as it is now", nil, nil },
		NewID:     func(p string) string { return fmt.Sprintf("%s-%d", p, n.Add(1)) },
		Record: func(inv companion.Invocation) error {
			mu.Lock()
			defer mu.Unlock()
			recs = append(recs, inv)
			return nil
		},
	}, f, &recs
}

// calling answers every call with n calls of echo until told the budget is
// spent, then in text.
func calling(n int) func(context.Context, model.Request) (*fixture.Answer, error) {
	return func(_ context.Context, r model.Request) (*fixture.Answer, error) {
		if r.NoCalls {
			return fixture.Text("out of budget"), nil
		}
		var calls []model.ToolCall
		for i := range n {
			calls = append(calls, model.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "echo", Args: map[string]any{"say": i}})
		}
		return fixture.Calling(calls...), nil
	}
}

// The budget bounds one turn's calls: past it, calls are refused saying
// so, the next request asks for an answer, and the model's answer ends the
// turn; a new turn has a budget of its own.
func TestBudget(t *testing.T) {
	l, f, _ := loop(t, calling(3))
	l.Budget = 5
	ctx := context.Background()
	conv, err := l.Start(ctx, "the case")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil)
	if err != nil || reply.Text != "out of budget" {
		t.Fatalf("reply %q, %v", reply.Text, err)
	}
	tool, ok := l.Tools[0].(*echo)
	if !ok {
		t.Fatalf("the first tool is %T", l.Tools[0])
	}
	ran := tool.runs.Load()
	if ran != 5 {
		t.Errorf("%d calls ran, want the budget's 5", ran)
	}
	reqs := f.Requests()
	last := reqs[len(reqs)-1]
	if !last.NoCalls {
		t.Error("the last request did not ask for an answer")
	}
	refusals := 0
	for _, m := range last.Messages {
		for _, r := range m.Results {
			if r.IsError && strings.Contains(r.Text, "budget") {
				refusals++
			}
		}
	}
	if refusals != 1 {
		t.Errorf("%d calls refused for the budget, want 1", refusals)
	}
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "continue", nil); err != nil {
		t.Fatal(err)
	}
	if ran := tool.runs.Load(); ran != 10 {
		t.Errorf("after a second turn %d calls ran, want 10", ran)
	}
}

// A model that keeps calling tools after its budget is spent is stopped,
// not called without end.
func TestBudgetIgnored(t *testing.T) {
	l, f, _ := loop(t, func(context.Context, model.Request) (*fixture.Answer, error) {
		return fixture.Calling(model.ToolCall{ID: "c", Name: "echo", Args: map[string]any{"say": 1}}), nil
	})
	l.Budget = 1
	ctx := context.Background()
	conv, _ := l.Start(ctx, "the case")
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil); err == nil {
		t.Fatal("no error")
	}
	if n := len(f.Requests()); n > 5 {
		t.Errorf("%d model calls", n)
	}
}

// usageOf answers as respond does, each answer reporting u.
func usageOf(respond func(context.Context, model.Request) (*fixture.Answer, error), u model.Usage) func(context.Context, model.Request) (*fixture.Answer, error) {
	return func(ctx context.Context, r model.Request) (*fixture.Answer, error) {
		a, err := respond(ctx, r)
		if a != nil {
			a.Usage = u
		}
		return a, err
	}
}

// The token budget bounds one turn's input, summed over its calls, each
// call's estimate counted where its provider reports no input: past it,
// the calls the model asks for are refused saying so, the next request
// asks for an answer, and the answer ends the turn; the engineer's next
// turn has a budget of its own.
func TestTokenBudget(t *testing.T) {
	unreported := model.Usage{InputTokens: model.Unavailable, CacheReadTokens: model.Unavailable, CacheWriteTokens: model.Unavailable,
		OutputTokens: model.Unavailable, ReasoningTokens: model.Unavailable, Normalization: model.NormalizationAmbiguous}
	l, f, recs := loop(t, usageOf(calling(1), unreported))
	// About 260,000 estimated tokens a call: the second call passes the
	// budget, the call budget far from spent.
	l.System = strings.Repeat("a ", 200_000)
	ctx := context.Background()
	conv, _ := l.Start(ctx, "the case")
	reply, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil)
	if err != nil || reply.Text != "out of budget" {
		t.Fatalf("reply %q, %v", reply.Text, err)
	}
	reqs := f.Requests()
	if len(reqs) != 3 || !reqs[2].NoCalls || reqs[1].NoCalls {
		t.Fatalf("%d calls, the last asking for an answer %v", len(reqs), reqs[len(reqs)-1].NoCalls)
	}
	var sum int64
	for _, r := range (*recs)[:2] {
		sum += r.EstimatedTokens
	}
	if sum <= companion.TokenBudget || (*recs)[0].EstimatedTokens > companion.TokenBudget {
		t.Errorf("the first two calls' estimates sum to %d", sum)
	}
	results := reqs[2].Messages[len(reqs[2].Messages)-1]
	if !strings.Contains(results.Text, "token budget") || len(results.Results) != 1 || !results.Results[0].IsError || !strings.Contains(results.Results[0].Text, "token budget") {
		t.Errorf("the refusal and the note: %+v", results)
	}
	tool, ok := l.Tools[0].(*echo)
	if !ok {
		t.Fatalf("the loop's first tool is %T, want the echo", l.Tools[0])
	}
	if tool.runs.Load() != 1 {
		t.Errorf("%d tool calls ran, want the first call's 1", tool.runs.Load())
	}
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "continue", nil); err != nil {
		t.Fatal(err)
	}
	if reqs := f.Requests(); reqs[3].NoCalls {
		t.Error("the next turn began with its budget spent")
	}
}

// A token the provider read from its cache counts a tenth of an uncached
// one against the token budget: calls reporting 30,000 uncached and
// 300,000 cached input tokens each, 330,000 sent, pass the budget only at
// their seventh, 60,000 counted a call; whole, the second would have.
func TestTokenBudgetCountsCacheReadsAtATenth(t *testing.T) {
	cached := model.Usage{InputTokens: model.Reported(30_000), CacheReadTokens: model.Reported(300_000), CacheWriteTokens: model.Reported(0),
		OutputTokens: model.Reported(20), ReasoningTokens: model.Unavailable, Normalization: model.NormalizationNativeDisjoint}
	l, f, _ := loop(t, usageOf(calling(1), cached))
	ctx := context.Background()
	conv, _ := l.Start(ctx, "the case")
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	first := slices.IndexFunc(reqs, func(r model.Request) bool { return r.NoCalls })
	if first != 7 || len(reqs) != 8 {
		t.Errorf("%d calls, the first asking for an answer at %d; want the eighth of 8", len(reqs), first)
	}
}

// Redaction applies to every message before a call: the provider never
// sees the secret, and the stored conversation keeps it, unredacted, for
// the engineer.
func TestRedactedBeforeEachCall(t *testing.T) {
	l, f, _ := loop(t, func(context.Context, model.Request) (*fixture.Answer, error) { return fixture.Text("ok"), nil })
	ctx := context.Background()
	conv, _ := l.Start(ctx, "the case\nDB_PASSWORD=hunter2hunter2")
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlc2lnbmF0dXJl", nil); err != nil {
		t.Fatal(err)
	}
	sent := f.Requests()[0]
	for _, m := range sent.Messages {
		if strings.Contains(m.Text, "hunter2hunter2") || strings.Contains(m.Text, "eyJhbGciOiJIUzI1NiJ9") {
			t.Errorf("a secret was sent: %q", m.Text)
		}
	}
	msgs, _ := l.Store.Messages(ctx, conv)
	if !strings.Contains(msgs[0].Text, "hunter2hunter2") {
		t.Error("the stored case block lost what it holds")
	}
}

// A gate's refusal sends nothing and records the call refused, as a
// barred account or as egress beyond what is allowed.
func TestGateRefuses(t *testing.T) {
	for _, c := range []struct {
		err     error
		outcome string
	}{{companion.ErrBarred, companion.OutcomeBarred}, {companion.ErrEgressClass, companion.OutcomeEgressRefused}, {companion.ErrEgress, companion.OutcomeEgressRefused}} {
		l, f, recs := loop(t, nil)
		l.Gate = func(context.Context) (string, error) { return "", fmt.Errorf("%w: refused", c.err) }
		ctx := context.Background()
		conv, _ := l.Start(ctx, "the case")
		if _, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil); !errors.Is(err, c.err) {
			t.Errorf("err %v, want %v", err, c.err)
		}
		if len(f.Requests()) != 0 || len(*recs) != 1 || (*recs)[0].Outcome != c.outcome {
			t.Errorf("%v: %d calls, records %+v", c.err, len(f.Requests()), *recs)
		}
	}
}

// An answer stopped at its output cap that repeats one fragment is an
// output loop; one that only ran long is over its budget.
func TestOutputLimit(t *testing.T) {
	for _, c := range []struct {
		text, outcome string
	}{{strings.Repeat("the node is NotReady because ", 40), companion.OutcomeOutputLoop}, {"a long, varied answer", companion.OutcomeBudgetExceeded}} {
		l, _, recs := loop(t, nil)
		l.Provider = capped{c.text}
		ctx := context.Background()
		conv, _ := l.Start(ctx, "the case")
		reply, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil)
		if !errors.Is(err, model.ErrOutputLimit) || reply.Text != c.text {
			t.Errorf("reply %.40q, err %v", reply.Text, err)
		}
		if got := (*recs)[0].Outcome; got != c.outcome {
			t.Errorf("outcome %s, want %s", got, c.outcome)
		}
	}
}

// capped answers with its text, stopped at the output cap.
type capped struct{ text string }

func (capped) Describe() model.Descriptor {
	return model.Descriptor{ID: "capped", EgressClass: "local_only"}
}

func (c capped) Call(context.Context, model.Request, func(model.Delta)) (*model.Response, error) {
	return &model.Response{Text: c.text, Usage: fixture.Usage()}, model.ErrOutputLimit
}

// Compaction has the model write a handoff and begins a new conversation,
// linked to the old, from the case as it is now, what follows the block
// (the case ledger) and the handoff; the old conversation stays.
func TestCompact(t *testing.T) {
	l, _, _ := loop(t, func(_ context.Context, r model.Request) (*fixture.Answer, error) {
		if strings.Contains(r.Messages[len(r.Messages)-1].Text, "handoff") {
			return fixture.Text("Established: worker-1 NotReady [e3:2]."), nil
		}
		return fixture.Text("ok"), nil
	})
	l.AfterBlock = func(context.Context) (string, error) { return "# Case ledger\n\nthe ledger, as it is now", nil }
	ctx := context.Background()
	conv, _ := l.Start(ctx, "the case, as it began")
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "what failed?", nil); err != nil {
		t.Fatal(err)
	}
	next, err := l.Compact(ctx, conv, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Store.Conversation(ctx, next)
	if err != nil || c.CompactedFrom != conv {
		t.Fatalf("the new conversation %+v, %v", c, err)
	}
	msgs, _ := l.Store.Messages(ctx, next)
	if len(msgs) != 3 || msgs[0].Text != "the case, as it is now" || msgs[1].Kind != companion.KindUpdate || !strings.HasPrefix(msgs[1].Text, "# Case ledger") ||
		msgs[2].Kind != companion.KindHandoff || !strings.Contains(msgs[2].Text, "worker-1 NotReady [e3:2]") {
		t.Errorf("the new conversation begins %+v", msgs)
	}
	if latest, _ := l.Store.Latest(ctx); latest.ID != next {
		t.Errorf("the latest conversation is %s, want %s", latest.ID, next)
	}
	if old, _ := l.Store.Messages(ctx, conv); len(old) < 4 {
		t.Errorf("the old conversation has %d messages", len(old))
	}
}

// A turn past the context limit compacts first, saying so, and continues
// in the new conversation; a conversation the model has not answered in
// has nothing to hand off and is not compacted.
func TestContextLimitCompacts(t *testing.T) {
	l, _, _ := loop(t, func(context.Context, model.Request) (*fixture.Answer, error) { return fixture.Text("ok"), nil })
	l.ContextLimit = 50
	ctx := context.Background()
	conv, _ := l.Start(ctx, strings.Repeat("a long case ", 100))
	var compacted int
	on := func(e companion.Event) {
		if e.Kind == companion.EventCompacted {
			compacted++
		}
	}
	reply, err := l.Turn(ctx, conv, companion.KindEngineer, "go", on)
	if err != nil || compacted != 0 || reply.Conversation != conv {
		t.Fatalf("the first turn: compacted %d, conversation %s, %v", compacted, reply.Conversation, err)
	}
	reply, err = l.Turn(ctx, conv, companion.KindEngineer, "and then?", on)
	if err != nil {
		t.Fatal(err)
	}
	if compacted != 1 || reply.Conversation == conv {
		t.Errorf("compacted %d, conversation %s", compacted, reply.Conversation)
	}
}

// recorder is a tool whose result ends the run, as a task's one tool does.
type recorder struct{ runs atomic.Int64 }

func (*recorder) Decl() model.Tool {
	return model.Tool{Name: "record", Description: "Record.", Parameters: map[string]any{"type": "object"}}
}

func (r *recorder) Run(context.Context, companion.Call) companion.Result {
	r.runs.Add(1)
	return companion.Result{Text: "recorded", End: true}
}

// A task runs in a conversation of its own kind, apart from the main
// conversation, from the engine's request; a result that ends the run
// stops it after one model call, its results stored, the model not called
// again; every call is recorded.
func TestTaskEndsOnItsToolsResult(t *testing.T) {
	l, f, recs := loop(t, func(context.Context, model.Request) (*fixture.Answer, error) {
		return fixture.Calling(model.ToolCall{ID: "r1", Name: "record", Args: map[string]any{}}), nil
	})
	rec := &recorder{}
	l.Tools = []companion.Tool{rec}
	ctx := context.Background()
	reply, err := l.Task(ctx, "ledger", "read these comments", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.Requests()); n != 1 || rec.runs.Load() != 1 || len(*recs) != 1 {
		t.Fatalf("%d model calls, %d tool runs, %d records; want 1 each", n, rec.runs.Load(), len(*recs))
	}
	conv, err := l.Store.Conversation(ctx, reply.Conversation)
	if err != nil || conv.Kind != "ledger" {
		t.Fatalf("the task's conversation %+v, %v", conv, err)
	}
	msgs, err := l.Store.Messages(ctx, reply.Conversation)
	if err != nil || len(msgs) != 3 || msgs[0].Kind != companion.KindEngine || msgs[2].Kind != companion.KindResults || msgs[2].Results[0].Text != "recorded" {
		t.Errorf("the task's messages %+v, %v", msgs, err)
	}
	if _, err := l.Store.Latest(ctx); !errors.Is(err, companion.ErrNoConversation) {
		t.Errorf("a task's conversation was taken for the main one: %v", err)
	}
}

// TestAskOneCall: Ask has the model answer the engine's message in exactly
// one call offering no tool, counted in the conversation's turn and kept
// after it; a tool called anyway is refused, not run, and ends the ask
// with an error after that one call, its refusal kept so the conversation
// stays whole.
func TestAskOneCall(t *testing.T) {
	l, f, recs := loop(t, func(context.Context, model.Request) (*fixture.Answer, error) {
		return fixture.Text("the revision"), nil
	})
	ctx := context.Background()
	conv, err := l.Start(ctx, "the case")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "What failed?", nil); err != nil {
		t.Fatal(err)
	}
	got, err := l.Ask(ctx, conv, companion.KindRevise, "Write it again.", nil)
	if err != nil || got != "the revision" {
		t.Fatalf("Ask: %q, %v", got, err)
	}
	reqs := f.Requests()
	if len(reqs) != 2 || !reqs[1].NoCalls || (*recs)[1].Turn != 1 {
		t.Errorf("%d calls, the ask's no calls %v, its turn %d", len(reqs), reqs[1].NoCalls, (*recs)[1].Turn)
	}
	msgs, err := l.Store.Messages(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(msgs); msgs[n-2].Kind != companion.KindRevise || msgs[n-1].Kind != companion.KindModel || msgs[n-1].Text != "the revision" {
		t.Errorf("the conversation ends %s, %s %q", msgs[n-2].Kind, msgs[n-1].Kind, msgs[n-1].Text)
	}

	e := &echo{}
	l, f, _ = loop(t, func(context.Context, model.Request) (*fixture.Answer, error) {
		return fixture.Calling(model.ToolCall{ID: "c1", Name: "echo", Args: map[string]any{"say": "x"}}), nil
	})
	l.Tools = []companion.Tool{e}
	if conv, err = l.Start(ctx, "the case"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ask(ctx, conv, companion.KindRevise, "Write it again.", nil); err == nil || len(f.Requests()) != 1 || e.runs.Load() != 0 {
		t.Errorf("a tool called in an ask: %v, %d calls, %d runs", err, len(f.Requests()), e.runs.Load())
	}
	if msgs, _ = l.Store.Messages(ctx, conv); msgs[len(msgs)-1].Kind != companion.KindResults {
		t.Errorf("the refused call's result is not kept: %s", msgs[len(msgs)-1].Kind)
	}
}

// reader is a tool whose result shows lines 3 to 5 of e7, and, where end,
// ends the run.
type reader struct{ end bool }

func (*reader) Decl() model.Tool {
	return model.Tool{Name: "read", Description: "Read.", Parameters: map[string]any{"type": "object"}}
}

func (r *reader) Run(context.Context, companion.Call) companion.Result {
	return companion.Result{Text: "e7 lines 3–5", Shows: []companion.Shown{{Name: "e7", From: 3, To: 5}}, End: r.end}
}

// TestExposureRecordsWhatWasSent: what a conversation was shown of the
// evidence is what the messages a sent request carried show: none while
// the gate refuses the call, the case block's and a tool result's once
// calls carry them, and not a result stored after the last call, which
// no request carried.
func TestExposureRecordsWhatWasSent(t *testing.T) {
	refuse := true
	l, _, _ := loop(t, func(_ context.Context, r model.Request) (*fixture.Answer, error) {
		if len(r.Messages[len(r.Messages)-1].Results) > 0 {
			return fixture.Text("done"), nil
		}
		return fixture.Calling(model.ToolCall{ID: "c1", Name: "read", Args: map[string]any{}}), nil
	})
	l.Tools = []companion.Tool{&reader{}}
	l.Gate = func(context.Context) (string, error) {
		if refuse {
			return "", fmt.Errorf("synthetic: %w", companion.ErrBarred)
		}
		return "", nil
	}
	ctx := context.Background()
	conv, err := l.Start(ctx, "the case", companion.Shown{Name: "e1"}, companion.Shown{Name: "e2", From: 1, To: 4})
	if err != nil {
		t.Fatal(err)
	}
	exposure := func() []companion.Shown {
		t.Helper()
		got, err := l.Store.Exposure(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil); !errors.Is(err, companion.ErrBarred) {
		t.Fatalf("the refused turn: %v", err)
	}
	if got := exposure(); len(got) != 0 {
		t.Errorf("a refused call recorded %v", got)
	}
	refuse = false
	if _, err := l.Turn(ctx, conv, companion.KindEngineer, "go", nil); err != nil {
		t.Fatal(err)
	}
	want := []companion.Shown{{Name: "e1"}, {Name: "e2", From: 1, To: 4}, {Name: "e7", From: 3, To: 5}}
	if got := exposure(); !slices.Equal(got, want) {
		t.Errorf("exposure %v, want %v", got, want)
	}
	if c, err := l.Store.Conversation(ctx, conv); err != nil || !c.Exposure {
		t.Errorf("the conversation's record is not marked kept: %+v, %v", c, err)
	}

	l.Tools = []companion.Tool{&reader{end: true}}
	other, err := l.Start(ctx, "the case")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Turn(ctx, other, companion.KindEngineer, "go", nil); err != nil {
		t.Fatal(err)
	}
	shows, err := l.Store.Shows(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Store.Exposure(ctx, other)
	if err != nil || len(got) != 0 || len(shows) != 1 {
		t.Errorf("a result no request carried: exposure %v, %v; stored %v", got, err, shows)
	}
}

// TestSystemIndexesExperts: the system prompt lists each installed expert
// by ID and summary, with the routing keys it is primary for where it has
// any.
func TestSystemIndexesExperts(t *testing.T) {
	got := companion.System([]companion.ExpertLine{
		{ID: "fixture.storage", Summary: "Storage (fixture)", Keys: []string{"a", "b"}},
		{ID: "fixture.core", Summary: "Core (fixture)"},
	}, "", nil, "")
	if !strings.Contains(got, "- `fixture.storage`: Storage (fixture) (routing keys: a, b)\n") {
		t.Errorf("an expert with routing keys:\n%s", got)
	}
	if !strings.Contains(got, "- `fixture.core`: Core (fixture)\n") {
		t.Errorf("an expert with none:\n%s", got)
	}
}

// TestSystemHouseSection: the deployment's house text is a section of its
// own at the end of the system prompt, once and as given, and a prompt
// with none, or with only white space, has no section without a body.
func TestSystemHouseSection(t *testing.T) {
	const house = "Support is Example Support.\n\n- Articles are in the Example knowledge base.\n- `examplectl` reads a live system."
	experts := []companion.ExpertLine{{ID: "fixture.core", Summary: "Core (fixture)"}}
	intentions := []companion.Intention{{ID: "update", Label: "Update", Audience: "customer", Purpose: "Say what is known."}}
	got := companion.System(experts, "Plain.", intentions, "\n"+house+"\n\n")
	if strings.Count(got, house) != 1 || !strings.HasSuffix(got, "\n\n"+house+"\n") {
		t.Errorf("the house text is not the prompt's last section, once and as given:\n%s", got)
	}
	if without := companion.System(experts, "Plain.", intentions, ""); !strings.HasPrefix(got, without) || strings.Count(got[len(without):], "\n## ") != 1 {
		t.Errorf("the house text is not one section appended to the prompt without it:\n%s", got[min(len(without), len(got)):])
	}
	for _, none := range []string{"", " \n\t"} {
		got := companion.System(nil, "", nil, none)
		lines := strings.Split(strings.TrimSpace(got), "\n")
		for i, line := range lines {
			if !strings.HasPrefix(line, "#") {
				continue
			}
			body := false
			for _, next := range lines[i+1:] {
				if strings.HasPrefix(next, "#") {
					break
				}
				if strings.TrimSpace(next) != "" {
					body = true
					break
				}
			}
			if !body && i > 0 {
				t.Errorf("house %q: the section %q has no body", none, line)
			}
		}
		if got != companion.System(nil, "", nil, "") {
			t.Errorf("house %q changes the prompt", none)
		}
	}
}

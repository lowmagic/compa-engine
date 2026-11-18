package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/ledger"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/model/openai"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// companionCase is the stub case the companion's tests work: its routing key's
// primary expert is the fixture openshift.networking.
var companionCase = Case{ID: "00000001", Subject: "Pods on two workers cannot reach cluster DNS after upgrade",
	Description: "After upgrading the cluster, pods on worker-1 and worker-2 time out resolving service names. A must-gather is attached.",
	Status:      "Waiting on Support", Severity: "2 (High)", Product: "OpenShift Container Platform", ProductVersion: "4.16",
	RoutingKeys: []string{"openshift-networking"}}

var companionComments = []Comment{
	{ID: "c1", Author: Party{Name: "customer"}, Created: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Body: "Started right after the upgrade finished. Only two workers affected."},
	{ID: "c2", Author: Party{Name: "support-engineer"}, Created: time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC), Body: "Requested a fresh must-gather; DNS operator looks healthy so far."},
}

// companionSession is the stub case acquired, its folder the fixture
// must-gather, with the fixture experts, any roots given, and the routing
// library installed, and the companion's model the fixture provider
// answering with respond.
func companionSession(t *testing.T, respond func(context.Context, model.Request) (*modelfixture.Answer, error), roots ...string) (*Session, *modelfixture.Provider) {
	t.Helper()
	td, err := filepath.Abs("../testdata")
	if err != nil {
		t.Fatal(err)
	}
	local := strings.Join(append([]string{filepath.Join(td, "experts")}, roots...), ", ")
	cfg, err := config.Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {local: ["+local+"]}\n"+
		"routingLibrary: "+filepath.Join(td, "routing", "library.yaml")+"\n"+
		"reasoning:\n  maxEgressClass: local_only\n  models:\n    fixture-model: {provider: fixture, model: fixture-model}\n  profiles:\n    companion: {model: fixture-model}\n    ledger: {model: fixture-model}\n"+
		"comments:\n  style: Plain and direct.\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{config: cfg, home: t.TempDir()}
	c := testCase(t, 1<<20)
	c.root = filepath.Join(td, "artifacts", "must-gather")
	e.cases, e.artifacts = nopCases{}, &snapshotArtifacts{root: c.root}
	var n atomic.Int64
	c.newID = func(p string) string { return fmt.Sprintf("%s-%d", p, n.Add(1)) }
	c.now = func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) }
	if _, err := e.acquire(context.Background(), c, companionCase, companionComments); err != nil {
		t.Fatal(err)
	}
	f := modelfixture.New()
	f.Respond = respond
	e.models = map[string]model.Provider{"fixture": f}
	return &Session{caseStore: &CaseStore{e: e, c: c}, kase: companionCase}, f
}

// script answers the companion's calls in order, one answer a call; past
// its end, "done".
func script(answers ...*modelfixture.Answer) func(context.Context, model.Request) (*modelfixture.Answer, error) {
	var n atomic.Int64
	return func(context.Context, model.Request) (*modelfixture.Answer, error) {
		i := int(n.Add(1)) - 1
		if i >= len(answers) {
			return modelfixture.Text("done"), nil
		}
		return answers[i], nil
	}
}

func call(id, name string, args map[string]any) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Args: args}
}

// lastResults are the results the newest request carries, by call ID.
func lastResults(r model.Request) map[string]model.ToolResult {
	out := map[string]model.ToolResult{}
	for _, m := range r.Messages {
		for _, res := range m.Results {
			out[res.CallID] = res
		}
	}
	return out
}

// TestNoRoutingLibraryPreloadsNothing: a deployment with no routing
// library gets experts by the model's loading from the index alone: the
// first call holds the case block and the question, no engine load_expert,
// and the index still lists the installed experts.
func TestNoRoutingLibraryPreloadsNothing(t *testing.T) {
	s, f := companionSession(t, script(modelfixture.Text("A synthetic answer.")))
	s.e.config.RoutingLibrary = ""
	if _, err := s.Talk(context.Background(), "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d model calls, want 1", len(reqs))
	}
	for i, m := range reqs[0].Messages {
		if len(m.Calls) > 0 {
			t.Errorf("message %d is a call with no routing library: %+v", i, m.Calls)
		}
	}
	if !strings.Contains(reqs[0].System, "`openshift.networking`") {
		t.Errorf("the index lacks the installed expert:\n%s", reqs[0].System)
	}
}

// TestCompanionConversation: a turn on the stub case begins its
// conversation from the case block, every item named, with the case routing key's
// primary expert preloaded; the model's calls to a primitive and a
// catalog tool run, their results stored as evidence under new names and
// returned; and the second call repeats the first call's prefix byte for
// byte, so a provider's cache serves it.
func TestCompanionConversation(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "grep", map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"}),
			call("c2", "run_tool", map[string]any{"id": "networking.ovn_errors", "params": map[string]any{}}),
		),
		modelfixture.Text("The OVN controller logs errors ", "[e9:1]."),
	))
	ctx := context.Background()
	reply, err := s.Talk(ctx, "What do the OVN logs say?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "The OVN controller logs errors [e9:1]." {
		t.Errorf("reply %q", reply.Text)
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	first, second := reqs[0], reqs[1]
	block := first.Messages[0].Text
	for _, want := range []string{"# The case", "## e1 · case details", "Pods on two workers cannot reach cluster DNS", "· attachments", "· comment"} {
		if !strings.Contains(block, want) {
			t.Errorf("the case block lacks %q:\n%s", want, block)
		}
	}
	if !first.Messages[0].Break {
		t.Error("the case block is not a cache break")
	}
	preload := first.Messages[1]
	if preload.Role != model.RoleModel || len(preload.Calls) != 1 || preload.Calls[0].Name != "load_expert" || preload.Calls[0].Args["id"] != "openshift.networking" {
		t.Fatalf("the preload: %+v", preload)
	}
	// The key the preload went by is the one the case's stored details
	// carry, on their routing keys line.
	if latest, _, err := caseCurrent(s.c); err != nil {
		t.Error(err)
	} else if details, err := s.c.ev.Get(latest[TypeCaseMetadata]); err != nil || !strings.Contains(string(details.Content), "routing keys: openshift-networking\n") {
		t.Errorf("the case details lack the routing keys line: %v\n%s", err, details.Content)
	}
	if keys := caseRoutingKeys(s.c); !slices.Equal(keys, []string{"openshift-networking"}) {
		t.Errorf("the case's routing keys: %q", keys)
	}
	if guidance := first.Messages[2].Results[0].Text; !strings.Contains(guidance, "# Expert openshift.networking") || !strings.Contains(guidance, "networking.ovn_errors") {
		t.Errorf("the preloaded guidance:\n%s", guidance)
	}
	if !strings.Contains(first.System, "`openshift.networking`") || !strings.Contains(first.System, "OVN-Kubernetes pod networking and cluster DNS") || !strings.Contains(first.System, "Plain and direct.") {
		t.Errorf("the system prompt lacks the expert index, its summaries or the house style")
	}

	results := lastResults(second)
	for _, id := range []string{"c1", "c2"} {
		r := results[id]
		if r.IsError || !strings.HasPrefix(r.Text, "e") {
			t.Errorf("result %s: %+v", id, r)
		}
		name, _, _ := strings.Cut(r.Text, " ")
		evID, err := evidenceByName(s.c, name)
		if err != nil || !s.c.ev.Exists(evID) {
			t.Errorf("result %s names %s, which is not stored evidence: %v", id, name, err)
		}
	}

	// The prefix: the same system prompt and tools, and every message the
	// first call sent, unchanged, at the head of the second.
	if first.System != second.System {
		t.Error("the system prompt changed between calls")
	}
	if d := cmp.Diff(first.Tools, second.Tools); d != "" {
		t.Errorf("the tools changed between calls (-first +second):\n%s", d)
	}
	if d := cmp.Diff(first.Messages, second.Messages[:len(first.Messages)]); d != "" {
		t.Errorf("the second call does not begin with the first call's messages (-first +second):\n%s", d)
	}

	var records int
	if err := s.c.st.DB().QueryRow(`SELECT count(*) FROM invocations WHERE task_type = 'companion'`).Scan(&records); err != nil || records != 2 {
		t.Errorf("%d invocation records, %v; want 2", records, err)
	}
	conv, err := companion.Store{DB: s.c.st.DB()}.Latest(ctx)
	if err != nil || len(conv.Pins) != 1 {
		t.Errorf("the conversation's pins: %+v, %v", conv.Pins, err)
	}
}

// TestCompanionRefusesCalls: a call naming no tool, one whose parameters
// its schema refuses, and a command the allowlist refuses are not run;
// each result is the refusal with its reason, nothing is stored for them,
// and the valid call of the same response runs.
func TestCompanionRefusesCalls(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "rm_everything", map[string]any{}),
			call("c2", "grep", map[string]any{"path": "namespaces"}),
			call("c3", "command", map[string]any{"argv": []any{"rm", "-rf", "namespaces"}}),
			call("c4", "run_tool", map[string]any{"id": "networking.ovn_errors", "params": map[string]any{"max": "lots"}}),
			call("c5", "find", map[string]any{"path": "namespaces"}),
		),
		modelfixture.Text("Only the listing ran."),
	))
	ctx := context.Background()
	before, _ := s.c.ev.List()
	if _, err := s.Talk(ctx, "Clean up.", nil); err != nil {
		t.Fatal(err)
	}
	results := lastResults(f.Requests()[1])
	for id, want := range map[string]string{"c1": "no tool", "c2": "pattern", "c3": "rm", "c4": "max"} {
		if r := results[id]; !r.IsError || !strings.Contains(r.Text, want) {
			t.Errorf("result %s: %+v, want a refusal naming %q", id, r, want)
		}
	}
	if r := results["c5"]; r.IsError || !strings.HasPrefix(r.Text, "e") {
		t.Errorf("the valid call's result: %+v", r)
	}
	after, _ := s.c.ev.List()
	if len(after) != len(before)+1 {
		t.Errorf("the calls stored %d items, want the valid call's 1", len(after)-len(before))
	}
}

// TestCompanionConsult: a consult runs in a conversation of its own under
// the main one, beginning with the same case block, its expert's guidance
// and the question; its answer returns as the consult call's result.
func TestCompanionConsult(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "consult", map[string]any{"expert": "openshift.platform", "question": "Are the workers Ready?"})),
		modelfixture.Text("Both workers are Ready."),
		modelfixture.Text("The platform expert finds both workers Ready."),
	))
	ctx := context.Background()
	reply, err := s.Talk(ctx, "Ask the platform expert about the nodes.", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 3 || reply.Text != "The platform expert finds both workers Ready." {
		t.Fatalf("%d calls, reply %q", len(reqs), reply.Text)
	}
	consult := reqs[1]
	if consult.Messages[0].Text != reqs[0].Messages[0].Text {
		t.Error("the consult does not begin with the main conversation's case block")
	}
	if q := consult.Messages[1].Text; !strings.Contains(q, "# Expert openshift.platform") || !strings.Contains(q, "Are the workers Ready?") {
		t.Errorf("the consult's question:\n%s", q)
	}
	if r := lastResults(reqs[2])["c1"]; r.IsError || !strings.Contains(r.Text, "Both workers are Ready.") {
		t.Errorf("the consult's result: %+v", r)
	}
	st := companion.Store{DB: s.c.st.DB()}
	main, _ := st.Latest(ctx)
	consults, err := st.Consults(ctx, main.ID)
	if err != nil || len(consults) != 1 || consults[0].Expert != "openshift.platform" {
		t.Errorf("consults kept: %+v, %v", consults, err)
	}
}

// TestCompanionCaseUpdate: a comment that arrives after the conversation
// began is appended as a case update, the case block left unchanged.
func TestCompanionCaseUpdate(t *testing.T) {
	s, f := companionSession(t, nil)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	added := append(append([]Comment(nil), companionComments...),
		Comment{ID: "c4", Author: Party{Name: "customer"}, Created: time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC), Body: "The new must-gather is attached now."})
	if _, err := s.e.acquire(ctx, s.c, companionCase, added); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Anything new?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	first, last := reqs[0], reqs[len(reqs)-1]
	if first.Messages[0].Text != last.Messages[0].Text {
		t.Error("the case block changed")
	}
	var update string
	for _, m := range last.Messages {
		if strings.HasPrefix(m.Text, "# Case update") {
			update = m.Text
		}
	}
	if !strings.Contains(update, "The new must-gather is attached now.") {
		t.Errorf("no case update holds the new comment; messages:\n%s", texts(last))
	}
	if strings.Contains(first.Messages[0].Text, "The new must-gather is attached now.") {
		t.Error("the first call already held the new comment")
	}
}

// TestConsultGetsCaseUpdates: a consult is given, after the case block,
// the case updates its main conversation was given since, so a correction
// that came after the case block reaches the expert before the question.
func TestConsultGetsCaseUpdates(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Text("Nothing new yet."),
		modelfixture.Calling(call("c1", "consult", map[string]any{"expert": "openshift.platform", "question": "When did the outage begin?"})),
		modelfixture.Text("At 07:40, as corrected."),
		modelfixture.Text("The platform expert dates it to 07:40."),
	))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	added := append(append([]Comment(nil), companionComments...),
		Comment{ID: "c4", Author: Party{Name: "customer"}, Created: time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC), Body: "Correction: the outage began at 07:40, not 08:16."})
	if _, err := s.e.acquire(ctx, s.c, companionCase, added); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Ask the platform expert when it began.", nil); err != nil {
		t.Fatal(err)
	}
	var consult *model.Request
	for _, r := range f.Requests() {
		if last := r.Messages[len(r.Messages)-1]; strings.Contains(last.Text, "When did the outage begin?") {
			consult = &r
		}
	}
	if consult == nil {
		t.Fatal("no consult request")
	}
	update := slices.IndexFunc(consult.Messages, func(m model.Message) bool {
		return strings.HasPrefix(m.Text, "# Case update") && strings.Contains(m.Text, "the outage began at 07:40")
	})
	if update < 1 || update != len(consult.Messages)-2 {
		t.Errorf("the correction is not given between the case block and the question; messages:\n%s", texts(*consult))
	}
}

func texts(r model.Request) string {
	var b strings.Builder
	for _, m := range r.Messages {
		j, _ := json.Marshal(m)
		b.Write(j)
		b.WriteString("\n")
	}
	return b.String()
}

// TestCompanionToolKeywords: every tool's parameter schema uses only the
// JSON Schema keywords every provider accepted live, since a provider
// refuses a whole request over one it does not take; with a reference
// service registered, its tools too.
func TestCompanionToolKeywords(t *testing.T) {
	s, _ := companionSession(t, nil)
	s.e.references = map[string]ReferenceAdapter{"kcs": &fakeReferences{id: "kcs"}}
	accepted := map[string]bool{}
	for _, k := range vertexgemini.Keywords {
		if slices.Contains(vertexanthropic.Keywords, k) && slices.Contains(openai.Keywords, k) {
			accepted[k] = true
		}
	}
	tools := s.e.companionTools(s.c)
	var names []string
	for _, tl := range tools {
		d := tl.Decl()
		names = append(names, d.Name)
		for _, k := range model.Keywords(d.Parameters) {
			if !accepted[k] {
				t.Errorf("tool %s uses %q, which no live call accepted on every provider", d.Name, k)
			}
		}
	}
	if !slices.Contains(names, "kcs_search") {
		t.Errorf("no kcs_search among %v", names)
	}
}

// byCall are the results the newest request carries for the given calls,
// in the order given.
func byCall(r model.Request, ids ...string) []model.ToolResult {
	all := lastResults(r)
	out := make([]model.ToolResult, len(ids))
	for i, id := range ids {
		out[i] = all[id]
	}
	return out
}

// TestCompanionCatalogTools: run_tool runs a catalog tool no loaded expert
// lists and refuses one given invalid parameters; an expert using a
// catalog loads beside the preloaded one, which uses none, listing each
// tool with its parameters, a required one marked; and a later turn reads
// the first turn's result by its name.
func TestCompanionCatalogTools(t *testing.T) {
	td := t.TempDir()
	install(t, td, "core-0.1.0", "core-0.1.1", "core-0.2.0", "network", "storage")
	// fixture.storage uses the core catalog's pods too, whose namespace is
	// required.
	p := filepath.Join(td, "storage", "expert.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), "tools: [node_ready, degraded_operators]", "tools: [node_ready, degraded_operators, pods]", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var first string
	s, f := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch len(r.Messages) {
		case 4: // the case, the preload's call and result, the question
			return modelfixture.Calling(
				call("c1", "run_tool", map[string]any{"id": "fixture.core.degraded_operators", "params": map[string]any{}}),
				call("c2", "run_tool", map[string]any{"id": "fixture.core.node_ready", "params": map[string]any{"node": 7}}),
				call("c3", "load_expert", map[string]any{"id": "fixture.storage"}),
			), nil
		case 6:
			first, _, _ = strings.Cut(lastResults(r)["c1"].Text, " ")
			return modelfixture.Text("Degraded operators are listed in [" + first + ":1]."), nil
		case 8: // the next turn
			return modelfixture.Calling(call("c4", "evidence_read", map[string]any{"id": first})), nil
		}
		return modelfixture.Text("done"), nil
	}, td)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Which operators are degraded?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Show that list again.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d model calls, want 4", len(reqs))
	}
	r := byCall(reqs[1], "c1", "c2", "c3")
	if r[0].IsError || !strings.Contains(r[0].Text, "fixture.core.degraded_operators@") {
		t.Errorf("the catalog tool's result: %+v", r[0])
	}
	if !r[1].IsError || !strings.Contains(r[1].Text, "node") {
		t.Errorf("invalid parameters: %+v, want a refusal naming the parameter", r[1])
	}
	if r[2].IsError || !strings.Contains(r[2].Text, "# Expert fixture.storage") {
		t.Errorf("the catalog-using expert's guidance: %+v", r[2])
	}
	// Its own test and its catalog's tools, sorted by ID, each in a line
	// with its parameters.
	var listed []string
	for _, l := range strings.Split(r[2].Text, "\n") {
		if sig, ok := strings.CutPrefix(l, "- `"); ok {
			sig, _, _ = strings.Cut(sig, "`")
			listed = append(listed, sig)
		}
	}
	want := []string{"fixture.core.degraded_operators()", "fixture.core.node_ready(node)", "fixture.core.pods(namespace*)", "storage.node_files()"}
	if d := cmp.Diff(want, listed); d != "" {
		t.Errorf("the tools load_expert lists (-want +got):\n%s", d)
	}
	if !strings.Contains(r[2].Text, "* marks a required one") {
		t.Errorf("load_expert's listing does not say what * marks:\n%s", r[2].Text)
	}
	conv, err := companion.Store{DB: s.c.st.DB()}.Latest(ctx)
	if err != nil || len(conv.Pins) != 2 {
		t.Errorf("pins %d, %v; want the preloaded expert's and fixture.storage's", len(conv.Pins), err)
	}
	read := byCall(reqs[3], "c4")[0]
	if read.IsError || !strings.Contains(read.Text, first) {
		t.Errorf("the later turn's read of %s: %+v", first, read)
	}
}

// TestCompanionLoadListsToolsOnce: an expert loaded into a conversation
// after another that shares a catalog tool names the shared tool by ID
// only and lists its own in full, after the session is reopened too; a
// consult's own conversation lists every tool in full.
func TestCompanionLoadListsToolsOnce(t *testing.T) {
	td := t.TempDir()
	install(t, td, "core-0.1.1", "network", "storage")
	// fixture.network at the core catalog fixture.storage resolves, so the
	// two share node_ready as listed.
	p := filepath.Join(td, "network", "expert.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), "version: '>=0.2.0 <0.3.0'", "version: '>=0.1.0 <0.2.0'", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "load_expert", map[string]any{"id": "fixture.storage"})),
		modelfixture.Text("Loaded."),
		modelfixture.Calling(call("c2", "load_expert", map[string]any{"id": "fixture.network"})),
		modelfixture.Text("Loaded too."),
		modelfixture.Calling(call("c3", "consult", map[string]any{"expert": "fixture.network", "question": "Are the workers Ready?"})),
		modelfixture.Text("Both workers are Ready."),
		modelfixture.Text("The network expert finds both workers Ready."),
	), td)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Load the storage expert.", nil); err != nil {
		t.Fatal(err)
	}
	s = reopenHistorySession(t, s)
	if _, err := s.Talk(ctx, "Load the network expert.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Ask the network expert.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 7 {
		t.Fatalf("%d model calls, want 7", len(reqs))
	}
	listing := func(text string) []string {
		var out []string
		for _, l := range strings.Split(text, "\n") {
			if sig, ok := strings.CutPrefix(l, "- `"); ok {
				sig, _, _ = strings.Cut(sig, "`")
				out = append(out, sig)
			}
		}
		return out
	}
	first := byCall(reqs[1], "c1")[0]
	if d := cmp.Diff([]string{"fixture.core.degraded_operators()", "fixture.core.node_ready(node)", "storage.node_files()"}, listing(first.Text)); first.IsError || d != "" {
		t.Errorf("the first load lists (-want +got):\n%s\n%s", d, first.Text)
	}
	second := byCall(reqs[3], "c2")[0]
	if d := cmp.Diff([]string{"fixture.core.pods(namespace*)"}, listing(second.Text)); second.IsError || d != "" {
		t.Errorf("the second load, after the reopen, lists (-want +got):\n%s\n%s", d, second.Text)
	}
	if !strings.Contains(second.Text, "Also uses, listed above: fixture.core.node_ready.\n") {
		t.Errorf("the second load does not name the shared tool by ID:\n%s", second.Text)
	}
	consult := reqs[5].Messages[1].Text
	if d := cmp.Diff([]string{"fixture.core.node_ready(node)", "fixture.core.pods(namespace*)"}, listing(consult)); d != "" || strings.Contains(consult, "Also uses") {
		t.Errorf("the consult lists (-want +got):\n%s\n%s", d, consult)
	}
}

// TestToolSignature: a tool's signature names its required parameters
// first, marked, a type that is not a string, and past the bound a count
// of the rest.
func TestToolSignature(t *testing.T) {
	params := map[string]expert.Param{
		"since":     {Type: "integer", Default: 0},
		"namespace": {Type: "string"},
		"node":      {Type: "string", Default: ""},
	}
	if got, want := signature(expert.Test{ID: "x.pods", Params: params}), "x.pods(namespace*, node, since:integer)"; got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	many := map[string]expert.Param{}
	for i := range signatureParams + 3 {
		many[fmt.Sprintf("p%02d", i)] = expert.Param{Type: "string", Default: ""}
	}
	if got := signature(expert.Test{ID: "x.many", Params: many}); !strings.HasSuffix(got, ", p07, 3 more)") {
		t.Errorf("a long signature %q, want the first %d and a count of 3", got, signatureParams)
	}
}

// TestExcerptEvidence: the name ExcerptEvidence reads is the one
// excerptHead wrote, and a result that is not an excerpt names none.
func TestExcerptEvidence(t *testing.T) {
	var it evidence.Item
	it.Size.Lines, it.Size.Bytes = 3, 1
	for _, lower := range []bool{false, true} {
		it.Size.LowerBound = lower
		head := excerptHead("e12", "find · a · b", it)
		if got := ExcerptEvidence(head + "\n1  x\n"); len(got) != 1 || got[0] != "e12" {
			t.Errorf("ExcerptEvidence(%q) = %v, want [e12]", head, got)
		}
	}
	for _, s := range []string{"e12 (log, 3 lines): lines 1–3", "e12 · the answer", "x1 · find · 3 lines, 1 byte", "# Expert fixture.storage"} {
		if got := ExcerptEvidence(s); got != nil {
			t.Errorf("ExcerptEvidence(%q) = %v, want none", s, got)
		}
	}
}

// TestCompanionPinAcrossUpgrade: an expert loaded into a conversation runs
// its tests at the version pinned after the installed copy is upgraded,
// from the case's snapshot; the conversation /compact starts loads the
// upgrade.
func TestCompanionPinAcrossUpgrade(t *testing.T) {
	root := t.TempDir()
	install(t, root, "core-0.1.1", "storage")
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "load_expert", map[string]any{"id": "fixture.storage"})),
		modelfixture.Text("Loaded."),
		modelfixture.Calling(call("c2", "run_tool", map[string]any{"id": "storage.node_files", "params": map[string]any{}}),
			call("c3", "describe_tool", map[string]any{"id": "storage.node_files"})),
		modelfixture.Text("Ran."),
		modelfixture.Text("Handoff: the node files were listed."),
		modelfixture.Calling(call("c4", "load_expert", map[string]any{"id": "fixture.storage"})),
		modelfixture.Text("Loaded again."),
	), root)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Load the storage expert.", nil); err != nil {
		t.Fatal(err)
	}
	// The upgrade: 0.2.0 in place of 0.1.0, its test at 0.2.0 too.
	for file, from := range map[string]string{"expert.yaml": "version: 0.1.0,", "tests.yaml": "version: 0.1.0"} {
		p := filepath.Join(root, "storage", file)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(data), from, strings.Replace(from, "0.1.0", "0.2.0", 1), 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.e.installed = nil
	if _, err := s.Talk(ctx, "List the node files.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Load the storage expert.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 7 {
		t.Fatalf("%d model calls, want 7", len(reqs))
	}
	r := byCall(reqs[3], "c2", "c3")
	if r[0].IsError || !strings.Contains(r[0].Text, "storage.node_files@0.1.0") || !strings.Contains(r[1].Text, "storage.node_files@0.1.0") {
		t.Errorf("after the upgrade the conversation ran %q and described %q; want the pinned 0.1.0", r[0].Text, r[1].Text)
	}
	if g := byCall(reqs[6], "c4")[0]; g.IsError || !strings.Contains(g.Text, "# Expert fixture.storage 0.2.0") {
		t.Errorf("the compacted conversation loaded %+v; want the upgrade", g)
	}
}

// TestCompanionCompactCarriesItems: the conversation /compact starts holds
// the engineer's items beside the handoff, as they were kept.
func TestCompanionCompactCarriesItems(t *testing.T) {
	s, _ := companionSession(t, script(
		modelfixture.Text("The DNS pods on worker-1 restart every few minutes."),
		modelfixture.Text("Handoff: DNS pods restart on worker-1."),
	))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What is wrong with DNS?", nil); err != nil {
		t.Fatal(err)
	}
	pinned, err := s.Pin(ctx)
	if err != nil || pinned.Name() != "i1" {
		t.Fatalf("pinned %+v, %v", pinned, err)
	}
	next, err := s.Compact(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := companion.Store{DB: s.c.st.DB()}.Messages(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	var handoff string
	for _, m := range msgs {
		if m.Kind == companion.KindHandoff {
			handoff = m.Text
		}
	}
	for _, want := range []string{"Handoff: DNS pods restart on worker-1.", "## i1 · pinned answer", "The DNS pods on worker-1 restart every few minutes."} {
		if !strings.Contains(handoff, want) {
			t.Errorf("the new conversation's handoff lacks %q:\n%s", want, handoff)
		}
	}
}

// TestCheckCites: an answer's citations resolve against the case — evidence
// the conversation showed, an artifact under the root, lines within the
// end — and each that does not says why: unknown evidence, evidence never
// shown, lines past the end, an unknown path.
func TestCheckCites(t *testing.T) {
	s, _ := companionSession(t, nil)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	// A stored item no conversation was shown, named for the case.
	hidden, _, err := s.Ingest(strings.NewReader("one\ntwo\n"), "pasted text", "")
	if err != nil {
		t.Fatal(err)
	}
	hiddenName, err := s.NameEvidence(hidden.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	answer := "Details [e1:1-2], past the end [e1:9999], nothing [e999], unshown [" + hiddenName + ":1]; " +
		"the node cluster-scoped-resources/core/nodes/worker-1.yaml:3, past its end cluster-scoped-resources/core/nodes/worker-1.yaml:90, and nowhere/at/all.yaml:1."
	cites := Cites(answer)
	checks, err := s.CheckCites(ctx, cites)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, c := range checks {
		what := cites[i].Name + cites[i].Path
		if c.OK {
			got = append(got, what+" ok")
			continue
		}
		got = append(got, what+" "+c.Why)
	}
	want := []string{
		"e1 ok",
		"e1 e1 has 9 lines",
		"e999 no evidence e999",
		hiddenName + " " + hiddenName + " was never shown in this conversation",
		"cluster-scoped-resources/core/nodes/worker-1.yaml ok",
		"cluster-scoped-resources/core/nodes/worker-1.yaml cluster-scoped-resources/core/nodes/worker-1.yaml has 9 lines",
		"nowhere/at/all.yaml no artifact nowhere/at/all.yaml",
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("citation checks (-want +got):\n%s", d)
	}
}

// TestCiteOfUnshownItemStaysFlagged: an answer citing an item no tool,
// case block or engineer showed the conversation is flagged once the answer
// is stored, and in a later answer: the model's own text discloses nothing.
func TestCiteOfUnshownItemStaysFlagged(t *testing.T) {
	var cite atomic.Pointer[string]
	s, _ := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		if c := cite.Load(); c != nil {
			return modelfixture.Text("It shows " + *c + "."), nil
		}
		return modelfixture.Text("done"), nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	hidden, _, err := s.Ingest(strings.NewReader("one\ntwo\n"), "pasted text", "")
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.NameEvidence(hidden.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := "[" + name + ":2]"
	cite.Store(&c)
	for _, ask := range []string{"What does it show?", "And now?"} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
		checks, err := s.CheckCites(ctx, Cites("It shows ["+name+":2]."))
		if err != nil {
			t.Fatal(err)
		}
		if len(checks) != 1 || checks[0].OK || checks[0].Why != name+" was never shown in this conversation" {
			t.Errorf("after %q, the answer's own citation of %s: %+v, want it flagged never shown", ask, name, checks)
		}
	}
}

// TestCiteOfNamedOnlyItemFlagged: an item the case block names without
// its content — an older comment on its line, a comment too large to
// give, named with its size — is not shown, so a citation of it is
// flagged, while a comment given in full is shown; reading the large one
// with evidence_read shows it.
func TestCiteOfNamedOnlyItemFlagged(t *testing.T) {
	var read atomic.Pointer[string]
	var asked atomic.Bool
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isLedgerCall(r) {
			return modelfixture.Text("done"), nil
		}
		if name := read.Load(); name != nil && !asked.Swap(true) {
			return modelfixture.Calling(call("r1", "evidence_read", map[string]any{"id": *name})), nil
		}
		return modelfixture.Text("done"), nil
	})
	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	more := []Comment{
		{ID: "c3", Author: Party{Name: "customer"}, Created: day, Body: "A synthetic third comment."},
		{ID: "c4", Author: Party{Name: "customer"}, Created: day.Add(24 * time.Hour), Body: "A synthetic fourth comment."},
		{ID: "c5", Author: Party{Name: "customer"}, Created: day.Add(48 * time.Hour), Body: "A synthetic fifth comment."},
		{ID: "c6", Author: Party{Name: "customer"}, Created: day.Add(72 * time.Hour), Body: "A synthetic sixth comment."},
		{ID: "c7", Author: Party{Name: "customer"}, Created: day.Add(96 * time.Hour), Body: strings.Repeat("A synthetic log line from the node.\n", 2500)},
	}
	addComments(t, s, more...)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	comments, err := s.LedgerComments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, cm := range comments {
		names[cm.Head.ID] = cm.Name
	}
	older, large, full := names["c1"], names["c7"], names["c6"]
	verdicts := func() []string {
		checks, err := s.CheckCites(ctx, Cites("["+older+":1] ["+large+":1] ["+full+":1]"))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range checks {
			out = append(out, fmt.Sprint(c.OK))
		}
		return out
	}
	if d := cmp.Diff([]string{"false", "false", "true"}, verdicts()); d != "" {
		t.Errorf("the older %s, the large %s and the full %s (-want +got):\n%s", older, large, full, d)
	}
	read.Store(&large)
	if _, err := s.Talk(ctx, "Read the long one.", nil); err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]string{"false", "true", "true"}, verdicts()); d != "" {
		t.Errorf("after reading %s (-want +got):\n%s", large, d)
	}
}

// TestCitesNumberTheLastName: in a citation naming several pieces of
// evidence, a bare line number belongs to the last name before it, and a
// bare name is that item whole, as in [e1, e4].
func TestCitesNumberTheLastName(t *testing.T) {
	var got []string
	for _, c := range Cites("refused [e35:4, e40:18, 24, 25-26] and [e12:2-3, 7]; reported [e1, e4] and [e5, e6:2, 3]") {
		got = append(got, fmt.Sprintf("%s:%d-%d", c.Name, c.From, c.To))
	}
	want := []string{"e35:4-0", "e40:18-0", "e40:24-0", "e40:25-26", "e12:2-3", "e12:7-0", "e1:0-0", "e4:0-0", "e5:0-0", "e6:2-0", "e6:3-0"}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("citations (-want +got):\n%s", d)
	}
}

// TestCitesReadLedgerItems: a bracket naming case-ledger items among
// evidence names, as in [l26, e45] and [e36,
// l22, e63], is read: each evidence name with its lines and
// each ledger item by its ID, the evidence names apart. A bare line after
// a ledger item, or a ledger item with lines, is no form read.
func TestCitesReadLedgerItems(t *testing.T) {
	var got []string
	for _, c := range Cites("held [l26, e45] and [e36, l22, e63:4-5, 7]; read [l3]; not [e1, l2, 4] nor [l2:3]") {
		got = append(got, fmt.Sprintf("%s %s%s:%d-%d", c.Text, c.Name, c.Ledger, c.From, c.To))
	}
	both := "[e36, l22, e63:4-5, 7]"
	want := []string{"[l26, e45] l26:0-0", "[l26, e45] e45:0-0", both + " e36:0-0", both + " l22:0-0", both + " e63:4-5", both + " e63:7-0", "[l3] l3:0-0"}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("citations (-want +got):\n%s", d)
	}
}

// TestLedgerItemCitedFlagged: a case-ledger item cited, alone or in a
// bracket with evidence names, is flagged, never resolved as evidence:
// it is the ledger's reading of its comment, named by its number and
// evidence name, whose lines to cite instead; an ID no item has names
// nothing. The evidence names beside it are checked, and read, as any:
// no such bracket is flagged as a form not read, the editor is told of
// each ledger item and quoted the evidence alone, a statement citing only
// a ledger item is a candidate citing nothing, and the ranked evidence and
// the trajectory count the evidence names, never a ledger ID.
func TestLedgerItemCitedFlagged(t *testing.T) {
	const log = "nodes/worker-1/dns.log"
	answer := func(e string) string {
		return "The lookups timed out [l1, " + e + ":3-4]. The customer restarted the pods at 09:00 [l1]. The lookups failed again [" + e + ":20, l9]."
	}
	s, names := readFilesSession(t, []readFile{{log, numberedLines(30, nil)}}, func(n map[string]string) string { return answer(n[log]) })
	e := names()[log]
	ctx := context.Background()
	comments, err := s.LedgerComments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(comments, func(c LedgerComment) bool { return c.Head.ID == "c2" })
	if i < 0 {
		t.Fatalf("no comment c2 in %+v", comments)
	}
	comment := comments[i]
	id, err := evidenceByName(s.c, comment.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertLedgerItem(ctx, s.c.st.DB(), s.c.id, 1, LedgerItem{ID: "l1", Kind: ledger.Reports, Who: AuthorSupport,
		Text: "A synthetic report: the pods were restarted.", CommentID: id, MadeBy: "engineer"}, "2026-11-06T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	cites := Cites(answer(e))
	checks, err := s.CheckCites(ctx, cites)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, c := range checks {
		what := cites[i].Name + cites[i].Ledger
		if c.OK {
			got = append(got, what+" ok")
			continue
		}
		got = append(got, what+" "+c.Why)
	}
	l1 := fmt.Sprintf("l1 is the case ledger's reading of comment c#%d (%s), not evidence: cite that comment's lines instead", comment.Number, comment.Name)
	l9 := "l9 names nothing: the case ledger has no item l9"
	if d := cmp.Diff([]string{"l1 " + l1, e + " ok", "l1 " + l1, e + " ok", "l9 " + l9}, got); d != "" {
		t.Errorf("citation checks (-want +got):\n%s", d)
	}
	flags, err := s.AnswerFlags(ctx, []string{answer(e)})
	if err != nil || len(flags) != 1 || len(flags[0]) != 0 {
		t.Errorf("the answer's flags: %+v, %v; want none", flags, err)
	}
	req, err := s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Quotes) != 1 || req.Quotes[0].Name != e || len(req.Quotes[0].Cites) != 2 {
		t.Errorf("quoted %+v; want %s alone, cited twice", req.Quotes, e)
	}
	for _, want := range []string{"- [l1, " + e + ":3-4]: " + l1 + "\n", "- [l1]: " + l1 + "\n", "- [" + e + ":20, l9]: " + l9 + "\n", "- s2 " + CandidateUncited + ".\n"} {
		if !strings.Contains(req.Text, want) {
			t.Errorf("the editor's request lacks %q:\n%s", want, req.Text)
		}
	}
	var ranked []string
	for _, u := range citeUses(cites, rankCited, 0, "cited by the model") {
		ranked = append(ranked, fmt.Sprintf("%s %v", u.name, u.ranges))
	}
	if d := cmp.Diff([]string{e + " [{3 4} {20 20}]"}, ranked); d != "" {
		t.Errorf("the ranked evidence's uses (-want +got):\n%s", d)
	}
	if added := wholeAdded(answer(e), "Revised [l1] [l9].", nil); len(added) != 0 {
		t.Errorf("a revision citing ledger items added %q", added)
	}
	tr, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Turns) != 1 || tr.Turns[0].Evidence.Cited != 1 {
		t.Errorf("the trajectory's evidence: %+v; want %s cited", tr.Turns, e)
	}
}

// TestCustomerDraftCarriesCertaintyReview: a draft the customer will read is
// asked with a certainty review — the
// most certain wording in the model's own voice, the words it never uses
// and the root-cause rule — and an internal note without it.
func TestCustomerDraftCarriesCertaintyReview(t *testing.T) {
	s, f := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return modelfixture.Text("A synthetic draft."), nil
	})
	s.e.config.Companion.SupportName = "Acme Care"
	s.e.config.Comments.Intentions = []config.Intention{
		{ID: "customer-update", Label: "Customer update", Audience: "customer", Purpose: "Tell the customer what was found."},
		{ID: "handover", Label: "Handover", Audience: "internal", Purpose: "Hand the case to the next engineer."},
	}
	ctx := context.Background()
	for _, id := range []string{"customer-update", "handover"} {
		if _, _, err := s.Draft(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.Requests()
	asked := func(r model.Request) string { return r.Messages[len(r.Messages)-1].Text }
	customer, internal := asked(reqs[len(reqs)-2]), asked(reqs[len(reqs)-1])
	for _, want := range []string{"Draft Customer update", "the evidence strongly suggests", "smoking gun", "Never name a cause in your own voice"} {
		if !strings.Contains(customer, want) {
			t.Errorf("the customer draft's request lacks %q:\n%s", want, customer)
		}
	}
	if !strings.Contains(internal, "Draft Handover") || !strings.Contains(internal, "for Acme Care only, as an internal note") || strings.Contains(internal, "root cause") {
		t.Errorf("the internal note's request:\n%s", internal)
	}
	req, err := s.ReviewRequest(ctx, ReviewAsk{Item: "i2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Text, "the draft i2 for handover, for Acme Care only") {
		t.Errorf("the internal note's review request:\n%s", req.Text)
	}
}

func TestConfiguredSupportCaseBlockAndHandoff(t *testing.T) {
	s, f := companionSession(t, script(modelfixture.Text("Synthetic answer."), modelfixture.Text("Synthetic handoff.")))
	s.e.config.Companion.SupportName = "Acme Care"
	var comments []Comment
	for i := range caseFullComments + 1 {
		comments = append(comments, Comment{
			ID: fmt.Sprintf("support-%d", i), Author: Party{Name: "Sam Synthetic"}, AuthorKind: AuthorSupport,
			Created: time.Date(2026, 10, 3+i, 9, 0, 0, 0, time.UTC), Body: "Synthetic support comment.",
		})
	}
	addComments(t, s, comments...)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Summarize the case.", nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range f.Requests()[0].Messages {
		if strings.Contains(m.Text, "2026-10-03 09:00 UTC · Acme Care") {
			found = true
		}
	}
	if !found {
		t.Error("the case block does not name the configured support organization")
	}
	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	last := reqs[len(reqs)-1]
	ask := last.Messages[len(last.Messages)-1].Text
	if !strings.Contains(ask, "what is awaited from the customer and from Acme Care,") {
		t.Errorf("the handoff request: %s", ask)
	}
}

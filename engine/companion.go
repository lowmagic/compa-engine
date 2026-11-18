package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/model/openai"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
	"github.com/lowmagic/compa-engine/internal/redact"
	"github.com/lowmagic/compa-engine/internal/routing"
)

// companionTask is the task the companion's model profile is resolved for,
// its class companion unless configuration names another.
const companionTask = "companion"

// companionVersion names the companion's loop and prompt in its records.
const companionVersion = "0.1.0"

// caseInline is the largest item the case block shows whole; a larger one
// is named with its size, for evidence_read.
const caseInline = 64 << 10

// geminiCacheTTL is how long an explicit Gemini cache lives unless
// configuration says otherwise (reasoning.vertex.cacheMinutes): long enough
// to span a turn's calls and the engineer's reading between turns.
const geminiCacheTTL = 10 * time.Minute

// orientAsk is the opening turn's instruction, the engine's words.
const orientAsk = "Orient me on this case: the orientation your instructions describe for the opening of a case."

// orientNoLedger is said with orientAsk when the case ledger was still
// reading the comments as the opening began.
const orientNoLedger = "The case ledger is still reading the comments and is not in this conversation yet; orient from the case block, and say that the ledger follows."

// TalkEvent reports a turn's progress as it runs.
type TalkEvent = companion.Event

// TalkReply is what a turn ended with: the text to print — the editor's
// correction or the model's revision where the editor ran — and what the
// editor's check made of it.
type TalkReply struct {
	companion.Reply
	Check AnswerCheck
}

// TalkEventKind says what a TalkEvent reports.
type TalkEventKind = companion.EventKind

// The kinds of TalkEvent: a piece of the answer's text; of the model's
// thinking; a tool call starting (Call set) and done (Call and Result); a
// model call made (Invocation set, its usage and timing); the conversation
// compacted (Conversation the new one, Text the handoff); a warning; a
// step of the editor's check beginning (Text StageChecking or
// StageRevising); and the answer's text replaced in what the engineer is
// shown (Text the text that replaces it).
const (
	TalkText      = companion.EventText
	TalkThinking  = companion.EventThinking
	TalkToolStart = companion.EventToolStart
	TalkToolDone  = companion.EventToolDone
	TalkCall      = companion.EventCall
	TalkCompacted = companion.EventCompacted
	TalkWarning   = companion.EventWarning
	TalkStage     = companion.EventStage
	TalkReplaced  = companion.EventReplaced
)

// Talk sends the engineer's words to the case's conversation, beginning
// one if the case has none, and runs it until the model answers.
func (s *Session) Talk(ctx context.Context, message string, on func(TalkEvent)) (TalkReply, error) {
	return s.turn(ctx, companion.KindEngineer, message, 0, on)
}

// Orient runs the opening turn, on the opening's budget of tool calls:
// the case's problem and status, its history from the comments, what is
// awaited, what each attachment can answer, and the open questions with
// the data that would answer each. Where the ledger started by Prepare is still reading,
// the model is told it follows.
func (s *Session) Orient(ctx context.Context, on func(TalkEvent)) (TalkReply, error) {
	ask := orientAsk
	if j := s.ledgerJob(); j != nil && !j.ended() {
		ask += " " + orientNoLedger
	}
	return s.turn(ctx, companion.KindEngine, ask, companion.OpeningBudget, on)
}

// Compact has the model write a handoff of the case's conversation and
// continues in a new one begun from the case as it is now and the
// handoff; it returns the new conversation's ID.
func (s *Session) Compact(ctx context.Context, on func(TalkEvent)) (string, error) {
	l, err := s.loop(ctx)
	if err != nil {
		return "", err
	}
	conv, err := s.conversation(ctx, l)
	if err != nil {
		return "", err
	}
	return l.Compact(ctx, conv, on)
}

// turn runs one turn on budget tool calls, the loop's default where 0,
// then has the editor check its answer.
func (s *Session) turn(ctx context.Context, kind companion.Kind, message string, budget int, on func(TalkEvent)) (TalkReply, error) {
	l, err := s.loop(ctx)
	if err != nil {
		return TalkReply{}, err
	}
	l.Budget = budget
	conv, err := s.conversation(ctx, l)
	if err != nil {
		return TalkReply{}, err
	}
	reply, err := l.Turn(ctx, conv, kind, message, on)
	if err != nil {
		return TalkReply{Reply: reply}, err
	}
	return s.edit(ctx, l, reply, on)
}

// loop is the companion's loop for the session's case, on the model the
// engineer chose or the companion's profile names.
func (s *Session) loop(ctx context.Context) (*companion.Loop, error) {
	e, c := s.e, s.c
	pc, err := s.profile()
	if err != nil {
		return nil, err
	}
	p, err := e.modelProvider(pc.Provider)
	if err != nil {
		return nil, err
	}
	d := p.Describe()
	l := s.storeLoop()
	l.Provider, l.Model, l.Effort = p, pc.Model, pc.Effort
	l.Gate = e.companionGate(c, d.EgressClass)
	l.Policy = redact.For(d.EgressClass)
	l.Record = func(inv companion.Invocation) error {
		return recordCall(c, companionTask, pc, d, inv)
	}
	return l, nil
}

// storeLoop is the companion's loop without a model: enough to estimate
// a conversation and to begin one, which call none.
func (s *Session) storeLoop() *companion.Loop {
	c := s.c
	return &companion.Loop{
		Store:       companion.Store{DB: c.st.DB()},
		System:      s.e.companionSystem(),
		Tools:       s.e.companionTools(c),
		SupportName: s.SupportName(),
		CaseBlock: func(context.Context) (string, []companion.Shown, error) {
			return caseBlock(c, s.inventory(), s.SupportName())
		},
		AfterBlock: func(ctx context.Context) (string, error) { return s.ledgerMessage(ctx, nil) },
		Carry:      func(ctx context.Context) (string, error) { return carried(ctx, c) },
		Now:        c.now,
		NewID:      c.newID,
	}
}

// conversation is the case's current conversation: the newest, with what
// the case gained since it was last shown appended as an update, then
// what of the ledger it has not seen; or a new one begun from the case
// block, the case routing key's primary expert preloaded, then the ledger. These
// are appended only here, as a turn begins, never while one runs.
func (s *Session) conversation(ctx context.Context, l *companion.Loop) (string, error) {
	conv, err := l.Store.Latest(ctx)
	var id string
	var msgs []companion.Message
	switch {
	case err == nil:
		id = conv.ID
		if msgs, err = l.Store.Messages(ctx, id); err != nil {
			return "", err
		}
		update, shows, err := caseUpdate(s.c, s.inventory(), msgs)
		if err != nil {
			return "", err
		}
		if update != "" {
			if err := l.Update(ctx, id, update, shows...); err != nil {
				return "", err
			}
		}
	case !errors.Is(err, companion.ErrNoConversation):
		return "", err
	default:
		block, shows, err := caseBlock(s.c, s.inventory(), s.SupportName())
		if err != nil {
			return "", err
		}
		if id, err = l.Start(ctx, block, shows...); err != nil {
			return "", err
		}
		if expertID := s.e.primaryExpert(s.c); expertID != "" {
			if _, err := l.Preload(ctx, id, "load_expert", map[string]any{"id": expertID}); err != nil {
				return "", err
			}
		}
	}
	led, err := s.ledgerMessage(ctx, msgs)
	if err != nil {
		return "", err
	}
	if led != "" {
		if err := l.Update(ctx, id, led); err != nil {
			return "", err
		}
	}
	return id, nil
}

// primaryExpert is the installed expert the routing library names primary for
// the case's first routing key it knows, or none.
func (e *Engine) primaryExpert(c *openCase) string {
	lib, err := routing.LoadLibrary(e.config.RoutingLibraryPath())
	if err != nil {
		return ""
	}
	installed := map[string]bool{}
	for _, d := range e.installedExperts() {
		installed[d.ID] = true
	}
	for _, name := range caseRoutingKeys(c) {
		key, ok := lib.Canonical(name)
		if !ok {
			continue
		}
		for _, r := range lib.Experts(key.ID) {
			if r.Relationship == "primary" && installed[r.ExpertID] {
				return r.ExpertID
			}
		}
	}
	return ""
}

// companionSystem is the system prompt: the companion's method, the
// installed experts by ID, summary and primary routing keys, the deployment's
// house style and intentions, and its house section.
func (e *Engine) companionSystem() string {
	var lines []companion.ExpertLine
	idx := e.experts().idx
	for _, d := range e.installedExperts() {
		entry, ok := idx.Describe(d.ID, d.Version)
		if !ok {
			continue
		}
		line := companion.ExpertLine{ID: d.ID, Summary: entry.Manifest.Metadata.Summary}
		for _, r := range entry.Manifest.Spec.Routing.Keys {
			if r.Relationship == "primary" {
				line.Keys = append(line.Keys, r.ID)
			}
		}
		lines = append(lines, line)
	}
	var intentions []companion.Intention
	for _, in := range e.config.Comments.Intentions {
		intentions = append(intentions, companion.Intention{ID: in.ID, Label: in.Label, Audience: in.Audience, Purpose: in.Purpose})
	}
	return companion.System(lines, e.config.Comments.Style, intentions, e.config.Companion.House)
}

// egressRank orders egress classes from the host outward.
var egressRank = map[string]int{"local_only": 0, "internal_network": 1, "external_provider": 2}

// companionGate is the check before every model call of the case: the
// provider's egress within what configuration allows, then the
// barred-accounts rule, read afresh each call, naming the engineer's
// override where one lets the call through.
func (e *Engine) companionGate(c *openCase, egress string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		allowed := e.config.MaxEgressClass()
		if rank, ok := egressRank[egress]; !ok || rank > egressRank[allowed] {
			return "", fmt.Errorf("%w: %s goes further than reasoning.maxEgressClass %s allows", companion.ErrEgressClass, egress, allowed)
		}
		d := e.barredDecision(ctx, c)
		if d.refused {
			return "", fmt.Errorf("%w: %s", companion.ErrBarred, d.reason)
		}
		if d.overridden != nil {
			return d.overridden.ID, nil
		}
		return "", nil
	}
}

// modelProvider is the provider a profile names, kept for the engine so its
// caches outlive one turn. A Vertex or OpenAI-compatible provider is made
// at its first call (deferredProvider), after the gate, never before it.
func (e *Engine) modelProvider(id string) (model.Provider, error) {
	e.modelsMu.Lock()
	defer e.modelsMu.Unlock()
	if p, ok := e.models[id]; ok {
		return p, nil
	}
	var p model.Provider
	switch id {
	case vertexgemini.ID:
		// Describe reads nothing of its provider.
		p = &deferredProvider{desc: (*vertexgemini.Provider).Describe(nil), build: func(ctx context.Context) (model.Provider, error) {
			v, _ := e.config.VertexSettings()
			ttl := geminiCacheTTL
			if v.CacheMinutes > 0 {
				ttl = time.Duration(v.CacheMinutes) * time.Minute
			}
			return vertexgemini.New(ctx, vertexgemini.Options{Project: v.Project, Location: v.Location, CacheTTL: ttl})
		}}
	case vertexanthropic.ID:
		p = &deferredProvider{desc: (*vertexanthropic.Provider).Describe(nil), build: func(ctx context.Context) (model.Provider, error) {
			v, _ := e.config.VertexSettings()
			return vertexanthropic.New(ctx, vertexanthropic.Options{Project: v.Project, Location: v.Location})
		}}
	case openai.ID:
		o := e.config.OpenAISettings()
		p = &deferredProvider{desc: openai.Describe(o.EgressClass), build: func(context.Context) (model.Provider, error) {
			key := ""
			if o.APIKeyEnv != "" {
				if key = os.Getenv(o.APIKeyEnv); key == "" {
					return nil, fmt.Errorf("openai: %s is not set (reasoning.openai.apiKeyEnv)", o.APIKeyEnv)
				}
			}
			return openai.New(openai.Options{BaseURL: o.BaseURL, APIKey: key, EgressClass: o.EgressClass, CacheMarks: o.CacheMarks})
		}}
	case "fixture":
		// Answers from the script its model names: for driving the real
		// binary without a model, never a deployment's model.
		p = modelfixture.Scripted()
	default:
		return nil, fmt.Errorf("no model provider %q (reasoning.models)", id)
	}
	if e.models == nil {
		e.models = map[string]model.Provider{}
	}
	e.models[id] = p
	return p, nil
}

// deferredProvider is a provider made at its first call rather than when a
// profile names it. Making one looks up its credentials, and the gate
// (companionGate) runs before every call, so a call the gate refuses, such
// as one past reasoning.maxEgressClass, is refused with the gate's reason
// and recorded as its outcome, with no credential looked up.
type deferredProvider struct {
	desc  model.Descriptor
	build func(context.Context) (model.Provider, error)
	mu    sync.Mutex
	made  model.Provider
}

func (d *deferredProvider) Describe() model.Descriptor { return d.desc }

// Call makes the provider where no call has, then sends r through it. A
// provider that could not be made is tried again at the next call.
func (d *deferredProvider) Call(ctx context.Context, r model.Request, stream func(model.Delta)) (*model.Response, error) {
	d.mu.Lock()
	if d.made == nil {
		// The provider outlives the call that made it: its credentials
		// must not end with the call's context.
		p, err := d.build(context.WithoutCancel(ctx))
		if err != nil {
			d.mu.Unlock()
			return nil, err
		}
		d.made = p
	}
	p := d.made
	d.mu.Unlock()
	return p.Call(ctx, r, stream)
}

// recordCall keeps one model call of the case's — the companion's, or a
// task's such as the ledger's — as an invocation record of its task,
// priced from the engine's table where its usage allows, so usage
// aggregates with every other call of the case. The record's
// InvestigationID holds the case's ID, which keys its usage.
func recordCall(c *openCase, task string, pc config.ProfileChoice, d model.Descriptor, in companion.Invocation) error {
	rec := invocations.Record{
		InvocationID: in.ID, InvestigationID: c.id, CorrelationID: in.Conversation,
		TaskType: task, Trigger: "case:" + c.id,
		HarnessID: "companion", AdapterVersion: companionVersion,
		Capabilities:           &invocations.Capabilities{EgressClass: d.EgressClass, UsageReporting: "exact", UsageScope: invocations.ScopePerInvocation},
		Model:                  invocations.ModelIdentity{Provider: in.Provider, ModelID: in.Model, Source: "engine_selected", Effort: in.Effort},
		ModelProfile:           &invocations.ModelProfile{Class: pc.Class, Source: pc.Source, Effort: pc.Effort},
		RedactionPolicyVersion: in.Policy,
		Request:                in.Request, RawResponse: in.Raw, Thinking: in.Thinking,
		ConversationID: in.Conversation, Turn: in.Turn, RebuiltFrom: in.Parent, ResentFrom: in.ResentFrom,
		Usage: in.Usage, Cache: in.Cache,
		QueuedAt: in.Started, StartedAt: in.Started, CompletedAt: in.Completed, Duration: in.Completed.Sub(in.Started),
		Override: in.Override, Outcome: in.Outcome, Problems: in.Problems,
		TurnCount: model.Reported(int64(in.Call)),
	}
	for _, call := range in.Calls {
		rec.ToolCalls = append(rec.ToolCalls, invocations.ToolCall{ID: call.ID, Test: call.Name, Params: call.Args})
	}
	rec.Cost = pricing.Unavailable
	if in.Cost != nil {
		amount := in.Cost.Amount
		rec.Cost = pricing.Cost{Amount: &amount, Currency: in.Cost.Currency, Fidelity: model.FidelityReported}
	} else if cost, ok := pricing.Vertex().Estimate(in.Provider, in.Model, in.Usage, in.Started); ok {
		rec.Cost = cost
	}
	return invocations.Put(c.st.DB(), rec)
}

// caseIntro opens the case block: what it holds, said once.
const caseIntro = `# The case

What the case's source reports, as the conversation began. Its details. Its attachments: the artifact inventory where there is one — each attachment's kind, size and state, the captures identified in it with their target and capture time, and whether each is current, outdated, a duplicate or uncertain, as the engine's rules decide without a model — or else their listing. The newest comments in full, newest first and digested: a comment repeating another, quoted replies, boilerplate recurring across comments and the middle of long runs of log lines are cut, and each cut is marked. Then every older comment by its name, date and author only. Each item is named; read one whole, an older comment included, with evidence_read.`

// caseFullComments is how many of the newest comments the case block
// gives in full; the older ones it names, one line each, since in a long
// case they are the bulk of the block and mostly out of date, and the
// ledger carries what they asked and settled.
const caseFullComments = 5

// chatIntro opens a troubleshooting chat with no case.
const chatIntro = `# No case

This is a troubleshooting chat with no case: there are no case details, comments or attachments. Work from what the engineer says.`

// caseBlock is the case as a new conversation begins with it, each item
// named for the case: its details, the inventory inv where there is one
// or else the attachment overview, the newest comments in full and the
// older ones by name; and the lines it shows with their content. The same
// case and inventory make the same bytes. A chat with no case says so,
// and where its artifact directory is.
func caseBlock(c *openCase, inv *Inventory, supportName string) (string, []companion.Shown, error) {
	if c.chat {
		if c.root == "" {
			return chatIntro + " There are no artifacts to read.", nil, nil
		}
		return chatIntro + " The tools read the artifacts in the directory the engineer gave, " + c.root + ", as the artifact root.", nil, nil
	}
	ids, comments, err := caseItemsNow(c, inv)
	if err != nil {
		return "", nil, err
	}
	full := comments[:min(len(comments), caseFullComments)]
	var b strings.Builder
	b.WriteString(caseIntro)
	b.WriteString("\n")
	shows, err := caseItems(c, &b, append(ids, full...), inv)
	if err != nil {
		return "", nil, err
	}
	older := comments[len(full):]
	if len(older) == 0 {
		return b.String(), shows, nil
	}
	fmt.Fprintf(&b, "\n## Older comments\n\n%s, newest first, by name, date and author; read one with evidence_read.\n\n", text.Plural(len(older), "older comment"))
	for _, id := range older {
		line, err := olderComment(c, id, supportName)
		if err != nil {
			return "", nil, err
		}
		b.WriteString(line + "\n")
	}
	return b.String(), shows, nil
}

// olderComment is an older comment as the case block names it: its name,
// when it was written and by which side.
func olderComment(c *openCase, id, supportName string) (string, error) {
	name, err := evidenceName(c, id)
	if err != nil {
		return "", err
	}
	it, err := c.ev.Get(id)
	if err != nil {
		return "", err
	}
	cm := commentOf(id, it.Content)
	when := "date unknown"
	if !cm.Created.IsZero() {
		when = cm.Created.UTC().Format("2006-01-02 15:04 UTC")
	}
	who := "author unknown"
	switch cm.AuthorKind {
	case "":
	case AuthorSupport:
		who = supportName
	default:
		who = cm.AuthorKind
	}
	if cm.Automated {
		who += ", automated"
	}
	return olderPrefix(name) + when + " · " + who, nil
}

// olderPrefix begins an older comment's line in the case block, which
// caseUpdate counts as showing it.
func olderPrefix(name string) string { return "- " + name + " · comment · " }

// caseUpdate is what the case gained since its items were last shown in a
// conversation, given its messages, and the lines it shows, or nothing.
func caseUpdate(c *openCase, inv *Inventory, msgs []companion.Message) (string, []companion.Shown, error) {
	fresh, err := freshItems(c, inv, msgs)
	if err != nil || len(fresh) == 0 {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString("# Case update\n\nNew or changed since the case was last shown here, as its source reports it now; a changed item replaces the one of the same comment or kind shown before.\n")
	shows, err := caseItems(c, &b, fresh, inv)
	if err != nil {
		return "", nil, err
	}
	return b.String(), shows, nil
}

// freshItems are the case's current items, as caseItemsNow gives them,
// that no case block or case update of the conversation shows: under its
// heading, or, for an older comment, on its line. A name alone is not a
// showing: the ledger names the comments it reads.
func freshItems(c *openCase, inv *Inventory, msgs []companion.Message) ([]string, error) {
	ids, comments, err := caseItemsNow(c, inv)
	if err != nil {
		return nil, err
	}
	var shown strings.Builder
	for _, m := range msgs {
		if m.Kind == companion.KindCase || (m.Kind == companion.KindUpdate && !strings.HasPrefix(m.Text, ledgerHeading)) {
			shown.WriteString(m.Text)
		}
	}
	var fresh []string
	for _, id := range append(ids, comments...) {
		named, err := isNamed(c, id)
		if err != nil {
			return nil, err
		}
		if !named {
			fresh = append(fresh, id)
			continue
		}
		name, err := evidenceName(c, id)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(shown.String(), "\n## "+name+" · ") && !strings.Contains(shown.String(), "\n"+olderPrefix(name)) {
			fresh = append(fresh, id)
		}
	}
	return fresh, nil
}

// caseItems writes each item under its name: whole up to caseInline, the
// comments digested, a larger item by its size, and the inventory inv, where
// one of ids is its item, as its text with each identity's evidence named.
// It returns what it gave of each item's content: an item whole, the
// lines a digest kept, and nothing of one named by its size.
func caseItems(c *openCase, b *strings.Builder, ids []string, inv *Inventory) ([]companion.Shown, error) {
	inputs, err := evidenceInputs(c, ids, caseInline)
	if err != nil {
		return nil, err
	}
	inputs, _ = digestComments(inputs)
	var shows []companion.Shown
	for _, in := range inputs {
		name, err := evidenceName(c, in.Ref.ID)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(b, "\n## %s · %s\n", name, caseItemTitle(in.Ref.Type))
		if inv != nil && in.Ref.ID == inv.Evidence {
			t, err := inventoryText(c, *inv)
			if err != nil {
				return nil, err
			}
			b.WriteString(t)
			shows = append(shows, companion.Shown{Name: name})
			continue
		}
		if in.Ref.Note != "" {
			for _, l := range strings.Split(in.Ref.Note, "\n") {
				fmt.Fprintf(b, "! %s\n", l)
			}
		}
		if !in.Inline {
			size := "its size unknown"
			if in.Size != nil {
				size = fmt.Sprintf("%d lines, %d bytes", in.Size.Lines, in.Size.Bytes)
			}
			fmt.Fprintf(b, notShown+"%s: read it with evidence_read %s.\n", size, name)
			continue
		}
		b.WriteString(strings.TrimRight(in.Text, "\n"))
		b.WriteString("\n")
		if in.Lines == nil {
			shows = append(shows, companion.Shown{Name: name})
			continue
		}
		// A digest's text: the stored lines it kept, 0 for its own.
		var kept []int
		for _, n := range in.Lines {
			if n > 0 {
				kept = append(kept, n)
			}
		}
		shows = append(shows, lineRanges(name, kept)...)
	}
	return shows, nil
}

// lineRanges are the stored line numbers nums, ascending, of item name as
// ranges of consecutive lines.
func lineRanges(name string, nums []int) []companion.Shown {
	var out []companion.Shown
	for _, n := range nums {
		if k := len(out) - 1; k >= 0 && out[k].To == n-1 {
			out[k].To = n
			continue
		}
		out = append(out, companion.Shown{Name: name, From: n, To: n})
	}
	return out
}

// inventoryText is the inventory's text with each identity run's evidence
// by its short name.
func inventoryText(c *openCase, inv Inventory) (string, error) {
	atts := make([]InventoryAttachment, len(inv.Attachments))
	for i, ia := range inv.Attachments {
		ia.Runs = slices.Clone(ia.Runs)
		for j, r := range ia.Runs {
			if r.Evidence == "" {
				continue
			}
			name, err := evidenceName(c, r.Evidence)
			if err != nil {
				return "", err
			}
			ia.Runs[j].Evidence = name
		}
		atts[i] = ia
	}
	inv.Attachments = atts
	return inv.Text(), nil
}

func caseItemTitle(t string) string {
	switch t {
	case TypeCaseMetadata:
		return "case details"
	case TypeArtifactOverview:
		return "attachments"
	case TypeArtifactInventory:
		return "artifact inventory"
	case TypeCaseComment:
		return "comment"
	}
	return strings.ReplaceAll(t, "_", " ")
}

// caseItemsNow are the case's items a conversation is shown: its details
// and its inventory inv where there is one, in place of the attachment
// overview, then its comments, newest first, as companionItems gives them.
func caseItemsNow(c *openCase, inv *Inventory) (ids, comments []string, err error) {
	all, err := companionItems(c)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range all {
		it, err := c.ev.Get(id)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case it.Ref.Type == TypeCaseComment:
			comments = append(comments, id)
		case it.Ref.Type == TypeArtifactOverview && inv != nil && inv.Evidence != "":
		default:
			ids = append(ids, id)
		}
	}
	if inv != nil && inv.Evidence != "" {
		ids = append(ids, inv.Evidence)
	}
	return ids, comments, nil
}

// companionItems are the case's own items as the case block shows them:
// its latest details and attachment overview, then the latest version of
// each comment, newest first, those the engineer's exclusion rules leave
// out of what models read left out.
func companionItems(c *openCase) ([]string, error) {
	latest, comments, err := caseCurrent(c)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range []string{TypeCaseMetadata, TypeArtifactOverview} {
		if id := latest[t]; id != "" {
			ids = append(ids, id)
		}
	}
	rules, err := exclusions(c)
	if err != nil {
		return nil, err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		id := latest[comments[i]]
		if len(rules) > 0 {
			it, err := c.ev.Get(id)
			if err != nil {
				return nil, err
			}
			if ExcludedBy(rules, commentOf(id, it.Content)) != nil {
				continue
			}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// caseRoutingKeys are the routing keys the case's latest details name.
func caseRoutingKeys(c *openCase) []string {
	latest, _, err := caseCurrent(c)
	if err != nil || latest[TypeCaseMetadata] == "" {
		return nil
	}
	it, err := c.ev.Get(latest[TypeCaseMetadata])
	if err != nil {
		return nil
	}
	head, _, _ := strings.Cut(string(it.Content), "\n\n")
	for _, l := range strings.Split(head, "\n") {
		if v, ok := strings.CutPrefix(l, "routing keys: "); ok {
			return strings.Split(v, ", ")
		}
	}
	return nil
}

// evidenceInputs offers items to a request: one no larger than maxInline
// is loaded and inlined, a larger one referenced with its size, never read.
func evidenceInputs(c *openCase, ids []string, maxInline int64) ([]evidence.Input, error) {
	out := make([]evidence.Input, 0, len(ids))
	for _, id := range ids {
		it, err := c.ev.Describe(id)
		if err != nil {
			return nil, err
		}
		if it.Size.Bytes > maxInline {
			size := it.Size
			out = append(out, evidence.Input{Ref: it.Ref, Size: &size})
			continue
		}
		if it, err = c.ev.Get(id); err != nil {
			return nil, err
		}
		out = append(out, it.Input())
	}
	return out, nil
}

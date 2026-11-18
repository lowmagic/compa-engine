// Package companion is the engine's conversation loop: one conversation
// per case, sent whole on every model call, append-only after its fixed
// prefix, so consecutive calls share what a provider has cached. The model
// answers in free text and asks for tools; the loop checks each call
// against the tool table, runs it through the tool, and sends the results
// back until the model answers. It applies the egress policy and the
// case's gate before every call, and records every call
// (docs/MODEL_API.md).
//
// The loop knows tools only through Tool: the engine builds the table, the
// case block and the gate, so this package holds no case machinery.
package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/redact"
)

// Defaults, each overridden by configuration where it says so.
const (
	// DefaultBudget is how many tool calls one engineer turn may make,
	// consults included, before the model is told to answer.
	DefaultBudget = 40
	// OpeningBudget is the opening turn's tool calls: the case block and
	// the ledger hold the case's state, so the calls are for what the
	// inventory leaves open.
	OpeningBudget = 8
	// TokenBudget is the input one turn's model calls may sum to, consults
	// included, before the model is told to answer, each call counted by
	// charge: each call sends the whole conversation, so a long tool loop
	// costs about calls times context. The first turns measured live
	// ended one at 11 calls when cache reads counted whole.
	TokenBudget = 400_000
	// CacheReadWeight is what a token read from the provider's cache counts
	// for against the token budget, an uncached input token counting 1: a
	// cache read is priced at a tenth of input on every model in
	// internal/model/pricing's table, so the budget bounds a turn's input
	// cost rather than the context it resends.
	CacheReadWeight = 0.1
	// DefaultCompactAt is the estimated input past which compaction is
	// offered.
	DefaultCompactAt = 150_000
	// maxConcurrent is how many calls of one response run at once.
	maxConcurrent = 8
	// afterBudget is how many more model calls a turn makes once its budget
	// is spent and the model still asks for tools, before the turn ends.
	afterBudget = 2
)

// Tool is one function the model may ask for.
type Tool interface {
	Decl() model.Tool
	Run(ctx context.Context, c Call) Result
}

// Call is one tool call as a tool runs it.
type Call struct {
	ID   string
	Name string
	Args map[string]any
	// Conversation is the conversation that asked: the main one or a
	// consult.
	Conversation string
	// Consult runs a sub-conversation with expert's guidance on question
	// and returns its answer; nil within a consult, which may not consult.
	Consult func(ctx context.Context, expert, guidance, question string) (string, error)
}

// Result is what a tool returns: the text the model reads, whether it is
// a refusal or a failure, and what the tool stored.
type Result struct {
	Text    string
	IsError bool
	// Evidence is the short names of what the tool stored, for the record
	// and the chat.
	Evidence []string
	// Shows are the evidence lines Text gives under their stored numbers,
	// as the tool bounded them; a name alone is none.
	Shows []Shown
	// Pin is the pin of an expert the tool loaded into the conversation.
	Pin json.RawMessage
	// End ends the run once this response's results are stored: the model
	// is not called again, as when a task's one tool has taken what the
	// task asked for.
	End bool
}

// ErrBarred marks a gate's refusal under the barred-accounts rule; the
// error wrapping it says why, never the account.
var ErrBarred = errors.New("model calls are refused for this case")

// ErrEgress means a call still held what its egress policy forbids after
// redaction, so it was not sent.
var ErrEgress = errors.New("the call still holds what its egress class forbids")

// ErrEgressClass marks a gate's refusal of a provider whose egress class
// goes further than configuration allows; the error wrapping it names both
// classes. Nothing of the call was looked at.
var ErrEgressClass = errors.New("the provider's egress class is not allowed")

// Outcomes of a model call (docs/MODEL_API.md §The invocation record).
const (
	OutcomeAnswered       = "answered"
	OutcomeCalledTools    = "called_tools"
	OutcomeEgressRefused  = "egress_refused"
	OutcomeBarred         = "barred_account"
	OutcomeCancelled      = "cancelled"
	OutcomeTransportError = "transport_error"
	OutcomeOutputLoop     = "output_loop"
	OutcomeBudgetExceeded = "budget_exceeded"
)

// Invocation is one model call as the loop saw it, for the engine to keep.
type Invocation struct {
	ID string
	// Conversation is the conversation the call served, and Parent its main
	// conversation where it is a consult's.
	Conversation, Parent string
	// Turn counts the conversation's turns from 1; Call counts the turn's
	// model calls from 1.
	Turn, Call      int
	Provider        string
	Model           string // as asked for
	ReportedModel   string // as the provider reported it
	Effort          string
	Policy          string
	Usage           model.Usage
	Cache           *model.Cache // what the provider did with a cache of its own, where it makes one
	Outcome         string
	Problems        []string
	Calls           []model.ToolCall
	Started, First  time.Time
	Completed       time.Time
	Request, Raw    []byte
	Thinking        string
	EstimatedTokens int64
	// Override is the engineer's override a call went out under, where
	// the gate let it through on one.
	Override string
	// ResentFrom is the call a resent call sent again.
	ResentFrom string
	// Cost is what the provider reported the call cost; nil where it
	// reported none.
	Cost *model.Money
}

// EventKind says what an Event reports.
type EventKind int

// The events a turn reports as it runs. EventStage and EventReplaced are
// the engine's own, after the model has answered: a step it runs on the
// answer, Text naming it, and the answer's text replaced in what the
// engineer is shown, Text the text that replaces it.
const (
	EventText EventKind = iota
	EventThinking
	EventToolStart
	EventToolDone
	EventCall
	EventCompacted
	EventWarning
	EventStage
	EventReplaced
)

// Event reports the loop's progress as it happens, in memory only: the
// stored conversation and the invocation records are what stays.
type Event struct {
	Kind         EventKind
	Conversation string
	Text         string
	Call         *model.ToolCall
	Result       *Result
	Invocation   *Invocation
}

// Reply is what one turn ended with.
type Reply struct {
	Text         string
	Conversation string
	// Estimate is the conversation's estimated input at the turn's end,
	// and Compact whether it passed the point where compaction is offered.
	Estimate int64
	Compact  bool
}

// Loop runs a case's conversations.
type Loop struct {
	Store    Store
	Provider model.Provider
	Model    string
	Effort   string
	// System is the companion prompt with the expert index: the same bytes
	// on every call.
	System string
	Tools  []Tool
	// SupportName names the support organization in the handoff request.
	SupportName string
	// Gate refuses a call the case may not make, wrapping ErrBarred, or
	// names the engineer's override it lets the call through under; nil
	// gates nothing.
	Gate func(ctx context.Context) (override string, err error)
	// Policy redacts everything a call sends, chosen by the provider's
	// egress class.
	Policy redact.Policy
	// Record keeps each model call's invocation; a failure to keep one is
	// reported as a warning and does not stop the turn.
	Record          func(Invocation) error
	Budget          int
	MaxOutputTokens int
	CompactAt       int64
	// ContextLimit is the estimated input past which a turn compacts the
	// conversation before it starts, saying so; zero never does.
	ContextLimit int64
	// CaseBlock is the case as a new conversation begins with it, for a
	// compaction, and the evidence lines it shows.
	CaseBlock func(ctx context.Context) (string, []Shown, error)
	// AfterBlock is what a compaction's new conversation is given after its
	// case block and before the handoff, as an update, such as the case
	// ledger; nil or empty gives nothing.
	AfterBlock func(ctx context.Context) (string, error)
	// Carry is what a compaction carries into the new conversation beside
	// the handoff, such as the engineer's pinned items; nil or empty
	// carries nothing.
	Carry func(ctx context.Context) (string, error)
	Now   func() time.Time
	NewID func(prefix string) string
	// MaxCalls is how many model calls the loop makes in all, over every
	// run, before it refuses the next with ErrCallsSpent, sending and
	// recording nothing; zero bounds nothing.
	MaxCalls int
	made     atomic.Int64
}

// ErrCallsSpent says a loop's MaxCalls are spent: the call was not made.
var ErrCallsSpent = errors.New("the model calls allowed are spent")

// CallsLeft is how many model calls the loop may still make under
// MaxCalls; with no MaxCalls, -1.
func (l *Loop) CallsLeft() int {
	if l.MaxCalls <= 0 {
		return -1
	}
	return max(0, l.MaxCalls-int(l.made.Load()))
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Loop) newID(prefix string) string {
	if l.NewID != nil {
		return l.NewID(prefix)
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// Start begins a case's conversation with its case block, which shows,
// a cache break after it.
func (l *Loop) Start(ctx context.Context, caseBlock string, shows ...Shown) (string, error) {
	return l.begin(ctx, "", Message{Message: model.Message{Text: caseBlock}, Shows: shows}, "", Message{})
}

// begin makes a main conversation linked to from: block, then after as an
// update, then handoff where it has text.
func (l *Loop) begin(ctx context.Context, from string, block Message, after string, handoff Message) (string, error) {
	id := l.newID("conv")
	now := l.now()
	if err := l.Store.create(ctx, Conversation{ID: id, Kind: "main", CompactedFrom: from, CreatedAt: now}); err != nil {
		return "", err
	}
	block.Role, block.Break, block.Kind, block.CreatedAt = model.RoleUser, true, KindCase, now
	if err := l.Store.append(ctx, id, block); err != nil {
		return "", err
	}
	if after != "" {
		if err := l.Store.append(ctx, id, Message{Message: model.Message{Role: model.RoleUser, Text: after}, Kind: KindUpdate, CreatedAt: now}); err != nil {
			return "", err
		}
	}
	if handoff.Text != "" {
		handoff.Role, handoff.Break, handoff.Kind, handoff.CreatedAt = model.RoleUser, true, KindHandoff, now
		if err := l.Store.append(ctx, id, handoff); err != nil {
			return "", err
		}
	}
	return id, nil
}

// Estimate is a conversation's estimated input as its next call would
// send it, by model.EstimatorVersion.
func (l *Loop) Estimate(ctx context.Context, conv string) (int64, error) {
	msgs, err := l.Store.Messages(ctx, conv)
	if err != nil {
		return 0, err
	}
	return l.estimate(msgs), nil
}

// Update appends what changed in the case since the conversation began,
// and the evidence lines it shows: the case block itself is never
// rewritten.
func (l *Loop) Update(ctx context.Context, conv, text string, shows ...Shown) error {
	return l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: KindUpdate, CreatedAt: l.now(), Shows: shows})
}

// Task runs a conversation of its own, of kind, begun from text, the
// engine's request, which shows the evidence lines shows, apart from the
// case's main conversation and its consults: a task with its own system
// prompt and tools, such as the case ledger's. It spends the loop's
// budget, may not consult, and its calls pass the gate and the policy and
// are recorded as every call is.
func (l *Loop) Task(ctx context.Context, kind, text string, on func(Event), shows ...Shown) (Reply, error) {
	if on == nil {
		on = func(Event) {}
	}
	id := l.newID(kind)
	now := l.now()
	if err := l.Store.create(ctx, Conversation{ID: id, Kind: kind, CreatedAt: now}); err != nil {
		return Reply{}, err
	}
	if err := l.Store.append(ctx, id, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: KindEngine, CreatedAt: now, Shows: shows}); err != nil {
		return Reply{Conversation: id}, err
	}
	budget := l.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	answer, err := l.run(ctx, id, "", &turn{number: 1, left: budget, tokensLeft: TokenBudget}, true, on)
	return Reply{Text: answer, Conversation: id}, err
}

// Preload runs the tool name for conv as if the model had asked, storing
// the call and its result as the model's message and the results, and
// returns the result: how the engine loads the case routing key's primary expert
// before the first turn, and an expert the engineer loads. The call is
// the engine's, so no invocation records it.
func (l *Loop) Preload(ctx context.Context, conv, name string, args map[string]any) (string, error) {
	var tool Tool
	for _, t := range l.Tools {
		if t.Decl().Name == name {
			tool = t
		}
	}
	if tool == nil {
		return "", fmt.Errorf("companion: no tool %q to preload", name)
	}
	r := tool.Run(ctx, Call{ID: "preload-" + name, Name: name, Args: args, Conversation: conv})
	if r.IsError {
		return "", fmt.Errorf("companion: preloading %s: %s", name, r.Text)
	}
	if len(r.Pin) > 0 {
		if err := l.Store.AddPin(ctx, conv, r.Pin); err != nil {
			return "", err
		}
	}
	msgs, err := l.Store.Messages(ctx, conv)
	if err != nil {
		return "", err
	}
	// Each preload's call ID is its own, so a provider pairing calls with
	// results by ID never sees two alike.
	c := model.ToolCall{ID: fmt.Sprintf("preload-%s-%d", name, len(msgs)+1), Name: name, Args: args}
	now := l.now()
	if err := l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleModel, Calls: []model.ToolCall{c}}, Kind: KindModel, CreatedAt: now}); err != nil {
		return "", err
	}
	return r.Text, l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Results: []model.ToolResult{{CallID: c.ID, Name: name, Text: r.Text}}}, Kind: KindResults, CreatedAt: now, Shows: r.Shows})
}

// turn is what one engineer turn spends, shared with its consults: tool
// calls, and the estimated input of its model calls, where tokensLeft is
// set.
type turn struct {
	number     int
	mu         sync.Mutex
	left       int
	tokensLeft int64
	tokenBound bool
	// once ends the run after its first model call, whatever it answered.
	once bool
}

// take spends one call of the budget, false when none is left.
func (t *turn) take() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.left <= 0 || t.tokenBound {
		return false
	}
	t.left--
	return true
}

// spend counts a model call's charge against the token budget, where the
// turn has one.
func (t *turn) spend(charged int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tokensLeft <= 0 {
		return
	}
	t.tokensLeft -= charged
	t.tokenBound = t.tokensLeft <= 0
}

func (t *turn) spent() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.left <= 0 || t.tokenBound
}

// charge is what a model call counts against the token budget: its input
// as the provider reported it, uncached and cache-written tokens whole and
// cache reads at CacheReadWeight; its estimate est, the whole conversation
// as sent, where the provider reported no input or cache-read count.
func charge(est int64, u model.Usage) int64 {
	if !u.InputTokens.Known || !u.CacheReadTokens.Known {
		return est
	}
	n := u.InputTokens.Value + int64(float64(u.CacheReadTokens.Value)*CacheReadWeight)
	if u.CacheWriteTokens.Known {
		n += u.CacheWriteTokens.Value
	}
	return n
}

// why is the engine's word for the budget spent: the token budget's, or
// the tool calls'.
func (t *turn) why() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tokenBound {
		return tokensSpent
	}
	return budgetSpent
}

// budgetSpent is the engine's word to a model whose turn has no tool calls
// left, and the refusal of a call made anyway.
const budgetSpent = "The tool budget for this turn is spent. Answer now with what you have, and say what you would read next."

// tokensSpent is the engine's word to a model whose turn's calls have sent
// as much input as a turn may, and the refusal of a call made anyway.
const tokensSpent = "The token budget for this turn is spent: its calls have sent as much of the conversation as a turn may. Answer now with what you have, and say what you would read next."

// Turn sends one message — the engineer's words, kind KindEngineer, or the
// engine's instruction, KindEngine — and runs the conversation until the
// model answers.
func (l *Loop) Turn(ctx context.Context, conv string, kind Kind, text string, on func(Event)) (Reply, error) {
	if on == nil {
		on = func(Event) {}
	}
	msgs, err := l.Store.Messages(ctx, conv)
	if err != nil {
		return Reply{Conversation: conv}, err
	}
	// Only a conversation the model has answered in has anything to hand
	// off: one whose case block alone passes the limit would compact anew
	// on every turn.
	if est := l.estimate(msgs); l.ContextLimit > 0 && est > l.ContextLimit && l.CaseBlock != nil && answered(msgs) {
		next, err := l.Compact(ctx, conv, on)
		if err != nil {
			return Reply{Conversation: conv}, fmt.Errorf("companion: compacting at %d estimated tokens: %w", est, err)
		}
		conv = next
		if msgs, err = l.Store.Messages(ctx, conv); err != nil {
			return Reply{Conversation: conv}, err
		}
	}
	number := 1
	for _, m := range msgs {
		if m.Kind == KindEngineer || m.Kind == KindEngine {
			number++
		}
	}
	if err := l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: kind, CreatedAt: l.now()}); err != nil {
		return Reply{Conversation: conv}, err
	}
	budget := l.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	t := &turn{number: number, left: budget, tokensLeft: TokenBudget}
	answer, err := l.run(ctx, conv, "", t, false, on)
	reply := Reply{Text: answer, Conversation: conv}
	if msgs, merr := l.Store.Messages(ctx, conv); merr == nil {
		reply.Estimate = l.estimate(msgs)
		at := l.CompactAt
		if at <= 0 {
			at = DefaultCompactAt
		}
		reply.Compact = reply.Estimate > at
	}
	return reply, err
}

// handoffAsk is what a compaction asks of the model. Its qualifiers keep
// what a summary drops while keeping a conclusion and its citation: who
// asserted it, the evidence against it, how far a search reached, and
// which identity and time a citation holds for.
func handoffAsk(supportName string) string {
	return `Write a handoff of this conversation for a fresh conversation that will continue it without it, concise, keeping what could change the next step. For each material point, say whether it is observed, attributed (and to whom) or inferred, and cite its evidence by name and lines with the identity and time window it holds for. Keep the strongest material evidence against each open explanation, and every correction and retraction, as such. Say what is ruled out, by which check, and only as far as that check reached; keep missing, truncated or uninspected evidence as unknown, not as absent. Give the open hypotheses with what would confirm or rule out each. Then what is awaited from the customer and from ` + supportName + `, the experts loaded, and what the engineer asked for last before this request. A point repeated by a model or a summary, this one included, is no more certain for it. Do not call tools.`
}

// handoffIntro says, before a handoff in the conversation it begins,
// whose words it is.
const handoffIntro = "A handoff of the earlier conversation of this case, written by a model; its points are its reading, not verified:\n\n"

// HandoffText is a handoff as the conversation it begins is given it,
// said to be a model's reading.
func HandoffText(handoff string) string { return handoffIntro + handoff }

// correctionIntro says, before the editor's correction of an answer in
// the conversation, what it is, so the model reads it at its next call.
const correctionIntro = "The engine's editor corrected the wording of your answer above against the lines it cites, and the engineer was shown this text in its place; your answer stays stored as you wrote it:\n\n"

// CorrectionText is the editor's corrected text of an answer as the
// conversation is given it, a KindCorrection message.
func CorrectionText(corrected string) string { return correctionIntro + corrected }

// Corrected is the corrected text a KindCorrection message carries.
func Corrected(message string) string {
	text, _ := strings.CutPrefix(message, correctionIntro)
	return text
}

// Correct appends the editor's correction of the answer before it, as a
// KindCorrection message: the answer stays as the model wrote it, and the
// model reads the correction at its next call.
func (l *Loop) Correct(ctx context.Context, conv, corrected string) error {
	return l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: CorrectionText(corrected)}, Kind: KindCorrection, CreatedAt: l.now()})
}

// shownIntro says, after a revision that was not taken, what the
// engineer was shown in place of the answer, so the model reads it.
const shownIntro = "Your revision was not taken. The engineer was shown this text as your answer above:\n\n"

// ShownText is a KindShown message's text: what the engineer was shown.
func ShownText(shown string) string { return shownIntro + shown }

// Show appends, after a revision that was not taken, the text the
// engineer was shown in place of the answer, as a KindShown message.
func (l *Loop) Show(ctx context.Context, conv, shown string) error {
	return l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: ShownText(shown)}, Kind: KindShown, CreatedAt: l.now()})
}

// AddReview appends the reviewer's reading of an answer or an item, the
// engine's text, as a KindReview message, with invocation the reviewer's
// call whose findings it holds: the answer and the item stay as written,
// and the model reads the review at its next call. It calls no model.
func (l *Loop) AddReview(ctx context.Context, conv, invocation, text string) error {
	return l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: KindReview, Invocation: invocation, CreatedAt: l.now()})
}

// AddVerdict appends the engineer's verdict that a span of an answer is
// unsupported or incorrect, the engine's text, which shows the evidence
// lines shows, as a KindVerdict message: the answer stays as written, and
// the model reads the verdict at its next call. It calls no model.
func (l *Loop) AddVerdict(ctx context.Context, conv, text string, shows ...Shown) error {
	return l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: KindVerdict, CreatedAt: l.now(), Shows: shows})
}

// Replacement is the text a message puts in place of the answer before
// it, as the engineer was shown it: a correction's or a KindShown
// message's; ok is false for any other message.
func Replacement(m Message) (text string, ok bool) {
	switch m.Kind {
	case KindCorrection:
		return Corrected(m.Text), true
	case KindShown:
		text, _ = strings.CutPrefix(m.Text, shownIntro)
		return text, true
	}
	return "", false
}

// Compact has the model write a handoff of conv and starts a new
// conversation from the case block as it is now and that handoff, linked
// to conv. The old conversation stays as it was.
func (l *Loop) Compact(ctx context.Context, conv string, on func(Event)) (string, error) {
	if l.CaseBlock == nil {
		return "", errors.New("companion: no case block to start a new conversation from")
	}
	if on == nil {
		on = func(Event) {}
	}
	handoff, err := l.Handoff(ctx, conv, on)
	if err != nil {
		return "", err
	}
	text := HandoffText(handoff)
	if l.Carry != nil {
		carried, err := l.Carry(ctx)
		if err != nil {
			return "", err
		}
		if carried != "" {
			text += "\n\n" + carried
		}
	}
	next, err := l.Restart(ctx, conv, text)
	if err != nil {
		return "", err
	}
	on(Event{Kind: EventCompacted, Conversation: next, Text: handoff})
	return next, nil
}

// Handoff has the model write a handoff of conv, as a compaction asks it:
// one call over conv that may call no tool, the ask and the handoff kept
// in conv. It returns the handoff.
func (l *Loop) Handoff(ctx context.Context, conv string, on func(Event)) (string, error) {
	if on == nil {
		on = func(Event) {}
	}
	if err := l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: handoffAsk(l.SupportName)}, Kind: KindEngine, CreatedAt: l.now()}); err != nil {
		return "", err
	}
	return l.run(ctx, conv, "", &turn{}, false, on)
}

// Ask sends the engine's message of kind, such as KindRevise, and has the
// model answer it in exactly one call over conv that may call no tool: a
// call it makes anyway is refused and ends the run with an error. The ask
// and the answer are kept in conv, the call counted in the turn the
// conversation is in. It returns the answer.
func (l *Loop) Ask(ctx context.Context, conv string, kind Kind, text string, on func(Event)) (string, error) {
	if on == nil {
		on = func(Event) {}
	}
	msgs, err := l.Store.Messages(ctx, conv)
	if err != nil {
		return "", err
	}
	number := 0
	for _, m := range msgs {
		if m.Kind == KindEngineer || m.Kind == KindEngine {
			number++
		}
	}
	if err := l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: kind, CreatedAt: l.now()}); err != nil {
		return "", err
	}
	return l.run(ctx, conv, "", &turn{number: number, once: true}, false, on)
}

// Restart begins a new main conversation linked to from, as a
// compaction's is, from the case block as it is now, then AfterBlock's
// text, then carried, what the engine carries from the earlier
// conversation, which shows the evidence lines shows, as its handoff
// message. It calls no model.
func (l *Loop) Restart(ctx context.Context, from, carried string, shows ...Shown) (string, error) {
	if l.CaseBlock == nil {
		return "", errors.New("companion: no case block to start a new conversation from")
	}
	block, blockShows, err := l.CaseBlock(ctx)
	if err != nil {
		return "", err
	}
	after := ""
	if l.AfterBlock != nil {
		if after, err = l.AfterBlock(ctx); err != nil {
			return "", err
		}
	}
	return l.begin(ctx, from, Message{Message: model.Message{Text: block}, Shows: blockShows}, after, Message{Message: model.Message{Text: carried}, Shows: shows})
}

// run calls the model on conv, runs the calls it asks for and sends their
// results back, until it answers in text; the answer is returned. parent
// is the main conversation of a consult, consult true there.
func (l *Loop) run(ctx context.Context, conv, parent string, t *turn, consult bool, on func(Event)) (string, error) {
	tools := make(map[string]Tool, len(l.Tools))
	decls := make([]model.Tool, 0, len(l.Tools))
	for _, tool := range l.Tools {
		d := tool.Decl()
		tools[d.Name] = tool
		decls = append(decls, d)
	}
	ignored := 0
	for n := 1; ; n++ {
		// The newest message ends this call, so everything through it is
		// sent unchanged from now on: a cache break.
		if err := l.Store.markBreak(ctx, conv); err != nil {
			return "", err
		}
		msgs, err := l.Store.Messages(ctx, conv)
		if err != nil {
			return "", err
		}
		est := l.estimate(msgs)
		req := model.Request{Model: l.Model, Effort: l.Effort, System: l.System, Tools: decls, MaxOutputTokens: l.MaxOutputTokens, NoCalls: t.spent()}
		for _, m := range msgs {
			req.Messages = append(req.Messages, m.Message)
		}
		resp, inv, err := l.call(ctx, Invocation{Conversation: conv, Parent: parent, Turn: t.number, Call: n, EstimatedTokens: est}, req, on)
		if resp == nil {
			return "", err
		}
		// Charged once the provider says what it read from its cache, so a
		// call over the bound still has its calls refused (take).
		t.spend(charge(est, resp.Usage))
		if aerr := l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleModel, Text: resp.Text, Calls: resp.Calls}, Kind: KindModel, Invocation: inv.ID, CreatedAt: l.now()}); aerr != nil {
			return "", aerr
		}
		if err != nil || len(resp.Calls) == 0 {
			return resp.Text, err
		}
		if t.spent() {
			ignored++
		}
		results, shows, end := l.runCalls(ctx, conv, t, consult, tools, resp.Calls, on)
		// The note is for the model's next call: none is made where a
		// result ended the run or the loop's calls are spent.
		note := ""
		if t.spent() && !end && (l.MaxCalls <= 0 || l.made.Load() < int64(l.MaxCalls)) {
			note = t.why()
		}
		if err := l.Store.append(ctx, conv, Message{Message: model.Message{Role: model.RoleUser, Results: results, Text: note}, Kind: KindResults, CreatedAt: l.now(), Shows: shows}); err != nil {
			return "", err
		}
		if end {
			return resp.Text, nil
		}
		if t.once {
			return resp.Text, errors.New("companion: the model called a tool where it was asked to answer")
		}
		if ignored > afterBudget {
			return resp.Text, errors.New("companion: the model kept calling tools after the turn's budget was spent")
		}
	}
}

// runCalls runs one response's calls, at most maxConcurrent at once, each
// checked against the turn's budget and the tool table first; the results
// keep the calls' order, shows are the evidence lines they give, in that
// order, and end says a result ended the run.
func (l *Loop) runCalls(ctx context.Context, conv string, t *turn, consult bool, tools map[string]Tool, calls []model.ToolCall, on func(Event)) ([]model.ToolResult, []Shown, bool) {
	results := make([]model.ToolResult, len(calls))
	shown := make([][]Shown, len(calls))
	ended := make([]bool, len(calls))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, c := range calls {
		results[i] = model.ToolResult{CallID: c.ID, Name: c.Name}
		tool, ok := tools[c.Name]
		switch {
		case !t.take():
			results[i].Text, results[i].IsError = t.why(), true
			continue
		case !ok:
			results[i].Text, results[i].IsError = fmt.Sprintf("There is no tool %q.", c.Name), true
			continue
		}
		call := Call{ID: c.ID, Name: c.Name, Args: c.Args, Conversation: conv}
		if !consult {
			call.Consult = func(ctx context.Context, expert, guidance, question string) (string, error) {
				return l.consult(ctx, conv, t, expert, guidance, question, on)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			on(Event{Kind: EventToolStart, Conversation: conv, Call: &c})
			r := tool.Run(ctx, call)
			if len(r.Pin) > 0 {
				if err := l.Store.AddPin(ctx, conv, r.Pin); err != nil {
					r.Text += "\n(the engine could not record this expert's pin: " + err.Error() + ")"
				}
			}
			results[i].Text, results[i].IsError, ended[i], shown[i] = r.Text, r.IsError, r.End, r.Shows
			on(Event{Kind: EventToolDone, Conversation: conv, Call: &c, Result: &r})
		}()
	}
	wg.Wait()
	return results, slices.Concat(shown...), slices.Contains(ended, true)
}

// consultLead introduces a consult's question to the expert answering it.
const consultLead = `You are consulted as an expert by the companion working this case, on one question. Your answer goes back to the companion, which answers the engineer. Lead with the answer at the certainty the evidence supports, citing evidence by name and lines, with its identity, version, capture and time where they matter. Keep what you observed, what is attributed and what you infer apart. Name the strongest unresolved alternative where it changes the answer, the next check that would settle it where one is needed, and the checks you ran, so the companion does not repeat them. Do not consult another expert.`

// The headings of a consult's ask before its guidance, with the expert's
// ID, and before its question.
const (
	consultGuidanceHead = "\n\n## Your guidance: "
	consultQuestionHead = "\n\n## The question\n\n"
)

// ConsultGuidance is the guidance in ask, a consult's ask of question as
// consult wrote it, and false where ask is not one.
func ConsultGuidance(ask, question string) (string, bool) {
	rest, ok := strings.CutSuffix(ask, consultQuestionHead+question)
	if !ok {
		return "", false
	}
	if _, rest, ok = strings.Cut(rest, consultGuidanceHead); !ok {
		return "", false
	}
	_, guidance, ok := strings.Cut(rest, "\n\n")
	return guidance, ok
}

// consult runs a sub-conversation on question with expert's guidance: the
// main conversation's case block first, the same bytes, so it shares the
// main conversation's cached prefix; then each case update the main
// conversation was given since — comments and attachments that came later,
// the case ledger and its updates — so a correction after the case block
// reaches the expert as it reached the companion; then the guidance and
// the question. It spends the turn's budget, and its transcript stays.
func (l *Loop) consult(ctx context.Context, parent string, t *turn, expert, guidance, question string, on func(Event)) (string, error) {
	msgs, err := l.Store.Messages(ctx, parent)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 || msgs[0].Kind != KindCase {
		return "", errors.New("companion: the conversation has no case block to consult on")
	}
	// The case block and updates are given again, with what they show.
	shows, err := l.Store.Shows(ctx, parent)
	if err != nil {
		return "", err
	}
	id := l.newID("consult")
	now := l.now()
	if err := l.Store.create(ctx, Conversation{ID: id, Kind: "consult", Parent: parent, Expert: expert, CreatedAt: now}); err != nil {
		return "", err
	}
	if err := l.Store.append(ctx, id, Message{Message: model.Message{Role: model.RoleUser, Text: msgs[0].Text, Break: true}, Kind: KindCase, CreatedAt: now, Shows: shows[msgs[0].Seq]}); err != nil {
		return "", err
	}
	for _, m := range msgs[1:] {
		if m.Kind != KindUpdate {
			continue
		}
		if err := l.Store.append(ctx, id, Message{Message: model.Message{Role: model.RoleUser, Text: m.Text}, Kind: KindUpdate, CreatedAt: now, Shows: shows[m.Seq]}); err != nil {
			return "", err
		}
	}
	text := consultLead + consultGuidanceHead + expert + "\n\n" + guidance + consultQuestionHead + question
	if err := l.Store.append(ctx, id, Message{Message: model.Message{Role: model.RoleUser, Text: text}, Kind: KindConsult, CreatedAt: now}); err != nil {
		return "", err
	}
	return l.run(ctx, id, parent, t, true, on)
}

// call makes one model call: the gate, then redaction, then the provider,
// recording the invocation whatever happens. inv says where the call
// belongs; a call with no conversation, a resent one, records nothing of
// what a conversation was shown. A nil response means nothing came back.
func (l *Loop) call(ctx context.Context, inv Invocation, req model.Request, on func(Event)) (*model.Response, Invocation, error) {
	if l.MaxCalls > 0 && l.made.Add(1) > int64(l.MaxCalls) {
		return nil, Invocation{}, fmt.Errorf("%w: %d", ErrCallsSpent, l.MaxCalls)
	}
	conv := inv.Conversation
	inv.ID = l.newID("call")
	inv.Provider, inv.Model, inv.Effort, inv.Policy = l.Provider.Describe().ID, req.Model, req.Effort, l.Policy.Version
	inv.Started = l.now()
	finish := func(outcome string, err error) {
		inv.Outcome, inv.Completed = outcome, l.now()
		if err != nil {
			inv.Problems = append(inv.Problems, err.Error())
		}
		if l.Record != nil {
			if err := l.Record(inv); err != nil {
				on(Event{Kind: EventWarning, Conversation: conv, Text: "the call's invocation record was not kept: " + err.Error()})
			}
		}
		on(Event{Kind: EventCall, Conversation: conv, Invocation: &inv})
	}
	if l.Gate != nil {
		override, err := l.Gate(ctx)
		inv.Override = override
		if err != nil {
			outcome := OutcomeTransportError
			switch {
			case errors.Is(err, ErrBarred):
				outcome = OutcomeBarred
			case errors.Is(err, ErrEgress), errors.Is(err, ErrEgressClass):
				outcome = OutcomeEgressRefused
			}
			finish(outcome, err)
			return nil, inv, err
		}
	}
	sent, err := redactRequest(l.Policy, req)
	if err != nil {
		finish(OutcomeEgressRefused, err)
		return nil, inv, err
	}
	// The request is kept as sent, redacted, for debugging; it marshals
	// from plain values, so it cannot fail.
	inv.Request, _ = json.Marshal(sent)
	// What the request carries of the evidence is recorded as it is sent:
	// a record not kept leaves the lines unshown to the citation check,
	// never shown where they were not, so the call goes on, warned.
	if conv != "" {
		if err := l.Store.request(ctx, conv, inv.ID); err != nil {
			on(Event{Kind: EventWarning, Conversation: conv, Text: "what the call carried of the evidence was not recorded: " + err.Error()})
		}
	}
	resp, err := l.Provider.Call(ctx, sent, func(d model.Delta) {
		if inv.First.IsZero() {
			inv.First = l.now()
		}
		if d.Text != "" {
			on(Event{Kind: EventText, Conversation: conv, Text: d.Text})
		}
		if d.Thinking != "" {
			on(Event{Kind: EventThinking, Conversation: conv, Text: d.Thinking})
		}
	})
	if resp != nil {
		inv.Usage, inv.Cache, inv.ReportedModel, inv.Raw, inv.Thinking, inv.Calls = resp.Usage, resp.Cache, resp.ModelID, resp.Raw, resp.Thinking, resp.Calls
		inv.Cost = resp.Cost
	}
	switch {
	case err == nil && len(resp.Calls) > 0:
		finish(OutcomeCalledTools, nil)
	case err == nil:
		finish(OutcomeAnswered, nil)
	case ctx.Err() != nil:
		finish(OutcomeCancelled, err)
	case errors.Is(err, model.ErrOutputLimit) && resp != nil:
		outcome := OutcomeBudgetExceeded
		if p, r, ok := looped([]byte(resp.Text)); ok {
			outcome = OutcomeOutputLoop
			err = fmt.Errorf("%w: the answer repeats a %d-byte fragment %d times", err, p, r)
		}
		finish(outcome, err)
	default:
		finish(OutcomeTransportError, err)
	}
	return resp, inv, err
}

// Resend sends req, the request an earlier call, from, sent as it was
// stored, once more: through the gate, the policy and the provider as any
// call, recorded as a call of its own resent from that one. It reads and
// writes no conversation, so nothing a conversation holds or was shown
// changes.
func (l *Loop) Resend(ctx context.Context, from string, req model.Request, on func(Event)) (*model.Response, Invocation, error) {
	if on == nil {
		on = func(Event) {}
	}
	return l.call(ctx, Invocation{ResentFrom: from}, req, on)
}

// redactRequest applies the egress policy to everything a call carries
// that is not the engine's own: the system prompt, which holds the expert
// index, every message's text, the model's call arguments and the tools'
// results. A call still holding what the policy forbids is refused. The
// policy is deterministic, so a conversation redacts to the same prefix
// on every call.
func redactRequest(p redact.Policy, r model.Request) (model.Request, error) {
	out := r
	out.System = p.String(r.System)
	out.Messages = make([]model.Message, len(r.Messages))
	texts := []string{out.System}
	for i, m := range r.Messages {
		c := m
		c.Text = p.String(m.Text)
		texts = append(texts, c.Text)
		if len(m.Calls) > 0 {
			c.Calls = make([]model.ToolCall, len(m.Calls))
			for j, call := range m.Calls {
				args, err := redactArgs(p, call.Args)
				if err != nil {
					return model.Request{}, err
				}
				call.Args = args
				c.Calls[j] = call
				b, _ := json.Marshal(args) // decoded from JSON, so it encodes again
				texts = append(texts, string(b))
			}
		}
		if len(m.Results) > 0 {
			c.Results = make([]model.ToolResult, len(m.Results))
			for j, res := range m.Results {
				res.Text = p.String(res.Text)
				c.Results[j] = res
				texts = append(texts, res.Text)
			}
		}
		out.Messages[i] = c
	}
	for _, s := range texts {
		if kind, ok := p.Holds(s); ok {
			return model.Request{}, fmt.Errorf("%w: a %s remains under policy %s", ErrEgress, kind, p.Version)
		}
	}
	return out, nil
}

func redactArgs(p redact.Policy, args map[string]any) (map[string]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("companion: call arguments: %w", err)
	}
	if b, err = p.JSON(b); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("companion: call arguments: %w", err)
	}
	return out, nil
}

// estimate is a conversation's estimated input, labelled by
// model.EstimatorVersion wherever it is shown; never recorded as usage.
func (l *Loop) estimate(msgs []Message) int64 {
	n := model.EstimateTokens(l.System)
	for _, tool := range l.Tools {
		d := tool.Decl()
		b, _ := json.Marshal(d.Parameters) // a JSON schema of plain values
		n += model.EstimateTokens(d.Name+" "+d.Description) + model.EstimateTokens(string(b))
	}
	for _, m := range msgs {
		n += model.EstimateTokens(m.Text)
		for _, c := range m.Calls {
			b, _ := json.Marshal(c.Args) // decoded from JSON, so it encodes again
			n += model.EstimateTokens(c.Name) + model.EstimateTokens(string(b))
		}
		for _, r := range m.Results {
			n += model.EstimateTokens(r.Text)
		}
	}
	return n
}

// answered reports whether the model has written in a conversation.
func answered(msgs []Message) bool {
	for _, m := range msgs {
		if m.Kind == KindModel && m.Invocation != "" {
			return true
		}
	}
	return false
}

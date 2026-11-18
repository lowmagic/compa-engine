package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	apischema "github.com/lowmagic/compa-engine/internal/schema"
)

// Trajectory is what each turn of the case's main conversation did, its
// compaction ancestors' first: the calls its model made, the evidence they
// returned and what became of it, the experts it loaded and consulted, the
// input its model calls sent as their records hold it and the expert
// guidance's share of it, and what the editor made of its answer; then the
// reviews the engineer asked for. It measures the companion; it is derived
// from the stored rows each time, writes nothing and calls no model.
type Trajectory struct {
	// Conversations are the main conversation and its compaction
	// ancestors, oldest first; none where the case has no conversation.
	Conversations []string         `json:"conversations"`
	Turns         []TrajectoryTurn `json:"turns"`
	Reviews       []ReviewRun      `json:"reviews,omitempty"`
	Totals        TrajectoryTotals `json:"totals"`
}

// TrajectoryTurn is one turn: the engineer's message or the engine's ask,
// numbered from 1 across the conversations, and what followed it until the
// next. An expert the engine loads before a conversation's first turn is
// that turn's.
type TrajectoryTurn struct {
	Number       int    `json:"number"`
	Conversation string `json:"conversation"`
	// Kind is what asked for the turn: talk, the engineer's words; opening,
	// analyze, draft, catch-up or handoff, the engine's ask for one; engine,
	// another of the engine's.
	Kind string `json:"kind"`
	// Calls are the tool calls the model made in the main conversation,
	// Refused those whose result was a refusal or a failure; BudgetSpent
	// says the engine told the model the turn's tool or token budget was
	// spent, in the main conversation or a consult.
	Calls       int                `json:"calls"`
	Refused     int                `json:"refused"`
	BudgetSpent bool               `json:"budget_spent"`
	Evidence    TrajectoryEvidence `json:"evidence"`
	// Repeated are the calls naming the tool and the arguments of a call
	// made earlier in the main history, the engine's included.
	Repeated []RepeatedCall `json:"repeated,omitempty"`
	Experts  []ExpertLoad   `json:"experts,omitempty"`
	Consults []ConsultRun   `json:"consults,omitempty"`
	// Asked are the turn's calls in the main conversation to a tool that
	// takes a question, each with it as written, and Questions their
	// counts; a question is reported as it is, never corrected.
	Asked     []CallQuestion `json:"call_questions,omitempty"`
	Questions QuestionCounts `json:"questions"`
	// Answers are the answers the turn's calls in the main conversation
	// gave, each with what checking it as a statement flagged
	// (CallAnswers), and FlaggedAnswers how many were flagged.
	Answers        []ItemAnswer `json:"answers,omitempty"`
	FlaggedAnswers int          `json:"flagged_answers"`
	// ModelCalls are the turn's model calls, its consults' included, in
	// order; Input and CacheRead their sums.
	ModelCalls []ModelCallTokens `json:"model_calls,omitempty"`
	Input      TokenSum          `json:"input_tokens"`
	CacheRead  TokenSum          `json:"cache_read_tokens"`
	// Guidance is the expert guidance the turn's conversation held and its
	// model calls in that conversation sent; a consult's is its own.
	Guidance GuidanceShare `json:"guidance"`
	// Check is the editor's check of the turn's answer, where one is kept.
	Check *TurnCheck `json:"check,omitempty"`
}

// TrajectoryEvidence is the evidence items tools returned in a turn, each
// counted in the first turn one returned it, and what became of them
// after: read on with evidence_read in the main conversation, by name or
// by ID, cited in one of the model's answers as it wrote it, by name or by
// the path of a file it read (citedByPath), both, or neither. ReadOn,
// Cited, Both and Neither add up to Returned. Beside them, Answered are
// those a later call's answers named, whatever else became of them, and
// None those with no answer, citation or read-on, a part of Neither.
type TrajectoryEvidence struct {
	Returned     int      `json:"returned"`
	ReadOn       int      `json:"read_on"`
	Cited        int      `json:"cited"`
	Both         int      `json:"both"`
	Neither      int      `json:"neither"`
	NeitherNames []string `json:"neither_names,omitempty"`
	Answered     int      `json:"answered"`
	None         int      `json:"none"`
	NoneNames    []string `json:"none_names,omitempty"`
}

// RepeatedCall is a call repeating an earlier one: its tool, its arguments
// as JSON, and the turn the first was made in.
type RepeatedCall struct {
	Tool      string `json:"tool"`
	Args      string `json:"args"`
	FirstTurn int    `json:"first_turn"`
}

// CallQuestion is a call's question: its tool, the question as the model
// wrote it, one line, empty where it gave none (Missing); the turn of the
// earlier response that asked it already, compared as questionKey makes
// them, where it repeats one; whether it is vague (vagueQuestion); and
// whether it restates the action instead of asking (restatesAction).
type CallQuestion struct {
	Tool     string `json:"tool"`
	Question string `json:"question"`
	Missing  bool   `json:"missing,omitempty"`
	Repeats  int    `json:"repeats_turn,omitempty"`
	Vague    bool   `json:"vague,omitempty"`
	Restates bool   `json:"restates_action,omitempty"`
}

// QuestionCounts count calls to tools that take a question: how many,
// how many gave none, and of those that gave one, how many repeat a
// question an earlier response of the main history asked, in any turn;
// how many are vague: under four words, or every word a function word or
// a generic one, such as file, log, error or output (vagueQuestion); and
// how many restate the action: no question mark, or opening with an
// imperative such as find, read or check (restatesAction).
type QuestionCounts struct {
	Calls     int `json:"calls"`
	None      int `json:"no_question"`
	Repeated  int `json:"repeated"`
	Vague     int `json:"vague"`
	Restating int `json:"restating_action"`
}

func (q *QuestionCounts) and(o QuestionCounts) {
	q.Calls += o.Calls
	q.None += o.None
	q.Repeated += o.Repeated
	q.Vague += o.Vague
	q.Restating += o.Restating
}

// ExpertLoad is one load_expert call: the expert, whether the engine made
// it — the case routing key's primary expert, or one the engineer loaded — and
// whether it was refused.
type ExpertLoad struct {
	ID       string `json:"id"`
	ByEngine bool   `json:"by_engine,omitempty"`
	Refused  bool   `json:"refused,omitempty"`
}

// ConsultRun is one consult call: the expert, the consult's conversation
// (none where it was refused before it ran), whether it was refused or
// failed, the tool calls the consult made, how many of those repeat the
// tool and arguments of a call the main conversation made up to the
// response that asked for the consult, and the guidance the consult's
// model calls sent: its ask's, then any it loaded.
type ConsultRun struct {
	Expert       string         `json:"expert"`
	Conversation string         `json:"conversation,omitempty"`
	Refused      bool           `json:"refused,omitempty"`
	Calls        int            `json:"calls"`
	Repeats      int            `json:"repeats"`
	Guidance     *GuidanceShare `json:"guidance,omitempty"`
}

// GuidanceLoad is one expert's guidance as a conversation was given it —
// by load_expert, the engine's preload or a consult's ask — by the expert
// and the version its heading names, with its tokens by the engine's
// estimator.
type GuidanceLoad struct {
	ID      string      `json:"id"`
	Version string      `json:"version"`
	Tokens  model.Count `json:"tokens"`
}

// GuidanceShare is the expert guidance some model calls of a conversation
// sent: Experts the guidance loaded in it by the last of them, by ID and
// version, Tokens its sum; Sent the guidance the calls sent, each counting
// what was loaded before it; Prompt their whole input as the provider
// reported it — uncached, cache read and, where the provider has the
// category, cache write — unavailable where Unavailable calls' was not
// reported or no call was made; and Share, Sent over Prompt. Tokens and
// Sent are estimates, labelled with their estimator; no provider counts
// them.
type GuidanceShare struct {
	Experts     []GuidanceLoad `json:"experts,omitempty"`
	Tokens      model.Count    `json:"tokens"`
	Calls       int            `json:"model_calls"`
	Sent        model.Count    `json:"sent_tokens"`
	Prompt      model.Count    `json:"prompt_tokens"`
	Unavailable int            `json:"unavailable_calls"`
	Share       Share          `json:"share_of_prompt"`
}

// Share is an estimate's part of a reported count, as a fraction, naming
// the estimate's estimator; unavailable where the count was not reported,
// never zero.
type Share struct {
	Value     float64
	Known     bool
	Estimator string
}

// MarshalJSON writes "unavailable" for an unknown share, never 0.
func (s Share) MarshalJSON() ([]byte, error) {
	if !s.Known {
		return []byte(`"unavailable"`), nil
	}
	return json.Marshal(struct {
		Value     float64 `json:"value"`
		Estimator string  `json:"estimator_version"`
	}{s.Value, s.Estimator})
}

// ModelCallTokens is one model call's input as its invocation record
// holds it: reported, estimated with its estimator, or unavailable; and
// what its provider did with a cache of its own making, where it makes
// one.
type ModelCallTokens struct {
	Invocation   string       `json:"invocation"`
	Conversation string       `json:"conversation"`
	Input        model.Count  `json:"input_tokens"`
	CacheRead    model.Count  `json:"cache_read_tokens"`
	Cache        *model.Cache `json:"cache,omitempty"`
}

// TokenSum is a sum of the counts some calls' records hold: Unavailable
// says how many held none, so Value is then a floor; Estimators name the
// estimators of any estimated count in it, each once.
type TokenSum struct {
	Value       int64    `json:"value"`
	Unavailable int      `json:"unavailable_calls"`
	Estimators  []string `json:"estimators,omitempty"`
}

func (s *TokenSum) add(c model.Count) {
	if !c.Known {
		s.Unavailable++
		return
	}
	s.Value += c.Value
	if c.Fidelity == model.FidelityEstimated && !slices.Contains(s.Estimators, c.EstimatorVersion) {
		s.Estimators = append(s.Estimators, c.EstimatorVersion)
	}
}

func (s *TokenSum) and(o TokenSum) {
	s.Value += o.Value
	s.Unavailable += o.Unavailable
	for _, e := range o.Estimators {
		if !slices.Contains(s.Estimators, e) {
			s.Estimators = append(s.Estimators, e)
		}
	}
}

// TurnCheck is the editor's check of a turn's answer, as kept: the
// answer's label, why it went unchecked, how many anchors the revision was
// asked to fulfil, whether it revised the answer or why not, the
// citations naming lines it added (AnswerCheck.Added), the citations of a
// whole item, [e4], it added naming an item neither the answer as written
// nor the editor's corrected citations cite, and the check's model calls,
// the revision's after them.
type TurnCheck struct {
	Answer         string      `json:"answer"`
	Unchecked      string      `json:"unchecked,omitempty"`
	Anchors        int         `json:"anchors"`
	Revised        bool        `json:"revised"`
	RevisionFailed string      `json:"revision_failed,omitempty"`
	AddedLines     []string    `json:"added_lines,omitempty"`
	AddedWhole     []string    `json:"added_whole,omitempty"`
	Calls          []CheckCall `json:"calls,omitempty"`
}

// CheckCall is one model call of a check, or of the revision its anchors
// asked for: what it was — first, retry, follow-up or revision — its
// outcome as recorded, how long it took, and, for a retry, why it was
// asked, as the check's conversation holds it (retryReason).
type CheckCall struct {
	Kind     string        `json:"kind"`
	Outcome  string        `json:"outcome"`
	Duration time.Duration `json:"duration_ns"`
	Reason   string        `json:"reason,omitempty"`
}

// The kinds of a check's model calls: its first; one after a refusal or an
// answer in text; the editor's follow-up on statements left unaccounted;
// and the main model's revision for the anchors.
const (
	CallFirst    = "first"
	CallRetry    = "retry"
	CallFollowUp = "follow-up"
	CallRevision = "revision"
)

// ReviewRun is a review the engineer asked for: what it reviewed, its
// outcome as kept and its model calls.
type ReviewRun struct {
	Of      string      `json:"of"`
	Outcome string      `json:"outcome"`
	Calls   []CheckCall `json:"calls,omitempty"`
}

// TrajectoryTotals are the turns' figures over the case.
type TrajectoryTotals struct {
	Turns       int                `json:"turns"`
	Calls       int                `json:"calls"`
	Refused     int                `json:"refused"`
	BudgetSpent int                `json:"budget_spent_turns"`
	Evidence    TrajectoryEvidence `json:"evidence"`
	Repeated    int                `json:"repeated"`
	Questions   QuestionCounts     `json:"questions"`
	// Answers counts the answers the calls gave, FlaggedAnswers those
	// flagged.
	Answers        int `json:"answers"`
	FlaggedAnswers int `json:"flagged_answers"`
	// Loaded counts the experts loaded, ByEngine those the engine loaded;
	// refused loads are in neither.
	Loaded         int      `json:"experts_loaded"`
	ByEngine       int      `json:"experts_by_engine"`
	Consults       int      `json:"consults"`
	ConsultCalls   int      `json:"consult_calls"`
	ConsultRepeats int      `json:"consult_repeats"`
	ModelCalls     int      `json:"model_calls"`
	Input          TokenSum `json:"input_tokens"`
	CacheRead      TokenSum `json:"cache_read_tokens"`
	// Guidance is the turns' guidance together, ConsultGuidance the
	// consults' (sumGuidance).
	Guidance        GuidanceShare `json:"guidance"`
	ConsultGuidance GuidanceShare `json:"consult_guidance"`
	Checked         int           `json:"checked"`
	Revised         int           `json:"revised"`
	AddedLines      int           `json:"added_lines"`
	AddedWhole      int           `json:"added_whole"`
	// Reviews counts the reviews asked for; CheckTime the checks' and
	// revisions' model calls, by kind.
	Reviews   int        `json:"reviews"`
	CheckTime CheckTimes `json:"check_time"`
}

// CheckTimes are model calls of checks and revisions by kind: how many,
// and the longest.
type CheckTimes map[string]CallTime

// CallTime is how many calls of a kind, and the longest.
type CallTime struct {
	Calls   int           `json:"calls"`
	Longest time.Duration `json:"longest_ns"`
}

func (t *CheckTimes) add(calls []CheckCall) {
	if *t == nil && len(calls) > 0 {
		*t = CheckTimes{}
	}
	for _, c := range calls {
		ct := (*t)[c.Kind]
		ct.Calls++
		ct.Longest = max(ct.Longest, c.Duration)
		(*t)[c.Kind] = ct
	}
}

// The openings of the engine's asks a turn's kind is read from, beside
// orientAsk and analyzeAsk: draftWords's and the catch-up's, which both
// begin with them.
const (
	draftLead   = "Draft "
	catchUpLead = "Catch the engineer up."
)

// evidenceReadTool is evidence.read as the companion offers it.
var evidenceReadTool = strings.ReplaceAll(toolRead, ".", "_")

// consultRow is a consult conversation as the trajectory reads it: its
// question, its calls by key, its model calls, whether the engine told it
// the budget was spent, and the guidance its model calls sent of what it
// loaded itself, its ask's added once matched; used once matched to its
// call.
type consultRow struct {
	conv     companion.Conversation
	question string
	keys     []string
	models   []ModelCallTokens
	budget   bool
	guidance guidanceTally
	used     bool
}

// guidanceTally is the guidance some model calls of one conversation
// sent: the experts' guidance loaded in it so far, how many calls, the
// guidance they sent, each counting what was loaded before it, and their
// whole input as reported, unreported counting the calls whose was not.
type guidanceTally struct {
	experts    []GuidanceLoad
	calls      int
	sent       int64
	prompt     int64
	unreported int
}

// held is the guidance's tokens.
func (g guidanceTally) held() int64 {
	var n int64
	for _, x := range g.experts {
		n += x.Tokens.Value
	}
	return n
}

// call counts a model call sent holding the guidance loaded so far, with
// the usage its provider reported.
func (g *guidanceTally) call(u model.Usage) {
	g.calls++
	g.sent += g.held()
	if n, ok := promptTokens(u); ok {
		g.prompt += n
	} else {
		g.unreported++
	}
}

func (g guidanceTally) share() GuidanceShare {
	out := GuidanceShare{Experts: g.experts, Tokens: model.Estimated(g.held(), model.EstimatorVersion), Calls: g.calls,
		Sent: model.Estimated(g.sent, model.EstimatorVersion), Prompt: model.Unavailable, Unavailable: g.unreported}
	if g.calls > 0 && g.unreported == 0 {
		out.Prompt = model.Reported(g.prompt)
		if g.prompt > 0 {
			out.Share = Share{Value: float64(g.sent) / float64(g.prompt), Known: true, Estimator: model.EstimatorVersion}
		}
	}
	return out
}

// sumGuidance is shares together: each expert at each version once, at
// its largest, and the calls' figures summed, the input unavailable where
// any call's was.
func sumGuidance(shares []GuidanceShare) GuidanceShare {
	var g guidanceTally
	for _, s := range shares {
		for _, x := range s.Experts {
			i := slices.IndexFunc(g.experts, func(y GuidanceLoad) bool { return y.ID == x.ID && y.Version == x.Version })
			switch {
			case i < 0:
				g.experts = append(g.experts, x)
			case x.Tokens.Value > g.experts[i].Tokens.Value:
				g.experts[i] = x
			}
		}
		g.calls += s.Calls
		g.sent += s.Sent.Value
		g.prompt += s.Prompt.Value
		g.unreported += s.Unavailable
	}
	return g.share()
}

// promptTokens is a call's whole input as its provider reported it:
// uncached, cache read and, where the provider has the category, cache
// write, as the companion's token budget counts it; false where one was
// not reported.
func promptTokens(u model.Usage) (int64, bool) {
	reported := func(c model.Count) bool { return c.Known && c.Fidelity == model.FidelityReported }
	if !reported(u.InputTokens) || !reported(u.CacheReadTokens) || u.CacheWriteTokens.Known && !reported(u.CacheWriteTokens) {
		return 0, false
	}
	n := u.InputTokens.Value + u.CacheReadTokens.Value
	if u.CacheWriteTokens.Known {
		n += u.CacheWriteTokens.Value
	}
	return n, true
}

// guidanceOf is the expert guidance g is, as expertGuidance wrote it, by
// the expert and version its heading names, with its estimated tokens;
// false for any other text, as load_expert's word that the expert is
// loaded already.
func guidanceOf(g string) (GuidanceLoad, bool) {
	rest, ok := strings.CutPrefix(g, expertHeading)
	if !ok {
		return GuidanceLoad{}, false
	}
	head, _, _ := strings.Cut(rest, "\n")
	id, version, ok := strings.Cut(head, " ")
	if !ok {
		return GuidanceLoad{}, false
	}
	return GuidanceLoad{ID: id, Version: version, Tokens: model.Estimated(model.EstimateTokens(g), model.EstimatorVersion)}, true
}

// Trajectory is the case's trajectory; a case with no conversation has
// one with no turns.
func (s *Session) Trajectory(ctx context.Context) (Trajectory, error) {
	out := Trajectory{Conversations: []string{}, Turns: []TrajectoryTurn{}}
	history, err := s.ConversationHistory(ctx)
	if err != nil || len(history) == 0 {
		return out, err
	}
	for _, m := range history {
		if n := len(out.Conversations); n == 0 || out.Conversations[n-1] != m.ConversationID {
			out.Conversations = append(out.Conversations, m.ConversationID)
		}
	}
	consults, err := s.consultRows(ctx, out.Conversations)
	if err != nil {
		return out, err
	}
	runs, err := s.checkHistory(ctx)
	if err != nil {
		return out, err
	}
	revisions, err := s.revisionCalls(ctx, history)
	if err != nil {
		return out, err
	}
	c := s.c
	takes := map[string]bool{}
	for _, tl := range s.e.companionTools(c) {
		if n := tl.Decl().Name; TakesQuestion(n) {
			takes[n] = true
		}
	}
	p := trajectoryPass{history: history, consults: consults, seen: map[string]int{}, questions: map[string]int{}, got: returnedItems{}, answered: map[string]int{},
		takes:      func(name string) bool { return takes[name] },
		returnedAt: map[string]int{}, lastRead: map[string]int{}, lastCite: map[string]int{}, lastAnswer: map[int]int{},
		shown: map[string]string{}, lastPathCite: map[string]int{}, editorCalls: map[string][]CheckCall{}, revisions: revisions,
		short: func(id string) string {
			if !IsEvidenceID(id) {
				return id
			}
			if name, err := evidenceName(c, id); err == nil {
				return name
			}
			return id
		},
		paths: func(name string) []string {
			id, err := evidenceByName(c, name)
			if err != nil {
				return nil
			}
			it, err := c.ev.Describe(id)
			if err != nil || it.Collection == nil || it.Location == "" {
				return nil
			}
			return strings.Split(it.Location, "\n")
		}}
	for _, r := range runs {
		switch r.role {
		case check.Editor:
			p.editorCalls[r.checked] = r.calls
		case check.Reviewer:
			out.Reviews = append(out.Reviews, ReviewRun{Of: r.name, Outcome: r.outcome, Calls: r.calls})
		}
	}
	p.run()
	x, err := s.exposure(ctx)
	if err != nil {
		return out, err
	}
	for i := range p.turns {
		t := &p.turns[i]
		answers := make([]*ItemAnswer, len(t.Answers))
		for j := range t.Answers {
			answers[j] = &t.Answers[j]
		}
		if err := s.checkAnswerSpans(ctx, x, answers); err != nil {
			return out, err
		}
		for _, a := range t.Answers {
			if len(a.Flags) > 0 {
				t.FlaggedAnswers++
			}
		}
	}
	out.Turns = p.turns
	out.Totals = totals(p.turns, out.Reviews)
	return out, nil
}

// checkRecord is a check as kept, with its model calls: its role, the call
// that wrote what it checked, the name it checked, and its outcome.
type checkRecord struct {
	role                   check.Role
	checked, name, outcome string
	calls                  []CheckCall
}

// checkHistory are the case's checks, oldest first, each with its model calls
// as their records hold them: the first, then each a retry with its reason
// or a follow-up, as the check's conversation holds what asked for it
// (askedFor).
func (s *Session) checkHistory(ctx context.Context) ([]checkRecord, error) {
	calls, ids, err := s.checkCalls(ctx)
	if err != nil {
		return nil, err
	}
	kept, convs, err := s.checkRows(ctx)
	if err != nil {
		return nil, err
	}
	st := companion.Store{DB: s.c.st.DB()}
	for i := range kept {
		r := &kept[i]
		r.calls = calls[convs[i]]
		if len(r.calls) > 1 {
			msgs, err := st.Messages(ctx, convs[i])
			if err != nil {
				return nil, err
			}
			askedFor(msgs, ids[convs[i]], r.calls)
		}
	}
	return kept, nil
}

// checkCalls are the editor's and the reviewer's model calls, by the check
// conversation each served, in order — calls started at the same instant
// in the order recorded — the first, then retries; and their invocations'
// IDs, in the same order.
func (s *Session) checkCalls(ctx context.Context) (map[string][]CheckCall, map[string][]string, error) {
	rows, err := s.c.st.DB().QueryContext(ctx, `SELECT id, COALESCE(json_extract(record, '$.conversation_id'), ''), outcome, COALESCE(json_extract(record, '$.duration'), 0)
		FROM invocations WHERE investigation_id = ? AND task_type IN (?, ?) ORDER BY started_at, rowid`, s.c.id, EditorTask, ReviewTask)
	if err != nil {
		return nil, nil, fmt.Errorf("the checks' calls: %w", err)
	}
	defer rows.Close()
	calls, ids := map[string][]CheckCall{}, map[string][]string{}
	for rows.Next() {
		var id, conv, outcome string
		var ns int64
		if err := rows.Scan(&id, &conv, &outcome, &ns); err != nil {
			return nil, nil, fmt.Errorf("the checks' calls: %w", err)
		}
		kind := CallFirst
		if len(calls[conv]) > 0 {
			kind = CallRetry
		}
		calls[conv] = append(calls[conv], CheckCall{Kind: kind, Outcome: outcome, Duration: time.Duration(ns)})
		ids[conv] = append(ids[conv], id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("the checks' calls: %w", err)
	}
	return calls, ids, nil
}

// askedFor sets each of a check's calls after the first, ids their
// invocations in order, to what asked for it, read from msgs, the check's
// conversation: the messages after the previous call's answer and before
// its own, or, for a call whose answer is not kept, after the last answer
// kept. The follow-up's ask makes it a follow-up; anything else a retry,
// with its reason (retryReason).
func askedFor(msgs []companion.Message, ids []string, calls []CheckCall) {
	answers := make(map[string]int, len(calls))
	for i, m := range msgs {
		if m.Kind == companion.KindModel && m.Invocation != "" {
			answers[m.Invocation] = i
		}
	}
	from := 0
	for j := range calls {
		end, ok := answers[ids[j]]
		if ok = ok && end >= from; !ok {
			end = len(msgs)
		}
		if j > 0 {
			asked := msgs[from:end]
			if slices.ContainsFunc(asked, func(m companion.Message) bool {
				return m.Kind == companion.KindEngine && strings.HasPrefix(m.Text, followUpHeading)
			}) {
				calls[j].Kind = CallFollowUp
			} else {
				var answer companion.Message
				if from > 0 {
					answer = msgs[from-1]
				}
				calls[j].Kind, calls[j].Reason = CallRetry, retryReason(answer, asked)
			}
		}
		if ok {
			from = end + 1
		}
	}
}

// retryReason is why a check's retry was asked, given the previous call's
// answer and the messages after it: the first refusal sent back as a
// result — check_findings's of findings refused whole, the findings
// schema's name taken off its problems, or the loop's of a call it did not
// run, its first line each — or, where the answer called no tool, that it
// answered in text; otherwise that its reason is not recorded.
func retryReason(answer companion.Message, after []companion.Message) string {
	for _, m := range after {
		for _, r := range m.Results {
			if !r.IsError {
				continue
			}
			line, _, _ := strings.Cut(r.Text, "\n")
			if why, ok := strings.CutPrefix(line, findingsRefused); ok {
				return "findings refused: " + strings.TrimPrefix(why, apischema.Refusal(apischema.CheckFindings))
			}
			return "call refused: " + line
		}
	}
	if answer.Kind == companion.KindModel && len(answer.Calls) == 0 {
		return "answered in text"
	}
	return "not recorded"
}

// checkRows are the case's checks as kept, oldest first, with the
// conversation of each.
func (s *Session) checkRows(ctx context.Context) ([]checkRecord, []string, error) {
	rows, err := s.c.st.DB().QueryContext(ctx, `SELECT role, checked_invocation_id, checked_name, check_conversation_id, outcome FROM checks
		WHERE investigation_id = ? ORDER BY created_at, rowid`, s.c.id)
	if err != nil {
		return nil, nil, fmt.Errorf("the checks: %w", err)
	}
	defer rows.Close()
	var out []checkRecord
	var convs []string
	for rows.Next() {
		var r checkRecord
		var role, conv string
		if err := rows.Scan(&role, &r.checked, &r.name, &conv, &r.outcome); err != nil {
			return nil, nil, fmt.Errorf("the checks: %w", err)
		}
		r.role = check.Role(role)
		out, convs = append(out, r), append(convs, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("the checks: %w", err)
	}
	return out, convs, nil
}

// revisionCalls are the revisions' model calls in history, by the index
// of the answer each wrote, as their records hold them.
func (s *Session) revisionCalls(ctx context.Context, history []ConversationMessage) (map[int]CheckCall, error) {
	out := map[int]CheckCall{}
	for i, m := range history {
		if !Revision(history, i) || m.Invocation == "" {
			continue
		}
		var outcome string
		var ns int64
		err := s.c.st.DB().QueryRowContext(ctx, `SELECT outcome, COALESCE(json_extract(record, '$.duration'), 0) FROM invocations WHERE id = ?`, m.Invocation).Scan(&outcome, &ns)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("the revision's call: %w", err)
		}
		out[i] = CheckCall{Kind: CallRevision, Outcome: outcome, Duration: time.Duration(ns)}
	}
	return out, nil
}

// consultRows are the consults run under each of convs.
func (s *Session) consultRows(ctx context.Context, convs []string) (map[string][]*consultRow, error) {
	st := companion.Store{DB: s.c.st.DB()}
	out := map[string][]*consultRow{}
	for _, id := range convs {
		cs, err := st.Consults(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			msgs, err := st.Messages(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			row := &consultRow{conv: c}
			for _, m := range msgs {
				switch m.Kind {
				case companion.KindConsult:
					row.question = m.Text
				case companion.KindModel:
					for _, call := range m.Calls {
						row.keys = append(row.keys, callKey(call))
					}
					if m.Invocation != "" {
						sum, err := invocations.GetSummary(s.c.st.DB(), m.Invocation)
						if err != nil {
							return nil, err
						}
						row.models = append(row.models, ModelCallTokens{Invocation: m.Invocation, Conversation: c.ID, Input: sum.Usage.InputTokens, CacheRead: sum.Usage.CacheReadTokens, Cache: sum.Cache})
						row.guidance.call(sum.Usage)
					}
				case companion.KindResults:
					row.budget = row.budget || m.Text != ""
					for _, r := range m.Results {
						if l, ok := loadedGuidance(r); ok {
							row.guidance.experts = append(row.guidance.experts, l)
						}
					}
				}
			}
			out[id] = append(out[id], row)
		}
	}
	return out, nil
}

// trajectoryPass is one pass over the main history, oldest first.
type trajectoryPass struct {
	history  []ConversationMessage
	consults map[string][]*consultRow
	turns    []TrajectoryTurn
	// turnAt is each message's turn, by index, -1 before its
	// conversation's first.
	turnAt []int
	// seen is each call's key and the number of the turn first making it.
	seen map[string]int
	// pending are the experts loaded before a conversation's first turn.
	pending []ExpertLoad
	// returned are the items tools returned, in order, returnedTurn the
	// turn of each, returnedAt the index of the message returning it and
	// shown the result's text the model read it in; lastRead and lastCite
	// the index of the newest evidence_read and answer naming each,
	// lastPathCite of the newest answer citing each artifact by path;
	// lastAnswer each turn's newest answer, by index.
	returned           []string
	returnedTurn       []int
	returnedAt         map[string]int
	shown              map[string]string
	lastRead, lastCite map[string]int
	lastPathCite       map[string]int
	lastAnswer         map[int]int
	// short is the short name an evidence ID names, a short name as it
	// is; paths are the artifact paths a collected item read, none for
	// another item.
	short func(string) string
	paths func(string) []string
	// editorCalls are the editor's model calls by the call that wrote the
	// answer checked; revisions each revision's call, by the index of the
	// answer it wrote.
	editorCalls map[string][]CheckCall
	revisions   map[int]CheckCall
	// handoffs are the history's handoff messages.
	handoffs []string
	// asked are the calls of the newest model message by ID, askedByEngine
	// whether the engine made them; conversation and cur are the current
	// message's conversation and turn, -1 before its first.
	asked         map[string]model.ToolCall
	askedByEngine bool
	conversation  string
	cur           int
	// takes says whether a tool of the companion's table takes a question;
	// questions are the questions asked so far, by questionKey, each with
	// the number of the turn first asking it.
	takes     func(string) bool
	questions map[string]int
	// got are the items the main history's tools returned so far, and
	// answered the index of the newest model message whose calls answered
	// each, by short name.
	got      returnedItems
	answered map[string]int
	// guidance is the expert guidance loaded in the current conversation,
	// in order; tallies each turn's, by index.
	guidance []GuidanceLoad
	tallies  []guidanceTally
}

func (p *trajectoryPass) run() {
	p.turnAt = make([]int, len(p.history))
	p.cur = -1
	for i, m := range p.history {
		if m.ConversationID != p.conversation {
			p.conversation, p.cur, p.guidance = m.ConversationID, -1, nil
		}
		switch m.Kind {
		case MessageEngineer, MessageEngine:
			p.turns = append(p.turns, TrajectoryTurn{Number: len(p.turns) + 1, Conversation: m.ConversationID, Kind: turnKind(m), Experts: p.pending})
			p.pending = nil
			p.cur = len(p.turns) - 1
			p.tallies = append(p.tallies, guidanceTally{})
		case MessageModel:
			p.model(i, m)
		case MessageResults:
			p.results(i, m)
		case MessageHandoff:
			p.handoffs = append(p.handoffs, m.Text)
		}
		p.turnAt[i] = p.cur
		if p.cur >= 0 {
			p.tallies[p.cur].experts = p.guidance
		}
	}
	p.evidence()
	p.checks()
	for i, t := range p.turns {
		a, ok := p.lastAnswer[i]
		if !ok || t.Kind != "engine" {
			continue
		}
		lead := companion.HandoffText(p.history[a].Text)
		if slices.ContainsFunc(p.handoffs, func(h string) bool { return strings.HasPrefix(h, lead) }) {
			p.turns[i].Kind = "handoff"
		}
	}
	for i := range p.turns {
		t := &p.turns[i]
		for _, c := range t.ModelCalls {
			t.Input.add(c.Input)
			t.CacheRead.add(c.CacheRead)
		}
		t.Guidance = p.tallies[i].share()
	}
}

// nextTurn is the number of the turn a call made now is counted in.
func (p *trajectoryPass) nextTurn() int {
	if p.cur >= 0 {
		return p.turns[p.cur].Number
	}
	return len(p.turns) + 1
}

// question is call c's question as the trajectory reports it, counted in
// the current turn; asked gathers the questions of c's response.
func (p *trajectoryPass) question(c model.ToolCall, asked map[string]bool) CallQuestion {
	q := CallQuestion{Tool: c.Name, Question: QuestionOf(c.Name, c.Args)}
	n := &p.turns[p.cur].Questions
	n.Calls++
	if q.Question == "" {
		q.Missing = true
		n.None++
		return q
	}
	k := questionKey(q.Question)
	if first, ok := p.questions[k]; ok {
		q.Repeats = first
		n.Repeated++
	}
	asked[k] = true
	if vagueQuestion(q.Question) {
		q.Vague = true
		n.Vague++
	}
	if restatesAction(q.Question) {
		q.Restates = true
		n.Restating++
	}
	return q
}

func (p *trajectoryPass) model(i int, m ConversationMessage) {
	// A call with no invocation is the engine's own (Loop.Preload).
	p.askedByEngine = m.Invocation == ""
	p.asked = make(map[string]model.ToolCall, len(m.Calls))
	// A question repeats one of an earlier response: the calls of one
	// response may share theirs.
	asked := map[string]bool{}
	for _, c := range m.Calls {
		p.asked[c.ID] = c
		key := callKey(c)
		first, made := p.seen[key]
		if !made {
			p.seen[key] = p.nextTurn()
		}
		if p.askedByEngine || p.cur < 0 {
			continue
		}
		t := &p.turns[p.cur]
		t.Calls++
		if made {
			t.Repeated = append(t.Repeated, RepeatedCall{Tool: c.Name, Args: callArgs(c), FirstTurn: first})
		}
		if p.takes(c.Name) {
			t.Asked = append(t.Asked, p.question(c, asked))
		}
		for _, a := range callAnswersOf(c.Name, c.Args) {
			if f := p.got.itemFlag(a); f != "" {
				a.Flags = append(a.Flags, f)
			} else if len(a.Flags) == 0 {
				p.answered[a.Item] = i
			}
			t.Answers = append(t.Answers, a)
		}
	}
	for k := range asked {
		if _, ok := p.questions[k]; !ok {
			p.questions[k] = p.nextTurn()
		}
	}
	if IsAnswer(m) {
		for _, c := range Cites(m.Text) {
			switch {
			case c.Name != "":
				p.lastCite[c.Name] = i
			case c.Path != "":
				p.lastPathCite[path.Clean(c.Path)] = i
			}
		}
		if p.cur >= 0 {
			p.lastAnswer[p.cur] = i
		}
	}
	if p.cur >= 0 && m.Invocation != "" && m.Call != nil {
		t := &p.turns[p.cur]
		t.ModelCalls = append(t.ModelCalls, ModelCallTokens{Invocation: m.Invocation, Conversation: m.ConversationID, Input: m.Call.Usage.InputTokens, CacheRead: m.Call.Usage.CacheReadTokens, Cache: m.Call.Cache})
		p.tallies[p.cur].call(m.Call.Usage)
	}
}

func (p *trajectoryPass) results(i int, m ConversationMessage) {
	if p.cur >= 0 && m.Text != "" {
		// The only text a results message carries is the engine's word
		// that the turn's budget is spent.
		p.turns[p.cur].BudgetSpent = true
	}
	for _, r := range m.Results {
		if l, ok := loadedGuidance(r); ok {
			p.guidance = append(slices.Clip(p.guidance), l)
		}
		c := p.asked[r.CallID]
		if p.askedByEngine {
			if r.Name == "load_expert" {
				p.load(ExpertLoad{ID: strParam(c.Args, "id"), ByEngine: true})
			}
			continue
		}
		if p.cur < 0 {
			continue
		}
		t := &p.turns[p.cur]
		if r.IsError {
			t.Refused++
		}
		switch r.Name {
		case "load_expert":
			t.Experts = append(t.Experts, ExpertLoad{ID: strParam(c.Args, "id"), Refused: r.IsError})
		case consultTool:
			t.Consults = append(t.Consults, p.consult(t, c, r.IsError))
		case evidenceReadTool:
			if !r.IsError {
				for _, name := range strings.Fields(strParam(c.Args, "id")) {
					p.lastRead[p.short(name)] = i
				}
			}
		}
		if r.IsError {
			continue
		}
		p.got.add([]model.ToolResult{r}, p.asked, p.short)
		for _, name := range ExcerptEvidence(r.Text) {
			if _, ok := p.returnedAt[name]; ok {
				continue
			}
			p.returnedAt[name], p.shown[name] = i, r.Text
			p.returned = append(p.returned, name)
			p.returnedTurn = append(p.returnedTurn, p.cur)
		}
	}
}

// load counts an expert loaded in the current turn, or the next where the
// conversation has none yet.
func (p *trajectoryPass) load(l ExpertLoad) {
	if p.cur < 0 {
		p.pending = append(p.pending, l)
		return
	}
	p.turns[p.cur].Experts = append(p.turns[p.cur].Experts, l)
}

// loadedGuidance is the guidance r, load_expert's result, gave, and false
// for another result, a refusal or load_expert's word that the expert is
// loaded already.
func loadedGuidance(r model.ToolResult) (GuidanceLoad, bool) {
	if r.Name != "load_expert" || r.IsError {
		return GuidanceLoad{}, false
	}
	return guidanceOf(r.Text)
}

// consult is consult call c's run: the first consult of the conversation
// not yet matched with c's expert and question, with the guidance its ask
// gave each of its model calls.
func (p *trajectoryPass) consult(t *TrajectoryTurn, c model.ToolCall, refused bool) ConsultRun {
	expert, question := strParam(c.Args, "expert"), strParam(c.Args, "question")
	run := ConsultRun{Expert: expert, Refused: refused}
	if strings.TrimSpace(question) == "" {
		return run
	}
	for _, row := range p.consults[p.conversation] {
		if row.used || row.conv.Expert != expert || !strings.HasSuffix(row.question, "\n\n"+question) {
			continue
		}
		row.used = true
		run.Conversation, run.Calls = row.conv.ID, len(row.keys)
		for _, k := range row.keys {
			if _, ok := p.seen[k]; ok {
				run.Repeats++
			}
		}
		t.ModelCalls = append(t.ModelCalls, row.models...)
		t.BudgetSpent = t.BudgetSpent || row.budget
		g := row.guidance
		if ask, ok := companion.ConsultGuidance(row.question, question); ok {
			if l, ok := guidanceOf(ask); ok {
				g.experts = append([]GuidanceLoad{l}, g.experts...)
				g.sent += l.Tokens.Value * int64(g.calls)
			}
		}
		share := g.share()
		run.Guidance = &share
		break
	}
	return run
}

// evidence counts each returned item in its turn by what became of it.
func (p *trajectoryPass) evidence() {
	for k, name := range p.returned {
		if p.returnedTurn[k] < 0 {
			continue
		}
		at := p.returnedAt[name]
		r, read := p.lastRead[name]
		c, cited := p.lastCite[name]
		a, answered := p.answered[name]
		read, cited, answered = read && r > at, cited && c > at || p.citedByPath(name, at), answered && a > at
		e := &p.turns[p.returnedTurn[k]].Evidence
		e.Returned++
		switch {
		case read && cited:
			e.Both++
		case read:
			e.ReadOn++
		case cited:
			e.Cited++
		default:
			e.Neither++
			e.NeitherNames = append(e.NeitherNames, name)
		}
		if answered {
			e.Answered++
		} else if !read && !cited {
			e.None++
			e.NoneNames = append(e.NoneNames, name)
		}
	}
}

// citedByPath says whether an answer after at cites, by path, a file the
// item named read: the one file it read, or one its result names, so a
// search over a directory is not credited with every file it opened.
func (p *trajectoryPass) citedByPath(name string, at int) bool {
	read := p.paths(name)
	for _, f := range read {
		f = path.Clean(f)
		if c, ok := p.lastPathCite[f]; ok && c > at && (len(read) == 1 || strings.Contains(p.shown[name], f)) {
			return true
		}
	}
	return false
}

// checks gives each turn the editor's check of its newest answer.
func (p *trajectoryPass) checks() {
	for _, v := range AnswerViews(p.history) {
		ti := p.turnAt[v.Original]
		c := p.history[v.Original].Check
		if ti < 0 || c == nil {
			continue
		}
		tc := &TurnCheck{Answer: v.Label, Unchecked: c.Unchecked, Anchors: len(c.Anchors), Revised: c.Revised, RevisionFailed: c.RevisionFailed, AddedLines: c.Added,
			Calls: slices.Clone(p.editorCalls[p.history[v.Original].Invocation])}
		if c.Revised && Revision(p.history, v.At) {
			tc.AddedWhole = wholeAdded(p.history[v.Original].Text, v.Text, c.CorrectedCites)
		}
		if r, ok := p.revisions[v.At]; ok {
			tc.Calls = append(tc.Calls, r)
		}
		p.turns[ti].Check = tc
	}
}

// wholeAdded are the citations of a whole item, [e4], that revised holds
// naming an item neither answer, the answer as written, nor the editor's
// corrected citations cite, each once, in revised's order:
// revisionAdded's counterpart for a citation naming no lines, which it
// leaves out.
func wholeAdded(answer, revised string, corrected []string) []string {
	had := map[string]bool{}
	for _, c := range Cites(answer) {
		had[c.Name] = true
	}
	for _, cc := range corrected {
		for _, c := range Cites(cc) {
			had[c.Name] = true
		}
	}
	var out []string
	for _, c := range Cites(revised) {
		if c.Name == "" || c.From != 0 || had[c.Name] {
			continue
		}
		had[c.Name] = true
		out = append(out, "["+c.Name+"]")
	}
	return out
}

// turnKind is what asked for a turn, read from its message.
func turnKind(m ConversationMessage) string {
	switch {
	case m.Kind == MessageEngineer:
		return "talk"
	case strings.HasPrefix(m.Text, orientAsk):
		return "opening"
	case m.Text == analyzeAsk || m.Text == analyzeAskBefore:
		return "analyze"
	case strings.HasPrefix(m.Text, draftLead):
		return "draft"
	case strings.HasPrefix(m.Text, catchUpLead):
		return "catch-up"
	}
	return "engine"
}

// callKey is a call's tool and the arguments it ran with, alike for calls
// alike whatever each asked.
func callKey(c model.ToolCall) string { return c.Name + "\x00" + callArgs(c) }

// callArgs are the arguments a call ran with, its question taken out
// (ranArgs), as JSON, keys sorted.
func callArgs(c model.ToolCall) string {
	// Decoded from JSON, so it encodes again.
	b, _ := json.Marshal(ranArgs(c.Name, c.Args))
	return string(b)
}

func totals(turns []TrajectoryTurn, reviews []ReviewRun) TrajectoryTotals {
	var t TrajectoryTotals
	t.Turns = len(turns)
	t.Reviews = len(reviews)
	for _, r := range reviews {
		t.CheckTime.add(r.Calls)
	}
	var guidance, consulted []GuidanceShare
	for _, u := range turns {
		guidance = append(guidance, u.Guidance)
		t.Calls += u.Calls
		t.Refused += u.Refused
		if u.BudgetSpent {
			t.BudgetSpent++
		}
		e := &t.Evidence
		e.Returned += u.Evidence.Returned
		e.ReadOn += u.Evidence.ReadOn
		e.Cited += u.Evidence.Cited
		e.Both += u.Evidence.Both
		e.Neither += u.Evidence.Neither
		e.NeitherNames = append(e.NeitherNames, u.Evidence.NeitherNames...)
		e.Answered += u.Evidence.Answered
		e.None += u.Evidence.None
		e.NoneNames = append(e.NoneNames, u.Evidence.NoneNames...)
		t.Repeated += len(u.Repeated)
		t.Questions.and(u.Questions)
		t.Answers += len(u.Answers)
		t.FlaggedAnswers += u.FlaggedAnswers
		for _, x := range u.Experts {
			if x.Refused {
				continue
			}
			t.Loaded++
			if x.ByEngine {
				t.ByEngine++
			}
		}
		for _, c := range u.Consults {
			t.Consults++
			t.ConsultCalls += c.Calls
			t.ConsultRepeats += c.Repeats
			if c.Guidance != nil {
				consulted = append(consulted, *c.Guidance)
			}
		}
		t.ModelCalls += len(u.ModelCalls)
		t.Input.and(u.Input)
		t.CacheRead.and(u.CacheRead)
		if c := u.Check; c != nil {
			t.Checked++
			if c.Revised {
				t.Revised++
			}
			t.AddedLines += len(c.AddedLines)
			t.AddedWhole += len(c.AddedWhole)
			t.CheckTime.add(c.Calls)
		}
	}
	t.Guidance, t.ConsultGuidance = sumGuidance(guidance), sumGuidance(consulted)
	return t
}

// RenderTrajectory is the trajectory as compa case trajectory prints it: one
// block a turn, then the totals.
func RenderTrajectory(t Trajectory) string {
	if len(t.Turns) == 0 {
		return "no turns: the case has no conversation yet\n"
	}
	var b strings.Builder
	for _, u := range t.Turns {
		fmt.Fprintf(&b, "── turn %d · %s · %s\n", u.Number, u.Kind, text.Safe(u.Conversation))
		line := "  " + text.Plural(u.Calls, "call") + fmt.Sprintf(", %d refused", u.Refused)
		if len(u.Repeated) > 0 {
			line += fmt.Sprintf(", %d repeated", len(u.Repeated))
		}
		if u.BudgetSpent {
			line += "; the turn's budget was spent"
		}
		b.WriteString(line + "\n")
		for _, r := range u.Repeated {
			fmt.Fprintf(&b, "  repeated %s %s, first in turn %d\n", text.Safe(r.Tool), text.Safe(text.Ellipsis(r.Args, 120)), r.FirstTurn)
		}
		if u.Questions.Calls > 0 {
			b.WriteString("  " + questionWords(u.Questions) + "\n")
		}
		for _, q := range u.Asked {
			b.WriteString("  " + askedWords(q) + "\n")
		}
		if n := len(u.Answers); n > 0 {
			fmt.Fprintf(&b, "  answers: %d given, %d flagged\n", n, u.FlaggedAnswers)
		}
		for _, a := range u.Answers {
			b.WriteString("  " + answerWords(a) + "\n")
		}
		if u.Evidence.Returned > 0 {
			b.WriteString("  " + evidenceWords(u.Evidence) + "\n")
		}
		for _, x := range u.Experts {
			fmt.Fprintf(&b, "  expert %s %s\n", text.Safe(x.ID), loadWords(x))
		}
		for _, c := range u.Consults {
			fmt.Fprintf(&b, "  consulted %s: %s\n", text.Safe(c.Expert), consultWords(c))
			if g := c.Guidance; g != nil && len(g.Experts) > 0 {
				b.WriteString("    guidance " + guidanceWords(*g) + "\n")
			}
		}
		if n := len(u.ModelCalls); n > 0 {
			in := make([]string, n)
			cache := make([]string, n)
			recorded := false
			for i, c := range u.ModelCalls {
				in[i] = countWords(c.Input)
				cache[i] = cacheWords(c.Cache)
				recorded = recorded || c.Cache != nil
			}
			fmt.Fprintf(&b, "  %s, input tokens %s: %s; cache read %s\n", text.Plural(n, "model call"), strings.Join(in, " "), sumWords(u.Input), sumWords(u.CacheRead))
			if recorded {
				b.WriteString("  cache by call: " + strings.Join(cache, "; ") + "\n")
			}
		}
		if len(u.Guidance.Experts) > 0 {
			b.WriteString("  guidance " + guidanceWords(u.Guidance) + "\n")
		}
		if c := u.Check; c != nil {
			b.WriteString("  " + checkWords(*c) + "\n")
			writeCalls(&b, c.Calls)
		}
	}
	if len(t.Reviews) > 0 {
		b.WriteString("── reviews\n")
		for _, r := range t.Reviews {
			fmt.Fprintf(&b, "  review of %s: %s\n", text.Safe(r.Of), text.Safe(r.Outcome))
			writeCalls(&b, r.Calls)
		}
	}
	o := t.Totals
	b.WriteString("── totals\n")
	fmt.Fprintf(&b, "  %s, %s, %d refused, %d repeated; %d over budget\n", text.Plural(o.Turns, "turn"), text.Plural(o.Calls, "call"), o.Refused, o.Repeated, o.BudgetSpent)
	if o.Questions.Calls > 0 {
		b.WriteString("  " + questionWords(o.Questions) + "\n")
	}
	if o.Answers > 0 {
		fmt.Fprintf(&b, "  answers: %d given, %d flagged\n", o.Answers, o.FlaggedAnswers)
	}
	if o.Evidence.Returned > 0 {
		b.WriteString("  " + evidenceWords(o.Evidence) + "\n")
	}
	fmt.Fprintf(&b, "  %s loaded, %d by the engine; %s, %s, %d repeating the main conversation's\n", text.Plural(o.Loaded, "expert"), o.ByEngine, text.Plural(o.Consults, "consult"), text.Plural(o.ConsultCalls, "call"), o.ConsultRepeats)
	fmt.Fprintf(&b, "  %s, input tokens %s; cache read %s, as the records hold them\n", text.Plural(o.ModelCalls, "model call"), sumWords(o.Input), sumWords(o.CacheRead))
	if len(o.Guidance.Experts) > 0 {
		b.WriteString("  guidance " + guidanceWords(o.Guidance) + "\n")
	}
	if len(o.ConsultGuidance.Experts) > 0 {
		b.WriteString("  consults' guidance " + guidanceWords(o.ConsultGuidance) + "\n")
	}
	fmt.Fprintf(&b, "  %s checked, %d revised; added by revisions: %s with lines, %s whole; %s\n", text.Plural(o.Checked, "answer"), o.Revised, text.Plural(o.AddedLines, "citation"), text.Plural(o.AddedWhole, "citation"), text.Plural(o.Reviews, "review"))
	if len(o.CheckTime) > 0 {
		var parts []string
		for _, k := range []string{CallFirst, CallRetry, CallFollowUp, CallRevision} {
			if ct, ok := o.CheckTime[k]; ok {
				parts = append(parts, fmt.Sprintf("%s %d, longest %s", k, ct.Calls, seconds(ct.Longest)))
			}
		}
		b.WriteString("  check calls: " + strings.Join(parts, "; ") + "\n")
	}
	return b.String()
}

// guidanceWords is guidance some model calls sent: each expert at its
// version with its tokens, their sum, what the calls sent and its share
// of their input, or why that is unavailable, the estimator named.
func guidanceWords(g GuidanceShare) string {
	held := make([]string, len(g.Experts))
	for i, x := range g.Experts {
		held[i] = fmt.Sprintf("%s %s %d", text.Safe(x.ID), text.Safe(x.Version), x.Tokens.Value)
	}
	s := fmt.Sprintf("%s: %d tokens; %d sent over %s, ", strings.Join(held, ", "), g.Tokens.Value, g.Sent.Value, text.Plural(g.Calls, "model call"))
	switch {
	case g.Share.Known:
		s += fmt.Sprintf("%.1f%% of their input", 100*g.Share.Value)
	case g.Unavailable > 0:
		s += "share of their input unavailable, " + text.Plural(g.Unavailable, "call") + " unreported"
	default:
		s += "share of their input unavailable"
	}
	return s + " (estimated, " + g.Tokens.EstimatorVersion + ")"
}

// writeCalls writes a check's calls in order, one a line beneath it: each
// its kind, time and outcome, and a retry's reason cut to a line.
func writeCalls(b *strings.Builder, calls []CheckCall) {
	for _, c := range calls {
		b.WriteString("    " + c.Kind + " " + seconds(c.Duration) + " " + text.Safe(c.Outcome))
		if c.Reason != "" {
			b.WriteString(" — " + text.Safe(text.Ellipsis(c.Reason, 120)))
		}
		b.WriteString("\n")
	}
}

// seconds is a duration in seconds to a tenth.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%.1f s", d.Seconds())
}

// questionWords are the counts of calls taking a question: how many asked
// one, gave none, repeated an earlier one, were vague or restated the
// action.
func questionWords(q QuestionCounts) string {
	return fmt.Sprintf("questions: %d of %s asked one; %d with none, %d repeating an earlier response's, %d vague, %d restating the action",
		q.Calls-q.None, text.Plural(q.Calls, "call"), q.None, q.Repeated, q.Vague, q.Restating)
}

// askedWords is one call's question as the text gives it, cut to a line,
// marked where it repeats an earlier response's, is vague or restates the
// action.
func askedWords(q CallQuestion) string {
	if q.Missing {
		return "? " + text.Safe(q.Tool) + ": no question"
	}
	s := "? " + text.Safe(q.Tool) + ": " + text.Safe(text.Ellipsis(q.Question, 160))
	if q.Repeats > 0 {
		s += fmt.Sprintf(" [repeats turn %d]", q.Repeats)
	}
	if q.Vague {
		s += " [vague]"
	}
	if q.Restates {
		s += " [restates the action]"
	}
	return s
}

// evidenceWords are what became of the items returned: read on, cited,
// both or neither, then those answered and, named, those with no answer,
// citation or read-on.
func evidenceWords(e TrajectoryEvidence) string {
	s := fmt.Sprintf("%s returned: %d read on, %d cited, %d both, %d neither; %d answered, %d with no answer, citation or read-on",
		text.Plural(e.Returned, "item"), e.ReadOn, e.Cited, e.Both, e.Neither, e.Answered, e.None)
	if len(e.NoneNames) > 0 {
		s += " (" + strings.Join(e.NoneNames, ", ") + ")"
	}
	return s
}

// answerWords is one answer as the text gives it, cut to a line, with
// what was flagged.
func answerWords(a ItemAnswer) string {
	s := "= " + text.Safe(a.Item) + " on " + text.Safe(a.Tool) + ": " + text.Safe(text.Ellipsis(a.Answer, 160))
	if len(a.Flags) > 0 {
		s += " ⚠ " + text.Safe(text.Ellipsis(strings.Join(a.Flags, "; "), 200))
	}
	return s
}

func loadWords(x ExpertLoad) string {
	switch {
	case x.Refused:
		return "refused"
	case x.ByEngine:
		return "loaded by the engine"
	}
	return "loaded"
}

func consultWords(c ConsultRun) string {
	s := text.Plural(c.Calls, "call") + fmt.Sprintf(", %d repeating the main conversation's", c.Repeats)
	if c.Refused {
		s += "; refused or failed"
	}
	return s
}

// countWords is a count as a record holds it, an estimate naming its
// estimator.
func countWords(c model.Count) string {
	switch {
	case !c.Known:
		return "unavailable"
	case c.Fidelity == model.FidelityEstimated:
		return fmt.Sprintf("%d (estimated, %s)", c.Value, c.EstimatorVersion)
	}
	return fmt.Sprint(c.Value)
}

// cacheWords is what a call's provider did with a cache of its own, as its
// record holds it: the cache read by the messages it holds, and a failed
// make by the first line of the provider's error.
func cacheWords(c *model.Cache) string {
	if c == nil {
		return "none recorded"
	}
	read := "read none"
	if c.Name != "" {
		read = "(" + text.Plural(c.Messages, "message") + ")"
	}
	switch c.Outcome {
	case model.CacheMade:
		return "made " + read
	case model.CacheReused:
		return "reused " + read
	case model.CacheRefused:
		reason, _, _ := strings.Cut(c.Error, "\n")
		if c.Name != "" {
			read = "reused " + read
		}
		return "create refused: " + text.Safe(text.Ellipsis(reason, 160)) + ", " + read
	case model.CacheUnderMinimum:
		return "under the minimum"
	case model.CacheNoBreak:
		return "no break"
	}
	return text.Safe(c.Outcome)
}

func sumWords(s TokenSum) string {
	w := fmt.Sprint(s.Value)
	if s.Unavailable > 0 {
		w = fmt.Sprintf("at least %d, %s unavailable", s.Value, text.Plural(s.Unavailable, "call"))
	}
	if len(s.Estimators) > 0 {
		w += " (estimated in part: " + strings.Join(s.Estimators, ", ") + ")"
	}
	return w
}

func checkWords(c TurnCheck) string {
	s := "check of " + c.Answer
	switch {
	case c.Unchecked != "":
		return s + ": unchecked, " + text.Safe(c.Unchecked)
	case c.Revised:
		s += ": revised for " + text.Plural(c.Anchors, "anchor")
	case c.RevisionFailed != "":
		s += ": the revision for " + text.Plural(c.Anchors, "anchor") + " failed"
	default:
		s += ": not revised"
	}
	if len(c.AddedLines) > 0 {
		s += "; added with lines " + strings.Join(c.AddedLines, " ")
	}
	if len(c.AddedWhole) > 0 {
		s += "; added whole " + strings.Join(c.AddedWhole, " ")
	}
	return s
}

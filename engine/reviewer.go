package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/invocations"
)

// The reviewer: on the
// engineer's request, a second opinion of the model the deployment's
// reviewer profile names on the last answer as shown or on an item, given
// the text, its statements, what the deterministic checks found and the
// lines it cites, never the conversation. Its findings are in a fixed
// form — the claim, its support, the strongest material alternative, the
// missing discriminator, the maximum justified wording — and go into the
// case stream as a message of their own after the conversation's last,
// which the main model reads at its next call. The answer and the item
// stay as written; the review is a model's reading, never evidence, never
// an answer. A deployment with no reviewer profile offers none.

// ReviewTask is the task type the reviewer's calls are recorded as, and
// its model profile resolved for (class reviewer).
const ReviewTask = "answers.review"

// reviewConversation is the kind of a reviewer call's conversation.
const reviewConversation = "review"

// ErrNoReviewer says the deployment configures no reviewer, so none is
// offered or run.
var ErrNoReviewer = errors.New("no reviewer is configured (reasoning.profiles." + config.DefaultProfileClass[ReviewTask] + ")")

// reviewerBound is how long a review may take, its retry included, before
// it is said to have failed and nothing is kept of it in the conversation:
// a larger model's call over a request of reviewerRequestBytes, waited
// for by the engineer who asked. A live benchmark measured
// claude-sonnet-4-6 at 51–88 s a review, its retry included where it
// made one: the bound is about twice the slowest.
var reviewerBound = 180 * time.Second

// How a review ended, as kept.
const (
	checkReviewed = "reviewed"
	checkFailed   = "failed"
)

// reviewerCheck is the reviewer's.
var reviewerCheck = checkTask{task: ReviewTask, conversation: reviewConversation, system: check.ReviewerPrompt()}

// Review is the reviewer's reading of an answer or an item, as the chat
// prints it beneath and the main conversation keeps it.
type Review struct {
	// Of is what was reviewed, a7 or i3; Model the reviewer's model ID.
	Of, Model string
	// Statements counts the text's statements; Findings, Supported and
	// Skipped are what the reviewer said of them, as validated.
	Statements int
	Findings   []CheckFinding
	Supported  []string
	Skipped    []check.Skipped
	// Usage is the review's model calls' tokens and cost.
	Usage UsageAggregate
}

// accounted counts the statements the review said something of.
func (r Review) accounted() int {
	seen := map[string]bool{}
	for _, f := range r.Findings {
		seen[f.Statement] = true
	}
	for _, s := range r.Supported {
		seen[s] = true
	}
	for _, s := range r.Skipped {
		seen[s.Statement] = true
	}
	return len(seen)
}

// Counts are the review's figures in a line: 9 statements: 6 supported, 2
// findings, 1 skipped; with those it said nothing of, where there are
// any.
func (r Review) Counts() string {
	s := fmt.Sprintf("%s: %d supported, %s, %d skipped", text.Plural(r.Statements, "statement"), len(r.Supported), text.Plural(len(r.Findings), "finding"), len(r.Skipped))
	if n := r.Statements - r.accounted(); n > 0 {
		s += fmt.Sprintf(", %d not accounted for", n)
	}
	return s
}

// Footer is what the line beneath a review says, in order: what it is of,
// its figures, the model and its usage.
func (r Review) Footer() []string {
	out := []string{"review of " + r.Of, r.Counts()}
	if r.Model != "" {
		out = append(out, r.Model)
	}
	if u := r.Usage.Compact(); u != "" {
		out = append(out, u)
	}
	return out
}

// ReviewNote is one of a review's findings as it is shown: the statement,
// its kind and the words at fault (Text); why, with the lines it rests
// on; the strongest alternative and the discriminator; and the most the
// lines justify (AtMost).
type ReviewNote struct {
	Text, Why, Alternative, AtMost string
}

// Notes are the review's findings as the chat prints them and the main
// conversation keeps them.
func (r Review) Notes() []ReviewNote {
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	out := make([]ReviewNote, 0, len(r.Findings))
	for _, f := range r.Findings {
		n := ReviewNote{Text: f.Statement + " " + f.Kind + " · " + strconv.Quote(flat(f.Quote)), Why: flat(f.Why), AtMost: flat(f.Suggested)}
		if len(f.Evidence) > 0 {
			n.Why += " (" + strings.Join(f.Evidence, ", ") + ")"
		}
		var alt []string
		if a := flat(f.Alternative); a != "" {
			alt = append(alt, "alternative: "+a)
		}
		if d := flat(f.Discriminator); d != "" {
			alt = append(alt, "discriminator: "+d)
		}
		n.Alternative = strings.Join(alt, " · ")
		out = append(out, n)
	}
	return out
}

// ReviewHeading heads a review in the main conversation: what it is of,
// by which model, and that it is a model's reading.
func ReviewHeading(of, modelID string) string {
	return "A review of " + of + " by `" + modelID + "`: a second opinion from another model, its reading, not verified and never evidence."
}

// reviewText is a review as the main conversation keeps it and the model
// reads it: its heading, its findings in their compact form, and
// its figures.
func reviewText(r Review) string {
	var b strings.Builder
	b.WriteString(ReviewHeading(r.Of, r.Model) + "\n\n")
	for _, n := range r.Notes() {
		b.WriteString("- " + n.Text + " · " + n.Why + "\n")
		if n.Alternative != "" {
			b.WriteString("  " + n.Alternative + "\n")
		}
		if n.AtMost != "" {
			b.WriteString("  at most: " + n.AtMost + "\n")
		}
	}
	if len(r.Findings) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(r.Counts() + ".")
	return b.String()
}

// ReviewerModel is the model ID the reviewer calls, or nothing where the
// deployment configures no reviewer.
func (s *Session) ReviewerModel() string {
	pc := s.e.config.Profile(ReviewTask)
	if !pc.Defined {
		return ""
	}
	return pc.Model
}

// Review has the reviewer review what ask names, an item or the
// conversation's last answer as the engineer was shown it, given what ask
// asks (ReviewRequest), and appends what it found to the main
// conversation as a message of its own, once, after its last message; it
// calls the reviewer's model at most checkBudget times, within
// reviewerBound. A review that fails, passes its bound or is cancelled
// is kept as failed in the checks and appends nothing. The caller runs no
// turn meanwhile: the chat refuses /review while one runs.
func (s *Session) Review(ctx context.Context, ask ReviewAsk, on func(TalkEvent)) (Review, error) {
	pc := s.e.config.Profile(ReviewTask)
	if !pc.Defined {
		return Review{}, ErrNoReviewer
	}
	if on == nil {
		on = func(TalkEvent) {}
	}
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	if errors.Is(err, companion.ErrNoConversation) {
		return Review{}, errNothingToCheck
	}
	if err != nil {
		return Review{}, err
	}
	req, of, err := s.reviewRequest(ctx, ask)
	if err != nil {
		return Review{}, err
	}
	run := checkRun{role: check.Reviewer, conv: conv.ID, checked: of.invocation, name: of.name}
	bounded, cancel := context.WithTimeout(ctx, reviewerBound)
	err = s.runCheck(bounded, reviewerCheck, pc, req, &run, on)
	cancel()
	// How the review ended is kept whatever the engineer does now, each
	// write within a bound of its own (finalContext).
	if ctx.Err() != nil {
		s.keepCheck(ctx, run, checkFailed, "cancelled", on)
		return Review{}, ctx.Err()
	}
	if why := reviewerFailed(err); why != "" {
		s.keepCheck(ctx, run, checkFailed, why, on)
		return Review{}, errors.New(why)
	}
	r := Review{Of: of.name, Model: pc.Model, Statements: run.statements,
		Findings: run.findings.Findings, Supported: run.findings.Supported, Skipped: run.findings.Skipped}
	l := &companion.Loop{Store: st, Now: s.c.now}
	fin, finished := finalContext(ctx)
	err = l.AddReview(fin, conv.ID, run.madeBy, reviewText(r))
	finished()
	if err != nil {
		s.keepCheck(ctx, run, checkFailed, err.Error(), on)
		return Review{}, err
	}
	s.keepCheck(ctx, run, checkReviewed, "", on)
	usage, err := reviewUsage(s.c)
	if err != nil {
		on(TalkEvent{Kind: TalkWarning, Conversation: conv.ID, Text: "the review's usage could not be read: " + err.Error()})
	}
	r.Usage = usage[run.checkConv]
	return r, nil
}

// reviewerFailed says why a review failed, err its error, or nothing
// where it did not.
func reviewerFailed(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "the reviewer passed its " + reviewerBound.String() + " bound"
	}
	return "the reviewer failed: " + err.Error()
}

// reviewUsage is the usage of the reviewer's calls, by their
// conversation.
func reviewUsage(c *openCase) (map[string]UsageAggregate, error) {
	aggs, err := invocations.AggregateUsage(c.st.DB(), c.id, invocations.ByTaskType, invocations.ByConversation)
	if err != nil {
		return nil, err
	}
	out := map[string]UsageAggregate{}
	for _, a := range aggs {
		if a.Key[invocations.ByTaskType] == ReviewTask {
			out[a.Key[invocations.ByConversation]] = a
		}
	}
	return out, nil
}

// reviews are the case's reviews kept in its conversations, by the
// reviewer's call whose findings each holds, as the chat prints them.
func reviews(ctx context.Context, c *openCase) (map[string]Review, error) {
	// The checks are read first and their rows closed: the store has one
	// connection, which reading the records needs.
	all, err := keptReviews(ctx, c)
	if err != nil {
		return nil, err
	}
	out := map[string]Review{}
	if len(all) == 0 {
		return out, nil
	}
	usage, err := reviewUsage(c)
	if err != nil {
		return nil, err
	}
	for _, k := range all {
		// A call whose record was not kept leaves its review without its
		// model, shown all the same.
		sum, err := invocations.GetSummary(c.st.DB(), k.madeBy)
		if err != nil && !errors.Is(err, invocations.ErrNotFound) {
			return nil, err
		}
		k.r.Model, k.r.Usage = sum.Model.ModelID, usage[k.checkConv]
		out[k.madeBy] = k.r
	}
	return out, nil
}

// keptReview is a review as its check is kept: the reviewer's call whose
// findings it holds, its conversation, and the review but its model and
// usage.
type keptReview struct {
	madeBy, checkConv string
	r                 Review
}

// keptReviews are the case's reviews that were kept, oldest first.
func keptReviews(ctx context.Context, c *openCase) ([]keptReview, error) {
	rows, err := c.st.DB().QueryContext(ctx, `SELECT invocation_id, check_conversation_id, checked_name, statements, findings FROM checks
		WHERE investigation_id = ? AND role = ? AND outcome = ? ORDER BY created_at, rowid`, c.id, string(check.Reviewer), checkReviewed)
	if err != nil {
		return nil, fmt.Errorf("the reviews: %w", err)
	}
	defer rows.Close()
	var all []keptReview
	for rows.Next() {
		var k keptReview
		var findings string
		if err := rows.Scan(&k.madeBy, &k.checkConv, &k.r.Of, &k.r.Statements, &findings); err != nil {
			return nil, fmt.Errorf("the reviews: %w", err)
		}
		var f check.Findings
		if err := json.Unmarshal([]byte(findings), &f); err != nil {
			return nil, fmt.Errorf("the review of %s: %w", k.r.Of, err)
		}
		k.r.Findings, k.r.Supported, k.r.Skipped = f.Findings, f.Supported, f.Skipped
		all = append(all, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the reviews: %w", err)
	}
	return all, nil
}

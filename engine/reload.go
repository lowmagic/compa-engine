package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/companion"
)

// Reloading a conversation once its provider's cache has expired:
// going on as it is, or
// restarting it from the case's state and what the engine carries from it,
// with or without a handoff the model writes.

// ReloadMode is a way on for a conversation whose cache has expired.
type ReloadMode string

// The ways on, in the order the chat offers them: the conversation as it
// is, resent whole without the cache; a new conversation begun from the
// case's state and what the engine carries from the old one, with no
// model call; and the same with the model's handoff of the old one.
const (
	ReloadFull      ReloadMode = "full"
	ReloadSuggested ReloadMode = "suggested"
	ReloadSummary   ReloadMode = "summary"
)

// Bounds on the ranked evidence a restart carries: the items, their lines
// in all, and the first lines of an item ranked without any named.
const (
	carryRanked = 12
	carryBytes  = 24 << 10
	carryHead   = 20
)

// errNoConversation is a reload's or a catch-up's refusal on a case the
// companion has not begun a conversation for.
var errNoConversation = errors.New("there is no conversation yet")

// ReloadOffer is what the ways on cost, as the chat offers them. Reading
// it calls no model and writes nothing.
type ReloadOffer struct {
	// Estimate is the conversation's estimated input as its next call
	// resends it, by model.EstimatorVersion.
	Estimate int64
	// Ranked is how many ranked evidence items a restart carries.
	Ranked int
}

// ReloadOffer is what reloading the case's conversation would cost.
func (s *Session) ReloadOffer(ctx context.Context) (ReloadOffer, error) {
	var o ReloadOffer
	conv, err := companion.Store{DB: s.c.st.DB()}.Latest(ctx)
	if errors.Is(err, companion.ErrNoConversation) {
		return o, errNoConversation
	}
	if err != nil {
		return o, err
	}
	if o.Estimate, err = s.storeLoop().Estimate(ctx, conv.ID); err != nil {
		return o, err
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return o, err
	}
	_, _, o.Ranked, err = s.rankedLines(ctx, history)
	return o, err
}

// Reloaded is what a reload did.
type Reloaded struct {
	Mode ReloadMode
	// Conversation is the conversation the next turn continues: the same
	// one for ReloadFull, the new one otherwise.
	Conversation string
	// Estimate is that conversation's estimated input as its next call
	// sends it, by model.EstimatorVersion: for ReloadFull, what it resends
	// without the cache.
	Estimate int64
	// Ranked is how many ranked evidence items the new conversation was
	// given, and Handoff the model's handoff for ReloadSummary.
	Ranked  int
	Handoff string
}

// Reload goes on with the case's conversation in mode. ReloadFull changes
// nothing: the next call sends the conversation as it is. ReloadSuggested
// begins a new conversation linked to it as a compaction's is, from the
// case block as the case is now, the whole case ledger and what the
// engine carries from it — the engineer's items, the top of the ranked
// evidence as the lines cited or pointed at, and the last exchange — with
// no model call; the same inputs make the same bytes. ReloadSummary adds,
// before what is carried, the model's handoff of the old conversation, as
// a compaction asks for it: one call over it, given what the case gained
// first. on reports that call's events.
func (s *Session) Reload(ctx context.Context, mode ReloadMode, on func(TalkEvent)) (Reloaded, error) {
	out := Reloaded{Mode: mode}
	switch mode {
	case ReloadFull, ReloadSuggested, ReloadSummary:
	default:
		return out, fmt.Errorf("there is no reload %q: full, suggested or summary", mode)
	}
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	if errors.Is(err, companion.ErrNoConversation) {
		return out, errNoConversation
	}
	if err != nil {
		return out, err
	}
	out.Conversation = conv.ID
	l := s.storeLoop()
	if mode == ReloadFull {
		out.Estimate, err = l.Estimate(ctx, conv.ID)
		return out, err
	}
	if mode == ReloadSummary {
		if l, err = s.loop(ctx); err != nil {
			return out, err
		}
		if out.Conversation, err = s.conversation(ctx, l); err != nil {
			return out, err
		}
	}
	// What is carried is read before the handoff, whose ask and answer
	// are no part of the exchange it carries.
	carry, shows, n, err := s.carriedText(ctx)
	if err != nil {
		return out, err
	}
	msgs, err := st.Messages(ctx, out.Conversation)
	if err != nil {
		return out, err
	}
	unread := unreadVerdicts(msgs)
	if mode == ReloadSummary {
		if out.Handoff, err = l.Handoff(ctx, out.Conversation, on); err != nil {
			return out, err
		}
		carry = companion.HandoffText(out.Handoff) + "\n\n" + carry
	}
	next, err := l.Restart(ctx, out.Conversation, carry, shows...)
	if err != nil {
		return out, err
	}
	// A verdict carried shows the lines it quotes as it did.
	verdictShows, err := st.Shows(ctx, out.Conversation)
	if err != nil {
		return out, err
	}
	for _, verdict := range unread {
		if err := l.AddVerdict(ctx, next, verdict.Text, verdictShows[verdict.Seq]...); err != nil {
			return out, err
		}
	}
	out.Conversation, out.Ranked = next, n
	out.Estimate, err = l.Estimate(ctx, next)
	return out, err
}

// unreadVerdicts are the engineer's verdicts in msgs, a conversation's
// messages, that no model call has read yet, those after its last model
// message, in order: a restart carries them, so the next call reads them
// as it would have.
func unreadVerdicts(msgs []companion.Message) []companion.Message {
	var out []companion.Message
	for _, m := range msgs {
		switch m.Kind {
		case companion.KindModel:
			out = nil
		case companion.KindVerdict:
			out = append(out, m)
		}
	}
	return out
}

// carriedHeading begins what a restart carries from the earlier
// conversation, the engine's words.
const carriedHeading = "# Carried from the earlier conversation"

const carriedIntro = carriedHeading + `

This conversation continues an earlier one of this case, begun afresh from the case as it is now and the case ledger. The engine carried what follows from the earlier one, without a model: the engineer's items, the evidence it used most with the lines cited or pointed at, and its last exchange. Every evidence name it used still names the same item; read any with evidence_read.`

// carriedText is what a restart carries from the case's conversation and
// its compaction ancestors, the evidence lines it shows, and how many
// ranked items it holds.
func (s *Session) carriedText(ctx context.Context) (string, []companion.Shown, int, error) {
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return "", nil, 0, err
	}
	var b strings.Builder
	b.WriteString(carriedIntro)
	items, err := carried(ctx, s.c)
	if err != nil {
		return "", nil, 0, err
	}
	if items != "" {
		b.WriteString("\n\n" + items)
	}
	ranked, shows, n, err := s.rankedLines(ctx, history)
	if err != nil {
		return "", nil, 0, err
	}
	if ranked != "" {
		b.WriteString("\n\n" + ranked)
	}
	if last := lastExchange(history); last != "" {
		b.WriteString("\n\n" + last)
	}
	return b.String(), shows, n, nil
}

const rankedIntro = `# Ranked evidence

The evidence the earlier conversation used most, best first, as the engine ranks it from what was done with each — lines the engineer pointed at, cited in a pinned item or draft, cited in an answer, returned by a tool — and how recently. Each item with the lines cited or pointed at, numbered as stored, or its first lines where none was named.`

// rankedLines is the top of the ranked evidence of history as a restart
// carries it (carryRanked), each item under its name, type and why it
// ranked, the lines it shows, and how many items.
func (s *Session) rankedLines(ctx context.Context, history []ConversationMessage) (string, []companion.Shown, int, error) {
	ranked, err := s.rankHistory(ctx, history, 0)
	if err != nil || len(ranked) == 0 {
		return "", nil, 0, err
	}
	body, items, err := s.carryRanked(ctx, ranked, rankedCarry{
		head: func(r RankedEvidence, it EvidenceItem) string {
			return fmt.Sprintf("\n## %s · %s · %s\n\n", r.Name, it.Ref.Type, strings.Join(r.Why, ", "))
		},
		cut: "… cut here at the bound on what is carried; read on with evidence_read %s from line %d.\n",
	})
	if err != nil || len(items) == 0 {
		return "", nil, 0, err
	}
	var shows []companion.Shown
	for _, ci := range items {
		shows = append(shows, ci.shown...)
	}
	return rankedIntro + "\n" + strings.TrimRight(body, "\n"), shows, len(items), nil
}

// rankedCarry is how a carry of ranked evidence writes an item: head heads
// it; cut, given its name and the first line left out, ends the item the
// bound cuts.
type rankedCarry struct {
	head func(RankedEvidence, EvidenceItem) string
	cut  string
}

// carriedItem is a ranked item a carry wrote: its name, its kind
// (EvidenceKind), the lines it holds, and the lines of it given.
type carriedItem struct {
	name, kind string
	lines      int
	shown      []companion.Shown
}

// carryRanked writes the top of ranked as a restart carries it: at most
// carryRanked items, each under f.head, with the lines cited or pointed
// at, or its first carryHead lines where none was named, said so, and no
// more than carryBytes of them in all, the item that reaches the bound cut
// and said so (f.cut). Items the engineer's exclusions leave out of what
// models read are left out. It returns the text and the items written, one
// that could not be read said so and giving none of its lines.
func (s *Session) carryRanked(ctx context.Context, ranked []RankedEvidence, f rankedCarry) (string, []carriedItem, error) {
	out := withdrawn(s.c)
	var b strings.Builder
	var items []carriedItem
	room := carryBytes
	for _, r := range ranked {
		if len(items) == carryRanked || room <= 0 {
			break
		}
		if out(r.EvidenceID) {
			continue
		}
		it, err := s.c.ev.Describe(r.EvidenceID)
		if err != nil {
			return "", nil, err
		}
		head := f.head(r, it)
		ranges, first := r.Ranges, ""
		if len(ranges) == 0 {
			ranges, first = []LineRange{{From: 1, To: carryHead}}, fmt.Sprintf("Its first lines, of %s.\n", text.Plural(int(it.Size.Lines), "line"))
		}
		if len(head)+len(first) >= room {
			break
		}
		room -= len(head) + len(first)
		body, given, cut, err := carriedLines(ctx, s.c, r.EvidenceID, ranges, room)
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			body = "It could not be read: " + err.Error() + "\n"
		}
		b.WriteString(head + first + body)
		room -= len(body)
		items = append(items, carriedItem{name: r.Name, kind: EvidenceKind(it), lines: int(it.Size.Lines), shown: lineRanges(r.Name, given)})
		if cut > 0 {
			fmt.Fprintf(&b, f.cut, r.Name, cut)
			break
		}
	}
	return b.String(), items, nil
}

// carriedLines are the lines of item id in ranges, sorted and apart, each
// numbered as stored, "…" between ranges, up to room bytes, and the
// numbers of those given; cut is the first line left out for room, 0
// where none was.
func carriedLines(ctx context.Context, c *openCase, id string, ranges []LineRange, room int) (lines string, given []int, cut int, err error) {
	r, err := c.ev.Open(id)
	if err != nil {
		return "", nil, 0, err
	}
	defer r.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	width := len(strconv.Itoa(ranges[len(ranges)-1].To))
	var b strings.Builder
	k, prev := 0, 0
	for i := 1; ; i++ {
		for k < len(ranges) && i > ranges[k].To {
			k++
		}
		if k == len(ranges) {
			break
		}
		if err := ctx.Err(); err != nil {
			return "", nil, 0, err
		}
		line, rerr := text.ReadLine(br, lineMaxBytes)
		if rerr != nil && line == "" {
			break
		}
		if i >= ranges[k].From {
			l := fmt.Sprintf("%*d  %s\n", width, i, line)
			if prev > 0 && i > prev+1 {
				l = "…\n" + l
			}
			if b.Len()+len(l) > room {
				return b.String(), given, i, nil
			}
			b.WriteString(l)
			given = append(given, i)
			prev = i
		}
		if rerr != nil {
			break
		}
	}
	return b.String(), given, 0, nil
}

// lastExchange is the conversation's last exchange as a restart carries
// it, verbatim: the engineer's last message and the model's last answer
// as the engineer was shown it (AnswerViews), said to be the editor's
// correction where it is, or nothing where it has neither.
func lastExchange(history []ConversationMessage) string {
	var asked string
	for _, m := range history {
		if m.Kind == MessageEngineer {
			asked = m.Text
		}
	}
	var answer AnswerView
	if views := AnswerViews(history); len(views) > 0 {
		answer = views[len(views)-1]
	}
	if asked == "" && answer.Text == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("# The last exchange\n\nThe engineer's last message and the model's last answer in the earlier conversation, verbatim. The answer is the model's reading, not verified.")
	if asked != "" {
		b.WriteString("\n\n## The engineer\n\n" + strings.TrimSpace(asked))
	}
	switch {
	case answer.Corrected:
		b.WriteString("\n\n## The model, as the engine's editor corrected its wording\n\n" + strings.TrimSpace(answer.Text))
	case answer.Text != "":
		b.WriteString("\n\n## The model\n\n" + strings.TrimSpace(answer.Text))
	}
	return b.String()
}

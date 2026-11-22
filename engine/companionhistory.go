package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/invocations"
)

// ConversationMessage is a stored message with its conversation and the
// attribution of the model call that produced it, where there was one;
// for one of the model's answers the editor checked, what the check made
// of it; for a review, the review as the chat prints it; for an answer as
// the model first wrote it, the engineer's verdicts on its span flags.
type ConversationMessage struct {
	companion.Message
	ConversationID string
	Call           *invocations.Summary
	Check          *AnswerCheck
	Review         *Review
	Verdicts       []SpanVerdict
}

// Who wrote a ConversationMessage. History keeps internal messages too;
// the client chooses which kinds to display.
const (
	MessageEngineer   = companion.KindEngineer
	MessageModel      = companion.KindModel
	MessageResults    = companion.KindResults
	MessageHandoff    = companion.KindHandoff
	MessageEngine     = companion.KindEngine
	MessageCase       = companion.KindCase
	MessageUpdate     = companion.KindUpdate
	MessageConsult    = companion.KindConsult
	MessageCorrection = companion.KindCorrection
	MessageRevise     = companion.KindRevise
	MessageShown      = companion.KindShown
	// MessageReview is the reviewer's reading of an answer or an item:
	// never an answer, and never evidence.
	MessageReview = companion.KindReview
	// MessageVerdict is the engineer's verdict that a span of an answer is
	// unsupported or incorrect, as the model reads it: never an answer,
	// and never evidence.
	MessageVerdict = companion.KindVerdict
)

// IsAnswer reports whether m is one of the model's answers: its text with
// no tool call beside it.
func IsAnswer(m ConversationMessage) bool {
	return m.Kind == MessageModel && len(m.Calls) == 0 && strings.TrimSpace(m.Text) != ""
}

// Revision reports whether history[i] is the model's revision of the
// answer before it, which it answers the engine's revise message just
// before it with: shown, and named, in that answer's place, unless a
// message recording what was shown follows it (AnswerViews).
func Revision(history []ConversationMessage, i int) bool {
	return i > 0 && history[i-1].Kind == MessageRevise && IsAnswer(history[i])
}

// AnswerView is one of the model's answers as the engineer was shown it.
type AnswerView struct {
	// Label is the answer's name as the chat gives it, a1, a2, ….
	Label string
	// Text is the text shown; At the index in history of the message it
	// is from — the answer, its revision, the editor's correction or the
	// record of what was shown after a revision not taken — and Original
	// the index of the answer as the model first wrote it.
	Text         string
	At, Original int
	// Corrected says the text shown is the editor's correction, not the
	// model's own words.
	Corrected bool
	// Draft says the answer is a comment draft answering draftWords, and
	// DraftIntention is its intention ID (internal, update, kcs, …).
	Draft          bool
	DraftIntention string
}

// AnswerViews are history's answers as the engineer was shown them, in
// order, the one projection the chat, the editor's earlier answers, the
// ranked evidence and a reload's last exchange all read: each answer
// under its label, a revision, a correction or the record of what was
// shown taking its place.
func AnswerViews(history []ConversationMessage) []AnswerView {
	var out []AnswerView
	for i, m := range history {
		if text, ok := Replacement(m); ok {
			if len(out) > 0 {
				v := &out[len(out)-1]
				// What was shown after a revision not taken is the
				// editor's correction unless it is the answer as written.
				v.Text, v.At = text, i
				v.Corrected = m.Kind == MessageCorrection || strings.TrimSpace(text) != strings.TrimSpace(history[v.Original].Text)
			}
			continue
		}
		if !IsAnswer(m) {
			continue
		}
		if Revision(history, i) && len(out) > 0 {
			v := &out[len(out)-1]
			v.Text, v.At, v.Corrected = m.Text, i, false
			continue
		}
		var draft bool
		var intention string
		for j := i - 1; j >= 0; j-- {
			if prev := history[j]; prev.Kind == MessageEngineer || prev.Kind == MessageEngine {
				if prev.Kind == MessageEngine {
					intention, draft = draftIntentionOf(prev.Text)
				}
				break
			}
		}
		out = append(out, AnswerView{
			Label: "a" + strconv.Itoa(len(out)+1), Text: m.Text, At: i, Original: i,
			Draft: draft, DraftIntention: intention,
		})
	}
	return out
}

func draftIntentionOf(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, draftLead)
	if !ok {
		return "", false
	}
	head, _, ok := strings.Cut(rest, "), for ")
	if !ok {
		return "", true
	}
	if at := strings.LastIndexByte(head, '('); at >= 0 {
		return head[at+1:], true
	}
	return "", true
}

// Replacement is the text a message puts in place of the answer before
// it as the engineer was shown it — the editor's correction, or what was
// shown after a revision not taken — and false for any other message.
func Replacement(m ConversationMessage) (string, bool) { return companion.Replacement(m.Message) }

// ConversationHistory reads the main conversation and its compaction
// ancestors, oldest first, without starting a conversation or calling a
// model. Consult conversations are not included. Internal messages are
// retained so clients can choose their presentation without parsing prose.
func (s *Session) ConversationHistory(ctx context.Context) ([]ConversationMessage, error) {
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	if errors.Is(err, companion.ErrNoConversation) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lineage []companion.Conversation
	seen := map[string]bool{}
	for {
		if seen[conv.ID] {
			return nil, fmt.Errorf("companion: conversation history: cycle at %s", conv.ID)
		}
		if conv.Kind != "main" {
			return nil, fmt.Errorf("companion: conversation history: ancestor %s is not a main conversation", conv.ID)
		}
		seen[conv.ID] = true
		lineage = append(lineage, conv)
		if conv.CompactedFrom == "" {
			break
		}
		conv, err = st.Conversation(ctx, conv.CompactedFrom)
		if err != nil {
			return nil, err
		}
	}
	checks, err := answerChecks(ctx, s.c)
	if err != nil {
		return nil, err
	}
	reviewed, err := reviews(ctx, s.c)
	if err != nil {
		return nil, err
	}
	verdicts, err := spanVerdicts(ctx, s.c)
	if err != nil {
		return nil, err
	}
	var history []ConversationMessage
	for i := len(lineage) - 1; i >= 0; i-- {
		messages, err := st.Messages(ctx, lineage[i].ID)
		if err != nil {
			return nil, err
		}
		for j, message := range messages {
			entry := ConversationMessage{Message: message, ConversationID: lineage[i].ID, Verdicts: verdicts[answerKey{lineage[i].ID, j + 1}]}
			if message.Kind == MessageModel && message.Invocation != "" {
				summary, err := invocations.GetSummary(s.c.st.DB(), message.Invocation)
				if err != nil {
					return nil, err
				}
				entry.Call = &summary
				if k, ok := checks[message.Invocation]; ok {
					chk := k.of(message.Text)
					entry.Check = &chk
				}
			}
			if r, ok := reviewed[message.Invocation]; ok && message.Kind == MessageReview {
				entry.Review = &r
			}
			history = append(history, entry)
		}
	}
	// The citations a revision added are read from the revision as kept,
	// against the answer as written, the editor's corrected citations and
	// the lines its conversation was shown numbered before the answer, as
	// the turn found them.
	for _, v := range AnswerViews(history) {
		if c := history[v.Original].Check; c != nil && c.Revised && Revision(history, v.At) {
			conv := history[v.Original].ConversationID
			var before []companion.Message
			for _, m := range history[:v.Original] {
				if m.ConversationID == conv {
					before = append(before, m.Message)
				}
			}
			shown, err := shownLines(ctx, st, conv, before)
			if err != nil {
				return nil, err
			}
			c.Added = revisionAdded(history[v.Original].Text, v.Text, c.CorrectedCites, shown)
		}
		for k := range history[v.Original].Verdicts {
			history[v.Original].Verdicts[k].Answer = v.Label
		}
	}
	return history, nil
}

package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/companion"
)

// ErrNothingNew is a catch-up's answer where the case gained no comment or
// attachment the conversation has not been shown: no model is called.
var ErrNothingNew = errors.New("nothing new since you last worked this case")

// CatchUp asks the case's conversation what changed since the engineer
// last worked the case: the
// ledger reads the comments it has not, waited for up to the opening's
// bound; then one turn, on the opening's budget, begins with the case
// update and the ledger's update as every turn does, and the engine asks
// what the new comments and attachments, by name, change in the current
// understanding, briefly. on reports the turn's events and the ledger's
// calls. With nothing new it returns ErrNothingNew and calls no model.
func (s *Session) CatchUp(ctx context.Context, on func(TalkEvent)) (TalkReply, error) {
	if s.c.chat {
		return TalkReply{}, errors.New("a chat has no case to catch up on")
	}
	r, fresh, ok, err := s.reopenState(ctx)
	switch {
	case err != nil:
		return TalkReply{}, err
	case !ok:
		return TalkReply{}, errNoConversation
	case !r.Fresh():
		return TalkReply{}, ErrNothingNew
	}
	ledger := catchUpRead
	switch {
	case s.LedgerBlocked(ctx) != "":
		ledger = catchUpNoLedger
	case r.LedgerUnread > 0:
		reading, err := s.awaitLedger(ctx, on)
		if err != nil {
			return TalkReply{}, err
		}
		if reading {
			ledger = catchUpReading
		}
	}
	l, err := s.loop(ctx)
	if err != nil {
		return TalkReply{}, err
	}
	l.Budget = companion.OpeningBudget
	conv, err := s.conversation(ctx, l)
	if err != nil {
		return TalkReply{}, err
	}
	names := make([]string, 0, len(fresh))
	for _, id := range fresh {
		name, err := evidenceName(s.c, id)
		if err != nil {
			return TalkReply{}, err
		}
		names = append(names, name)
	}
	reply, err := l.Turn(ctx, conv, companion.KindEngine, catchUpAsk(r, names, ledger), on)
	if err != nil {
		return TalkReply{Reply: reply}, err
	}
	return s.edit(ctx, l, reply, on)
}

// awaitLedger has the ledger read the comments it has not and waits for it
// up to ledgerWait, telling on of its calls; reading says it was still
// reading when the wait ended. A failed run is said as a warning; the
// catch-up goes on with the comments themselves. Cancelling ctx cancels
// the run.
func (s *Session) awaitLedger(ctx context.Context, on func(TalkEvent)) (reading bool, err error) {
	calls := func(ev TalkEvent) {
		// Only the calls: the ledger's text and its conversation are its
		// own, not the turn's.
		if on != nil && ev.Kind == TalkCall {
			on(ev)
		}
	}
	j := s.StartLedger(ctx, calls, nil)
	wait := time.NewTimer(ledgerWait)
	defer wait.Stop()
	select {
	case <-j.Done():
	case <-wait.C:
		return true, nil
	case <-ctx.Done():
		j.Cancel()
		<-j.Done()
		return false, ctx.Err()
	}
	if _, err := j.Result(); err != nil && on != nil {
		on(TalkEvent{Kind: TalkWarning, Text: "the ledger could not read the new comments: " + err.Error() + "; the catch-up goes on from the comments themselves"})
	}
	return false, nil
}

// What a catch-up's ask says of the case ledger: it has read the new
// comments, is still reading them, or cannot run.
const (
	catchUpRead = iota
	catchUpReading
	catchUpNoLedger
)

// catchUpAsk is the engine's words asking for a catch-up on r's news: the
// new comments by name, the new attachments by theirs.
func catchUpAsk(r ReopenState, comments []string, ledger int) string {
	var news []string
	if len(comments) > 0 {
		news = append(news, text.Plural(len(comments), "new comment")+", "+strings.Join(comments, ", "))
	}
	switch {
	case len(r.NewAttachments) > 0:
		news = append(news, text.Plural(len(r.NewAttachments), "new attachment")+", "+strings.Join(r.NewAttachments, ", "))
	case r.Attachments:
		news = append(news, "a change in the attachments' inventory or listing")
	}
	var b strings.Builder
	b.WriteString(catchUpLead + " Since they last worked this case (" + r.Since.UTC().Format("2006-01-02 15:04 UTC") + "), it gained " +
		strings.Join(news, "; and ") + ". This conversation has been given them")
	switch ledger {
	case catchUpRead:
		b.WriteString(", with the case ledger's reading of the comments")
	case catchUpReading:
		b.WriteString("; the case ledger is still reading the new comments and follows")
	}
	b.WriteString(". Say briefly what changed and whether it answers or opens any question; read further only where something new needs it.")
	return b.String()
}

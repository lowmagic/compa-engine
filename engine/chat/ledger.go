package chat

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// ledger shows the case ledger, as a turn: the comments the ledger has not
// read read first, where there are any, its model's calls shown as a
// turn's; with none, no model is called. A failed read is said, and the
// ledger shown as it is.
func (m *model) ledger() {
	if m.s.IsChat() {
		m.print(errorLine("a chat has no case, so no comments and no ledger"))
		return
	}
	s := m.s
	m.startTurn(kindLedger, "reading the ledger", func(ctx context.Context, on func(engine.TalkEvent), send func(tea.Msg)) turnDone {
		n, err := s.LedgerUnread(ctx)
		if err != nil {
			return turnDone{err: err}
		}
		if n > 0 {
			send(stateMsg{"the ledger is reading " + text.Plural(n, "comment")})
			if run, err := s.UpdateLedger(ctx, on); err != nil {
				if ctx.Err() != nil {
					return turnDone{err: ctx.Err()}
				}
				send(noteMsg{warnLine(err.Error() + "; the ledger is shown as it was")})
			} else if len(run.Dropped) > 0 {
				send(noteMsg{warnLine("the ledger's update had " + text.Plural(len(run.Dropped), "element") + " dropped as invalid, the rest kept")})
			}
		}
		items, err := s.Ledger(ctx)
		if err != nil {
			return turnDone{err: err}
		}
		comments, err := s.LedgerComments(ctx)
		if err != nil {
			return turnDone{err: err}
		}
		return turnDone{ledger: engine.RenderLedger(comments, items, time.Local, m.s.SupportName(), m.s.SupportRole())}
	})
}

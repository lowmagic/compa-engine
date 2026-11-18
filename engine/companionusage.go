package engine

import "github.com/lowmagic/compa-engine/internal/invocations"

// CompanionTotalUsage reads every model call stored for this case or chat:
// the companion's, including consults, revisions and conversations
// replaced by compaction, the case ledger's, the editor's, the reviewer's
// and every call resent. It neither starts a conversation nor calls a
// provider. Cache counts are historical provider observations, not a claim
// that a cache is still live.
func (s *Session) CompanionTotalUsage() (UsageAggregate, error) {
	return invocations.TotalTaskUsage(s.c.st.DB(), s.c.id, companionTask, LedgerTask, EditorTask, ReviewTask, ResendTask)
}

package chat

import "github.com/lowmagic/compa-engine/engine"

// usageMsg is a read-only snapshot of the case or chat's stored calls.
// Reads triggered by successive calls may finish out of order.
type usageMsg struct {
	total engine.UsageAggregate
	err   error
}

func (m *model) applyUsage(snapshot usageMsg) {
	if snapshot.err != nil {
		m.print(warnLine("saved model usage could not be read: " + snapshot.err.Error()))
		return
	}
	if snapshot.total.Invocations >= m.usage.Invocations {
		m.usage = snapshot.total
	}
}

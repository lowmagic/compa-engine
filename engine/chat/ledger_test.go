package chat

import (
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
)

// TestLedgerCommand: /ledger has the ledger read the comments it has not,
// its call counted in the status bar's total like any other, and prints
// the ledger; with nothing unread, /ledger makes no call and prints it
// again.
func TestLedgerCommand(t *testing.T) {
	s := session(t, "ledger.json")
	h := start(t, s, Options{NoOpen: true})
	h.say("/ledger")
	out := flat(h.out())
	for _, want := range []string{"Case ledger", "Awaiting customer:", "[ask] Send a fresh must-gather. (c#3)", "[report] A synthetic report: it started after the upgrade."} {
		if !strings.Contains(out, want) {
			t.Errorf("/ledger printed no %q:\n%s", want, h.out())
		}
	}
	if len(h.m.records) != 1 || h.m.records[0].TaskType != engine.LedgerTask || h.m.usage.Invocations != 1 {
		t.Fatalf("the ledger's call: %d records, the total counting %d", len(h.m.records), h.m.usage.Invocations)
	}
	mark := len(h.printed)
	h.say("/ledger")
	if again := flat(h.since(mark)); !strings.Contains(again, "Send a fresh must-gather.") {
		t.Errorf("the second /ledger printed:\n%s", again)
	}
	total, err := s.CompanionTotalUsage()
	if err != nil || total.Invocations != 1 || len(h.m.records) != 1 {
		t.Errorf("the second /ledger called the model: %d stored calls, %d this run, %v", total.Invocations, len(h.m.records), err)
	}
}

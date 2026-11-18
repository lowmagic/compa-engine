package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
)

func TestStatusBarAboveEntry(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.typed("/e")
	v := h.m.View()
	rows := strings.Split(ansi.Strip(v.Content), "\n")
	entry := -1
	for i, r := range rows {
		if strings.HasPrefix(r, "› /e") {
			entry = i
		}
	}
	if entry < 1 || !strings.Contains(rows[entry-1], "case 00000001") {
		t.Errorf("status bar is not directly above the entry:\n%s", ansi.Strip(v.Content))
	}
	if !strings.Contains(strings.Join(rows[:entry-1], "\n"), "/evidence") {
		t.Errorf("command options did not appear above the bar:\n%s", ansi.Strip(v.Content))
	}
	if len(rows) >= h.m.height {
		t.Errorf("popover consumed the reserved scrollback row: %d rows in height %d", len(rows), h.m.height)
	}
}

func TestActivityShowsCancellationAndStopsWhenIdle(t *testing.T) {
	h := start(t, session(t, "slow.json"), Options{NoOpen: true})
	h.typed("go slowly")
	h.press("enter")
	h.until("streaming", func() bool { return h.m.turn != nil && h.m.turn.live.Len() > 0 })
	first := h.m.activity.View()
	if !strings.Contains(h.view(), "ctrl+c to stop") {
		t.Fatalf("running call has no cancellation hint:\n%s", h.view())
	}
	h.until("animated activity", func() bool { return h.m.activity.View() != first })
	h.press("ctrl+c")
	h.settle()
	if h.quit || h.m.turn != nil || h.m.spinning {
		t.Fatalf("cancelling left quit=%v turn=%v spinning=%v", h.quit, h.m.turn, h.m.spinning)
	}
	if view := h.view(); strings.Contains(view, "ctrl+c to stop") || !strings.Contains(view, "waiting for you") {
		t.Errorf("idle status still claims a running call:\n%s", view)
	}
}

func TestCumulativeSpendStaysVisibleBesideLongStatus(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.m.marks = []string{"e1:1-10", "e2:20-30", "e3:40-50", "e4:60-70"}
	h.m.usage = engine.UsageAggregate{Invocations: 1}
	h.m.usage.Cost.Estimated = map[string]float64{"USD": 1.25}
	bar := ansi.Strip(h.m.statusLine())
	if !strings.Contains(bar, "$1.25 est") || !strings.Contains(bar, "total") || ansi.StringWidth(bar) > h.m.width {
		t.Errorf("long status hides cumulative spend or exceeds the terminal:\n%s", bar)
	}
}

func TestIncompletePromptCountsDoNotInventCacheShare(t *testing.T) {
	reported := func(n int64) engine.TokenCount {
		return engine.TokenCount{Value: n, Known: true, Fidelity: engine.FidelityReported}
	}
	for _, usage := range []engine.TokenUsage{
		{InputTokens: reported(100), CacheReadTokens: reported(900)},
		{CacheReadTokens: reported(900), CacheWriteTokens: reported(0)},
	} {
		total := engine.UsageOf(engine.InvocationRecord{Usage: usage})
		shown := usageShown(total)
		if strings.Contains(shown, "%") || !strings.Contains(shown, "900 cached tok") {
			t.Errorf("incomplete prompt totals invented a percentage or hid the measured cache reads: %s", shown)
		}
	}
}

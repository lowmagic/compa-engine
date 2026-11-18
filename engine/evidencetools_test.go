package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/collect"
)

// toolRun runs one evidence tool over a case as an ask would, with what
// the engineer left out refused.
func toolRun(t *testing.T, c *openCase, test string, params map[string]any) collect.Record {
	t.Helper()
	rec, _ := evidenceToolsFor(c, withdrawn(c))[test](context.Background(), params)
	return rec
}

// ingestText stores text as ingested evidence and returns its ID.
func ingestText(t *testing.T, c *openCase, location, text string) string {
	t.Helper()
	it, _, err := ingest(c, strings.NewReader(text), location, "")
	if err != nil {
		t.Fatal(err)
	}
	return it.Ref.ID
}

// The evidence tools list, read and search the case's stored evidence,
// within their bounds, each result naming the items it read.
func TestEvidenceTools(t *testing.T) {
	_, c, ids := summaryCase(t)
	var log strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&log, "10:%02d:%02d kubelet line %d\n", i/60, i%60, i)
	}
	logID := ingestText(t, c, "kubelet.log", log.String())
	other := ingestText(t, c, "csi.log", "10:00:05 attach volume pvc-1\n10:00:06 attach failed: timed out\n")

	// list: newest first, filtered by type and by source or location.
	rec := toolRun(t, c, toolList, map[string]any{})
	out := string(rec.Output)
	if rec.Status != collect.StatusOK || !strings.HasPrefix(out, "6 items match, newest first") || strings.Index(out, other) > strings.Index(out, logID) {
		t.Errorf("list:\n%s", out)
	}
	if out := string(toolRun(t, c, toolList, map[string]any{"type": TypeCaseComment}).Output); !strings.HasPrefix(out, "2 items match") || !strings.Contains(out, ids[1]) || strings.Contains(out, logID) {
		t.Errorf("list by type:\n%s", out)
	}
	if out := string(toolRun(t, c, toolList, map[string]any{"contains": "csi.log"}).Output); !strings.HasPrefix(out, "1 item match") || !strings.Contains(out, other) {
		t.Errorf("list by location:\n%s", out)
	}

	// read: numbered lines from where asked; past the end, nothing.
	rec = toolRun(t, c, toolRead, map[string]any{"id": logID, "from": float64(500), "lines": float64(3)})
	want := logID + " (ingested, 1000 lines): lines 500–502\n500  10:08:20 kubelet line 500\n501  10:08:21 kubelet line 501\n502  10:08:22 kubelet line 502\n"
	if string(rec.Output) != want || rec.Truncated || strings.Join(rec.PathsRead, " ") != logID || rec.What.TestID != toolRead {
		t.Errorf("read:\n%s\nwant\n%s", rec.Output, want)
	}
	if out := string(toolRun(t, c, toolRead, map[string]any{"id": logID, "from": float64(1001)}).Output); !strings.Contains(out, "nothing from line 1001") {
		t.Errorf("read past the end: %q", out)
	}
	if rec := toolRun(t, c, toolRead, map[string]any{"id": "ev-0000000000000000"}); rec.Status != collect.StatusUnavailable || rec.Reason != "no evidence ev-0000000000000000" {
		t.Errorf("read of nothing: %s %q", rec.Status, rec.Reason)
	}
	long := ingestText(t, c, "long.log", strings.Repeat(strings.Repeat("x", 1500)+"\n", 60))
	if rec := toolRun(t, c, toolRead, map[string]any{"id": long, "lines": float64(60)}); !rec.Truncated || !strings.Contains(string(rec.Output), "cut at 64 KiB, read on from line") {
		t.Errorf("a read over 64 KiB is not cut: truncated %v, %d bytes", rec.Truncated, len(rec.Output))
	}

	// grep: matches in several items, with context, gaps marked, the
	// matches capped.
	rec = toolRun(t, c, toolGrep, map[string]any{"pattern": `line (100|101|110)$|attach failed`, "ids": logID + " " + other, "context": float64(1)})
	out = string(rec.Output)
	for _, w := range []string{
		"== " + logID + " (ingested, 1000 lines): 3 matches\n99- 10:01:39 kubelet line 99\n100: 10:01:40 kubelet line 100\n101: 10:01:41 kubelet line 101\n102- 10:01:42 kubelet line 102\n--\n109- 10:01:49 kubelet line 109\n110: 10:01:50 kubelet line 110\n111- 10:01:51 kubelet line 111\n",
		"== " + other + " (ingested, 2 lines): 1 match\n1- 10:00:05 attach volume pvc-1\n2: 10:00:06 attach failed: timed out\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("grep lacks\n%s\nin\n%s", w, out)
		}
	}
	if rec.Truncated || len(rec.PathsRead) != 2 {
		t.Errorf("grep read %v, truncated %v", rec.PathsRead, rec.Truncated)
	}
	if rec := toolRun(t, c, toolGrep, map[string]any{"pattern": "kubelet", "ids": logID, "max": float64(5)}); !rec.Truncated || strings.Count(string(rec.Output), ": 10:") != 5 {
		t.Errorf("grep past its limit:\n%s", rec.Output)
	}
	if rec := toolRun(t, c, toolGrep, map[string]any{"pattern": "(", "ids": logID}); rec.Status != collect.StatusUnavailable || !strings.Contains(rec.Reason, "does not compile") {
		t.Errorf("a bad pattern: %s %q", rec.Status, rec.Reason)
	}
}

// What the engineer left out of what models read is out of the evidence
// tools' reach, directly and through grep, and missing from the list; a
// result read from an item before it was left out is withdrawn with it,
// and so is a result read from that result.
func TestEvidenceToolsHonourExclusions(t *testing.T) {
	_, c, ids := summaryCase(t)
	c1, c2 := ids[1], ids[2]
	read := toolRun(t, c, toolRead, map[string]any{"id": c2})
	if err := c.ev.Put(read); err != nil {
		t.Fatal(err)
	}
	again := toolRun(t, c, toolGrep, map[string]any{"pattern": "must-gather", "ids": read.ID})
	if err := c.ev.Put(again); err != nil {
		t.Fatal(err)
	}
	if out := withdrawn(c); out(read.ID) || out(again.ID) {
		t.Fatal("results withdrawn before any rule")
	}

	if _, err := excludeComments(c, ExcludeComment, "c1"); err != nil {
		t.Fatal(err)
	}
	if rec := toolRun(t, c, toolRead, map[string]any{"id": c1}); rec.Status != collect.StatusUnavailable || !strings.Contains(rec.Reason, "left out of what models read") || strings.Contains(string(rec.Output), "Started after") {
		t.Errorf("read of a comment left out: %s %q %q", rec.Status, rec.Reason, rec.Output)
	}
	rec := toolRun(t, c, toolGrep, map[string]any{"pattern": ".", "ids": c1 + " " + c2})
	if strings.Contains(string(rec.Output), "Started after") || !strings.Contains(string(rec.Output), "Asked for a must-gather") || strings.Join(rec.PathsRead, " ") != c2 {
		t.Errorf("grep over a comment left out:\n%s", rec.Output)
	}
	if rec := toolRun(t, c, toolGrep, map[string]any{"pattern": ".", "ids": c1}); rec.Status != collect.StatusUnavailable {
		t.Errorf("grep over only a comment left out: %s", rec.Status)
	}
	if out := string(toolRun(t, c, toolList, map[string]any{}).Output); strings.Contains(out, c1) || !strings.Contains(out, c2) {
		t.Errorf("the list shows a comment left out:\n%s", out)
	}

	if _, err := excludeComments(c, ExcludeComment, "c2"); err != nil {
		t.Fatal(err)
	}
	out := withdrawn(c)
	if !out(read.ID) || !out(again.ID) {
		t.Errorf("results read from a comment left out are not withdrawn: read %v, read of the read %v", out(read.ID), out(again.ID))
	}
	if rec := toolRun(t, c, toolRead, map[string]any{"id": read.ID}); rec.Status != collect.StatusUnavailable {
		t.Errorf("a read of a withdrawn result: %s", rec.Status)
	}
}

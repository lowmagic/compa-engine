package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logRoot is an artifact root holding one file, log, with the given lines.
func logRoot(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func window(t *testing.T, root string, params map[string]any) Record {
	t.Helper()
	p := map[string]any{"path": "log"}
	for k, v := range params {
		p[k] = v
	}
	return run(t, root, prim("logs.window", p))
}

// The journal and syslog form, Mon DD HH:MM:SS with a space-padded day and
// no year, three fields of the line, dated from the capture's anchor.
func TestWindowYearlessFromAnchor(t *testing.T) {
	root := logRoot(t,
		"Oct  2 23:59:59 node1 kernel: before",
		"Oct  3 00:00:00 node1 kernel: oom-kill: first",
		"  continuation of the first",
		"Oct  3 00:30:00.123456 node1 kernel: second",
		"Oct 13 00:00:00 node1 kernel: after",
	)
	got := mustOK(t, window(t, root, map[string]any{"layout": "Jan _2 15:04:05", "anchor": "2026-10-21T08:00:00Z", "start": "2026-10-03T00:00:00Z", "end": "2026-10-03T01:00:00Z"}))
	if want := "Oct  3 00:00:00 node1 kernel: oom-kill: first\n  continuation of the first\nOct  3 00:30:00.123456 node1 kernel: second\n"; got != want {
		t.Errorf("got\n%s", got)
	}
	// Without an anchor a yearless stamp is in year 0, as before.
	if got := mustOK(t, window(t, root, map[string]any{"layout": "Jan _2 15:04:05", "start": "2026-10-03T00:00:00Z", "end": "2026-10-03T01:00:00Z"})); got != "" {
		t.Errorf("no anchor: got\n%s", got)
	}
}

// A December line before a January anchor belongs to the year before, and
// a line a few hours after the anchor, written while capturing, to its
// year; out of order, each line is dated alone.
func TestWindowCrossesTheYear(t *testing.T) {
	root := logRoot(t,
		"Jan  2 09:00:00 host app: during capture",
		"Dec 31 23:00:00 host app: new year's eve",
		"Jan  1 01:00:00 host app: new year",
		"Dec 30 12:00:00 host app: earlier",
	)
	p := map[string]any{"layout": "Jan _2 15:04:05", "anchor": "2027-01-02T08:00:00Z"}
	p["start"], p["end"] = "2026-12-31T00:00:00Z", "2027-01-01T02:00:00Z"
	if got, want := mustOK(t, window(t, root, p)), "Dec 31 23:00:00 host app: new year's eve\nJan  1 01:00:00 host app: new year\n"; got != want {
		t.Errorf("got\n%s", got)
	}
	p["start"], p["end"] = "2027-01-02T08:30:00Z", "2027-01-02T09:30:00Z"
	if got := mustOK(t, window(t, root, p)); got != "Jan  2 09:00:00 host app: during capture\n" {
		t.Errorf("a line just after the anchor: got\n%s", got)
	}
}

// A zone applies to stamps whose layout has none, by IANA name or offset;
// a stamp with its own offset keeps it.
func TestWindowZone(t *testing.T) {
	root := logRoot(t, "2026-10-21 10:00:00,123 INFO nova.compute: local ten")
	for _, z := range []string{"Europe/Prague", "+02:00", "+0200"} {
		p := map[string]any{"layout": "2006-01-02 15:04:05", "zone": z, "start": "2026-10-21T08:00:00Z", "end": "2026-10-21T08:00:01Z"}
		if got := mustOK(t, window(t, root, p)); got == "" {
			t.Errorf("zone %s: 10:00 local is not 08:00 UTC", z)
		}
	}
	if got := mustOK(t, window(t, root, map[string]any{"layout": "2006-01-02 15:04:05", "start": "2026-10-21T10:00:00Z", "end": "2026-10-21T10:00:01Z"})); got == "" {
		t.Error("with no zone a zoneless stamp is UTC")
	}
	offset := logRoot(t, "2026-10-21T10:00:00.000+0200 7f3a INFO dnf: offset")
	p := map[string]any{"layout": "2006-01-02T15:04:05.000-0700", "zone": "America/New_York", "start": "2026-10-21T08:00:00Z", "end": "2026-10-21T08:00:01Z"}
	if got := mustOK(t, window(t, offset, p)); got == "" {
		t.Error("a stamp's own offset was overridden by the zone")
	}
}

// klog's four level letters are skipped; a bracket before a stamp is
// skipped, and the one after it need not be in the layout.
func TestWindowSkip(t *testing.T) {
	root := logRoot(t,
		"I1021 10:00:00.000001    1234 server.go:10] info",
		"W1021 10:00:01.000001    1234 server.go:11] warning",
		"E1021 10:00:02.000001    1234 server.go:12] error",
		"F1021 10:00:03.000001    1234 server.go:13] fatal",
		"I1021 10:00:04.000001    1234 server.go:14] outside",
	)
	got := mustOK(t, window(t, root, map[string]any{"layout": "0102 15:04:05.000000", "skip": 1, "anchor": "2026-10-21T12:00:00Z", "start": "2026-10-21T10:00:00Z", "end": "2026-10-21T10:00:04Z"}))
	if strings.Count(got, "\n") != 4 || strings.Contains(got, "outside") {
		t.Errorf("klog: got\n%s", got)
	}
	bracketed := logRoot(t, "[2026-10-21 10:00:00,123] INFO ceph_volume.process: in", "[2026-10-21 11:00:00,123] INFO ceph_volume.process: out")
	got = mustOK(t, window(t, bracketed, map[string]any{"layout": "2006-01-02 15:04:05,000", "skip": 1, "start": "2026-10-21T10:00:00Z", "end": "2026-10-21T10:30:00Z"}))
	if !strings.Contains(got, ": in") || strings.Contains(got, ": out") {
		t.Errorf("bracketed: got\n%s", got)
	}
}

// A file none of whose lines the layout reads fails the stage, saying so,
// rather than returning what looks like a quiet window; an empty file is
// an empty window.
func TestWindowNoMatchingLine(t *testing.T) {
	root := logRoot(t, "Oct 21 10:00:00 host app: syslog form", "Oct 21 10:00:01 host app: again")
	rec := window(t, root, map[string]any{"start": "2026-10-21T00:00:00Z", "end": "2026-10-22T00:00:00Z"})
	if rec.Status != StatusFailed || !strings.Contains(rec.Reason, "no line of log has a stamp any default layout reads") {
		t.Errorf("default layouts: %s %q", rec.Status, rec.Reason)
	}
	rec = window(t, root, map[string]any{"layout": "2006-01-02 15:04:05", "start": "2026-10-21T00:00:00Z", "end": "2026-10-22T00:00:00Z"})
	if rec.Status != StatusFailed || !strings.Contains(rec.Reason, `layout "2006-01-02 15:04:05"`) {
		t.Errorf("given layout: %s %q", rec.Status, rec.Reason)
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "log"), nil, 0o644)
	if got := mustOK(t, window(t, empty, map[string]any{"start": "2026-10-21T00:00:00Z", "end": "2026-10-22T00:00:00Z"})); got != "" {
		t.Errorf("empty file: %q", got)
	}
}

func TestWindowRejectsBadParameters(t *testing.T) {
	root := logRoot(t, "2026-10-21T10:00:00Z line")
	for name, p := range map[string]map[string]any{
		"skip":   {"skip": -1},
		"anchor": {"anchor": "21 Oct 2026"},
		"zone":   {"zone": "Mars/Olympus"},
	} {
		p["start"], p["end"] = "2026-10-21T00:00:00Z", "2026-10-22T00:00:00Z"
		if rec := window(t, root, p); rec.Status != StatusFailed || !strings.Contains(rec.Reason, name) {
			t.Errorf("%s: %s %q", name, rec.Status, rec.Reason)
		}
	}
}

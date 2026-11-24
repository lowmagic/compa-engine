package collect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// logs writes nodes a to e, each a journal of 100 lines in which every
// tenth is an OOM kill, numbered across the files.
func logs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	k := 0
	for _, node := range []string{"node-a", "node-b", "node-c", "node-d", "node-e"} {
		var b strings.Builder
		for i := 1; i <= 100; i++ {
			if i%10 == 0 {
				k++
				fmt.Fprintf(&b, "kernel: oom-kill %d\n", k)
			} else {
				fmt.Fprintf(&b, "ok %s %d\n", node, i)
			}
		}
		os.MkdirAll(filepath.Join(root, node), 0o755)
		os.WriteFile(filepath.Join(root, node, "journal.log"), []byte(b.String()), 0o644)
	}
	return root
}

// From end keeps the last max_matches matches of the whole input,
// across files in the order they are read, each with its context, in
// order; the earlier ones left out mark the result truncated, and its stop
// says how many were kept of how many.
func TestGrepFromEnd(t *testing.T) {
	root := logs(t)
	rec := run(t, root, prim("grep", map[string]any{"pattern": "oom-kill", "glob": "*.log", "max_matches": 3, "from": "end", "context_before": 1}))
	want := "node-e/journal.log-79-ok node-e 79\nnode-e/journal.log:80:kernel: oom-kill 48\n" +
		"node-e/journal.log-89-ok node-e 89\nnode-e/journal.log:90:kernel: oom-kill 49\n" +
		"node-e/journal.log-99-ok node-e 99\nnode-e/journal.log:100:kernel: oom-kill 50\n"
	if got := mustOK(t, rec); got != want {
		t.Errorf("got\n%s", got)
	}
	if !rec.Truncated || rec.Stopped == nil || !strings.Contains(rec.Stopped.Summary(), "kept the last 3 of 50 matches") {
		t.Errorf("truncated %v, stop %q", rec.Truncated, rec.Stopped.Summary())
	}
	// Across files: the last matches of the whole input.
	got := mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "oom-kill (9|10|11|12)$", "glob": "*.log", "max_matches": 3, "from": "end"})))
	if got != "node-a/journal.log:100:kernel: oom-kill 10\nnode-b/journal.log:10:kernel: oom-kill 11\nnode-b/journal.log:20:kernel: oom-kill 12\n" {
		t.Errorf("across files: got\n%s", got)
	}
	// All of them, when there are no more than max_matches: complete.
	rec = run(t, root, prim("grep", map[string]any{"pattern": "oom-kill 5", "glob": "*.log", "from": "end"}))
	if rec.Truncated || rec.Stopped != nil || strings.Count(string(rec.Output), "\n") != 2 {
		t.Errorf("complete: truncated %v stop %+v\n%s", rec.Truncated, rec.Stopped, rec.Output)
	}
	if r := run(t, root, prim("grep", map[string]any{"pattern": "x", "path": "node-a/journal.log", "from": "middle"})); r.Status == StatusOK {
		t.Error("from middle accepted")
	}
}

// What it holds stays within the output cap: with many matches and a
// small cap, the last are kept and printed, and the output fits.
func TestGrepFromEndHoldsWithinTheCap(t *testing.T) {
	root := logs(t)
	r := &Runner{Limits: Limits{OutputBytes: 200}}
	rec := r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{prim("grep", map[string]any{"pattern": "oom-kill", "glob": "*.log", "from": "end"})}}})
	out := string(rec.Output)
	if len(out) > 200 || !strings.HasSuffix(out, "node-e/journal.log:100:kernel: oom-kill 50\n") || !rec.Truncated {
		t.Errorf("%d bytes, truncated %v:\n%s", len(out), rec.Truncated, out)
	}
}

// From end with keep emits each surviving file's leading lines once in
// order before its kept matches, drops headers for files whose matches
// were evicted, and gives a kept line ':' only when it is itself a
// surviving match.
func TestGrepKeepFromEnd(t *testing.T) {
	root := t.TempDir()
	fileA := "hdr-a-1\nhdr-a-2\nhit-a-1\nok\nhit-a-2\n"
	fileB := "hdr-b-1\nhit-b-hdr\nhdr-b-3\nok\nhit-b-1\nhit-b-2\n"
	for name, body := range map[string]string{"a.txt": fileA, "b.txt": fileB} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Only b.txt's last 2 matches survive: a.txt's header is evicted,
	// and b.txt line 2 (which matched earlier but was evicted) keeps '-'.
	rec := run(t, root, prim("grep", map[string]any{"pattern": "hit", "glob": "*.txt", "max_matches": 2, "from": "end", "keep": 3}))
	want := "b.txt-1-hdr-b-1\n" +
		"b.txt-2-hit-b-hdr\n" +
		"b.txt-3-hdr-b-3\n" +
		"b.txt:5:hit-b-1\n" +
		"b.txt:6:hit-b-2\n"
	if got := mustOK(t, rec); got != want {
		t.Errorf("evicted earlier file and match: got\n%s\nwant\n%s", got, want)
	}
	if !rec.Truncated || rec.Stopped == nil || !strings.Contains(rec.Stopped.Summary(), "kept the last 2 of 5 matches") {
		t.Errorf("truncated %v, stop %q", rec.Truncated, rec.Stopped.Summary())
	}

	// When a match inside 1..keep survives, it prints once in order with ':'.
	got := mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "hit", "path": "b.txt", "max_matches": 3, "from": "end", "keep": 3, "context_before": 1})))
	want = "b.txt-1-hdr-b-1\n" +
		"b.txt:2:hit-b-hdr\n" +
		"b.txt-3-hdr-b-3\n" +
		"b.txt-4-ok\n" +
		"b.txt:5:hit-b-1\n" +
		"b.txt:6:hit-b-2\n"
	if got != want {
		t.Errorf("surviving kept match: got\n%s\nwant\n%s", got, want)
	}

	// Across two surviving files, each gets its own header before its matches.
	got = mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "hit", "glob": "*.txt", "max_matches": 4, "from": "end", "keep": 2})))
	want = "a.txt-1-hdr-a-1\n" +
		"a.txt-2-hdr-a-2\n" +
		"a.txt:5:hit-a-2\n" +
		"b.txt-1-hdr-b-1\n" +
		"b.txt:2:hit-b-hdr\n" +
		"b.txt:5:hit-b-1\n" +
		"b.txt:6:hit-b-2\n"
	if got != want {
		t.Errorf("two surviving files: got\n%s\nwant\n%s", got, want)
	}

	// Stays within the output cap when headers and matches compete for room.
	r := &Runner{Limits: Limits{OutputBytes: 120}}
	rec = r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{
		prim("grep", map[string]any{"pattern": "hit", "glob": "*.txt", "from": "end", "keep": 2}),
	}}})
	out := string(rec.Output)
	if len(out) > 120 || !strings.HasPrefix(out, "b.txt-1-hdr-b-1\n") || !strings.HasSuffix(out, "b.txt:6:hit-b-2\n") || !rec.Truncated {
		t.Errorf("cap: %d bytes, truncated %v:\n%s", len(out), rec.Truncated, out)
	}
}

// A file read only to the read bound is never read to its end: its last
// matches are those of what was read, and the stop says so.
func TestGrepFromEndAtTheReadBound(t *testing.T) {
	root := logs(t)
	r := &Runner{Limits: Limits{MaxFileBytes: 300}}
	rec := r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{prim("grep", map[string]any{"pattern": "oom-kill", "path": "node-a/journal.log", "max_matches": 1, "from": "end"})}}})
	if !rec.Truncated || rec.Stopped == nil || !strings.Contains(rec.Stopped.Summary(), "node-a/journal.log was read only to its first 300 bytes") || strings.Contains(string(rec.Output), "oom-kill 10") {
		t.Errorf("stop %q\n%s", rec.Stopped.Summary(), rec.Output)
	}
}

// A search that stops at its match limit says where it stopped and names
// the files it never reached; one that times out says so, with the same.
func TestStopsSayHowFarTheyGot(t *testing.T) {
	root := logs(t)
	rec := run(t, root, prim("grep", map[string]any{"pattern": "oom-kill", "glob": "*.log", "max_matches": 12}))
	s := rec.Stopped
	if s == nil || s.By != "match_limit" || s.At != "node-b/journal.log at line 30" || s.FilesGiven != 5 || s.FilesRead != 2 ||
		strings.Join(s.NotReached, " ") != "node-c/journal.log node-d/journal.log node-e/journal.log" {
		t.Fatalf("stop %+v", s)
	}
	if got := s.Summary(); got != "stopped: reached its match limit in node-b/journal.log at line 30; not reached: 3 of 5 files (node-c/journal.log, node-d/journal.log, node-e/journal.log)" {
		t.Errorf("summary %q", got)
	}
	if got := s.Short(); got != "stopped at the match limit · 3 of 5 files not read" {
		t.Errorf("short %q", got)
	}
	// Everything read: no stop.
	if rec := run(t, root, prim("grep", map[string]any{"pattern": "oom-kill", "glob": "*.log"})); rec.Stopped != nil || rec.Truncated {
		t.Errorf("a complete search stopped: %+v", rec.Stopped)
	}
	// Timed out: failed, and the stop names what was not reached.
	r := &Runner{Limits: Limits{Timeout: time.Nanosecond}}
	rec = r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{prim("grep", map[string]any{"pattern": "oom-kill", "glob": "*.log"})}}})
	if rec.Status != StatusFailed || rec.Stopped == nil || rec.Stopped.By != "timeout" || len(rec.Stopped.NotReached) == 0 {
		t.Errorf("timeout: %s %+v", rec.Status, rec.Stopped)
	}
	if got := rec.Stopped.Short(); !strings.HasPrefix(got, "timed out after 0 s · ") {
		t.Errorf("short %q", got)
	}
}

// A run allowed more time is other evidence, and no more than the ceiling.
func TestAllowing(t *testing.T) {
	root := logs(t)
	stage := []expert.Stage{prim("grep", map[string]any{"pattern": "oom-kill 1$", "glob": "*.log"})}
	r := &Runner{}
	a := r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: stage}})
	b := r.Allowing(2*time.Minute).Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: stage}})
	c := r.Allowing(time.Hour).Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: stage}})
	if a.ID == b.ID || b.What.Timeout != 2*time.Minute || c.What.Timeout != DefaultMaxTimeout || a.What.Timeout != 0 {
		t.Errorf("ids %s %s; allowances %v %v %v", a.ID, b.ID, a.What.Timeout, b.What.Timeout, c.What.Timeout)
	}
}

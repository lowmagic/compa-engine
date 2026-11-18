package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The whole-item quoting tests run on the companion's stub case with
// synthetic files under its root, which the main model reads, each by a
// call of its own, before its answer cites them.

// readFile is a synthetic file the main model reads.
type readFile struct{ path, content string }

// readFilesSession is the companion's stub case whose main model reads
// files, then answers with answer given the names the reads gave, by
// path; the names are returned once the turn has run.
func readFilesSession(t *testing.T, files []readFile, answer func(names map[string]string) string) (*Session, func() map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		path := filepath.Join(dir, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(f.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	names := map[string]string{}
	var calls atomic.Int64
	s, _ := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if calls.Add(1) == 1 {
			var reads []model.ToolCall
			for i, f := range files {
				reads = append(reads, call("r"+strconv.Itoa(i), "read", map[string]any{"path": f.path}))
			}
			return modelfixture.Calling(reads...), nil
		}
		res := lastResults(r)
		mu.Lock()
		defer mu.Unlock()
		for i, f := range files {
			if ev := ExcerptEvidence(res["r"+strconv.Itoa(i)].Text); len(ev) == 1 {
				names[f.path] = ev[0]
			}
		}
		return modelfixture.Text(answer(maps.Clone(names))), nil
	})
	s.c.root = dir
	if _, err := s.Talk(context.Background(), "What do the logs say?", nil); err != nil {
		t.Fatal(err)
	}
	return s, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		if len(names) != len(files) {
			t.Fatalf("the reads named %v", names)
		}
		return maps.Clone(names)
	}
}

// numberedLines are n synthetic log lines, line i saying what at holds
// for it or "routine line i".
func numberedLines(n int, at map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		line, ok := at[i]
		if !ok {
			line = fmt.Sprintf("2026-10-20T08:%02d:%02dZ worker-1 kubelet: routine line %d", i/60%60, i%60, i)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// TestCheckRequestQuotesItemWhole: an item a statement cites within its
// bound is quoted whole, every line under its number, the lines cited
// marked and the others not, said so with its citation; the findings are
// validated against every line of it.
func TestCheckRequestQuotesItemWhole(t *testing.T) {
	s, names := readFilesSession(t, []readFile{{"nodes/worker-1/dns.log", numberedLines(30, map[int]string{20: "2026-10-20T08:20:00Z worker-1 dns: lookup timed out for kubernetes.default.svc"})}},
		func(n map[string]string) string {
			return "The DNS lookups on worker-1 timed out [" + n["nodes/worker-1/dns.log"] + ":3-4]."
		})
	req, err := s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := names()["nodes/worker-1/dns.log"]
	if len(req.Quotes) != 1 || !req.Quotes[0].Whole || len(req.Quotes[0].Parts) != 1 {
		t.Fatalf("quoted %+v:\n%s", req.Quotes, req.Text)
	}
	if want := "[quoted whole, lines 1–30; cited as " + name + ":3-4; ▸ marks the lines cited]\n"; !strings.Contains(req.Text, want) {
		t.Errorf("the request lacks %q:\n%s", want, req.Text)
	}
	content := numberedLines(30, map[int]string{20: "2026-10-20T08:20:00Z worker-1 dns: lookup timed out for kubernetes.default.svc"})
	for i, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		mark := "  "
		if n := i + 1; n == 3 || n == 4 {
			mark = citedMark
		}
		if want := fmt.Sprintf("\n%s%2d  %s\n", mark, i+1, line); !strings.Contains(req.Text, want) {
			t.Errorf("the request lacks line %d as %q:\n%s", i+1, want, req.Text)
		}
	}
	if d := cmp.Diff(map[string][]check.Lines{name: {{From: 1, To: 30}}}, req.Context.Quoted); d != "" || req.Context.Items[name] != 30 {
		t.Errorf("the lines given (-want +got):\n%s; items %v", d, req.Context.Items)
	}
}

// TestCheckRequestItemPastItsBound: an item holding more than an item is
// quoted whole to is quoted as the lines cited with five lines either
// side, said so, windows that meet merged, the lines cited marked; only
// those lines are given to validate against.
func TestCheckRequestItemPastItsBound(t *testing.T) {
	content := numberedLines(2000, nil)
	if len(content) <= quoteItemBytes {
		t.Fatalf("the synthetic log is %d bytes, within the item's bound", len(content))
	}
	s, names := readFilesSession(t, []readFile{{"nodes/worker-1/kubelet.log", content}}, func(n map[string]string) string {
		e := n["nodes/worker-1/kubelet.log"]
		return "The kubelet logged routine lines [" + e + ":1000] and [" + e + ":1003-1004]. It logged more later [" + e + ":1500]."
	})
	req, err := s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := names()["nodes/worker-1/kubelet.log"]
	if len(req.Quotes) != 1 || req.Quotes[0].Whole {
		t.Fatalf("quoted %+v:\n%s", req.Quotes, req.Text)
	}
	want := "[not quoted whole: it holds more than the 64 KiB an item is quoted whole to; lines 995–1009, lines 1495–1505 quoted, the lines cited with up to 5 lines either side; cited as " +
		name + ":1000, " + name + ":1003-1004, " + name + ":1500; ▸ marks the lines cited]\n"
	if !strings.Contains(req.Text, want) {
		t.Errorf("the request lacks %q:\n%s", want, req.Text)
	}
	lines := strings.Split(content, "\n")
	for n := 990; n <= 1510; n++ {
		mark := "  "
		if n == 1000 || n == 1003 || n == 1004 || n == 1500 {
			mark = citedMark
		}
		numbered := fmt.Sprintf("\n%s%4d  %s\n", mark, n, lines[n-1])
		if in := n >= 995 && n <= 1009 || n >= 1495 && n <= 1505; in != strings.Contains(req.Text, numbered) {
			t.Errorf("line %d quoted %v; want %v:\n%s", n, !in, in, req.Text)
		}
	}
	if !strings.Contains(req.Text, "\n  1009  "+lines[1008]+"\n…\n  1495  ") {
		t.Errorf("the windows are not said apart:\n%s", req.Text)
	}
	if d := cmp.Diff(map[string][]check.Lines{name: {{From: 995, To: 1009}, {From: 1495, To: 1505}}}, req.Context.Quoted); d != "" || req.Context.Items[name] != 2000 {
		t.Errorf("the lines given (-want +got):\n%s; items %v", d, req.Context.Items)
	}
}

// bigLog is a synthetic log past an item's bound, its lines from 990 to
// 1010 long ones of just over 2 KiB, so three fit a window's bytes.
func bigLog() string {
	long := map[int]string{}
	for n := 990; n <= 1010; n++ {
		long[n] = fmt.Sprintf("2026-10-20T09:00:00Z worker-1 kubelet: long line %d %s", n, strings.Repeat("x", 2000))
	}
	return numberedLines(2000, long)
}

// TestCheckRequestWindowKeepsCitedLines: in an item past its bound whose
// lines are long, the line cited is quoted and given to validate against,
// with what context its window's bytes leave, nearest first: the context
// before it never spends them first.
func TestCheckRequestWindowKeepsCitedLines(t *testing.T) {
	content := bigLog()
	s, names := readFilesSession(t, []readFile{{"nodes/worker-1/kubelet.log", content}}, func(n map[string]string) string {
		return "The kubelet logged a long line [" + n["nodes/worker-1/kubelet.log"] + ":1000]."
	})
	req, err := s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := names()["nodes/worker-1/kubelet.log"]
	lines := strings.Split(content, "\n")
	if !strings.Contains(req.Text, "\n"+citedMark+"1000  "+lines[999]+"\n") {
		t.Errorf("the request does not quote line 1000, marked:\n%.3000s", req.Text)
	}
	if d := cmp.Diff(map[string][]check.Lines{name: {{From: 999, To: 1001}}}, req.Context.Quoted); d != "" {
		t.Errorf("the lines given (-want +got):\n%s", d)
	}
}

// TestCheckRequestWindowsKeepTheirBound: spans whose windows meet are not
// merged past a window's line bound, so a later citation keeps a window
// of its own; a span longer than the bound is cut, said so, and no line
// is quoted twice.
func TestCheckRequestWindowsKeepTheirBound(t *testing.T) {
	content := numberedLines(2000, nil)
	s, names := readFilesSession(t, []readFile{{"nodes/worker-1/kubelet.log", content}}, func(n map[string]string) string {
		e := n["nodes/worker-1/kubelet.log"]
		return "The kubelet logged routine lines [" + e + ":100-140]. It logged one more [" + e + ":150]."
	})
	req, err := s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := names()["nodes/worker-1/kubelet.log"]
	if d := cmp.Diff(map[string][]check.Lines{name: {{From: 95, To: 139}, {From: 145, To: 155}}}, req.Context.Quoted); d != "" {
		t.Errorf("the lines given (-want +got):\n%s\n%s", d, req.Text)
	}
	lines := strings.Split(content, "\n")
	for _, want := range []string{"[cut at 40 lines: lines 100–139 of lines 100–140 quoted]\n…\n", "\n" + citedMark + "150  " + lines[149] + "\n"} {
		if !strings.Contains(req.Text, want) {
			t.Errorf("the request lacks %q:\n%s", want, req.Text)
		}
	}
}

// TestCheckRequestHeaderSaysWhatIsQuoted: an item's heading says what of
// it is quoted: a citation past its end said so, with no line said
// marked; an item past its bound cited whole, its first lines quoted and
// said to be, and which.
func TestCheckRequestHeaderSaysWhatIsQuoted(t *testing.T) {
	s, names := readFilesSession(t, []readFile{{"nodes/worker-1/dns.log", numberedLines(30, nil)}, {"nodes/worker-1/kubelet.log", numberedLines(2000, nil)}},
		func(n map[string]string) string {
			return "DNS logged routine lines [" + n["nodes/worker-1/dns.log"] + ":99]. So did the kubelet [" + n["nodes/worker-1/kubelet.log"] + "]."
		})
	req, err := s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := names()
	dns, kubelet := n["nodes/worker-1/dns.log"], n["nodes/worker-1/kubelet.log"]
	for _, want := range []string{
		"[quoted whole, lines 1–30; cited as " + dns + ":99; " + dns + ":99 past its 30 lines]\n",
		"[not quoted whole: it holds more than the 64 KiB an item is quoted whole to; lines 1–45 quoted, the lines cited with up to 5 lines either side; cited as " + kubelet + "]\n" + citedWholeFirst,
	} {
		if !strings.Contains(req.Text, want) {
			t.Errorf("the request lacks %q:\n%.4000s", want, req.Text)
		}
	}
}

// TestCheckRequestTotalBound: an item that fits its own bound but not what
// the request has left is quoted as the lines cited with lines either
// side, said so, and the items after it still whole where they fit; the
// request stays within its bound and is the same bytes made again.
func TestCheckRequestTotalBound(t *testing.T) {
	files := []readFile{
		{"nodes/worker-1/dns.log", numberedLines(100, map[int]string{50: "2026-10-20T08:50:00Z worker-1 dns: lookup timed out for kubernetes.default.svc"})},
		{"nodes/worker-2/dns.log", numberedLines(4, map[int]string{2: "2026-10-20T08:02:00Z worker-2 dns: lookup timed out for kubernetes.default.svc"})},
	}
	s, names := readFilesSession(t, files, func(n map[string]string) string {
		return "DNS lookups timed out on worker-1 [" + n["nodes/worker-1/dns.log"] + ":50] and worker-2 [" + n["nodes/worker-2/dns.log"] + ":2]."
	})
	ctx := context.Background()
	answers, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answer := answersOf(answers)[0].text
	c, err := s.checked(ctx, answer, nil)
	if err != nil {
		t.Fatal(err)
	}
	cites := citesOf(c.Statements)
	none, err := s.checkRequest(ctx, check.Editor, "# Check this answer\n", c, cites, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	prefix, ok := strings.CutSuffix(none.Text, fmt.Sprintf(quotesSpent, 2))
	if !ok {
		t.Fatalf("a request with no room:\n%s", none.Text)
	}
	limit := len(prefix) + 2000
	if len(files[0].content) < 2000 {
		t.Fatalf("the first log is %d bytes, within what the request leaves", len(files[0].content))
	}
	req, err := s.checkRequest(ctx, check.Editor, "# Check this answer\n", c, cites, "", limit)
	if err != nil {
		t.Fatal(err)
	}
	n := names()
	first, second := n["nodes/worker-1/dns.log"], n["nodes/worker-2/dns.log"]
	if len(req.Text) > limit || len(req.Quotes) != 2 || req.Quotes[0].Whole || !req.Quotes[1].Whole ||
		!strings.Contains(req.Text, "[not quoted whole: it would pass the "+strconv.Itoa(limit)+" bytes the request is bounded to; lines 45–55 quoted, the lines cited with up to 5 lines either side; cited as "+first+":50;") {
		t.Errorf("a request bounded at %d bytes is %d:\n%s", limit, len(req.Text), req.Text)
	}
	if d := cmp.Diff(map[string][]check.Lines{first: {{From: 45, To: 55}}, second: {{From: 1, To: 4}}}, req.Context.Quoted); d != "" {
		t.Errorf("the lines given (-want +got):\n%s", d)
	}
	again, err := s.checkRequest(ctx, check.Editor, "# Check this answer\n", c, cites, "", limit)
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(req.Text, again.Text); d != "" {
		t.Errorf("the request made again differs (-first +again):\n%s", d)
	}
	full, err := s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Quotes) != 2 || !full.Quotes[0].Whole || !full.Quotes[1].Whole || len(full.Text) > editorRequestBytes {
		t.Errorf("within the editor's bound, the request quotes %+v in %d bytes", full.Quotes, len(full.Text))
	}
}

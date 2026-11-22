package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/chat"
	"github.com/lowmagic/compa-engine/engine/cli"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/stubcases"
	"github.com/lowmagic/compa-engine/internal/store"
)

// cliEnv runs the command line in process with a temporary home, a copy of
// the stub cases it can edit, and a directory of case folders. In process
// is enough for the lock: a second open of a store in one process is
// refused as a second process would be.
type cliEnv struct {
	t                *testing.T
	home, stub       string
	cases, caseStore string
	// stdin is what the next command reads from standard input.
	stdin string
}

func newEnv(t *testing.T) *cliEnv {
	t.Helper()
	// Resolved, since the artifact source reports the case folder's real
	// path, and macOS's temporary directory is under a link.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &cliEnv{t: t, home: filepath.Join(tmp, "home"), stub: filepath.Join(tmp, "stub"), cases: filepath.Join(tmp, "cases")}
	for _, dir := range []string{e.home, e.stub, filepath.Join(e.cases, "00000001", "0010-must-gather", "namespaces"), filepath.Join(e.cases, "00000001", ".statedir")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	folder := filepath.Join(e.cases, "00000001")
	for name, body := range map[string]string{
		"0010-must-gather/version":            "4.16.3\n",
		"0010-must-gather/namespaces/ns.yaml": "kind: Namespace\n",
		"0020-pods.log":                       "2026-10-01T00:00:00Z ok\n",
		".statedir/attachment-id":             "0010",
	} {
		os.WriteFile(filepath.Join(folder, name), []byte(body), 0o600)
	}
	for _, name := range []string{"00000001.yaml", "00000002.yaml"} {
		data, err := os.ReadFile(filepath.Join("../../testdata/cases", name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(e.stub, name), data, 0o600)
	}
	e.caseStore = filepath.Join(e.cases, "00000001", store.DirName)
	return e
}

func (e *cliEnv) run(args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	return e.runConfig(hostConfig, args...)
}

// runConfig runs the command line on the configuration given.
func (e *cliEnv) runConfig(config []byte, args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	var out, errb bytes.Buffer
	o := engine.Options{Cases: stubcases.New(e.stub), Artifacts: localdir.New(e.cases), Home: e.home, Config: config}
	code = cli.Run(context.Background(), args, o, strings.NewReader(e.stdin), &out, &errb)
	return out.String(), errb.String(), code
}

// runWith runs the command line on the configuration given, failing the
// test unless it succeeds.
func (e *cliEnv) runWith(config []byte, args ...string) string {
	e.t.Helper()
	out, errs, code := e.runConfig(config, args...)
	if code != 0 {
		e.t.Fatalf("compa %v: exit %d\n%s", args, code, errs)
	}
	return out
}

func (e *cliEnv) ok(args ...string) string {
	e.t.Helper()
	out, errs, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("compa %v: exit %d\n%s", args, code, errs)
	}
	return out
}

// hostConfig excludes a source's state directory, as a deployment's may.
var hostConfig = []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\ncollection: {exclude: [.statedir]}\n")

var evidenceID = regexp.MustCompile(`(?m)^(ev-[0-9a-f]{16})\s+(\S+)\s.*?(case \S+(?: comment \S+| artifacts| artifact counts)?)$`)

// listed maps each evidence item's location to its IDs, oldest first.
func (e *cliEnv) listed(caseID string) map[string][]string {
	out := map[string][]string{}
	for _, m := range evidenceID.FindAllStringSubmatch(e.ok("evidence", "list", caseID), -1) {
		out[m[3]] = append(out[m[3]], m[1])
	}
	return out
}

func TestOpenAcquiresTheCaseOnce(t *testing.T) {
	e := newEnv(t)
	out := e.ok("case", "open", "00000001")
	for _, want := range []string{"case 00000001: Pods on two workers", "store: " + e.caseStore, "attachments: 2, holding 3 files, 47 bytes", "6 items, 6 new"} {
		if !strings.Contains(out, want) {
			t.Errorf("first open lacks %q:\n%s", want, out)
		}
	}
	first := e.listed("00000001")
	if len(first) != 6 || len(first["case 00000001 comment c2"]) != 1 || len(first["case 00000001 artifacts"]) != 1 || len(first["case 00000001 artifact counts"]) != 1 {
		t.Fatalf("evidence after the first open: %v", first)
	}

	out = e.ok("case", "open", "00000001")
	if !strings.Contains(out, "6 items, 0 new") {
		t.Errorf("reopen:\n%s", out)
	}
	if again := e.listed("00000001"); len(again) != 6 || len(again["case 00000001 comment c2"]) != 1 {
		t.Errorf("reopening added evidence: %v", again)
	}

	// The comment is edited at the source: a new item, the old one kept.
	path := filepath.Join(e.stub, "00000001.yaml")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(data, []byte("must-gather attached."), []byte("must-gather attached; sosreport to follow."), 1), 0o600)
	if out := e.ok("case", "open", "00000001"); !strings.Contains(out, "6 items, 1 new") {
		t.Errorf("open after an edit:\n%s", out)
	}
	c2 := e.listed("00000001")["case 00000001 comment c2"]
	if len(c2) != 2 || c2[0] != first["case 00000001 comment c2"][0] {
		t.Fatalf("comment c2 after an edit: %v", c2)
	}
	if old := e.ok("evidence", "show", "00000001", c2[0]); !strings.Contains(old, "must-gather attached.\n") || !strings.Contains(old, "source: stub") {
		t.Errorf("the old comment:\n%s", old)
	}
	if edited := e.ok("evidence", "show", "00000001", c2[1]); !strings.Contains(edited, "sosreport to follow") {
		t.Errorf("the edited comment:\n%s", edited)
	}

	show := e.ok("case", "show", "00000001")
	if !strings.Contains(show, "7 items (artifact_counts 1, artifact_overview 1, case_comment 4, case_metadata 1)") || !strings.Contains(show, "subject: Pods on two workers") {
		t.Errorf("case show:\n%s", show)
	}
	if _, err := os.Stat(filepath.Join(e.home, store.DirName)); err == nil {
		t.Error("a case with a case folder was stored in the home directory")
	}
	ov := e.ok("evidence", "show", "00000001", first["case 00000001 artifacts"][0])
	for _, want := range []string{"artifacts of case 00000001 from local directory: 2 attachments", "attachment: 0010-must-gather\nstate: present\n", "attachment: 0020-pods.log\nstate: present\nsize: 24 bytes\nfile: 0020-pods.log\n"} {
		if !strings.Contains(ov, want) {
			t.Errorf("overview lacks %q:\n%s", want, ov)
		}
	}
	// The counts are an item of their own, so a count never changes the
	// overview the case block shows.
	if strings.Contains(ov, "files") {
		t.Errorf("the overview holds a count:\n%s", ov)
	}
	if counts := e.ok("evidence", "show", "00000001", first["case 00000001 artifact counts"][0]); !strings.Contains(counts, "attachment: 0010-must-gather\nstate: present\ncontents: 2 files, 23 bytes\n") {
		t.Errorf("the counts:\n%s", counts)
	}
	if strings.Contains(ov, ".statedir") || strings.Contains(ov, ".compa") {
		t.Errorf("the overview names an excluded entry:\n%s", ov)
	}
}

func TestSecondAkOnTheSameCaseIsRefused(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000001")
	held, err := store.Open(e.caseStore, "00000001", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for _, args := range [][]string{{"case", "open", "00000001"}, {"case", "show", "00000001"}, {"evidence", "list", "00000001"}} {
		if _, errs, code := e.run(args...); code != 1 || !strings.Contains(errs, store.ErrLocked.Error()) {
			t.Errorf("compa %v while the case is held: exit %d, %q", args, code, errs)
		}
	}
}

func TestCaseWithoutArtifactsIsStoredInHome(t *testing.T) {
	e := newEnv(t)
	out := e.ok("case", "open", "00000002")
	dir := filepath.Join(e.home, store.DirName, "cases", "00000002")
	if !strings.Contains(out, "artifacts: none found") || !strings.Contains(out, "store: "+dir) {
		t.Errorf("open:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "case.db")); err != nil {
		t.Error(err)
	}
}

// fetcher is a local-directory source that can fetch a case folder: it
// makes it, or fails as told.
type fetcher struct {
	engine.ArtifactSource
	base    string
	fail    error
	fetched []string
}

func (*fetcher) FetchCommand(id string) string { return "fetch " + id }

func (f *fetcher) Fetch(_ context.Context, id string, _ io.Reader, out io.Writer) error {
	f.fetched = append(f.fetched, id)
	if f.fail != nil {
		return f.fail
	}
	fmt.Fprintln(out, "fetching", id)
	return os.MkdirAll(filepath.Join(f.base, id), 0o700)
}

// A case with no folder is offered its fetch on a terminal, a yes the
// default, before its store is made: a folder fetched holds the store; a
// no, a failed fetch or no terminal leave it in the home directory, the
// failure said. A case with a folder is not asked about.
func TestCaseWithoutAFolderIsOfferedItsFetch(t *testing.T) {
	prompt := "fetch the case's artifacts with fetch 00000002? [Y/n] "
	for _, c := range []struct {
		name, id, typed string
		terminal        bool
		fail            error
		fetched         int
		want            []string
		inFolder        bool
	}{
		{"yes by default", "00000002", "\n", true, nil, 1, []string{prompt, "fetching 00000002"}, true},
		{"yes", "00000002", "Y\n", true, nil, 1, []string{prompt}, true},
		{"no", "00000002", "n\n", true, nil, 0, []string{prompt, "artifacts: none found"}, false},
		{"nothing typed", "00000002", "", true, nil, 0, []string{prompt, "artifacts: none found"}, false},
		{"failed", "00000002", "y\n", true, errors.New("no such case"), 1, []string{prompt, "fetch 00000002 failed: no such case; going on without the case folder", "artifacts: none found"}, false},
		{"no terminal", "00000002", "y\n", false, nil, 0, []string{"artifacts: none found"}, false},
		{"has a folder", "00000001", "y\n", true, nil, 0, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			cli.AsTerminal(t, c.terminal)
			f := &fetcher{ArtifactSource: localdir.New(e.cases), base: e.cases, fail: c.fail}
			var out, errb bytes.Buffer
			o := engine.Options{Cases: stubcases.New(e.stub), Artifacts: f, Home: e.home, Config: hostConfig}
			if code := cli.Run(context.Background(), []string{"case", "open", c.id}, o, strings.NewReader(c.typed), &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			got := out.String()
			if len(f.fetched) != c.fetched {
				t.Errorf("fetched %v, want %d", f.fetched, c.fetched)
			}
			if c.want == nil && strings.Contains(got, "[Y/n]") {
				t.Errorf("asked about a case with a folder:\n%s", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("output lacks %q:\n%s", w, got)
				}
			}
			folder := "store: " + filepath.Join(e.cases, c.id, store.DirName)
			if strings.Contains(got, folder) != c.inFolder {
				t.Errorf("store in the case folder: %v, want %v:\n%s", !c.inFolder, c.inFolder, got)
			}
		})
	}
}

// cancelAtPrompt is the terminal's output, which cancels the command when
// the fetch offer asks, as Ctrl+C there does.
type cancelAtPrompt struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelAtPrompt) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[Y/n]")) {
		w.cancel()
	}
	return w.Buffer.Write(p)
}

// Interrupted at the fetch offer, a command ends with nothing typed and
// nothing fetched, rather than waiting for Enter and taking it for a yes.
func TestInterruptedFetchOfferEnds(t *testing.T) {
	e := newEnv(t)
	cli.AsTerminal(t, true)
	f := &fetcher{ArtifactSource: localdir.New(e.cases), base: e.cases}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, typing := io.Pipe()
	defer typing.Close()
	out := &cancelAtPrompt{cancel: cancel}
	var errb bytes.Buffer
	o := engine.Options{Cases: stubcases.New(e.stub), Artifacts: f, Home: e.home, Config: hostConfig}
	code := make(chan int, 1)
	go func() { code <- cli.Run(ctx, []string{"case", "open", "00000002"}, o, in, out, &errb) }()
	select {
	case c := <-code:
		if c == 0 || !strings.Contains(errb.String(), context.Canceled.Error()) {
			t.Errorf("exit %d, want the context's error: %s", c, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still waiting for an answer after the interrupt:\n%s", out.String())
	}
	if len(f.fetched) != 0 {
		t.Errorf("fetched %v after the interrupt", f.fetched)
	}
}

// A case opened before its folder was found keeps its work when the
// folder appears: the home store is moved into the folder, even past its
// retention, rather than left behind and swept.
func TestHomeStoreMovesIntoTheCaseFolder(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000002")
	e.stdin = "an engineer's capture\n"
	e.ok("in", "00000002")
	home := filepath.Join(e.home, store.DirName, "cases", "00000002")
	old := time.Now().Add(-15 * 24 * time.Hour)
	s, err := store.Open(home, "00000002", store.Options{Now: func() time.Time { return old }})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.MkdirAll(filepath.Join(e.cases, "00000002"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := e.ok("case", "open", "00000002")
	if !strings.Contains(out, "moved the store made before the case folder was found from "+home) || !strings.Contains(out, "store: "+filepath.Join(e.cases, "00000002", store.DirName)) {
		t.Errorf("open:\n%s", out)
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("the home store is still there")
	}
	if listed := e.ok("evidence", "list", "00000002"); !strings.Contains(listed, "ingested") {
		t.Errorf("the capture did not move:\n%s", listed)
	}
}

// With a store in the folder already, a home store is not merged but kept
// from the sweep, and the engineer told where it is.
func TestHomeStoreBesideAFolderStoreIsKept(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000002")
	home := filepath.Join(e.home, store.DirName, "cases", "00000002")
	old := time.Now().Add(-15 * 24 * time.Hour)
	s, err := store.Open(home, "00000002", store.Options{Now: func() time.Time { return old }})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	folder := filepath.Join(e.cases, "00000002", store.DirName)
	f, err := store.Open(folder, "00000002", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	out := e.ok("case", "open", "00000002")
	if !strings.Contains(out, "an earlier store for this case is in "+home) || strings.Contains(out, "removed an expired store") {
		t.Errorf("open:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "case.db")); err != nil {
		t.Errorf("the home store was removed: %v", err)
	}
}

func TestOpenSweepsExpiredHomeStores(t *testing.T) {
	e := newEnv(t)
	expired := filepath.Join(e.home, store.DirName, "cases", "00000003")
	old := time.Now().Add(-15 * 24 * time.Hour)
	s, err := store.Open(expired, "00000003", store.Options{Now: func() time.Time { return old }})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if out := e.ok("case", "open", "00000001"); !strings.Contains(out, "removed an expired store: "+expired) {
		t.Errorf("open:\n%s", out)
	}
	if _, err := os.Stat(expired); err == nil {
		t.Error("the expired store is still there")
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"case", "open", "99999999"}, 1, "case not found"},
		{[]string{"case", "show", "00000001"}, 1, "has not been opened; run compa case open 00000001"},
		{[]string{"case", "open", "a/b"}, 1, "invalid case ID"},
		{[]string{"case", "open"}, 2, "case open <case>"},
		{[]string{"evidence", "show", "00000001"}, 2, "evidence show <case> <evidence-id>"},
		{[]string{"nonsense", "here"}, 2, "unknown command"},
	} {
		if _, errs, code := e.run(c.args...); code != c.code || !strings.Contains(errs, c.want) {
			t.Errorf("compa %v: exit %d, %q; want %d and %q", c.args, code, errs, c.code, c.want)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(e.home, store.DirName, "cases")); len(entries) != 0 {
		t.Errorf("failed commands left stores behind: %v", entries)
	}
	e.ok("case", "open", "00000001")
	if _, errs, code := e.run("evidence", "show", "00000001", "ev-0000000000000000"); code != 1 || !strings.Contains(errs, "has no evidence") {
		t.Errorf("unknown evidence: exit %d, %q", code, errs)
	}
}

func TestBoundedOutput(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000001")
	if out := e.ok("evidence", "list", "00000001", "--limit", "2"); !strings.Contains(out, "… 4 more; use --limit") || len(evidenceID.FindAllString(out, -1)) != 2 {
		t.Errorf("list --limit 2:\n%s", out)
	}
	id := e.listed("00000001")["case 00000001"][0]
	if out := e.ok("evidence", "show", "00000001", id, "--max-bytes", "40"); !strings.Contains(out, "more bytes; use --max-bytes") {
		t.Errorf("show --max-bytes 40:\n%s", out)
	}
}

var ingestedLine = regexp.MustCompile(`(?m)^(ev-[0-9a-f]{16})  (\S+)  (\d+) bytes, (\d+ lines?) \((new|already stored)\)$`)

func TestIngest(t *testing.T) {
	e := newEnv(t)
	if _, errs, code := e.run("in", "00000001"); code != 1 || !strings.Contains(errs, "has not been opened") {
		t.Errorf("ingest before open: exit %d, %q", code, errs)
	}
	e.ok("case", "open", "00000001")

	e.stdin = "NAME   READY\nnetwork  False\n"
	out := e.ok("in", "00000001", "--note", "oc get co from the customer")
	m := ingestedLine.FindStringSubmatch(out)
	if m == nil || m[2] != "stdin" || m[3] != "28" || m[4] != "2 lines" || m[5] != "new" {
		t.Fatalf("ingest stdin:\n%s", out)
	}
	fromStdin := m[1]

	file := filepath.Join(e.home, "co.txt")
	os.WriteFile(file, []byte(e.stdin), 0o600)
	e.stdin = ""
	first := ingestedLine.FindStringSubmatch(e.ok("in", "00000001", file))
	again := ingestedLine.FindStringSubmatch(e.ok("in", "00000001", file))
	if first == nil || again == nil || first[1] != again[1] || first[5] != "new" || again[5] != "already stored" || first[2] != file {
		t.Errorf("a file ingested twice: %v then %v", first, again)
	}
	if first != nil && first[1] == fromStdin {
		t.Error("the same text from stdin and from a file is one item; it should be two")
	}

	shown := e.ok("evidence", "show", "00000001", fromStdin)
	for _, want := range []string{"type: ingested", "source: engineer", "collected by: engineer", "location: stdin", "note: oc get co from the customer", "\nNAME   READY\nnetwork  False\n"} {
		if !strings.Contains(shown, want) {
			t.Errorf("show lacks %q:\n%s", want, shown)
		}
	}

	// Several files at once, one of them missing: the rest are stored.
	other := filepath.Join(e.home, "other.txt")
	os.WriteFile(other, []byte("x"), 0o600)
	out, errs, code := e.run("in", "00000001", other, filepath.Join(e.home, "missing.txt"))
	if code != 1 || !strings.Contains(errs, "1 of 2 inputs were not ingested") || !strings.Contains(out, other+"  1 bytes, 1 line (new)") {
		t.Errorf("ingest with a missing file: exit %d\n%s%s", code, out, errs)
	}
}

func TestShowReadsOnlyWhatItPrints(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000001")
	e.stdin = strings.Repeat("0123456789", 100_000)
	id := ingestedLine.FindStringSubmatch(e.ok("in", "00000001"))[1]
	out := e.ok("evidence", "show", "00000001", id, "--max-bytes", "25")
	if !strings.Contains(out, "\n0123456789012345678901234\n[… 999975 more bytes; use --max-bytes]\n") {
		t.Errorf("show --max-bytes 25:\n%s", out)
	}
}

// compa case with no subcommand is a mistake in the command line.
func TestCaseAloneIsAUsageError(t *testing.T) {
	e := newEnv(t)
	if _, errs, code := e.run("case"); code != 2 {
		t.Errorf("compa case: exit %d, %s", code, errs)
	}
}

// The chat, over a case or with none, needs the engineer's terminal for
// its input and output: without one it refuses, saying compa ask runs one
// turn without it, before the chat is run or a store made. On a terminal
// it runs.
func TestChatNeedsATerminal(t *testing.T) {
	for _, args := range [][]string{{"00000001"}, {"00000001", "--no-open"}, {"chat"}, {"chat", "--dir", "."}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e := newEnv(t)
			ran := 0
			cli.RunChat(t, func(context.Context, *engine.Session, chat.Options, io.Reader, io.Writer) error {
				ran++
				return nil
			})
			o := engine.Options{Cases: stubcases.New(e.stub), Artifacts: localdir.New(e.cases), Home: e.home, Config: hostConfig}
			var out, errb bytes.Buffer
			cli.AsTerminal(t, false)
			code := cli.Run(context.Background(), args, o, strings.NewReader("hello\n"), &out, &errb)
			if code != 1 || !strings.Contains(errb.String(), "compa ask <case> <question>") || ran != 0 {
				t.Errorf("with no terminal: exit %d, the chat run %d times: %s", code, ran, errb.String())
			}
			if _, err := os.Stat(e.caseStore); !os.IsNotExist(err) {
				t.Errorf("a case store was made: %v", err)
			}
			if _, err := os.Stat(filepath.Join(e.home, ".local", "share", "compa", "chats")); !os.IsNotExist(err) {
				t.Errorf("a chat store was made: %v", err)
			}
			cli.AsTerminal(t, true)
			if code := cli.Run(context.Background(), args, o, strings.NewReader(""), &out, &errb); code != 0 || ran != 1 {
				t.Errorf("on a terminal: exit %d, the chat run %d times: %s", code, ran, errb.String())
			}
		})
	}
}

// compa case comments over a case of 18 comments: --exclude-all leaves
// every comment out and --keep reads one whatever else; --show prints each
// comment whole, newest first, flush and unwrapped, for copying, and takes
// no rule or other flag.
func TestCaseCommentsShowAndAll(t *testing.T) {
	e := newEnv(t)
	data, err := os.ReadFile("../../testdata/cases/00000004.yaml")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.stub, "00000004.yaml"), data, 0o600)
	folder := filepath.Join(e.cases, "00000004")
	os.MkdirAll(filepath.Join(folder, "0010-must-gather.local.1.tgz"), 0o700)
	os.WriteFile(filepath.Join(folder, "0020-sosreport-node1-2026-10-01.tar.xz"), []byte("x"), 0o600)
	e.ok("case", "open", "00000004")
	if out := e.ok("case", "comments", "00000004", "--exclude-all"); !strings.Contains(out, "models read 0 of 18 comments") || !strings.Contains(out, "every comment") {
		t.Errorf("--exclude-all:\n%s", out)
	}
	if out := e.ok("case", "comments", "00000004", "--keep", "c5"); !strings.Contains(out, "models read 1 of 18 comments") {
		t.Errorf("--keep with all:\n%s", out)
	}
	if strings.Contains(e.ok("case", "comments", "00000004"), " ? ") {
		t.Error("an author kind the source gives none of shows as ?")
	}
	raw := e.ok("case", "comments", "00000004", "--show")
	if strings.Contains(raw, "\x1b[") {
		t.Error("--show to a file keeps colour")
	}
	if !strings.Contains(raw, "\nThank you for opening this case. I am support-engineer from the platform team and I will be working with you on the collector restarts you describe until we have an answer for them.\n") {
		t.Errorf("--show does not print c1's long line flush and whole:\n%s", raw)
	}
	first, last := regexp.MustCompile(`(?m)^c18 2027-01-10 09:55 PUBLIC +SUPPORT +Account Manager Two \(user-6\)$`).FindStringIndex(raw), regexp.MustCompile(`(?m)^c1 2026-10-01 09:00 PUBLIC +SUPPORT +support-engineer \(user-1\)$`).FindStringIndex(raw)
	if first == nil || last == nil || first[0] > last[0] {
		t.Errorf("--show does not head each comment, newest first: c18 at %v, c1 at %v:\n%s", first, last, raw)
	}
	if strings.Contains(raw, "▸ ") || strings.Contains(raw, "folds") {
		t.Errorf("--show folds or counts:\n%s", raw)
	}
	for _, args := range [][]string{{"--show", "--width", "80"}, {"--show", "--formatted"}} {
		if _, _, code := e.run(append([]string{"case", "comments", "00000004"}, args...)...); code != 2 {
			t.Errorf("case comments %v: exit %d, want 2", args, code)
		}
	}
	if _, _, code := e.run("case", "comments", "00000004", "--show", "--exclude-all"); code != 2 {
		t.Errorf("--show with a rule: exit %d", code)
	}
}

// TestParseIn: compa in outside a compa shell takes the case first and the
// files after it; inside one, the shell's case, the question as the note
// and files with -f, unless the case is named first; -c names the case
// the same way; -t's tags are kept in the note; the shell's socket is used
// for the shell's case only.
func TestParseIn(t *testing.T) {
	for _, tt := range []struct {
		name               string
		args               []string
		shellCase, socket  string
		id, note, toSocket string
		files              []string
		usage              bool
	}{
		{name: "outside", args: []string{"00000001", "--note", "oc get co", "a.txt"}, id: "00000001", note: "oc get co", files: []string{"a.txt"}},
		{name: "outside, no case", usage: true},
		{name: "in a shell", args: []string{"what does this answer?", "-t", "openshift, mcp", "-f", "a.txt"}, shellCase: "00000001", socket: "/s",
			id: "00000001", note: "what does this answer? · tags: openshift, mcp", files: []string{"a.txt"}, toSocket: "/s"},
		{name: "in a shell, nothing said", shellCase: "00000001", socket: "/s", id: "00000001", toSocket: "/s"},
		{name: "in a shell, the case first", args: []string{"00000001", "a.txt"}, shellCase: "00000001", socket: "/s",
			id: "00000001", files: []string{"a.txt"}, toSocket: "/s"},
		{name: "in a shell, two questions", args: []string{"one", "two"}, shellCase: "00000001", usage: true},
		{name: "in a shell, another case", args: []string{"-c", "00000002", "why?"}, shellCase: "00000001", socket: "/s", id: "00000002", note: "why?"},
		{name: "tags alone", args: []string{"-c", "00000002", "-t", "mcp"}, id: "00000002", note: "tags: mcp"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := cli.ParseIn(tt.args, tt.shellCase, tt.socket)
			if tt.usage {
				if !errors.Is(err, cli.ErrUsage) {
					t.Errorf("parseIn = %+v, %v; want a usage error", r, err)
				}
				return
			}
			if err != nil || r.ID != tt.id || r.Note != tt.note || r.Socket != tt.toSocket || strings.Join(r.Files, ",") != strings.Join(tt.files, ",") {
				t.Errorf("parseIn = %+v, %v", r, err)
			}
		})
	}
}

// TestIngestIntoAHeldCase: while a session holds the case and serves its
// store, compa in sends what it reads to it, through the socket the store
// names or the one the engineer's shell names, and says the name the
// chat gave it; the shell's form keeps the question and the tags as the
// note.
func TestIngestIntoAHeldCase(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000001")
	eng, err := engine.New(engine.Options{Cases: stubcases.New(e.stub), Artifacts: localdir.New(e.cases), Home: e.home, Config: hostConfig})
	if err != nil {
		t.Fatal(err)
	}
	s, err := eng.StartSession(t.Context(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv, err := s.ServeIngest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	e.stdin = "NAME   READY\nnetwork  False\n"
	out := e.ok("in", "00000001", "--note", "oc get co")
	if ingestedLine.FindStringSubmatch(out) == nil || !regexp.MustCompile(`(?m)^kept as e\d+ by the chat holding the case`).MatchString(out) {
		t.Errorf("compa in on the held case:\n%s", out)
	}

	t.Setenv("COMPA_CASE", "00000001")
	t.Setenv("COMPA_INGEST", srv.Socket())
	e.stdin = "machine-config  4.16.3  True\n"
	out = e.ok("in", "what does this answer?", "-t", "openshift,mcp")
	m := regexp.MustCompile(`(?m)^kept as (e\d+) by`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("compa in in the shell:\n%s", out)
	}
	it, err := s.EvidenceNamed(m[1])
	if err != nil || it.Note != "what does this answer? · tags: openshift, mcp" || string(it.Content) != e.stdin {
		t.Errorf("%s holds %q noted %q, %v", m[1], it.Content, it.Note, err)
	}
	if taken := srv.Take(); len(taken) != 2 {
		t.Errorf("the chat was given %+v", taken)
	}
}

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The conversation's start on the companion's stub case, whose comments
// are synthetic.

// fixedCases is a case source giving the stub case and the comments its
// test sets, named as companionSession's source, so the items it reads
// again are the items acquired.
type fixedCases struct{ comments []Comment }

func (fixedCases) Name() string                                          { return "nop" }
func (fixedCases) Case(context.Context, string) (Case, error)            { return companionCase, nil }
func (f fixedCases) Comments(context.Context, string) ([]Comment, error) { return f.comments, nil }

// companionRequests are the provider's requests but the ledger's.
func companionRequests(f *modelfixture.Provider) []model.Request {
	var out []model.Request
	for _, r := range f.Requests() {
		if !isLedgerCall(r) {
			out = append(out, r)
		}
	}
	return out
}

// messageStarting is the index of the first of a request's messages that
// begins with prefix, -1 where none does.
func messageStarting(r model.Request, prefix string) int {
	for i, m := range r.Messages {
		if strings.HasPrefix(m.Text, prefix) {
			return i
		}
	}
	return -1
}

// TestCaseBlockNamesOlderComments: the case block holds the newest five
// comments in full and each older one on a line of its name, date and
// author, the engineer's exclusions applied, and is the same bytes made
// twice; the first call sends it as made, and the next turn has no case
// update, a comment named on its line counting as shown.
func TestCaseBlockNamesOlderComments(t *testing.T) {
	s, f := companionSession(t, nil)
	ctx := context.Background()
	var comments []Comment
	for i := 1; i <= 8; i++ {
		kind := AuthorCustomer
		if i%2 == 0 {
			kind = AuthorSupport
		}
		comments = append(comments, Comment{ID: fmt.Sprintf("c%d", i), Author: Party{Name: "author " + kind}, AuthorKind: kind,
			Created: time.Date(2026, 10, i, 9, 0, 0, 0, time.UTC), Body: fmt.Sprintf("Synthetic comment number %d.", i)})
	}
	if _, err := s.e.acquire(ctx, s.c, companionCase, comments); err != nil {
		t.Fatal(err)
	}
	if _, err := excludeComments(s.c, ExcludeComment, "c2"); err != nil {
		t.Fatal(err)
	}
	block, _, err := caseBlock(s.c, nil, s.e.config.SupportName())
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := caseBlock(s.c, nil, s.e.config.SupportName())
	if err != nil || again != block {
		t.Fatalf("the block made twice differs, %v:\n%s\n---\n%s", err, block, again)
	}
	if n := strings.Count(block, " · comment\n"); n != 5 {
		t.Errorf("%d comments in full, want 5:\n%s", n, block)
	}
	for i := 4; i <= 8; i++ {
		if !strings.Contains(block, fmt.Sprintf("Synthetic comment number %d.", i)) {
			t.Errorf("comment c%d is not in full", i)
		}
	}
	for _, i := range []int{1, 2, 3} {
		if strings.Contains(block, fmt.Sprintf("Synthetic comment number %d.", i)) {
			t.Errorf("comment c%d is in full", i)
		}
	}
	if !strings.Contains(block, "\n## Older comments\n\n2 older comments, newest first") {
		t.Errorf("no older comments' section:\n%s", block)
	}
	var older []string
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "- e") && strings.Contains(l, " · comment · ") {
			older = append(older, l)
		}
	}
	if len(older) != 2 || !strings.HasSuffix(older[0], " · comment · 2026-10-03 09:00 UTC · customer") || !strings.HasSuffix(older[1], " · comment · 2026-10-01 09:00 UTC · customer") {
		t.Errorf("the older comments' lines: %q", older)
	}
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if reqs[0].Messages[0].Text != block {
		t.Error("the first call's case block is not the block as made")
	}
	if i := messageStarting(reqs[len(reqs)-1], "# Case update"); i >= 0 {
		t.Errorf("the second turn was given a case update:\n%s", reqs[len(reqs)-1].Messages[i].Text)
	}
}

// TestCaseBlockShowsTheInventory: with an inventory the case block gives
// its text, each identity's evidence by its short name, in place of the
// attachment overview; without one, the overview.
func TestCaseBlockShowsTheInventory(t *testing.T) {
	const a = "00000000-0000-4000-8000-00000000000a"
	s, _ := inventoryCase(t, extracted("0010-must-gather", "0020-must-gather"),
		capture{"must-gather", "0010-must-gather/mg", identity("0010-must-gather/mg", "cluster", a, "2026-09-24T12:00:00Z", "", "")},
		capture{"must-gather", "0020-must-gather/mg", identity("0020-must-gather/mg", "cluster", a, "2026-09-01T00:00:00Z", "", "")},
	)
	overview, _, err := caseBlock(s.c, nil, s.e.config.SupportName())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(overview, " · attachments\n") || strings.Contains(overview, " · artifact inventory\n") {
		t.Errorf("the block without an inventory:\n%s", overview)
	}
	inv := readInventory(t, s)
	block, _, err := caseBlock(s.c, &inv, s.e.config.SupportName())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{" · artifact inventory\nArtifact inventory: 2 attachments\n",
		"  - must-gather 0020-must-gather/mg · cluster " + a + " · captured 2026-09-01T00:00:00Z · outdated, superseded by 0010-must-gather/mg\n",
		"; identity in e"} {
		if !strings.Contains(block, want) {
			t.Errorf("the block lacks %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, " · attachments\n") || strings.Contains(block, "ev-") {
		t.Errorf("the block keeps the overview or an evidence ID:\n%s", block)
	}
	if again, _, _ := caseBlock(s.c, &inv, s.e.config.SupportName()); again != block {
		t.Error("the block made twice differs")
	}
}

// TestLedgerInTheConversation: [C10] preparation reads the ledger beside
// the rest and the opening waits for it, so the ledger's message follows
// the case block and the preload before the opening's first call; a later
// run's new items and changed statuses are appended at the next turn's
// start as an update, after the case update, everything the opening sent
// unchanged before them.
func TestLedgerInTheConversation(t *testing.T) {
	read := updating(reader)
	s, f := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isLedgerCall(r) {
			// A slow ledger, which the opening waits for.
			time.Sleep(50 * time.Millisecond)
		}
		return read(ctx, r)
	})
	s.e.cases = fixedCases{companionComments}
	ctx := context.Background()
	var mu sync.Mutex
	var states []string
	p, err := s.Prepare(ctx, PrepareOptions{Call: true, Progress: func(st string) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, st)
	}})
	if err != nil || p.Ledger.Running || p.Ledger.Err != nil || p.Ledger.Run.Read != 2 || p.Ledger.Items != 2 || p.Ledger.Unread != 0 {
		t.Fatalf("prepared %+v, %v", p.Ledger, err)
	}
	all := strings.Join(states, "\n")
	for _, want := range []string{"reading the comments", "identifying the attachments", "the ledger: reading 2 of 2 comments"} {
		if !strings.Contains(all, want) {
			t.Errorf("the progress said no %q:\n%s", want, all)
		}
	}
	if states[len(states)-1] != "" {
		t.Errorf("the progress ended on %q", states[len(states)-1])
	}
	if _, err := s.Orient(ctx, nil); err != nil {
		t.Fatal(err)
	}
	opening := companionRequests(f)[0]
	if i := messageStarting(opening, "# Case ledger\n"); i != 3 || !strings.Contains(opening.Messages[i].Text, "Send a fresh must-gather.") {
		t.Fatalf("the ledger's message is at %d:\n%s", i, texts(opening))
	}
	if ask := opening.Messages[len(opening.Messages)-1].Text; ask != orientAsk {
		t.Errorf("the opening asked %q", ask)
	}

	more := append(append([]Comment(nil), companionComments...),
		Comment{ID: "c3", Author: Party{Name: "customer"}, AuthorKind: AuthorCustomer, Created: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), Body: "Here is the must-gather."})
	s.e.cases = fixedCases{more}
	if err := s.ReadComments(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Anything new?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := companionRequests(f)
	last := reqs[len(reqs)-1]
	if d := cmp.Diff(opening.Messages, last.Messages[:len(opening.Messages)]); d != "" {
		t.Errorf("the turn does not begin with what the opening sent (-opening +turn):\n%s", d)
	}
	update, ledgerUpdate := messageStarting(last, "# Case update"), messageStarting(last, "# Case ledger update")
	if update < len(opening.Messages) || ledgerUpdate != update+1 {
		t.Fatalf("the case update at %d, the ledger's at %d:\n%s", update, ledgerUpdate, texts(last))
	}
	got := last.Messages[ledgerUpdate].Text
	for _, want := range []string{"## New items\n\n- l3 · ", "customer provides must-gather-1.tar.gz: A must-gather of the cluster.", "## Changed\n\n- l2 · ", "Send a fresh must-gather. · answered by "} {
		if !strings.Contains(got, want) {
			t.Errorf("the ledger's update lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- l1 · ") {
		t.Errorf("the update repeats an unchanged item:\n%s", got)
	}
	// Nothing changed since: the next turn is given no ledger message.
	if _, err := s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs = companionRequests(f)
	if n := strings.Count(texts(reqs[len(reqs)-1]), "# Case ledger"); n != 2 {
		t.Errorf("%d ledger messages after a turn with nothing new, want 2", n)
	}
}

// TestOpeningGoesOnWithoutASlowLedger: [C10] past the bound the opening
// orients without the ledger and tells the model so; the ledger keeps
// reading, and its result is appended at the next turn, after everything
// the opening sent.
func TestOpeningGoesOnWithoutASlowLedger(t *testing.T) {
	defer func(d time.Duration) { ledgerWait = d }(ledgerWait)
	ledgerWait = 20 * time.Millisecond
	release := make(chan struct{})
	read := updating(reader)
	s, f := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isLedgerCall(r) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return read(ctx, r)
	})
	s.e.cases = fixedCases{companionComments}
	ctx := context.Background()
	p, err := s.Prepare(ctx, PrepareOptions{Call: true})
	if err != nil || !p.Ledger.Running || p.Ledger.Job == nil {
		t.Fatalf("prepared %+v, %v", p.Ledger, err)
	}
	if _, err := s.Orient(ctx, nil); err != nil {
		t.Fatal(err)
	}
	opening := companionRequests(f)[0]
	if i := messageStarting(opening, "# Case ledger"); i >= 0 {
		t.Errorf("the opening has the ledger's message at %d", i)
	}
	if ask := opening.Messages[len(opening.Messages)-1].Text; ask != orientAsk+" "+orientNoLedger {
		t.Errorf("the opening asked %q", ask)
	}
	close(release)
	if run, err := p.Ledger.Job.Result(); err != nil || run.Read != 2 {
		t.Fatalf("the ledger's run: %+v, %v", run, err)
	}
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := companionRequests(f)
	last := reqs[len(reqs)-1]
	if d := cmp.Diff(opening.Messages, last.Messages[:len(opening.Messages)]); d != "" {
		t.Errorf("the turn does not begin with what the opening sent (-opening +turn):\n%s", d)
	}
	if i := messageStarting(last, "# Case ledger\n"); i < len(opening.Messages) {
		t.Errorf("the ledger's message at %d, want after the opening's %d messages:\n%s", i, len(opening.Messages), texts(last))
	}
}

// TestPrepareCancelsTheLedger: cancelling preparation cancels the ledger
// it began.
func TestPrepareCancelsTheLedger(t *testing.T) {
	read := updating(reader)
	s, _ := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isLedgerCall(r) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return read(ctx, r)
	})
	s.e.cases = fixedCases{companionComments}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := s.Prepare(ctx, PrepareOptions{Call: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare: %v", err)
	}
	j := s.ledgerJob()
	select {
	case <-j.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the ledger still runs")
	}
	if _, err := j.Result(); !errors.Is(err, context.Canceled) {
		t.Errorf("the ledger ended with %v", err)
	}
}

// TestOpeningBudget: the opening turn stops at its eight tool calls: the
// rest are refused, the model told to answer.
func TestOpeningBudget(t *testing.T) {
	s, f := companionSession(t, func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if r.NoCalls {
			return modelfixture.Text("oriented"), nil
		}
		return modelfixture.Calling(call("a", "find", map[string]any{"path": "namespaces"}), call("b", "find", map[string]any{"path": "namespaces"}),
			call("c", "find", map[string]any{"path": "namespaces"})), nil
	})
	reply, err := s.Orient(context.Background(), nil)
	if err != nil || reply.Text != "oriented" {
		t.Fatalf("reply %q, %v", reply.Text, err)
	}
	reqs := f.Requests()
	last := reqs[len(reqs)-1]
	ran, refused := 0, 0
	for _, m := range last.Messages {
		for _, r := range m.Results {
			switch {
			case strings.Contains(r.Text, "tool budget"):
				refused++
			case r.CallID != "" && !strings.HasPrefix(r.CallID, "preload-"):
				ran++
			}
		}
	}
	if ran != 8 || refused != 1 || !last.NoCalls {
		t.Errorf("%d calls ran, %d refused, the last request asking for an answer %v", ran, refused, last.NoCalls)
	}
}

// TestPrepareWithoutCall: preparing without a model call, as --no-open
// and a reopened case do, makes none, the ledger's included, and says what
// the ledger has yet to read.
func TestPrepareWithoutCall(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	s.e.cases = fixedCases{companionComments}
	p, err := s.Prepare(context.Background(), PrepareOptions{})
	if err != nil || !p.Ledger.NotAsked || p.Ledger.Job != nil || p.Ledger.Unread != 2 {
		t.Fatalf("prepared %+v, %v", p.Ledger, err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("%d model calls", n)
	}
}

// TestPrepareBarredMakesNoCall: on a barred case preparation starts no
// ledger and says why, and the opening that follows is refused: the
// provider is never called and no ledger call is recorded.
func TestPrepareBarredMakesNoCall(t *testing.T) {
	_, s, f, _ := barredCase(t, "1000000001", updating(reader), "1000000001")
	s.e.cases = fixedCases{companionComments}
	ctx := context.Background()
	p, err := s.Prepare(ctx, PrepareOptions{Call: true})
	if err != nil || p.Ledger.Job != nil || !strings.Contains(p.Ledger.Blocked, "barred") {
		t.Fatalf("prepared %+v, %v", p.Ledger, err)
	}
	_, err = s.Orient(ctx, nil)
	refusedAsBarred(t, "the opening", err)
	if n := len(f.Requests()); n != 0 {
		t.Errorf("%d model calls", n)
	}
	var ledgerRecords int
	if err := s.c.st.DB().QueryRow(`SELECT count(*) FROM invocations WHERE task_type = ?`, LedgerTask).Scan(&ledgerRecords); err != nil || ledgerRecords != 0 {
		t.Errorf("%d ledger records, %v", ledgerRecords, err)
	}
}

// TestReopenState: a reopened conversation's state says what it has yet
// to be shown, and that the provider's cache has expired only past the
// provider's lifetime and only for a large conversation; a provider with
// no cache has none to expire.
func TestReopenState(t *testing.T) {
	s, _ := companionSession(t, nil)
	s.e.cases = fixedCases{companionComments}
	s.e.config.Reasoning.Models["gemini-model"] = config.Model{Provider: "vertex-gemini", Model: "gemini-model"}
	ctx := context.Background()
	if _, ok, err := s.ReopenState(ctx); ok || err != nil {
		t.Fatalf("a case with no conversation: %v, %v", ok, err)
	}
	if _, err := s.Talk(ctx, "Read this: "+strings.Repeat("word ", 25_000), nil); err != nil {
		t.Fatal(err)
	}
	called := s.c.now()
	r, ok, err := s.ReopenState(ctx)
	if err != nil || !ok || r.NewComments != 0 || r.Attachments || r.LedgerUnread != 2 || !r.Large || r.CacheLifetime != 0 || r.CacheExpired || !r.LastCall.Equal(called) {
		t.Fatalf("on the fixture: %+v, %v", r, err)
	}
	if err := s.ChooseCompanionModel("gemini-model", ""); err != nil {
		t.Fatal(err)
	}
	s.c.now = func() time.Time { return called.Add(geminiCacheTTL - time.Second) }
	if r, _, _ = s.ReopenState(ctx); r.CacheLifetime != geminiCacheTTL || r.CacheExpired {
		t.Errorf("within the lifetime: %+v", r)
	}
	s.c.now = func() time.Time { return called.Add(geminiCacheTTL + time.Second) }
	if r, _, _ = s.ReopenState(ctx); !r.CacheExpired || !r.Large {
		t.Errorf("past the lifetime: %+v", r)
	}
	s.e.cases = fixedCases{append(append([]Comment(nil), companionComments...),
		Comment{ID: "c3", Author: Party{Name: "customer"}, Created: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), Body: "Still failing."})}
	if err := s.ReadComments(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _, _ = s.ReopenState(ctx); r.NewComments != 1 || r.LedgerUnread != 3 {
		t.Errorf("after a new comment: %+v", r)
	}
}

// TestCompactionGivesTheLedger: [C10] a compacted conversation's case
// block is followed by the whole ledger, which its next turn counts as
// seen.
func TestCompactionGivesTheLedger(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := context.Background()
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := companionRequests(f)
	last := reqs[len(reqs)-1]
	if i := messageStarting(last, "# Case ledger\n"); i != 1 || messageStarting(last, "A handoff") != 2 {
		t.Errorf("the compacted conversation begins:\n%s", texts(last))
	}
	if n := strings.Count(texts(last), "# Case ledger"); n != 1 {
		t.Errorf("%d ledger messages, want the one after the block", n)
	}
}

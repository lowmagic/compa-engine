package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/ledger"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The ledger's tests run on the companion's stub case, whose comments are
// synthetic: c1, the customer saying when it started, and c2, support
// asking for a fresh must-gather.

// batchComment is a comment as a ledger request gives it: its name, its
// ID at the source and its body.
type batchComment struct{ name, id, body string }

// ledgerBatchOf reads a ledger request's comments, oldest first.
func ledgerBatchOf(r model.Request) []batchComment {
	if len(r.Messages) == 0 {
		return nil
	}
	_, rest, ok := strings.Cut(r.Messages[0].Text, "\n# Comments to read\n")
	if !ok {
		return nil
	}
	var out []batchComment
	for _, sec := range strings.Split(rest, "\n## ")[1:] {
		head, content, _ := strings.Cut(sec, "\n")
		name, _, _ := strings.Cut(head, " · ")
		cm := batchComment{name: name}
		fields, body, _ := strings.Cut(content, "\n\n")
		for _, l := range strings.Split(fields, "\n") {
			if v, ok := strings.CutPrefix(l, "comment: "); ok {
				cm.id = v
			}
		}
		cm.body = strings.TrimSpace(body)
		out = append(out, cm)
	}
	return out
}

// isLedgerCall reports whether a request is the ledger's.
func isLedgerCall(r model.Request) bool {
	return r.System == ledger.Prompt() && len(r.Tools) == 1 && r.Tools[0].Name == ledger.ToolName
}

// updating answers a ledger call with update's arguments for its comments
// and the ledger so far, and any other call with text.
func updating(update func(batch []batchComment, held string) map[string]any) func(context.Context, model.Request) (*modelfixture.Answer, error) {
	return func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		if !isLedgerCall(r) {
			return modelfixture.Text("ok"), nil
		}
		held, _, _ := strings.Cut(r.Messages[0].Text, "\n# Comments to read\n")
		args := update(ledgerBatchOf(r), held)
		for _, k := range []string{"items", "updates"} {
			if v, _ := args[k].([]any); v == nil {
				args[k] = []any{}
			}
		}
		return modelfixture.Calling(call("u1", ledger.ToolName, args)), nil
	}
}

// reader is a fixture ledger model: an ask for a comment asking for a
// must-gather, what was provided for one sending it, settling an open ask
// for one; a newer capture superseding what an earlier comment provided;
// an edit of c1 withdrawing what c1 gave; none for an automated comment;
// nothing at all for a comment saying "nothing"; a report for the rest.
func reader(batch []batchComment, held string) map[string]any {
	var items, updates, none []any
	heldItem := func(prefix string) string {
		for _, l := range strings.Split(held, "\n") {
			if strings.HasPrefix(strings.SplitN(l, " ", 2)[0], "l") && strings.Contains(l, prefix) {
				return strings.SplitN(l, " ", 2)[0]
			}
		}
		return ""
	}
	for _, cm := range batch {
		switch {
		case strings.Contains(held, cm.name+" is an edited version"):
			if id := heldItem("reports (customer, from "); id != "" {
				updates = append(updates, map[string]any{"item": id, "status": "withdrawn", "because": cm.name})
			}
			items = append(items, map[string]any{"kind": "reports", "who": "customer", "text": "It began a week before the upgrade.", "comment": cm.name, "status": "none"})
		case strings.Contains(cm.body, "Requested a fresh must-gather"):
			items = append(items, map[string]any{"kind": "asks", "who": "support", "text": "Send a fresh must-gather.", "comment": cm.name, "status": "open"})
		case strings.Contains(cm.body, "Here is the must-gather"):
			items = append(items, map[string]any{"kind": "provides", "who": "customer", "text": "A must-gather of the cluster.", "comment": cm.name, "attachment": "must-gather-1.tar.gz", "status": "none"})
			if id := heldItem("asks (support, from "); id != "" && strings.Contains(held, id+" asks (support, from ") && !strings.Contains(held, "answered") {
				updates = append(updates, map[string]any{"item": id, "status": "answered", "because": cm.name})
			}
		case strings.Contains(cm.body, "A newer must-gather"):
			items = append(items, map[string]any{"kind": "provides", "who": "customer", "text": "A newer must-gather.", "comment": cm.name, "attachment": "must-gather-2.tar.gz", "status": "none"})
			if id := heldItem("provides (customer, from "); id != "" {
				updates = append(updates, map[string]any{"item": id, "status": "superseded", "because": cm.name, "note": "a newer capture was sent"})
			}
		case strings.Contains(cm.body, "skipped processing"):
			none = append(none, map[string]any{"comment": cm.name, "why": "automated"})
		case strings.Contains(cm.body, "nothing"):
		default:
			items = append(items, map[string]any{"kind": "reports", "who": "customer", "text": "It started after the upgrade.", "comment": cm.name, "status": "none"})
		}
	}
	out := map[string]any{"items": items, "updates": updates}
	if none != nil {
		out["none"] = none
	}
	return out
}

// addComments acquires the case again with its comments and those given.
func addComments(t *testing.T, s *Session, more ...Comment) {
	t.Helper()
	if _, err := s.e.acquire(context.Background(), s.c, companionCase, append(slices.Clone(companionComments), more...)); err != nil {
		t.Fatal(err)
	}
}

// ledgerCalls are the provider's requests that were the ledger's.
func ledgerCalls(f *modelfixture.Provider) []model.Request {
	var out []model.Request
	for _, r := range f.Requests() {
		if isLedgerCall(r) {
			out = append(out, r)
		}
	}
	return out
}

func mustItems(t *testing.T, s *Session) []LedgerItem {
	t.Helper()
	items, err := s.Ledger(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// TestLedgerItemsAndSettling: [C8] the ledger reads the comments it has
// not, oldest first, in a conversation of its own with its own prompt and
// one tool, recorded as comments.ledger; names its items l1, l2, … in the
// thread's order; makes no call with nothing new; a later run reads only
// the new comments, settles the earlier ask a new comment answers, and
// marks what was provided superseded by a newer capture, its attachment
// kept; and the rendering says so, by comment name.
func TestLedgerItemsAndSettling(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := context.Background()
	if n, err := s.LedgerUnread(ctx); err != nil || n != 2 {
		t.Fatalf("unread %d, %v; want the stub's 2 comments", n, err)
	}
	run, err := s.UpdateLedger(ctx, nil)
	if err != nil || run.Calls != 1 || run.Read != 2 || run.Added != 2 {
		t.Fatalf("first run %+v, %v", run, err)
	}
	reqs := ledgerCalls(f)
	if len(reqs) != 1 || len(f.Requests()) != 1 {
		t.Fatalf("%d ledger calls of %d, want the one", len(reqs), len(f.Requests()))
	}
	if msg := reqs[0].Messages[0].Text; !strings.Contains(msg, "The ledger is empty") || strings.Index(msg, "Started right after") > strings.Index(msg, "Requested a fresh") {
		t.Errorf("the first request:\n%s", msg)
	}
	var task, kind string
	if err := s.c.st.DB().QueryRow(`SELECT i.task_type, c.kind FROM invocations i JOIN companion_conversations c ON c.id = json_extract(i.record, '$.conversation_id')`).Scan(&task, &kind); err != nil || task != LedgerTask || kind != "ledger" {
		t.Errorf("the call's record: task %q in a %q conversation, %v", task, kind, err)
	}
	items := mustItems(t, s)
	if len(items) != 2 || items[0].ID != "l1" || items[0].Kind != ledger.Reports || items[1].ID != "l2" || !items[1].Open() || !strings.HasPrefix(items[1].Comment, "e") {
		t.Fatalf("items %+v", items)
	}
	if run, err := s.UpdateLedger(ctx, nil); err != nil || run.Calls != 0 || len(f.Requests()) != 1 {
		t.Errorf("a run with nothing new: %+v, %v, %d calls", run, err, len(f.Requests()))
	}

	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	addComments(t, s, Comment{ID: "c3", Author: Party{Name: "customer"}, AuthorKind: AuthorCustomer, Created: day, Body: "Here is the must-gather."})
	run, err = s.UpdateLedger(ctx, nil)
	if err != nil || run.Read != 1 || run.Added != 1 || run.Changed != 1 {
		t.Fatalf("the run reading c3: %+v, %v", run, err)
	}
	last := ledgerCalls(f)[1].Messages[0].Text
	if strings.Contains(last, "Started right after") || strings.Contains(last, "Requested a fresh") || !strings.Contains(last, "l2 asks (support, from ") {
		t.Errorf("the second run read more than the new comment, or not the ledger so far:\n%s", last)
	}
	items = mustItems(t, s)
	if items[1].Status != ledger.Answered || items[1].StatusBy != items[2].Comment || items[1].Open() {
		t.Errorf("l2 after c3: %+v", items[1])
	}
	if items[2].Kind != ledger.Provides || items[2].Attachment != "must-gather-1.tar.gz" || items[2].Status != "" {
		t.Errorf("l3: %+v", items[2])
	}

	addComments(t, s,
		Comment{ID: "c3", Author: Party{Name: "customer"}, AuthorKind: AuthorCustomer, Created: day, Body: "Here is the must-gather."},
		Comment{ID: "c4", Author: Party{Name: "customer"}, AuthorKind: AuthorCustomer, Created: day.Add(24 * time.Hour), Body: "A newer must-gather is attached, taken after the restart."})
	if run, err = s.UpdateLedger(ctx, nil); err != nil || run.Read != 1 || run.Changed != 1 {
		t.Fatalf("the run reading c4: %+v, %v", run, err)
	}
	items = mustItems(t, s)
	if len(items) != 4 || items[2].Status != ledger.Superseded || items[2].StatusBy != items[3].Comment || items[2].StatusNote != "a newer capture was sent" {
		t.Fatalf("l3 after c4: %+v", items)
	}
	if from := items[3].From; from.AuthorName != "customer" || from.AuthorKind != AuthorCustomer || !from.Created.Equal(day.Add(24*time.Hour)) || from.Body != "" {
		t.Errorf("l4's comment header: %+v", from)
	}
	text := ledgerText(items, nil, "Acme Care", "Acme Agent")
	for _, want := range []string{
		"# Case ledger", "## Reported and provided, by comment",
		"- l3 · customer provides must-gather-1.tar.gz: A must-gather of the cluster. (superseded by " + items[3].Comment + ": a newer capture was sent)",
		"## Settled", "- l2 · " + items[1].Comment + " · Acme Care asks, answered by " + items[2].Comment + ": Send a fresh must-gather.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the rendering lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Open asks") {
		t.Errorf("an answered ask is shown open:\n%s", text)
	}
}

// TestLedgerWithdrawsEditedItems: [C8] a comment saved again with its text
// unchanged is not read again; an edit is, told what its earlier version
// gave, and withdraws what it no longer says, the item's text kept.
func TestLedgerWithdrawsEditedItems(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := context.Background()
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	saved := slices.Clone(companionComments)
	saved[0].Updated = saved[0].Created.Add(time.Hour)
	if _, err := s.e.acquire(ctx, s.c, companionCase, saved); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LedgerUnread(ctx); err != nil || n != 0 {
		t.Fatalf("unread after a save with the text unchanged: %d, %v", n, err)
	}
	edited := slices.Clone(saved)
	edited[0].Updated, edited[0].Body = saved[0].Created.Add(2*time.Hour), "Not the upgrade after all: it began a week before."
	if _, err := s.e.acquire(ctx, s.c, companionCase, edited); err != nil {
		t.Fatal(err)
	}
	run, err := s.UpdateLedger(ctx, nil)
	if err != nil || run.Read != 1 || run.Withdrawn != 1 || run.Added != 1 || len(ledgerCalls(f)) != 2 {
		t.Fatalf("the run reading the edit: %+v, %v", run, err)
	}
	if msg := ledgerCalls(f)[1].Messages[0].Text; !strings.Contains(msg, "# Edited comments") || !strings.Contains(msg, "which gave l1.") {
		t.Errorf("the edit's request:\n%s", msg)
	}
	items := mustItems(t, s)
	if items[0].Status != ledger.Withdrawn || items[0].StatusBy != items[2].Comment || items[0].Text != "It started after the upgrade." || items[0].Open() {
		t.Errorf("l1 after the edit: %+v", items[0])
	}
	if len(items) != 3 || items[2].CommentID == items[0].CommentID {
		t.Errorf("items %+v", items)
	}
	if text := ledgerText(items, nil, s.SupportName(), s.SupportRole()); !strings.Contains(text, "- l1 · "+items[0].Comment+" · customer reports, withdrawn by "+items[2].Comment) {
		t.Errorf("the rendering:\n%s", text)
	}
}

// TestLedgerAccountsForEveryComment: [C8] every comment read is accounted
// for: by its items, by none with why, or, where the update says nothing
// of it, recorded so by the engine, never taken for a comment with
// nothing in it. The engineer's view numbers every comment as the case
// does, oldest first, a comment not read yet among them.
func TestLedgerAccountsForEveryComment(t *testing.T) {
	s, _ := companionSession(t, updating(reader))
	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	more := []Comment{
		{ID: "c3", Automated: true, Created: day, Body: "Automated triage skipped processing: nothing to do."},
		{ID: "c4", Author: Party{Name: "customer"}, Created: day.Add(time.Hour), Body: "Thanks, nothing more from us today."},
	}
	addComments(t, s, more...)
	run, err := s.UpdateLedger(context.Background(), nil)
	if err != nil || run.Read != 4 || run.Added != 2 || run.None != 1 || run.Unaccounted != 1 {
		t.Fatalf("run %+v, %v", run, err)
	}
	addComments(t, s, append(more, Comment{ID: "c5", Author: Party{Name: "customer"}, Created: day.Add(2 * time.Hour), Body: "One more synthetic comment."})...)
	comments, err := s.LedgerComments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, cm := range comments {
		got = append(got, fmt.Sprintf("c#%d %s %s", cm.Number, cm.Head.ID, cm.Outcome))
	}
	want := []string{"c#1 c1 items", "c#2 c2 items", "c#3 c3 none: automated", "c#4 c4 unaccounted", "c#5 c5 unread"}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("comments (-want +got):\n%s", d)
	}
}

// TestLedgerMarksTooLongCommentNotShown: [C8] a comment too long for a
// request to show is given to no ledger call and marked not shown by the
// engine, never read with nothing in it: the run says so, the engineer's
// view and the conversation's ledger name it as not read, and it is not
// offered again.
func TestLedgerMarksTooLongCommentNotShown(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := context.Background()
	day := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	body := strings.Repeat("A synthetic log line from the customer's node.\n", 1500) + "Please also send the sosreport of worker-2."
	addComments(t, s, Comment{ID: "c3", Author: Party{Name: "customer"}, Created: day, Body: body})
	run, err := s.UpdateLedger(ctx, nil)
	if err != nil || run.Read != 2 || run.NotShown != 1 {
		t.Fatalf("run %+v, %v; want 2 read and 1 not shown", run, err)
	}
	comments, err := s.LedgerComments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, cm := range comments {
		if cm.Head.ID == "c3" {
			name = cm.Name
			if cm.Outcome != OutcomeNotShown {
				t.Errorf("c3's outcome %q, want %q", cm.Outcome, OutcomeNotShown)
			}
		}
	}
	for _, r := range f.Requests() {
		for _, cm := range ledgerBatchOf(r) {
			if cm.name == name {
				t.Errorf("a ledger call was given %s", name)
			}
		}
	}
	if n, err := s.LedgerUnread(ctx); err != nil || n != 0 {
		t.Errorf("unread %d, %v; want none", n, err)
	}
	line := "- " + name + " · c#3 · not read: too long to show the ledger's model"
	conv, err := s.ledgerMessage(ctx, nil)
	if err != nil || !strings.Contains(conv, "## Not read by the ledger") || !strings.Contains(conv, line) {
		t.Errorf("the conversation's ledger, %v:\n%s", err, conv)
	}
	shown := []companion.Message{{Message: model.Message{Text: conv}, Kind: companion.KindUpdate}}
	if again, err := s.ledgerMessage(ctx, shown); err != nil || strings.Contains(again, line) {
		t.Errorf("shown again, %v:\n%s", err, again)
	}
	if view := RenderLedger(comments, mustItems(t, s), time.UTC, s.SupportName(), s.SupportRole()); !strings.Contains(view, "c#3 · customer · Oct 3, 2026, 9:00 AM UTC · "+name+" · not read: too long to show the ledger's model") {
		t.Errorf("the engineer's view:\n%s", view)
	}
}

// TestLedgerNumbersAfterAnItemAddedMeanwhile: an item added while the
// ledger's model is called is kept, and the run's items take the numbers
// after it: numbers come from what the run's own transaction sees, not
// from the ledger read before the call.
func TestLedgerNumbersAfterAnItemAddedMeanwhile(t *testing.T) {
	var s *Session
	var once sync.Once
	var addErr error
	read := updating(reader)
	s, _ = companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		once.Do(func() {
			addErr = insertLedgerItem(ctx, s.c.st.DB(), s.c.id, 1, LedgerItem{ID: "l1", Kind: ledger.Commits, Who: AuthorSupport,
				Text: "Support will read the must-gather.", CommentID: "ev-elsewhere", Status: ledger.Pending, MadeBy: "engineer"}, "2026-11-06T12:00:00Z")
		})
		return read(ctx, r)
	})
	run, err := s.UpdateLedger(context.Background(), nil)
	if addErr != nil || err != nil || run.Added != 2 {
		t.Fatalf("the run after an item was added: %+v, %v, %v", run, err, addErr)
	}
	items, err := ledgerItems(s.c)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if fmt.Sprint(ids) != "[l1 l2 l3]" {
		t.Errorf("items %v, want the one added and the run's after it", ids)
	}
}

// TestLedgerBarredMakesNoCall: [C8] on a barred case, and on one that
// cannot be checked, the ledger makes no call, its refusal recorded as
// barred_account under its own task, and reads nothing.
func TestLedgerBarredMakesNoCall(t *testing.T) {
	for _, number := range []string{"1000000001", ""} {
		_, s, f, _ := barredCase(t, number, updating(reader), "1000000001")
		ctx := context.Background()
		_, err := s.UpdateLedger(ctx, nil)
		refusedAsBarred(t, "the ledger, account "+number, err)
		if len(f.Requests()) != 0 {
			t.Errorf("account %q: the model was called %d times", number, len(f.Requests()))
		}
		var task, outcome string
		if err := s.c.st.DB().QueryRow(`SELECT task_type, outcome FROM invocations`).Scan(&task, &outcome); err != nil || task != LedgerTask || outcome != companion.OutcomeBarred {
			t.Errorf("account %q: the record %q %q, %v", number, task, outcome, err)
		}
		if n, err := s.LedgerUnread(ctx); err != nil || n != 2 {
			t.Errorf("account %q: unread %d, %v; want none read", number, n, err)
		}
	}
}

// TestLedgerLeavesOutExcludedComments: [C8] an item read from a comment
// the engineer has since left out of what models read is in neither the
// ledger shown nor its rendering nor the next run's request; numbering
// goes on after it.
func TestLedgerLeavesOutExcludedComments(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := context.Background()
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if text := ledgerText(mustItems(t, s), nil, s.SupportName(), s.SupportRole()); !strings.Contains(text, "Send a fresh must-gather.") || !strings.Contains(text, "Owed by the customer") {
		t.Fatalf("the rendering before c2 is left out:\n%s", text)
	}
	if _, err := excludeComments(s.c, ExcludeComment, "c2"); err != nil {
		t.Fatal(err)
	}
	items := mustItems(t, s)
	if len(items) != 1 || items[0].ID != "l1" {
		t.Errorf("items with c2 left out: %+v", items)
	}
	if text := ledgerText(items, nil, s.SupportName(), s.SupportRole()); strings.Contains(text, "Send a fresh must-gather.") {
		t.Errorf("the rendering carries c2's item:\n%s", text)
	}
	addComments(t, s, Comment{ID: "c3", Author: Party{Name: "customer"}, Created: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), Body: "It also fails on worker-3."})
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if msg := ledgerCalls(f)[1].Messages[0].Text; strings.Contains(msg, "l2 ") || !strings.Contains(msg, "l1 reports") {
		t.Errorf("the next run is shown:\n%s", msg)
	}
	if items = mustItems(t, s); len(items) != 2 || items[1].ID != "l3" {
		t.Errorf("the next run's items %+v", items)
	}
}

// TestLedgerCountsInTotalUsage: the ledger's calls count in the case's
// total, as the status bar shows it, beside the companion's.
func TestLedgerCountsInTotalUsage(t *testing.T) {
	s, _ := companionSession(t, updating(reader))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What changed?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	total, err := s.CompanionTotalUsage()
	if err != nil || total.Invocations != 2 {
		t.Errorf("the total counts %d calls, %v; want the turn's and the ledger's", total.Invocations, err)
	}
}

// TestLedgerRetriesOnce: a model answering in text is asked once more to
// call ledger_update; an update refused whole twice ends the batch with
// nothing read, after two calls.
func TestLedgerRetriesOnce(t *testing.T) {
	read := updating(reader)
	s, f := companionSession(t, func(ctx context.Context, r model.Request) (*modelfixture.Answer, error) {
		if isLedgerCall(r) && len(r.Messages) == 1 {
			return modelfixture.Text("Here is the ledger: nothing much."), nil
		}
		return read(ctx, r)
	})
	ctx := context.Background()
	if run, err := s.UpdateLedger(ctx, nil); err != nil || run.Added != 2 || len(f.Requests()) != 2 {
		t.Fatalf("after a text answer: %+v, %v, %d calls", run, err, len(f.Requests()))
	}
	if last := f.Requests()[1]; last.Messages[len(last.Messages)-1].Text != ledgerRetry {
		t.Errorf("the retry asked %q", last.Messages[len(last.Messages)-1].Text)
	}

	s, f = companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return modelfixture.Calling(call("u1", ledger.ToolName, map[string]any{"items": []any{}})), nil
	})
	_, err := s.UpdateLedger(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "refused") || len(f.Requests()) != 2 {
		t.Errorf("an update refused twice: %v, %d calls", err, len(f.Requests()))
	}
	if n, _ := s.LedgerUnread(ctx); n != 2 {
		t.Errorf("unread %d after a refused batch, want 2", n)
	}
}

// TestCaseUpdateShowsCommentsTheLedgerNamed: a comment the ledger read,
// and so named, before the conversation was shown it still reaches the
// conversation as a case update: a name is not a showing.
func TestCaseUpdateShowsCommentsTheLedgerNamed(t *testing.T) {
	s, f := companionSession(t, updating(reader))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What changed?", nil); err != nil {
		t.Fatal(err)
	}
	addComments(t, s, Comment{ID: "c3", Author: Party{Name: "customer"}, Created: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), Body: "It also fails on worker-3."})
	if _, err := s.UpdateLedger(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if last := texts(reqs[len(reqs)-1]); !strings.Contains(last, "# Case update") || !strings.Contains(last, "It also fails on worker-3.") {
		t.Errorf("the turn after the ledger read c3 was not shown it:\n%s", last)
	}
}

// TestRenderLedgerBounded: past its bound the rendering leaves out the
// oldest reports and says how many, and keeps every open ask.
func TestRenderLedgerBounded(t *testing.T) {
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	items := []LedgerItem{{ID: "l1", Kind: ledger.Asks, Who: AuthorSupport, Text: "Send a fresh must-gather.", Comment: "e1", Status: ledger.Open, From: CaseComment{Created: day}}}
	for i := 2; i <= 400; i++ {
		items = append(items, LedgerItem{ID: fmt.Sprintf("l%d", i), Kind: ledger.Reports, Who: AuthorCustomer,
			Text: strings.Repeat("a synthetic report ", 10), Comment: fmt.Sprintf("e%d", i), CommentID: fmt.Sprintf("ev-%d", i), From: CaseComment{Created: day.Add(time.Duration(i) * time.Hour)}})
	}
	text := ledgerText(items, nil, "Acme Care", "Acme Agent")
	if len(text) > ledgerRenderMax+200 || !strings.Contains(text, "left out for length") || !strings.Contains(text, "- l1 · e1 · Acme Care asks: Send a fresh must-gather.") {
		t.Errorf("%d bytes:\n%s", len(text), text)
	}
	if !strings.Contains(text, "### e400 ") || strings.Contains(text, "### e2 ") {
		t.Error("the newest reports were left out, or the oldest kept")
	}
}

// TestRenderLedgerByComment: the engineer's view lists every comment in
// the case's order, headed by its number, author, who they are, visibility,
// time in the zone given and evidence name, each with its items and their
// statuses by comment number; a comment that gave none is one line saying
// why; what the source left out of a header is left out.
func TestRenderLedgerByComment(t *testing.T) {
	at := time.Date(2026, 10, 29, 15, 27, 0, 0, time.UTC)
	est := time.FixedZone("EST", -5*60*60)
	jo := CaseComment{AuthorName: "Jo Synthetic", AuthorKind: AuthorCustomer, Visibility: string(VisibilityPublic), Created: at}
	sam := CaseComment{AuthorName: "Sam Synthetic", AuthorKind: AuthorSupport, Visibility: string(VisibilityPrivate), Created: at.Add(time.Hour)}
	bot := CaseComment{AuthorName: "A Bot", AuthorKind: AuthorSupport, Automated: true, Visibility: string(VisibilityPartner), Created: at.Add(2 * time.Hour)}
	comments := []LedgerComment{
		{Number: 1, Name: "e1", Head: sam, Outcome: "none: courtesy"},
		{Number: 2, Name: "e5", Head: sam, Outcome: OutcomeItems},
		{Number: 3, Name: "e2", Head: jo, Outcome: OutcomeItems},
		{Number: 4, Name: "e3", Head: bot, Outcome: "none: nothing_new"},
		{Number: 5, Name: "e4", Head: CaseComment{}, Outcome: OutcomeUnread},
	}
	items := []LedgerItem{
		{ID: "l1", Kind: ledger.Asks, Who: AuthorSupport, Text: "Was the SFP swapped at runtime?", Comment: "e5", CommentID: "ev-5", Number: 2, From: sam, Status: ledger.Answered, StatusBy: "e2", StatusByNumber: 3},
		{ID: "l2", Kind: ledger.Reports, Who: AuthorCustomer, Text: "Yes, the SFP is hot-swappable.", Comment: "e2", CommentID: "ev-2", Number: 3, From: jo},
		{ID: "l3", Kind: ledger.Asks, Who: AuthorCustomer, Text: "When will engineering reply?", Comment: "e2", CommentID: "ev-2", Number: 3, From: jo, Status: ledger.Open},
	}
	text := RenderLedger(comments, items, est, "Acme Care", "Acme Agent")
	want := `## Open asks

Awaiting Acme Care:
- [ask] When will engineering reply? (c#3)

## Comments

c#1 · Sam Synthetic · Acme Agent · Private · Oct 29, 2026, 11:27 AM EST · e1 · nothing: courtesy

### c#2 · Sam Synthetic · Acme Agent · Private · Oct 29, 2026, 11:27 AM EST · e5

- [ask] Was the SFP swapped at runtime? (answered in c#3)

### c#3 · Jo Synthetic · Customer · Public · Oct 29, 2026, 10:27 AM EST · e2

- [report] Yes, the SFP is hot-swappable.
- [ask] When will engineering reply?
- settles c#2 ask (answered)

c#4 · A Bot · Automated · Partner · Oct 29, 2026, 12:27 PM EST · e3 · nothing: nothing new
c#5 · e4 · not read yet
`
	if _, got, ok := strings.Cut(text, "## Open asks"); !ok || "## Open asks"+got != want {
		t.Errorf("the view (-want +got):\n%s", cmp.Diff(want, "## Open asks"+got))
	}
	// Legacy stored rows with duplicate verbs and extra support whitespace
	// render cleanly with compact tags, grouped commitments and no lN IDs.
	legacy := RenderLedger(comments[1:3], []LedgerItem{
		{ID: "l7", Kind: ledger.Asks, Who: AuthorSupport, Text: "asks why the PG count was increased and whether OSDs were added to the cluster", Comment: "e5", Number: 2, From: sam, Status: ledger.Answered, StatusBy: "e2", StatusByNumber: 3},
		{ID: "l8", Kind: ledger.Reports, Who: AuthorCustomer, Text: "customer reports: reports no new OSD was added when increasing PG count from 256 to 1024", Comment: "e2", Number: 3, From: jo},
		{ID: "l9", Kind: ledger.Commits, Who: AuthorSupport, Text: "Red  Hat commits to review the OSD map", Due: "Friday", Comment: "e5", Number: 2, From: sam, Status: ledger.Pending},
	}, est, "Red  Hat", "Associate")
	for _, wantSub := range []string{
		"## Pending commitments\n\nBy Red Hat:\n- [commit] review the OSD map (due Friday) (c#2)\n",
		"- [ask] why the PG count was increased and whether OSDs were added to the cluster (answered in c#3)",
		"- [report] no new OSD was added when increasing PG count from 256 to 1024",
	} {
		if !strings.Contains(legacy, wantSub) {
			t.Errorf("legacy ledger view lacks %q:\n%s", wantSub, legacy)
		}
	}
	if strings.Contains(legacy, "l7") || strings.Contains(legacy, "Red  Hat") || strings.Contains(legacy, "asks why") || strings.Contains(legacy, "reports no") {
		t.Errorf("legacy ledger view kept lN ID, extra spaces or duplicate verb:\n%s", legacy)
	}
	// The conversation's text names each comment by its evidence name first,
	// then its number, in UTC.
	if conv := ledgerText(items, nil, "Acme Care", "Acme Agent"); !strings.Contains(conv, "### e2 · c#3 · Jo Synthetic · Customer · Public · Oct 29, 2026, 3:27 PM UTC\n") || !strings.Contains(conv, "Owed by Acme Care:") {
		t.Errorf("the conversation's ledger:\n%s", conv)
	}
	// A first comment with items follows the heading after one blank row,
	// as one without does.
	if text := RenderLedger(comments[1:], items[:1], est, "Acme Care", "Acme Agent"); !strings.Contains(text, "## Comments\n\n### c#2 ") {
		t.Errorf("the view with a first comment holding items:\n%s", text)
	}
}

func TestConfiguredSupportLedgerMessages(t *testing.T) {
	s, _ := companionSession(t, script())
	s.e.config.Companion.SupportName = "Acme Care"
	s.e.config.Companion.SupportRole = "Acme Agent"
	ctx := context.Background()
	it := LedgerItem{ID: "l1", Kind: ledger.Commits, Who: AuthorSupport, Text: "Read the logs.", Status: ledger.Pending, MadeBy: "engineer"}
	if err := insertLedgerItem(ctx, s.c.st.DB(), s.c.id, 1, it, "2026-11-06T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	full, err := s.ledgerMessage(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "Acme Care commits: Read the logs.") {
		t.Errorf("the full ledger: %s", full)
	}
	shown := []companion.Message{{Kind: companion.KindUpdate, Message: model.Message{Text: full}}}
	if again, err := s.ledgerMessage(ctx, shown); err != nil || again != "" {
		t.Errorf("unchanged ledger shown again: %q, %v", again, err)
	}
	it.ID, it.Text = "l2", "Review the capture."
	if err := insertLedgerItem(ctx, s.c.st.DB(), s.c.id, 2, it, "2026-11-06T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	update, err := s.ledgerMessage(ctx, shown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(update, "Acme Care commits: Review the capture.") {
		t.Errorf("the ledger update: %s", update)
	}
	// An open ask of the customer's is owed by support, under its name.
	// and a support author's comment is headed by the role, not the name.
	owed := ledgerText([]LedgerItem{
		{ID: "l9", Kind: ledger.Asks, Who: AuthorCustomer, Text: "When is the fix due?", Comment: "e1", Status: ledger.Open},
		{ID: "l10", Kind: ledger.Reports, Who: AuthorSupport, Text: "The fix is in review.", Comment: "e2", CommentID: "ev-2",
			From: CaseComment{AuthorName: "Sam Synthetic", AuthorKind: AuthorSupport, Visibility: string(VisibilityPublic), Created: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}},
	}, nil, s.SupportName(), s.SupportRole())
	if !strings.Contains(owed, "\nOwed by Acme Care:\n- l9 ") {
		t.Errorf("the conversation's ledger does not say who owes the open ask:\n%s", owed)
	}
	if !strings.Contains(owed, "· Sam Synthetic · Acme Agent · Public ·") || !strings.Contains(owed, "Acme Care reports: The fix is in review.") {
		t.Errorf("the conversation's ledger does not head a support comment by the role and its item by the name:\n%s", owed)
	}
}

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/ledger"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/redact"
	"github.com/lowmagic/compa-engine/internal/store"
)

// The case ledger (docs/MODEL_API.md §The case ledger): what each case
// comment asks, commits to, reports or provides, and what later comments
// made of each item. A model reads only the comments the ledger has not,
// oldest first, in batches, each batch a conversation of its own through
// the companion's loop — its gate, its policy and its records — with the
// ledger so far, and answers by calling ledger_update; the engine checks
// the update, names and keeps its items. Items are the model's reading,
// shown as such: never evidence, never a finding.

// LedgerTask is the task type the ledger's calls are recorded as, and its
// model profile resolved for (class ledger).
const LedgerTask = "comments.ledger"

// ledgerConversation is the kind of a ledger call's conversation.
const ledgerConversation = "ledger"

// ledgerBatch is the most comments one ledger call reads.
const ledgerBatch = 25

// ledgerBudget is how many ledger_update calls one batch may make: the
// update, and one retry of an update refused whole.
const ledgerBudget = 2

// ledgerRetry is the engine's word to a ledger model that answered without
// calling ledger_update: the batch's one retry.
const ledgerRetry = "Answer by calling ledger_update with the whole update; write nothing else."

// LedgerItem is an item the case ledger holds, as the engineer is shown it.
type LedgerItem struct {
	ID, Kind, Who, Text string
	// Comment is the name of the comment the item comes from, CommentID
	// its evidence ID, Number its number in the case (0 where not known),
	// and From that comment's header as the source gave it: author, author
	// kind, visibility, when written; its body empty.
	Comment, CommentID string
	Number             int
	From               CaseComment
	Due, Attachment    string
	// Status is open or pending for an ask or a commitment not yet
	// settled, what settled it, superseded for what was provided and is no
	// longer current, withdrawn for an item an edit took back, and empty
	// for a report or what was provided that stands. StatusBy names the
	// comment that set it, or "engineer"; StatusByID is that comment's
	// evidence ID and StatusByNumber its number in the case; StatusNote
	// says why where a model or the engineer did.
	Status, StatusBy, StatusByID, StatusNote string
	StatusByNumber                           int
	// Corrected is the engineer's correction: "wrong", or what they said.
	Corrected string
	// MadeBy is the invocation that made the item, or engineer.
	MadeBy string
	At     time.Time
}

// Open says an ask or a commitment is not settled yet.
func (it LedgerItem) Open() bool {
	return it.Corrected != "wrong" && it.Status != "" && it.Status == ledger.Unsettled(it.Kind)
}

// LedgerRun is what a run of the ledger did.
type LedgerRun struct {
	// Calls are the batches read, each a conversation of its own; Read
	// the comments read.
	Calls, Read int
	Added       int
	// Changed are the held items a new comment settled or superseded, and
	// Withdrawn those an edit took back.
	Changed, Withdrawn int
	// None are the comments read that held nothing to record, Unaccounted
	// those the update said nothing of.
	None, Unaccounted int
	// NotShown are the comments too long to give a ledger call, marked so
	// by the engine and never read.
	NotShown int
	// Dropped are the update's elements validation took out, each with why.
	Dropped []string
}

func (r *LedgerRun) add(o LedgerRun) {
	r.Calls += o.Calls
	r.Read += o.Read
	r.Added += o.Added
	r.Changed += o.Changed
	r.Withdrawn += o.Withdrawn
	r.None += o.None
	r.Unaccounted += o.Unaccounted
	r.NotShown += o.NotShown
	r.Dropped = append(r.Dropped, o.Dropped...)
}

// LedgerUnread is how many comments the ledger has yet to read.
func (s *Session) LedgerUnread(context.Context) (int, error) {
	if s.c.chat {
		return 0, nil
	}
	unread, err := ledgerUnread(s.c)
	return len(unread), err
}

// UpdateLedger has the ledger read the comments it has not, oldest first,
// ledgerBatch a call, until none is left or a call fails; nothing to read
// makes no call. on reports each call's events, as a turn's.
func (s *Session) UpdateLedger(ctx context.Context, on func(TalkEvent)) (LedgerRun, error) {
	return s.updateLedger(ctx, on, nil)
}

// updateLedger is UpdateLedger telling progress, where set, before each
// batch: how many comments the run will have read once the batch is
// read, of how many it found unread.
func (s *Session) updateLedger(ctx context.Context, on func(TalkEvent), progress func(read, total int)) (LedgerRun, error) {
	var total LedgerRun
	if s.c.chat {
		return total, nil
	}
	s.ledger.Lock()
	defer s.ledger.Unlock()
	all := -1
	for {
		unread, err := ledgerUnread(s.c)
		if err != nil {
			return total, err
		}
		unread, long, err := ledgerShowable(ctx, s.c, unread)
		total.NotShown += long
		if err != nil || len(unread) == 0 {
			return total, err
		}
		if all < 0 {
			all = len(unread)
		}
		batch := unread[:min(len(unread), ledgerBatch)]
		if progress != nil {
			progress(min(total.Read+len(batch), all), all)
		}
		run, err := s.ledgerRun(ctx, batch, on)
		total.add(run)
		if err != nil {
			return total, err
		}
		if run.Read == 0 {
			return total, errors.New("the ledger read none of the comments it was given")
		}
	}
}

// ledgerShowable are the comments of unread a ledger call can be given,
// and how many were not: each too long for a request to show (caseInline)
// is marked not shown by the engine, never given to a call, so that "not
// read" is never kept as "read, with nothing to record". An edit makes a
// new version, read again.
func ledgerShowable(ctx context.Context, c *openCase, unread []CaseComment) ([]CaseComment, int, error) {
	out := make([]CaseComment, 0, len(unread))
	long := 0
	for _, cm := range unread {
		it, err := c.ev.Describe(cm.EvidenceID)
		if err != nil {
			return nil, long, err
		}
		if it.Size.Bytes <= caseInline {
			out = append(out, cm)
			continue
		}
		if _, err := c.st.DB().ExecContext(ctx, `INSERT INTO ledger_read (investigation_id, evidence_id, outcome) VALUES (?, ?, ?)
			ON CONFLICT (investigation_id, evidence_id) DO UPDATE SET outcome = excluded.outcome`, c.id, cm.EvidenceID, OutcomeNotShown); err != nil {
			return nil, long, fmt.Errorf("the comments the ledger read: %w", err)
		}
		long++
	}
	return out, long, nil
}

// Ledger is the case ledger as the engineer and the conversation are shown
// it: every item, in the order made, but those from comments the
// engineer's rules leave out of what models read, each comment by its
// name.
func (s *Session) Ledger(context.Context) ([]LedgerItem, error) {
	if s.c.chat {
		return nil, nil
	}
	items, err := ledgerItems(s.c)
	if err != nil {
		return nil, err
	}
	items = shownItems(items, withdrawn(s.c))
	nums := commentNumbers(s.c)
	from := map[string]CaseComment{}
	head := func(id string) CaseComment {
		if _, ok := from[id]; !ok {
			from[id] = commentHead(s.c, id)
		}
		return from[id]
	}
	for i := range items {
		it := &items[i]
		if it.Comment, err = evidenceName(s.c, it.CommentID); err != nil {
			return nil, err
		}
		if it.StatusByID != "" && it.StatusByID != "engineer" {
			if it.StatusBy, err = evidenceName(s.c, it.StatusByID); err != nil {
				return nil, err
			}
			it.StatusByNumber = nums[head(it.StatusByID).ID]
		}
		it.From = head(it.CommentID)
		it.Number = nums[it.From.ID]
	}
	return items, nil
}

// commentNumbers numbers the case's comments as the case lists them, by
// their IDs at the source: 1 for the oldest, by when each was written,
// ties by ID. An edited comment keeps its number; one the engineer's rules
// leave out keeps its own too.
func commentNumbers(c *openCase) map[string]int {
	all := caseCommentsAll(c)
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Created.Equal(all[j].Created) {
			return all[i].Created.Before(all[j].Created)
		}
		return all[i].ID < all[j].ID
	})
	out := make(map[string]int, len(all))
	for i, cm := range all {
		out[cm.ID] = i + 1
	}
	return out
}

// commentHead is a stored comment's header, its body left out; empty where
// it cannot be read.
func commentHead(c *openCase, id string) CaseComment {
	it, err := c.ev.Get(id)
	if err != nil || it.Ref.Type != TypeCaseComment {
		return CaseComment{}
	}
	cm := commentOf(id, it.Content)
	cm.Body, cm.Raw = "", ""
	return cm
}

// ledgerItems are the case's ledger items, in the order they were made,
// StatusBy holding what the store does: an evidence ID, or engineer.
func ledgerItems(c *openCase) ([]LedgerItem, error) {
	rows, err := c.st.DB().Query(`SELECT id, kind, who, text, comment_id, due, attachment, status, status_by, status_note, corrected, made_by, created_at
		FROM ledger_items WHERE investigation_id = ? ORDER BY n`, c.id)
	if err != nil {
		return nil, fmt.Errorf("the ledger's items: %w", err)
	}
	defer rows.Close()
	var out []LedgerItem
	for rows.Next() {
		var it LedgerItem
		var at string
		if err := rows.Scan(&it.ID, &it.Kind, &it.Who, &it.Text, &it.CommentID, &it.Due, &it.Attachment, &it.Status, &it.StatusByID, &it.StatusNote, &it.Corrected, &it.MadeBy, &at); err != nil {
			return nil, fmt.Errorf("the ledger's items: %w", err)
		}
		it.StatusBy = it.StatusByID
		it.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the ledger's items: %w", err)
	}
	return out, nil
}

// ledgerRead are the comments the ledger has read, by evidence ID.
func ledgerRead(c *openCase) (map[string]bool, error) {
	rows, err := c.st.DB().Query(`SELECT evidence_id FROM ledger_read WHERE investigation_id = ?`, c.id)
	if err != nil {
		return nil, fmt.Errorf("the comments the ledger read: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("the comments the ledger read: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the comments the ledger read: %w", err)
	}
	return out, nil
}

// ledgerComments are the comments models may read, oldest first: each the
// version the latest acquisition holds, the engineer's rules applied.
func ledgerComments(c *openCase) ([]CaseComment, error) {
	rules, err := exclusions(c)
	if err != nil {
		return nil, err
	}
	var out []CaseComment
	for _, cm := range caseCommentsAll(c) {
		if len(rules) > 0 && ExcludedBy(rules, cm) != nil {
			continue
		}
		out = append(out, cm)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// ledgerUnread are the comments models may read that the ledger has not.
// A comment is read once in each text: a version whose body the ledger
// read in an earlier version of the same comment is not read again, since
// a source makes a new version when a comment is saved unchanged.
func ledgerUnread(c *openCase) ([]CaseComment, error) {
	all, err := ledgerComments(c)
	if err != nil {
		return nil, err
	}
	read, err := ledgerRead(c)
	if err != nil {
		return nil, err
	}
	var texts map[string]bool
	var out []CaseComment
	for _, cm := range all {
		if read[cm.EvidenceID] {
			continue
		}
		if texts == nil {
			texts = ledgerTexts(c, read)
		}
		if !texts[commentText(cm)] {
			out = append(out, cm)
		}
	}
	return out, nil
}

// ledgerTexts are the comment texts the ledger has read, by commentText.
// A version that cannot be read counts for nothing, so its comment is read
// again.
func ledgerTexts(c *openCase, read map[string]bool) map[string]bool {
	out := make(map[string]bool, len(read))
	for id := range read {
		if it, err := c.ev.Get(id); err == nil && it.Ref.Type == TypeCaseComment {
			out[commentText(commentOf(id, it.Content))] = true
		}
	}
	return out
}

// commentText names a comment's text in one version: its ID and a hash of
// its body, whatever else the version changed.
func commentText(cm CaseComment) string {
	sum := sha256.Sum256([]byte(cm.Body))
	return cm.ID + "\x00" + hex.EncodeToString(sum[:])
}

// earlierVersions are, for each comment of a batch, the versions of the
// same comment the ledger read before: an edit's earlier text, whose items
// the edit may take back.
func earlierVersions(c *openCase, read map[string]bool, batch []CaseComment) map[string][]string {
	byComment := map[string][]string{}
	for id := range read {
		if it, err := c.ev.Get(id); err == nil && it.Ref.Type == TypeCaseComment {
			cm := commentOf(id, it.Content)
			byComment[cm.ID] = append(byComment[cm.ID], id)
		}
	}
	out := map[string][]string{}
	for _, cm := range batch {
		if cm.ID == "" {
			continue
		}
		for _, id := range byComment[cm.ID] {
			if id != cm.EvidenceID {
				out[cm.EvidenceID] = append(out[cm.EvidenceID], id)
			}
		}
		sort.Strings(out[cm.EvidenceID])
	}
	return out
}

// shownItems are the items models and the engineer are shown: those whose
// comment the engineer has not left out of what models read, since an
// item is a model's reading of its comment.
func shownItems(items []LedgerItem, left func(string) bool) []LedgerItem {
	var out []LedgerItem
	for _, it := range items {
		if !left(it.CommentID) {
			out = append(out, it)
		}
	}
	return out
}

// ledgerRun has the ledger's model read one batch, in a conversation of
// its own through the companion's loop, and keeps the update it accepted:
// new items named in order, held items moved, the batch marked read. A
// batch with no update accepted is not marked read, so it is read again
// next time.
func (s *Session) ledgerRun(ctx context.Context, batch []CaseComment, on func(TalkEvent)) (LedgerRun, error) {
	e, c := s.e, s.c
	pc := e.config.Profile(LedgerTask)
	if !pc.Defined {
		return LedgerRun{}, fmt.Errorf("no model is configured for the case ledger (reasoning.profiles.%s)", pc.Class)
	}
	p, err := e.modelProvider(pc.Provider)
	if err != nil {
		return LedgerRun{}, err
	}
	d := p.Describe()
	items, err := ledgerItems(c)
	if err != nil {
		return LedgerRun{}, err
	}
	shown := shownItems(items, withdrawn(c))
	read, err := ledgerRead(c)
	if err != nil {
		return LedgerRun{}, err
	}
	earlier := earlierVersions(c, read, batch)
	req, shows, vc, err := ledgerRequest(c, batch, shown, earlier)
	if err != nil {
		return LedgerRun{}, err
	}
	tool := &ledgerTool{vc: vc}
	l := &companion.Loop{
		Store:    companion.Store{DB: c.st.DB()},
		Provider: p,
		Model:    pc.Model,
		Effort:   pc.Effort,
		System:   ledger.Prompt(),
		Tools:    []companion.Tool{tool},
		Gate:     e.companionGate(c, d.EgressClass),
		Policy:   redact.For(d.EgressClass),
		Record: func(inv companion.Invocation) error {
			return recordCall(c, LedgerTask, pc, d, inv)
		},
		Budget: ledgerBudget,
		Now:    c.now,
		NewID:  c.newID,
	}
	reply, err := l.Task(ctx, ledgerConversation, req, on, shows...)
	if err != nil {
		return LedgerRun{Calls: 1}, fmt.Errorf("the case ledger: %w", err)
	}
	if !tool.called() {
		if reply, err = l.Turn(ctx, reply.Conversation, companion.KindEngine, ledgerRetry, on); err != nil {
			return LedgerRun{Calls: 1}, fmt.Errorf("the case ledger: %w", err)
		}
	}
	u, callID, dropped, why := tool.result()
	if u == nil {
		if why == "" {
			why = "the model answered without calling ledger_update"
		}
		return LedgerRun{Calls: 1}, fmt.Errorf("the case ledger read none of %s: %s", text.Plural(len(batch), "comment"), why)
	}
	madeBy, err := callInvocation(ctx, l.Store, reply.Conversation, callID)
	if err != nil {
		return LedgerRun{Calls: 1}, err
	}
	ids := make([]string, len(batch))
	for i, cm := range batch {
		ids[i] = cm.EvidenceID
	}
	run, err := applyLedger(ctx, c, *u, ids, madeBy, vc.Written)
	run.Calls = 1
	for _, d := range dropped {
		run.Dropped = append(run.Dropped, d.String())
	}
	return run, err
}

// callInvocation is the invocation whose response made the call given, in
// a task's conversation: the ledger's or the editor's.
func callInvocation(ctx context.Context, st companion.Store, conv, callID string) (string, error) {
	msgs, err := st.Messages(ctx, conv)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		for _, call := range m.Calls {
			if call.ID == callID {
				return m.Invocation, nil
			}
		}
	}
	return "", fmt.Errorf("no message of %s holds the call %s", conv, callID)
}

// ledgerTool is ledger_update for one batch: it validates the update
// against what the batch's request gave, keeps the first it accepts and
// ends the run; an update refused whole is said to the model, which may
// call again once.
type ledgerTool struct {
	vc ledger.Context

	mu       sync.Mutex
	calls    int
	accepted *ledger.Update
	callID   string
	dropped  []ledger.Rejected
	why      string
}

func (*ledgerTool) Decl() model.Tool { return ledger.Decl() }

func (t *ledgerTool) Run(_ context.Context, c companion.Call) companion.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.accepted != nil {
		return companion.Result{Text: "The update was already recorded; nothing more is taken.", IsError: true, End: true}
	}
	u, dropped, err := ledger.Validate(t.vc, c.Args)
	if err != nil {
		t.why = "the update was refused: " + err.Error()
		return companion.Result{Text: "The update was refused and nothing was recorded: " + err.Error() + "\nCall ledger_update again with the whole update corrected.",
			IsError: true, End: t.calls >= ledgerBudget}
	}
	t.accepted, t.callID, t.dropped = &u, c.ID, dropped
	out := fmt.Sprintf("Recorded: %s, %s, %s with nothing to record.", text.Plural(len(u.Items), "item"), text.Plural(len(u.Updates), "change"), text.Plural(len(u.None), "comment"))
	for _, d := range dropped {
		out += "\nDropped " + d.String()
	}
	return companion.Result{Text: out, End: true}
}

func (t *ledgerTool) called() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls > 0
}

func (t *ledgerTool) result() (*ledger.Update, string, []ledger.Rejected, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.accepted, t.callID, t.dropped, t.why
}

// ledgerRequest is a batch's request, the ledger call's one message — the
// ledger so far, the edited comments, and the comments to read, oldest
// first, each under its name, digested as the case block's are — the
// lines it shows of them, and what its update is validated against.
func ledgerRequest(c *openCase, batch []CaseComment, shown []LedgerItem, earlier map[string][]string) (string, []companion.Shown, ledger.Context, error) {
	vc := ledger.Context{Names: map[string]string{}, Written: map[string]time.Time{}, Earlier: earlier}
	var b strings.Builder
	b.WriteString("# The ledger so far\n\n")
	held := 0
	for _, it := range shown {
		if it.Corrected == "wrong" {
			continue
		}
		name, err := evidenceName(c, it.CommentID)
		if err != nil {
			return "", nil, vc, err
		}
		if _, ok := vc.Written[it.CommentID]; !ok {
			vc.Written[it.CommentID] = commentHead(c, it.CommentID).Created
		}
		vc.Held = append(vc.Held, ledger.Entry{ID: it.ID, Kind: it.Kind, Status: it.Status, Comment: it.CommentID})
		held++
		fmt.Fprintf(&b, "%s %s (%s, from %s", it.ID, it.Kind, it.Who, name)
		if it.Status != "" {
			b.WriteString(", " + it.Status)
			if it.StatusByID != "" && it.StatusByID != "engineer" {
				by, err := evidenceName(c, it.StatusByID)
				if err != nil {
					return "", nil, vc, err
				}
				b.WriteString(" by " + by)
			}
		}
		if it.Attachment != "" {
			b.WriteString(", attachment " + it.Attachment)
		}
		if it.Due != "" {
			b.WriteString(", due " + it.Due)
		}
		b.WriteString(")")
		if it.Corrected != "" {
			b.WriteString(" [corrected by the engineer]")
		}
		b.WriteString(": " + strings.Join(strings.Fields(it.Text), " ") + "\n")
	}
	if held == 0 {
		b.WriteString("The ledger is empty: these are the first comments it reads.\n")
	}
	ids := make([]string, len(batch))
	for i, cm := range batch {
		ids[i] = cm.EvidenceID
		name, err := evidenceName(c, cm.EvidenceID)
		if err != nil {
			return "", nil, vc, err
		}
		vc.Names[name] = cm.EvidenceID
		vc.Names[cm.EvidenceID] = cm.EvidenceID
		vc.Written[cm.EvidenceID] = cm.Created
	}
	// A comment's own ID at the source names it too: a model may cite the
	// header's comment field. Set after the names, so a name always wins.
	for _, cm := range batch {
		if _, taken := vc.Names[cm.ID]; cm.ID != "" && !taken {
			vc.Names[cm.ID] = cm.EvidenceID
		}
	}
	var edited strings.Builder
	for _, cm := range batch {
		var gave []string
		var was []string
		for _, id := range earlier[cm.EvidenceID] {
			name, err := evidenceName(c, id)
			if err != nil {
				return "", nil, vc, err
			}
			was = append(was, name)
			for _, it := range shown {
				if it.CommentID == id && it.Status != ledger.Withdrawn && it.Corrected != "wrong" {
					gave = append(gave, it.ID)
				}
			}
		}
		if len(was) == 0 {
			continue
		}
		name, _ := evidenceName(c, cm.EvidenceID) // named above
		gives := "no item"
		if len(gave) > 0 {
			gives = strings.Join(gave, ", ")
		}
		fmt.Fprintf(&edited, "%s is an edited version of %s, which gave %s.\n", name, strings.Join(was, ", "), gives)
	}
	if edited.Len() > 0 {
		b.WriteString("\n# Edited comments\n\nEach comment below is given again in an edited version. Withdraw each item its earlier version gave that the edit no longer says.\n\n")
		b.WriteString(edited.String())
	}
	b.WriteString("\n# Comments to read\n\nOldest first, each under its name.\n")
	shows, err := caseItems(c, &b, ids, nil)
	if err != nil {
		return "", nil, vc, err
	}
	return b.String(), shows, vc, nil
}

// applyLedger keeps an accepted update in one transaction: new items
// numbered after the highest the transaction sees, in the thread's order;
// held items moved; and each comment read marked with what the update
// made of it.
func applyLedger(ctx context.Context, c *openCase, u ledger.Update, read []string, madeBy string, written map[string]time.Time) (LedgerRun, error) {
	sort.SliceStable(u.Items, func(i, j int) bool { return written[u.Items[i].Comment].Before(written[u.Items[j].Comment]) })
	now := c.now().UTC().Format(time.RFC3339Nano)
	tx, err := c.st.DB().Begin()
	if err != nil {
		return LedgerRun{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Numbered from what the transaction sees, not from the items read
	// before the model's call: another may have been added since.
	n, err := lastLedgerN(ctx, tx, c.id)
	if err != nil {
		return LedgerRun{}, err
	}
	run := LedgerRun{Read: len(read), Added: len(u.Items)}
	for _, it := range u.Items {
		n++
		if err := insertLedgerItem(ctx, tx, c.id, n, LedgerItem{ID: fmt.Sprintf("l%d", n), Kind: it.Kind, Who: it.Who, Text: it.Text,
			CommentID: it.Comment, Due: it.Due, Attachment: it.Attachment, Status: it.Status, StatusByID: it.Because, MadeBy: madeBy}, now); err != nil {
			return LedgerRun{}, err
		}
	}
	for _, ch := range u.Updates {
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_items SET status = ?, status_by = ?, status_note = ? WHERE investigation_id = ? AND id = ?`,
			ch.Status, ch.Because, ch.Note, c.id, ch.Item); err != nil {
			return LedgerRun{}, fmt.Errorf("the ledger's %s: %w", ch.Item, err)
		}
		if ch.Status == ledger.Withdrawn {
			run.Withdrawn++
		} else {
			run.Changed++
		}
	}
	// What each comment read gave: items, a change to held ones, none with
	// why, or nothing the update said, which is recorded so, so that "held
	// nothing" is never mistaken for "not read".
	outcome := map[string]string{}
	for _, it := range u.Items {
		outcome[it.Comment] = "items"
	}
	for _, ch := range u.Updates {
		if outcome[ch.Because] == "" {
			outcome[ch.Because] = "changes"
		}
	}
	for _, nn := range u.None {
		if outcome[nn.Comment] == "" {
			outcome[nn.Comment] = "none: " + nn.Why
		}
	}
	for _, id := range read {
		o := outcome[id]
		switch {
		case o == "":
			o = "unaccounted"
			run.Unaccounted++
		case strings.HasPrefix(o, "none: "):
			run.None++
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_read (investigation_id, evidence_id, outcome) VALUES (?, ?, ?)
			ON CONFLICT (investigation_id, evidence_id) DO UPDATE SET outcome = excluded.outcome`, c.id, id, o); err != nil {
			return LedgerRun{}, fmt.Errorf("the comments the ledger read: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return LedgerRun{}, err
	}
	return run, nil
}

func insertLedgerItem(ctx context.Context, tx store.Querier, caseID string, n int, it LedgerItem, at string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ledger_items (investigation_id, id, n, kind, who, text, comment_id, due, attachment, status, status_by, status_note, corrected, made_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		caseID, it.ID, n, it.Kind, it.Who, it.Text, it.CommentID, it.Due, it.Attachment, it.Status, it.StatusByID, it.StatusNote, it.Corrected, it.MadeBy, at)
	if err != nil {
		return fmt.Errorf("the ledger's %s: %w", it.ID, err)
	}
	return nil
}

// lastLedgerN is the highest number the case's ledger items hold, read in
// the transaction that adds the next.
func lastLedgerN(ctx context.Context, tx store.Querier, caseID string) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(n), 0) FROM ledger_items WHERE investigation_id = ?`, caseID).Scan(&n); err != nil {
		return 0, fmt.Errorf("the ledger's numbers: %w", err)
	}
	return n, nil
}

// ledgerRenderMax bounds the ledger's rendering: past it, the oldest
// reports and settled items are left out, and how many is said.
const ledgerRenderMax = 16 << 10

// ledgerIntro opens the ledger's rendering.
const ledgerIntro = "A model's reading of the case's comments: what each asks, commits to, reports or provides, and what later comments made of it. Not verified and never evidence; each item cites its comment by name."

// ledgerText is the case ledger as the conversation is shown it, items as
// Ledger gives them: the open asks by who owes them, the pending
// commitments, then what was reported and provided by comment, oldest
// first, each comment under its name, its number in the case and its
// header, times in UTC, and the settled items one short line each; items
// the engineer marked wrong are left out. Past a bound the oldest reports
// and settled items are left out, and counted. Its item lines are what
// ledgerShows compares a conversation's earlier messages against. Last,
// notShown, the lines of comments too long for the ledger to read
// (ledgerNotShown), never left out for length.
func ledgerText(items []LedgerItem, notShown []string, supportName, supportRole string) string {
	var owedCustomer, owedSupport, pending, settled []LedgerItem
	var comments []string
	byComment := map[string][]LedgerItem{}
	for _, it := range items {
		switch {
		case it.Corrected == "wrong":
		case it.Open() && it.Kind == ledger.Asks && it.Who == AuthorSupport:
			owedCustomer = append(owedCustomer, it)
		case it.Open() && it.Kind == ledger.Asks:
			owedSupport = append(owedSupport, it)
		case it.Open():
			pending = append(pending, it)
		case it.Status == ledger.Withdrawn || it.Kind == ledger.Asks || it.Kind == ledger.Commits:
			settled = append(settled, it)
		default:
			if _, ok := byComment[it.CommentID]; !ok {
				comments = append(comments, it.CommentID)
			}
			byComment[it.CommentID] = append(byComment[it.CommentID], it)
		}
	}
	sort.SliceStable(comments, func(i, j int) bool {
		return byComment[comments[i]][0].From.Created.Before(byComment[comments[j]][0].From.Created)
	})
	var b strings.Builder
	b.WriteString("# Case ledger\n\n" + ledgerIntro + "\n")
	if len(owedCustomer)+len(owedSupport)+len(pending)+len(settled)+len(comments) == 0 {
		b.WriteString("\nThe ledger holds no items.\n")
		return b.String() + notShownText(notShown)
	}
	if len(owedCustomer)+len(owedSupport) > 0 {
		b.WriteString("\n## Open asks\n")
		for _, g := range []struct {
			who   string
			items []LedgerItem
		}{{"the customer", owedCustomer}, {supportName, owedSupport}} {
			if len(g.items) == 0 {
				continue
			}
			b.WriteString("\nOwed by " + g.who + ":\n")
			for _, it := range g.items {
				b.WriteString(ledgerLine(it, true, supportName) + "\n")
			}
		}
	}
	if len(pending) > 0 {
		b.WriteString("\n## Pending commitments\n\n")
		for _, it := range pending {
			b.WriteString(ledgerLine(it, true, supportName) + "\n")
		}
	}
	// What is left of the bound goes to the newest comments first, then to
	// the newest settled items; each group is kept whole or not at all.
	left := ledgerRenderMax - b.Len()
	var groups []string
	omitted := 0
	for i := len(comments) - 1; i >= 0; i-- {
		its := byComment[comments[i]]
		var g strings.Builder
		head := []string{its[0].Comment}
		if n := commentNumber(its[0].Number); n != "" {
			head = append(head, n)
		}
		fmt.Fprintf(&g, "\n### %s\n\n", strings.Join(append(head, commentFacts(its[0].From, time.UTC, supportRole)...), " · "))
		for _, it := range its {
			g.WriteString(ledgerLine(it, false, supportName) + "\n")
		}
		if g.Len() > left {
			omitted += len(its)
			continue
		}
		left -= g.Len()
		groups = append(groups, g.String())
	}
	var lines []string
	for i := len(settled) - 1; i >= 0; i-- {
		l := settledLine(settled[i], supportName) + "\n"
		if len(l) > left {
			omitted++
			continue
		}
		left -= len(l)
		lines = append(lines, l)
	}
	if len(groups) > 0 {
		b.WriteString("\n## Reported and provided, by comment\n")
		for i := len(groups) - 1; i >= 0; i-- {
			b.WriteString(groups[i])
		}
	}
	if len(lines) > 0 {
		b.WriteString("\n## Settled\n\n")
		for i := len(lines) - 1; i >= 0; i-- {
			b.WriteString(lines[i])
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n%s left out for length, the oldest first.\n", text.Plural(omitted, "earlier item"))
	}
	return b.String() + notShownText(notShown)
}

// notShownHeading heads the comments the ledger did not read, in the
// ledger's rendering and in an update.
const notShownHeading = "\n## Not read by the ledger\n\nToo long to show the ledger's model, so the ledger says nothing of them: what they ask, commit to or report is unknown here. Read one with evidence_read.\n\n"

func notShownText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return notShownHeading + strings.Join(lines, "\n") + "\n"
}

// ledgerNotShown are the lines of the comments the ledger did not read for
// length (OutcomeNotShown), in the case's order, one each: its name and
// number. Comments the engineer's rules leave out are left out.
func (s *Session) ledgerNotShown(ctx context.Context) ([]string, error) {
	comments, err := s.LedgerComments(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, cm := range comments {
		if cm.Outcome != OutcomeNotShown {
			continue
		}
		l := "- " + cm.Name
		if n := commentNumber(cm.Number); n != "" {
			l += " · " + n
		}
		out = append(out, l+" · not read: too long to show the ledger's model")
	}
	return out, nil
}

// commentNumber is a comment's number in the case as "c#8", or nothing
// where it is not known.
func commentNumber(n int) string {
	if n == 0 {
		return ""
	}
	return "c#" + strconv.Itoa(n)
}

// commentFacts are a comment's header as the engineer knows it in the
// case: its author, who they are, whether the customer sees it, and when
// it was written, as "Oct 29, 2026, 11:27 AM UTC" in loc. What the source
// does not say is left out, never guessed.
func commentFacts(cm CaseComment, loc *time.Location, supportRole string) []string {
	var parts []string
	if cm.AuthorName != "" {
		parts = append(parts, cm.AuthorName)
	}
	switch {
	case cm.Automated:
		parts = append(parts, "Automated")
	case cm.AuthorKind == AuthorSupport:
		parts = append(parts, supportRole)
	case cm.AuthorKind == AuthorCustomer:
		parts = append(parts, "Customer")
	case cm.AuthorKind != "":
		// Another kind the source reports, under its own name.
		kind := []rune(cm.AuthorKind)
		parts = append(parts, strings.ToUpper(string(kind[:1]))+string(kind[1:]))
	}
	switch Visibility(cm.Visibility) {
	case VisibilityPublic:
		parts = append(parts, "Public")
	case VisibilityPrivate:
		parts = append(parts, "Private")
	case VisibilityPartner:
		parts = append(parts, "Partner")
	}
	if !cm.Created.IsZero() {
		parts = append(parts, cm.Created.In(loc).Format("Jan 2, 2006, 3:04 PM MST"))
	}
	return parts
}

// ledgerLine is an item in full: its ID and comment, who, what it is, and
// for one shown by its comment its status.
func ledgerLine(it LedgerItem, whose bool, supportName string) string {
	var b strings.Builder
	b.WriteString("- " + it.ID)
	if whose {
		b.WriteString(" · " + it.Comment)
	}
	b.WriteString(" · " + whoShown(it.Who, supportName) + " " + it.Kind)
	if it.Attachment != "" {
		b.WriteString(" " + it.Attachment)
	}
	if it.Due != "" {
		b.WriteString(", due " + it.Due)
	}
	b.WriteString(": " + strings.Join(strings.Fields(it.Text), " "))
	if it.Status == ledger.Superseded {
		b.WriteString(" (superseded" + byShown(it) + ")")
	}
	if it.Corrected != "" {
		b.WriteString(" [corrected by the engineer]")
	}
	return b.String()
}

// settledLine is a settled or withdrawn item in one short line.
func settledLine(it LedgerItem, supportName string) string {
	const cut = 80
	t := strings.Join(strings.Fields(it.Text), " ")
	if r := []rune(t); len(r) > cut {
		t = string(r[:cut]) + "…"
	}
	return fmt.Sprintf("- %s · %s · %s %s, %s%s: %s", it.ID, it.Comment, whoShown(it.Who, supportName), it.Kind, strings.ReplaceAll(it.Status, "_", " "), byShown(it), t)
}

func byShown(it LedgerItem) string {
	s := ""
	if it.StatusBy != "" {
		s = " by " + it.StatusBy
	}
	if it.StatusNote != "" {
		s += ": " + strings.Join(strings.Fields(it.StatusNote), " ")
	}
	return s
}

func whoShown(who, supportName string) string {
	if who == AuthorSupport {
		return supportName
	}
	return who
}

// ledgerHeading begins every ledger message of a conversation, the full
// ledger's and an update's, as a case update: how a conversation's own
// messages tell what of the ledger it has seen.
const ledgerHeading = "# Case ledger"

// ledgerUpdateIntro opens an update of the ledger in a conversation.
const ledgerUpdateIntro = "# Case ledger update\n\nWhat the ledger's model made of the comments since the ledger was last shown here: new items, items whose status changed, and comments too long for it to read. Every other item stands as shown before. A model's reading: not verified and never evidence; each item cites its comment by name.\n"

// ledgerMessage is what of the ledger a conversation, given its messages,
// has yet to be shown, appended only at a turn's start: the whole
// rendering where it has been shown none and the ledger holds items or
// comments it did not read; an update of the new items, changed statuses
// and comments newly not read where it has; or nothing. What was shown is
// read from the conversation's own ledger messages, so a reopened or
// compacted conversation is judged by what it holds.
func (s *Session) ledgerMessage(ctx context.Context, msgs []companion.Message) (string, error) {
	items, err := s.Ledger(ctx)
	if err != nil {
		return "", err
	}
	notShown, err := s.ledgerNotShown(ctx)
	if err != nil {
		return "", err
	}
	seen, top, shown := ledgerSeen(msgs)
	if !shown {
		if len(notShown) > 0 {
			return ledgerText(items, notShown, s.SupportName(), s.SupportRole()), nil
		}
		for _, it := range items {
			if it.Corrected != "wrong" {
				return ledgerText(items, nil, s.SupportName(), s.SupportRole()), nil
			}
		}
		return "", nil
	}
	lines := ledgerLines(msgs)
	var unread []string
	for _, l := range notShown {
		if !lines[l] {
			unread = append(unread, l)
		}
	}
	var added, changed []string
	for _, it := range items {
		line, ok := seen[it.ID]
		switch {
		case !ok && ledgerN(it.ID) > top && it.Corrected != "wrong":
			added = append(added, ledgerUpdateLine(it, s.SupportName()))
		case ok && !ledgerShows(line, it, s.SupportName()):
			changed = append(changed, ledgerUpdateLine(it, s.SupportName()))
		}
	}
	if len(added)+len(changed)+len(unread) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(ledgerUpdateIntro)
	if len(added) > 0 {
		b.WriteString("\n## New items\n\n" + strings.Join(added, "\n") + "\n")
	}
	if len(changed) > 0 {
		b.WriteString("\n## Changed\n\n" + strings.Join(changed, "\n") + "\n")
	}
	b.WriteString(notShownText(unread))
	return b.String(), nil
}

// ledgerLines are the lines of a conversation's ledger messages.
func ledgerLines(msgs []companion.Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		if m.Kind != companion.KindUpdate || !strings.HasPrefix(m.Text, ledgerHeading) {
			continue
		}
		for _, l := range strings.Split(m.Text, "\n") {
			out[l] = true
		}
	}
	return out
}

// ledgerSeen reads a conversation's ledger messages: each item's line as
// last shown, the highest item number shown, and whether any was. An item
// a rendering left out for length counts as seen when a later one is.
func ledgerSeen(msgs []companion.Message) (seen map[string]string, top int, shown bool) {
	seen = map[string]string{}
	for _, m := range msgs {
		if m.Kind != companion.KindUpdate || !strings.HasPrefix(m.Text, ledgerHeading) {
			continue
		}
		shown = true
		for _, l := range strings.Split(m.Text, "\n") {
			rest, ok := strings.CutPrefix(l, "- ")
			if !ok {
				continue
			}
			id, _, _ := strings.Cut(rest, " · ")
			if n := ledgerN(id); n > 0 {
				seen[id] = l
				top = max(top, n)
			}
		}
	}
	return seen, top, shown
}

// ledgerN is an item ID's number, 0 for what is not one.
func ledgerN(id string) int {
	digits, ok := strings.CutPrefix(id, "l")
	if !ok || digits == "" {
		return 0
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ledgerShows reports whether a line shown is the item as it is now, in
// the form RenderLedger gives an item in its state, or an update's. An
// open item's form says no status, so a line of that form shows only an
// item still open.
func ledgerShows(line string, it LedgerItem, supportName string) bool {
	if line == ledgerUpdateLine(it, supportName) {
		return true
	}
	switch {
	case it.Corrected == "wrong":
		return false
	case it.Open():
		return line == ledgerLine(it, true, supportName)
	case it.Status == ledger.Withdrawn || it.Kind == ledger.Asks || it.Kind == ledger.Commits:
		return line == settledLine(it, supportName)
	}
	return line == ledgerLine(it, false, supportName)
}

// ledgerUpdateLine is an item as an update shows it: in full, with its
// status said where it has one.
func ledgerUpdateLine(it LedgerItem, supportName string) string {
	l := ledgerLine(it, true, supportName)
	switch {
	case it.Corrected == "wrong":
		l += " · marked wrong by the engineer"
	case it.Status != "" && it.Status != ledger.Superseded:
		l += " · " + strings.ReplaceAll(it.Status, "_", " ") + byShown(it)
	}
	return l
}

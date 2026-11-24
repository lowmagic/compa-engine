package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model/openai"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// Preparing a case: what
// the chat's opening and compa ask do before a conversation's model call —
// the comments read, the attachments counted and inventoried, and the
// ledger reading the comments it has not beside them, waited for up to a
// bound.

// ledgerWait bounds how long preparation waits for the ledger before the
// model call it precedes; past it the call goes on without it, and
// the ledger, still reading, reaches the conversation at a later turn. A
// variable so tests can shorten it.
var ledgerWait = 60 * time.Second

// claudeCacheTTL is how long Anthropic's prompt cache keeps a prefix
// unread: the provider's default lifetime, which the engine does not
// extend.
const claudeCacheTTL = 5 * time.Minute

// reopenLarge is the estimated input past which a conversation sent
// without its cache is said to be large on reopening.
const reopenLarge = 30_000

// PrepareOptions are what a caller of Prepare asks.
type PrepareOptions struct {
	// Call says the caller makes a model call once prepared — the opening
	// turn of a case new to the companion, or compa ask's turn: the ledger
	// then reads the comments it has not, beside the rest, and is waited
	// for up to a bound. Without it no model is called.
	Call bool
	// Progress, where set, is told what runs each time that changes, for a
	// status bar.
	Progress func(state string)
	// On, where set, is told the ledger's events, its calls among them,
	// even once Prepare has returned.
	On func(TalkEvent)
}

// Prepared is what preparing a case found.
type Prepared struct {
	// Inventory is the attachments' inventory, empty where it could not be
	// made.
	Inventory Inventory
	// Problems are what could not be read, each said; the rest of the case
	// was prepared.
	Problems []error
	Ledger   LedgerStatus
}

// LedgerStatus is the case ledger as preparation left it.
type LedgerStatus struct {
	// Blocked says why the ledger cannot run, a model call being refused
	// for the case or no model configured for it; NotAsked that it was not
	// run since the caller makes no model call. Either leaves Job nil.
	Blocked  string
	NotAsked bool
	// Job is the run Prepare began. Running says it was still reading when
	// the wait ended, Waited after how long: it goes on in the background,
	// and the conversation is given what it read at its next turn.
	// Otherwise Run and Err are what it did.
	Job     *LedgerJob
	Running bool
	Waited  time.Duration
	Run     LedgerRun
	Err     error
	// Items are the items the ledger holds that the engineer has not
	// marked wrong, and Unread the comments it has yet to read, when the
	// wait ended.
	Items, Unread int
}

// Prepare reads what a case's conversation is begun or continued from: the
// comments, the attachments' counts and their inventory; and, where the
// caller makes a model call and the case may make one, it starts the
// ledger as soon as the comments are read and waits for it, once the rest
// is done, up to ledgerWait from its start. Cancelling ctx while Prepare
// runs cancels the ledger too; once Prepare has returned, a ledger still
// reading goes on until it is done, its job cancelled or the session
// closed. A chat with no case has nothing to prepare.
func (s *Session) Prepare(ctx context.Context, o PrepareOptions) (Prepared, error) {
	var p Prepared
	if s.c.chat {
		return p, nil
	}
	st := &progress{tell: o.Progress}
	st.set("reading the comments")
	if err := s.ReadComments(ctx); err != nil {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		p.Problems = append(p.Problems, err)
	}
	p.Ledger.Blocked = s.LedgerBlocked(ctx)
	switch {
	case p.Ledger.Blocked != "":
	case !o.Call:
		p.Ledger.NotAsked = true
	default:
		p.Ledger.Job = s.StartLedger(ctx, o.On, func(read, total int) {
			st.ledger(fmt.Sprintf("the ledger: reading %d of %d comments", read, total))
		})
		// The job outlives Prepare, but not a cancellation of it. Cancelled
		// before Prepare returns, never by context.AfterFunc, whose stop can
		// win the race against the cancellation it was to run.
		job := p.Ledger.Job
		defer func() {
			if ctx.Err() != nil {
				job.Cancel()
			}
		}()
	}
	st.set("counting the attachments")
	if err := s.ReadCounts(ctx); err != nil {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		p.Problems = append(p.Problems, err)
	}
	st.set("identifying the attachments")
	inv, err := s.ReadInventory(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		p.Problems = append(p.Problems, err)
	}
	p.Inventory = inv
	if j := p.Ledger.Job; j != nil {
		st.set("")
		wait := time.NewTimer(max(ledgerWait-time.Since(j.started), 0))
		defer wait.Stop()
		select {
		case <-j.Done():
			p.Ledger.Run, p.Ledger.Err = j.Result()
		case <-wait.C:
			p.Ledger.Running, p.Ledger.Waited = true, ledgerWait
		case <-ctx.Done():
			return p, ctx.Err()
		}
	}
	st.set("")
	st.ledger("")
	items, err := s.Ledger(ctx)
	if err != nil {
		return p, err
	}
	for _, it := range items {
		if it.Corrected != "wrong" {
			p.Ledger.Items++
		}
	}
	if p.Ledger.Unread, err = s.LedgerUnread(ctx); err != nil {
		return p, err
	}
	return p, nil
}

// progress tells a status bar what preparation does, and what the ledger
// does beside it, as one state.
type progress struct {
	tell      func(string)
	mu        sync.Mutex
	main, led string
}

func (p *progress) set(s string) { p.update(func() { p.main = s }) }

func (p *progress) ledger(s string) { p.update(func() { p.led = s }) }

func (p *progress) update(change func()) {
	if p.tell == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	change()
	var parts []string
	for _, s := range []string{p.main, p.led} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	p.tell(strings.Join(parts, " · "))
}

// LedgerJob is a run of the ledger in the background.
type LedgerJob struct {
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
	run     LedgerRun
	err     error
}

// Done is closed once the run has ended.
func (j *LedgerJob) Done() <-chan struct{} { return j.done }

// Cancel stops the run; what its finished calls kept stays.
func (j *LedgerJob) Cancel() { j.cancel() }

// Result is what the run did and why it stopped, once Done is closed.
func (j *LedgerJob) Result() (LedgerRun, error) {
	<-j.done
	return j.run, j.err
}

// ended reports whether the run has ended.
func (j *LedgerJob) ended() bool {
	select {
	case <-j.done:
		return true
	default:
	}
	return false
}

// LedgerBlocked says why the ledger cannot run for the case — its model
// calls refused under the barred-accounts rule, or no model configured
// for the ledger — or nothing where it can. It calls no model.
func (s *Session) LedgerBlocked(ctx context.Context) string {
	if d := s.e.barredDecision(ctx, s.c); d.refused {
		return companion.ErrBarred.Error() + ": " + d.reason
	}
	if pc := s.e.config.Profile(LedgerTask); !pc.Defined {
		return fmt.Sprintf("no model is configured for the case ledger (reasoning.profiles.%s)", pc.Class)
	}
	return ""
}

// StartLedger has the ledger read the comments it has not, as
// UpdateLedger does, in the background and past ctx's end, telling on its
// events and progress, where set, how far it has read; Cancel or Close
// stops it. A run already going on is returned instead of a second.
func (s *Session) StartLedger(ctx context.Context, on func(TalkEvent), progress func(read, total int)) *LedgerJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && !s.job.ended() {
		return s.job
	}
	jctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j := &LedgerJob{started: time.Now(), cancel: cancel, done: make(chan struct{})}
	s.job = j
	go func() {
		defer close(j.done)
		defer cancel()
		j.run, j.err = s.updateLedger(jctx, on, progress)
	}()
	return j
}

// HasConversation reports whether the case has a conversation with the
// companion, so that opening it continues one rather than begins one.
func (s *Session) HasConversation(ctx context.Context) (bool, error) {
	_, err := companion.Store{DB: s.c.st.DB()}.Latest(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, companion.ErrNoConversation):
		return false, nil
	}
	return false, err
}

// ReopenState is what a case's conversation has yet to be shown, and what
// its next call will cost, as the case is reopened: none of it calls a
// model.
type ReopenState struct {
	// Since is when the conversation was last worked: the newest message
	// of the one last worked, as ReopenState says.
	Since time.Time
	// NewComments are the comments the conversation has not been shown,
	// new or edited, as its next turn's case update gives them.
	NewComments int
	// NewAttachments are the attachments, by name, the conversation has
	// not been shown; Attachments says the attachments as read now — the
	// inventory, or the listing where there is none — are not what it was
	// shown, whether or not any attachment is new.
	NewAttachments []string
	Attachments    bool
	// LedgerUnread is how many comments the ledger has yet to read.
	LedgerUnread int
	// LastCall is when the conversation's last model call was made, zero
	// where it has had none. CacheLifetime is how long its provider keeps
	// a cached prefix, zero for one that keeps none; CacheExpired says the
	// next call is past it.
	LastCall      time.Time
	CacheLifetime time.Duration
	CacheExpired  bool
	// Estimate is the conversation's estimated input as its next call
	// sends it, by model.EstimatorVersion; Large says it passes the size
	// past which resending it without the cache is worth saying.
	Estimate int64
	Large    bool
}

// ReopenState is what the case's conversation has yet to be shown and
// what its next call will cost; false where the case has no conversation.
// Read after Prepare, so the comments and the inventory are the case's
// now. What is new is judged against the conversation the engineer last
// worked: a conversation a reload or a compaction began holds the case as
// it was then, so until its first turn it is judged by the one it
// continues.
func (s *Session) ReopenState(ctx context.Context) (ReopenState, bool, error) {
	r, _, ok, err := s.reopenState(ctx)
	return r, ok, err
}

// reopenState is ReopenState with the IDs of the comments new to it.
func (s *Session) reopenState(ctx context.Context) (ReopenState, []string, bool, error) {
	var r ReopenState
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	if errors.Is(err, companion.ErrNoConversation) {
		return r, nil, false, nil
	}
	if err != nil {
		return r, nil, false, err
	}
	msgs, err := st.Messages(ctx, conv.ID)
	if err != nil {
		return r, nil, true, err
	}
	for _, m := range msgs {
		if m.Kind == companion.KindModel && m.Invocation != "" {
			r.LastCall = m.CreatedAt
		}
	}
	worked, err := workedMessages(ctx, st, conv, msgs)
	if err != nil {
		return r, nil, true, err
	}
	shown := ""
	for _, m := range worked {
		r.Since = m.CreatedAt
		if m.Kind == companion.KindCase || m.Kind == companion.KindUpdate {
			shown += m.Text
		}
	}
	fresh, err := freshItems(s.c, s.inventory(), worked)
	if err != nil {
		return r, nil, true, err
	}
	var comments []string
	for _, id := range fresh {
		it, err := s.c.ev.Get(id)
		if err != nil {
			return r, nil, true, err
		}
		switch it.Ref.Type {
		case TypeCaseComment:
			r.NewComments++
			comments = append(comments, id)
		case TypeArtifactInventory, TypeArtifactOverview:
			r.Attachments = true
		}
	}
	if r.Attachments {
		for _, a := range s.atts {
			if !strings.Contains(shown, a.Name) {
				r.NewAttachments = append(r.NewAttachments, a.Name)
			}
		}
	}
	if r.LedgerUnread, err = s.LedgerUnread(ctx); err != nil {
		return r, nil, true, err
	}
	r.CacheLifetime = s.cacheLifetime()
	r.CacheExpired = r.CacheLifetime > 0 && !r.LastCall.IsZero() && s.c.now().Sub(r.LastCall) > r.CacheLifetime
	if r.Estimate, err = s.storeLoop(ctx).Estimate(ctx, conv.ID); err != nil {
		return r, nil, true, err
	}
	r.Large = r.Estimate > reopenLarge
	return r, comments, true, nil
}

// Fresh reports whether the case gained comments or attachments the
// conversation has not been shown: what a catch-up asks about.
func (r ReopenState) Fresh() bool {
	return r.NewComments > 0 || len(r.NewAttachments) > 0 || r.Attachments
}

// workedMessages are the messages of the conversation the engineer last
// worked: conv's, msgs, once it has had a turn; else, through the
// conversations it continues, the newest one that has, or the first.
func workedMessages(ctx context.Context, st companion.Store, conv companion.Conversation, msgs []companion.Message) ([]companion.Message, error) {
	seen := map[string]bool{}
	for !worked(msgs) && conv.CompactedFrom != "" && !seen[conv.CompactedFrom] {
		seen[conv.ID] = true
		prev, err := st.Conversation(ctx, conv.CompactedFrom)
		if err != nil {
			return nil, err
		}
		if msgs, err = st.Messages(ctx, prev.ID); err != nil {
			return nil, err
		}
		conv = prev
	}
	return msgs, nil
}

// worked reports whether a conversation has had a turn: an engineer's or
// the engine's message.
func worked(msgs []companion.Message) bool {
	for _, m := range msgs {
		if m.Kind == companion.KindEngineer || m.Kind == companion.KindEngine {
			return true
		}
	}
	return false
}

// cacheLifetime is how long the provider of the session's model keeps a
// conversation's cached prefix: Gemini's explicit cache for its
// configured or default lifetime, Anthropic's prompt cache for its
// default, which the openai provider's cache marks ask for too; zero for a
// provider that caches nothing or caches as it alone knows.
func (s *Session) cacheLifetime() time.Duration {
	pc, err := s.profile()
	if err != nil {
		return 0
	}
	switch pc.Provider {
	case vertexgemini.ID:
		if v, _ := s.e.config.VertexSettings(); v.CacheMinutes > 0 {
			return time.Duration(v.CacheMinutes) * time.Minute
		}
		return geminiCacheTTL
	case vertexanthropic.ID:
		return claudeCacheTTL
	case openai.ID:
		if s.e.config.OpenAISettings().CacheMarks {
			return claudeCacheTTL
		}
	}
	return 0
}

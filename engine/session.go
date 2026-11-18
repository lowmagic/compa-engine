package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/invocations"
)

// Session is a case open in the chat: what the chat reads of the case and
// does to it goes through the session.
type Session struct {
	*caseStore
	// kase and atts are the case as StartSession read it before the chat
	// started, for the readings that follow in the background.
	kase Case
	atts []Attachment
	// chosen is the model and effort the engineer chose for the session's
	// calls, nil for the companion's profile; inv the inventory
	// ReadInventory last made, which conversations are shown; job the
	// ledger's run StartLedger began last.
	mu     sync.Mutex
	chosen *config.ProfileChoice
	inv    *Inventory
	job    *LedgerJob
	// ledger runs one UpdateLedger at a time, so two runs never read the
	// same comments.
	ledger sync.Mutex
}

// StartSession reads a case to the point the chat can start: the
// case from its source, its store, and its details and
// attachment list. ReadComments and ReadCounts read the rest in the
// background. fetch, where not nil, runs between the case's read and its
// store's making, so a case folder it fetches is where the store goes
// (the offer to fetch a case's artifacts, ArtifactFetcher). Close closes
// the case's store.
func (e *Engine) StartSession(ctx context.Context, id string, fetch func(ctx context.Context, id string) error) (*Session, error) {
	sc, err := e.startCase(ctx, id, fetch)
	if err != nil {
		return nil, err
	}
	return e.startedSession(sc), nil
}

// startedSession is the session of a case startCase read.
func (e *Engine) startedSession(sc startedCase) *Session {
	return &Session{caseStore: &CaseStore{e: e, c: sc.c}, kase: sc.kase, atts: sc.list.atts}
}

// startedCase is a case read as far as it is before anything waits on
// its comments or its walk: its details and attachment list.
type startedCase struct {
	c    *openCase
	kase Case
	list acquired
}

// startCase reads a case to the point its session can start:
// the case from its source, its store, and the reading
// of its details and attachment list, which costs no walk. fetch, where
// not nil, runs between the case's read and its store's making, so a
// case folder it fetches is where the store goes.
func (e *Engine) startCase(ctx context.Context, id string, fetch func(context.Context, string) error) (startedCase, error) {
	// The case is read before a store is made, so a mistyped case number
	// leaves nothing behind.
	kase, err := e.cases.Case(ctx, id)
	if err != nil {
		return startedCase{}, err
	}
	if fetch != nil {
		if err := fetch(ctx, id); err != nil {
			return startedCase{}, err
		}
	}
	c, err := e.open(ctx, id, true)
	if err != nil {
		return startedCase{}, err
	}
	a, err := e.readList(ctx, c, kase)
	if err != nil {
		c.Close()
		return startedCase{}, err
	}
	return startedCase{c: c, kase: kase, list: a}, nil
}

// Close stops the ledger's run StartLedger began, if it still runs, and
// waits for it, then closes the case's store.
func (s *Session) Close() error {
	if j := s.ledgerJob(); j != nil {
		j.Cancel()
		<-j.Done()
	}
	return s.c.Close()
}

// inventory is the inventory ReadInventory last made, nil before.
func (s *Session) inventory() *Inventory {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inv
}

// ledgerJob is the ledger's run StartLedger began last, nil before.
func (s *Session) ledgerJob() *LedgerJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.job
}

// CaseID is the case's number.
func (s *Session) CaseID() string { return s.c.id }

// ReadComments reads the case's comments from its source, saying why it
// could not; what was read before stands.
func (s *Session) ReadComments(ctx context.Context) error {
	cms, err := s.e.cases.Comments(ctx, s.c.id)
	if err == nil {
		_, err = s.e.readComments(s.c, s.kase, cms)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		err = fmt.Errorf("the case's comments could not be read: %w; what was read before stands", err)
	}
	return err
}

// ReadCounts counts the attachments StartSession listed, saying why it
// could not.
func (s *Session) ReadCounts(ctx context.Context) error {
	_, err := s.e.readCounts(ctx, s.c, s.atts)
	if err != nil && !errors.Is(err, context.Canceled) {
		err = fmt.Errorf("the attachments could not be counted: %w", err)
	}
	return err
}

// Intentions are the deployment's intentions a comment is drafted for.
func (s *Session) Intentions() []Intention { return s.e.config.Comments.Intentions }

// Invocation is the record of one invocation, by its ID.
func (s *Session) Invocation(id string) (InvocationRecord, error) {
	return invocations.Get(s.c.st.DB(), id)
}

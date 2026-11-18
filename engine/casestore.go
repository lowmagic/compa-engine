package engine

import (
	"context"
	"io"
	"time"

	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/store"
)

// CaseStore is a case's store, or a chat's, read alone: nothing is read
// from the case's source, so what it holds stays readable when the source
// is down. A Session holds one, so each of its methods is a session's too.
type CaseStore struct {
	e *Engine
	c *openCase
}

// caseStore names CaseStore where Session embeds it, so the field is not
// part of Session's API and only the methods are: a session is closed by
// its own Close, which stops its ledger first.
type caseStore = CaseStore

// ErrEvidenceNotFound is the error for evidence a case does not hold.
var ErrEvidenceNotFound = evidence.ErrNotFound

// OpenCaseStore opens the store of a case opened before, refusing a case
// that never was. Close closes it.
func (e *Engine) OpenCaseStore(ctx context.Context, id string) (*CaseStore, error) {
	c, err := e.open(ctx, id, false)
	if err != nil {
		return nil, err
	}
	return &CaseStore{e: e, c: c}, nil
}

// Close closes the store.
func (s *CaseStore) Close() error { return s.c.Close() }

// CaseOpening is what opening a case read: the case, where its store is,
// what its attachments hold and the evidence it gained, the experts
// routed to it, and the expired stores swept.
type CaseOpening struct {
	Case  Case
	Store StoreInfo
	// Attachments are the attachments' counts, nil where the case has no
	// artifact directory or nothing has counted it yet.
	Attachments *AttachmentCounts
	// Evidence counts the items the reading made current, New those it
	// added.
	Evidence, New int
	// Experts is the routing in a line: the experts installed and those
	// suggested, with why.
	Experts string
	// Removed are the expired stores swept, SweepErr why the sweep failed.
	Removed  []string
	SweepErr error
}

// AttachmentCounts are what a count of a case's attachments found. AtLeast
// says the walk stopped at its limit, so files and bytes are a floor.
type AttachmentCounts struct {
	Attachments int
	Files       int64
	Bytes       int64
	AtLeast     bool
}

// OpenCase reads the whole case at once, as compa case open does: its
// details and attachment list, its comments and its attachments' counts,
// then sweeps the stores whose retention ran out. fetch is as for
// StartSession.
func (e *Engine) OpenCase(ctx context.Context, id string, fetch func(ctx context.Context, id string) error) (CaseOpening, error) {
	s, err := e.startCase(ctx, id, fetch)
	if err != nil {
		return CaseOpening{}, err
	}
	c, a := s.c, s.list
	defer c.Close()
	comments, err := e.cases.Comments(ctx, id)
	if err != nil {
		return CaseOpening{}, err
	}
	b, err := e.readComments(c, s.kase, comments)
	if err != nil {
		return CaseOpening{}, err
	}
	a.and(b)
	n, err := e.readCounts(ctx, c, a.atts)
	if err != nil {
		return CaseOpening{}, err
	}
	a.and(n)
	o := CaseOpening{Case: s.kase, Store: (&CaseStore{e: e, c: c}).Store(), Evidence: a.total, New: len(a.added)}
	if t := a.artifacts; t != nil {
		o.Attachments = &AttachmentCounts{Attachments: t.attachments, Files: t.files, Bytes: t.bytes, AtLeast: t.cut}
	}
	// Swept after this case's store is open, so a case returning after
	// its retention ran out is reopened, not removed first.
	o.Removed, o.SweepErr = store.Sweep(e.home, e.config.HomeRetention(), time.Now())
	o.Experts = e.routed(s.kase)
	return o, nil
}

// StoreInfo is where a case's store and artifacts are.
type StoreInfo struct {
	// ArtifactRoot is the case's artifact directory, empty when it has
	// none, NoArtifacts then saying why.
	ArtifactRoot string
	NoArtifacts  error
	// Dir is the store's directory.
	Dir string
	// Notes say what opening did to the case's stores, for the engineer.
	Notes []string
}

// Store says where the store and the case's artifacts are.
func (s *CaseStore) Store() StoreInfo {
	return StoreInfo{ArtifactRoot: s.c.root, NoArtifacts: s.c.rootErr, Dir: s.c.storeDir, Notes: s.c.notes}
}

// Barred is the barred-accounts decision for the case now: the list read
// afresh, the account number as the case's source last gave it.
func (s *CaseStore) Barred(ctx context.Context) BarredDecision { return s.e.barredDecision(ctx, s.c) }

// EvidenceList is every stored item, described without its content,
// newest first.
func (s *CaseStore) EvidenceList() ([]EvidenceItem, error) { return s.c.ev.List() }

// Evidence is one stored item with its content.
func (s *CaseStore) Evidence(id string) (EvidenceItem, error) { return s.c.ev.Get(id) }

// DescribeEvidence is one stored item without its content.
func (s *CaseStore) DescribeEvidence(id string) (EvidenceItem, error) { return s.c.ev.Describe(id) }

// OpenEvidence streams one item's content, so reading its start reads
// only that much.
func (s *CaseStore) OpenEvidence(id string) (io.ReadCloser, error) { return s.c.ev.Open(id) }

// Comments are the case's comments as models are given them, newest
// first, whatever the rules leave out.
func (s *CaseStore) Comments() []CaseComment { return caseCommentsAll(s.c) }

// CommentRules are the rules that leave comments out of what models read.
func (s *CaseStore) CommentRules() ([]Exclusion, error) { return exclusions(s.c) }

// ExcludeComments records a rule of a kind, Exclude* or KeepComment,
// with its value where the kind takes one. A rule already held is not
// recorded twice.
func (s *CaseStore) ExcludeComments(kind, value string) (Exclusion, error) {
	return excludeComments(s.c, kind, value)
}

// IncludeComments removes a rule by its ID.
func (s *CaseStore) IncludeComments(ruleID string) error { return includeComments(s.c, ruleID) }

// Ingest stores what r yields as evidence the engineer brought in,
// reporting whether the item is new.
func (s *CaseStore) Ingest(r io.Reader, location, note string) (EvidenceItem, bool, error) {
	return ingest(s.c, r, location, note)
}

// OverrideNotice is what the engineer reads before overriding a barred-
// account check that could not be made; an error says why there is
// nothing to override.
func (s *CaseStore) OverrideNotice(ctx context.Context) ([]string, error) {
	return s.e.overrideNotice(ctx, s.c)
}

// AllowModels records the engineer's override, by login, when typed is
// the account's name as the source gave it, or the case number where it
// gave none.
func (s *CaseStore) AllowModels(ctx context.Context, typed, who string) (ModelsOverride, error) {
	return s.e.allowModels(ctx, s.c, typed, who)
}

// RevokeModels withdraws the case's override.
func (s *CaseStore) RevokeModels(ctx context.Context) error { return revokeModels(ctx, s.c) }

// Trajectory is Session.Trajectory over the store alone.
func (s *CaseStore) Trajectory(ctx context.Context) (Trajectory, error) {
	return (&Session{caseStore: s}).Trajectory(ctx)
}

// Resend is Session.Resend over the store alone.
func (s *CaseStore) Resend(ctx context.Context, id, modelName, effort string, on func(TalkEvent)) (Resent, error) {
	return (&Session{caseStore: s}).Resend(ctx, id, modelName, effort, on)
}

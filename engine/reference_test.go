package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
)

// fakeToken is the fake adapter's credential, which nothing it stores may
// hold.
const fakeToken = "fake-credential-7f3a9c"

// fakeReferences is a reference adapter for tests: it answers each search
// and fetch with what the test set, keeps what it was asked, and holds a
// credential of its own, as a real adapter's HTTP client would.
type fakeReferences struct {
	id       string
	token    string
	search   ReferenceSearchResult
	searchE  error
	pages    map[int]ReferenceDocument
	fetchE   error
	queries  []ReferenceQuery
	fetched  []int
	blockFor time.Duration
	// offer is what it serves; nil is search and fetch of solutions and
	// articles.
	offer *ReferenceOffer
}

func (f *fakeReferences) ServiceID() string { return f.id }

func (f *fakeReferences) Offer() ReferenceOffer {
	if f.offer != nil {
		return *f.offer
	}
	return ReferenceOffer{Operations: []string{ReferenceSearch, ReferenceFetch}, Kinds: []string{"solution", "article"}}
}

func (f *fakeReferences) Search(ctx context.Context, q ReferenceQuery) (ReferenceSearchResult, error) {
	f.queries = append(f.queries, q)
	if f.blockFor > 0 {
		select {
		case <-ctx.Done():
			return ReferenceSearchResult{}, ctx.Err()
		case <-time.After(f.blockFor):
		}
	}
	return f.search, f.searchE
}

func (f *fakeReferences) Fetch(_ context.Context, ref ReferenceID, page int) (ReferenceDocument, error) {
	f.fetched = append(f.fetched, page)
	return f.pages[page], f.fetchE
}

// referenceCase is a case on an engine with the fake adapter registered.
func referenceCase(t *testing.T, f *fakeReferences) (*Engine, *openCase) {
	t.Helper()
	e, c := actionCaseWith(t, "")
	refs, err := referencesByService([]ReferenceAdapter{f})
	if err != nil {
		t.Fatal(err)
	}
	e.references = refs
	return e, c
}

func str(s string) *string { return &s }

// A deployment's adapters are registered by service ID; two serving one
// service are refused, naming it, as are a nil adapter and one with no ID.
func TestReferenceAdapterInOptions(t *testing.T) {
	dir := t.TempDir()
	opts := func(refs ...ReferenceAdapter) Options {
		return Options{Cases: StubCases("../testdata/cases"), Artifacts: LocalDir(dir), Home: dir, References: refs}
	}
	e, err := New(opts(&fakeReferences{id: "kcs"}, &fakeReferences{id: "jira"}))
	if err != nil || len(e.references) != 2 || e.references["kcs"] == nil {
		t.Fatalf("two adapters: %v %v", e, err)
	}
	if _, err := New(opts(&fakeReferences{id: "kcs"}, &fakeReferences{id: "kcs"})); err == nil || !strings.Contains(err.Error(), `"kcs"`) {
		t.Errorf("two adapters for one service: %v", err)
	}
	if _, err := New(opts(&fakeReferences{})); err == nil {
		t.Error("an adapter with no service ID was registered")
	}
	if _, err := New(opts(nil)); err == nil {
		t.Error("a nil adapter was registered")
	}
	if _, err := New(opts(&fakeReferences{id: "kcs", offer: &ReferenceOffer{Operations: []string{ReferenceFetch}}})); err == nil {
		t.Error("an adapter naming no kind of document was registered")
	}
	if _, err := New(opts(&fakeReferences{id: "kcs", offer: &ReferenceOffer{Operations: []string{"browse"}, Kinds: []string{"solution"}}})); err == nil {
		t.Error("an adapter offering an operation that is neither search nor fetch was registered")
	}
}

// Nothing a lookup stores holds the adapter's credential, even when its
// error quotes an authorization header; the header's token is redacted.
func TestReferenceAdapterCredentialIsolation(t *testing.T) {
	f := &fakeReferences{id: "kcs", token: fakeToken, searchE: errors.New("401: Authorization: Bearer " + fakeToken), search: ReferenceSearchResult{Outcome: SearchDenied}}
	e, c := referenceCase(t, f)
	it, err := e.lookupSearch(context.Background(), c, ReferenceQuery{Service: "kcs", Text: "etcd leader elections"}, false)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := c.ev.Get(it.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{"source": stored.Ref.Provenance.Source, "collected by": stored.Ref.Provenance.CollectedBy, "location": stored.Location, "note": stored.Note, "content": string(stored.Content)} {
		if strings.Contains(s, fakeToken) || strings.Contains(strings.ToLower(s), "bearer "+fakeToken) {
			t.Errorf("the %s holds the credential: %q", what, s)
		}
	}
	if !strings.Contains(stored.Note, "denied") {
		t.Errorf("note %q", stored.Note)
	}
}

// What a lookup stores for one case is in that case's store alone.
func TestReferenceCaseIsolation(t *testing.T) {
	f := &fakeReferences{id: "kcs", search: ReferenceSearchResult{Total: 1, Documents: []SearchHit{{ID: "1", Kind: "Solution", Title: "etcd"}}}}
	e, one := referenceCase(t, f)
	_, other := actionCaseWith(t, "")
	it, err := e.lookupSearch(context.Background(), one, ReferenceQuery{Service: "kcs", Text: "etcd"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !one.ev.Exists(it.Ref.ID) || other.ev.Exists(it.Ref.ID) {
		t.Errorf("stored in its case %v, in another %v", one.ev.Exists(it.Ref.ID), other.ev.Exists(it.Ref.ID))
	}
}

// A lookup's result is stored, with an ID, by the time the lookup returns,
// so nothing can disclose it unstored; read back, it is offered to a
// request as it was stored, its note before its text.
func TestReferenceStorageBeforeDisclosure(t *testing.T) {
	f := &fakeReferences{id: "kcs", pages: map[int]ReferenceDocument{1: {Title: str("Etcd slow disk"), State: str("verified"), Body: "## Issue\n\netcd warns of slow fdatasync."}}}
	e, c := referenceCase(t, f)
	it, err := e.lookupFetch(context.Background(), c, ReferenceID{Service: "kcs", ID: "4321", Kind: "solution"}, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if it.Ref.ID == "" || !c.ev.Exists(it.Ref.ID) {
		t.Fatalf("not stored: %+v", it.Ref)
	}
	stored, err := c.ev.Get(it.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := stored.Input()
	if in.Text != "## Issue\n\netcd warns of slow fdatasync." || !strings.Contains(in.Ref.Note, "kcs solution 4321, page 1") || !strings.Contains(in.Ref.Note, "state verified") {
		t.Errorf("offered as %+v", in)
	}
}

// Each search outcome is its own stored item, its type reference_search,
// collected by reference, the outcome in its note: a complete search that
// found nothing says it describes the search; one that did not complete
// says it says nothing of what the service holds, and keeps no hits.
func TestReferenceSearchOutcomes(t *testing.T) {
	hits := []SearchHit{{ID: "1", Kind: "Solution", Title: "a", State: "verified"}, {ID: "2", Kind: "Article", Title: "b"}}
	cases := []struct {
		name string
		res  ReferenceSearchResult
		err  error
		note string
	}{
		{"complete_none", ReferenceSearchResult{}, nil, "the search found nothing, which describes the search"},
		{"complete", ReferenceSearchResult{Total: 2, Documents: hits}, nil, "complete, 2 found"},
		{"truncated", ReferenceSearchResult{Total: 40, Documents: hits}, nil, "2 of 40 returned"},
		{"truncated_no_total", ReferenceSearchResult{Outcome: SearchTruncated, Documents: hits}, nil, "2 returned (more available)"},
		{"denied", ReferenceSearchResult{Outcome: SearchDenied, Documents: hits}, errors.New("401"), "denied, so it says nothing"},
		{"failed", ReferenceSearchResult{}, errors.New("connection reset"), "failed, so it says nothing"},
		{"unavailable", ReferenceSearchResult{Outcome: SearchUnavailable}, errors.New("no token"), "unavailable, so it says nothing"},
	}
	ids := map[string]bool{}
	for _, tc := range cases {
		f := &fakeReferences{id: "kcs", search: tc.res, searchE: tc.err}
		e, c := referenceCase(t, f)
		it, err := e.lookupSearch(context.Background(), c, ReferenceQuery{Service: "kcs", Text: "etcd " + tc.name}, false)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if it.Ref.Type != evidence.TypeReferenceSearch || it.Ref.Provenance.CollectedBy != "reference" || it.Ref.Provenance.Source != "kcs" || it.Location != "etcd "+tc.name {
			t.Errorf("%s: %+v at %q", tc.name, it.Ref, it.Location)
		}
		if !strings.Contains(it.Note, tc.note) || !strings.Contains(it.Note, "never a match") {
			t.Errorf("%s: note %q", tc.name, it.Note)
		}
		failed := tc.name == "denied" || tc.name == "failed" || tc.name == "unavailable"
		if failed && strings.Contains(string(it.Content), "1. ") {
			t.Errorf("%s: kept hits:\n%s", tc.name, it.Content)
		}
		if tc.name == "complete" && !strings.Contains(string(it.Content), "1. Solution 1: a (state verified)") {
			t.Errorf("complete:\n%s", it.Content)
		}
		ids[it.Ref.ID] = true
	}
	if len(ids) != len(cases) {
		t.Errorf("%d distinct items for %d outcomes", len(ids), len(cases))
	}
}

// Each fetch outcome is its own stored item, of type reference_document;
// pages 1 and 2 of one document are two items with the same provenance
// and their own page numbers; a page past the bound is cut and marked
// truncated, and one refused or failed keeps no body.
func TestReferenceFetchOutcomes(t *testing.T) {
	long := strings.Repeat("x", referencePageBytes+10)
	cases := []struct {
		name string
		page int
		doc  ReferenceDocument
		err  error
		note string
	}{
		{"complete", 1, ReferenceDocument{Body: "one page"}, nil, "complete"},
		{"more_pages", 1, ReferenceDocument{Body: "first page", HasMore: true}, nil, "page 1; state unknown; more pages follow"},
		{"page 2", 2, ReferenceDocument{Body: "second page"}, nil, "page 2"},
		{"truncated", 1, ReferenceDocument{Body: long}, nil, "truncated: cut to its bound"},
		{"denied", 1, ReferenceDocument{Outcome: FetchDenied, Body: "should not be kept"}, errors.New("401"), "denied: nothing was read"},
		{"failed", 1, ReferenceDocument{}, errors.New("graphql error"), "failed: nothing was read"},
		{"unavailable", 1, ReferenceDocument{Outcome: FetchUnavailable}, nil, "unavailable: nothing was read"},
	}
	ids := map[string]bool{}
	for _, tc := range cases {
		f := &fakeReferences{id: "kcs", pages: map[int]ReferenceDocument{tc.page: tc.doc}, fetchE: tc.err}
		e, c := referenceCase(t, f)
		it, err := e.lookupFetch(context.Background(), c, ReferenceID{Service: "kcs", ID: "99", Kind: "solution"}, tc.page, false)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if it.Ref.Type != evidence.TypeReferenceDocument || it.Ref.Provenance.CollectedBy != "reference" || it.Location != "kcs solution 99" {
			t.Errorf("%s: %+v at %q", tc.name, it.Ref, it.Location)
		}
		if !strings.Contains(it.Note, tc.note) {
			t.Errorf("%s: note %q", tc.name, it.Note)
		}
		switch tc.name {
		case "truncated":
			if len(it.Content) != referencePageBytes || !it.Size.LowerBound {
				t.Errorf("truncated: %d bytes, %+v", len(it.Content), it.Size)
			}
		case "denied", "failed", "unavailable":
			if len(it.Content) != 0 {
				t.Errorf("%s: kept %q", tc.name, it.Content)
			}
		}
		ids[it.Ref.ID] = true
	}
	if len(ids) != len(cases) {
		t.Errorf("%d distinct items for %d outcomes", len(ids), len(cases))
	}
}

// The query is redacted before the adapter sees it, and what it was sent
// is what the stored item records; a query that would still hold a secret
// after redaction is refused before anything is sent.
func TestReferenceRedaction(t *testing.T) {
	f := &fakeReferences{id: "kcs"}
	e, c := referenceCase(t, f)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	it, err := e.lookupSearch(context.Background(), c, ReferenceQuery{Service: "kcs", Text: "login fails with token " + jwt}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 || strings.Contains(f.queries[0].Text, jwt) || !strings.Contains(f.queries[0].Text, "[REDACTED:") {
		t.Fatalf("the adapter was sent %+v", f.queries)
	}
	if it.Location != f.queries[0].Text {
		t.Errorf("stored as %q, sent %q", it.Location, f.queries[0].Text)
	}
	if _, err := lookupText(jwt); err != nil {
		t.Errorf("a token redaction removes was refused: %v", err)
	}
	if kind, ok := lookupPolicy.Holds("token " + jwt); !ok || kind == "" {
		t.Errorf("the policy does not find the token it redacts: %q %v", kind, ok)
	}
}

// A lookup whose query derives from the case is refused for a barred
// case, as a model call is, and nothing is sent; one the engineer typed
// that does not derive from the case is not checked. A lookup that runs
// past its time is cancelled and recorded as failed.
func TestReferenceBarredAndBounded(t *testing.T) {
	f := &fakeReferences{id: "kcs"}
	e, c := referenceCase(t, f)
	e.config.Reasoning.BarredAccounts = &config.BarredAccounts{List: barredList(t, "1234")}
	ctx := context.Background()
	if err := c.st.SetMeta(ctx, metaAccountNumber, "1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.lookupSearch(ctx, c, ReferenceQuery{Service: "kcs", Text: "from the case"}, true); err == nil || !strings.Contains(err.Error(), "barred_account") || len(f.queries) != 0 {
		t.Errorf("a barred case's lookup: %v, sent %+v", err, f.queries)
	}
	if _, err := e.lookupSearch(ctx, c, ReferenceQuery{Service: "kcs", Text: "typed by hand"}, false); err != nil || len(f.queries) != 1 {
		t.Errorf("a lookup the engineer typed: %v", err)
	}
	if _, err := e.lookupSearch(ctx, c, ReferenceQuery{Service: "jira", Text: "x"}, false); err == nil {
		t.Error("a lookup of a service no adapter serves ran")
	}
	f.blockFor = time.Minute
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	it, err := e.lookupSearch(tctx, c, ReferenceQuery{Service: "kcs", Text: "slow"}, false)
	if err != nil || !strings.Contains(it.Note, "failed") {
		t.Errorf("a lookup past its time: %v, %q", err, it.Note)
	}
}

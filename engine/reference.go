package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/redact"
)

// External knowledge-base references: a
// deployment passes an adapter per service, such as KCS, which searches
// and fetches documents by ID through its own HTTP client, holding its
// endpoint and credential. The engine redacts what it sends, bounds how
// long it waits, and stores every result as evidence, whatever its
// outcome, before any request can disclose it. A hit is a lead to test,
// never a match.

// ReferenceAdapter reaches one external knowledge service. The engine
// holds no endpoint, credential or auth token; each lives inside the
// adapter's own HTTP client.
type ReferenceAdapter interface {
	// ServiceID names the service, matching a lookup's service. Two
	// adapters with one ID are an error at New.
	ServiceID() string
	// Search runs a text search. The query's text is redacted already.
	// The context carries the lookup's deadline and cancellation.
	Search(ctx context.Context, q ReferenceQuery) (ReferenceSearchResult, error)
	// Fetch reads one page of a document by its identity, 1-based.
	Fetch(ctx context.Context, ref ReferenceID, page int) (ReferenceDocument, error)
	// Offer is what the adapter serves: the engine offers models only
	// those operations, so an adapter that cannot yet search offers only
	// fetch.
	Offer() ReferenceOffer
}

// Lookup is a knowledge-base operation a model asks for: one a
// LookupOffer offers, with its parameters.
type Lookup struct {
	Service   string         `json:"service"`
	Operation string         `json:"operation"`
	Params    map[string]any `json:"params"`
}

// LookupOffer is one knowledge-base operation offered to a model: a
// service's search or fetch (ReferenceSearch, ReferenceFetch), with its
// typed parameters.
type LookupOffer struct {
	Service     string                  `json:"service"`
	Operation   string                  `json:"operation"`
	Description string                  `json:"description,omitempty"`
	Params      map[string]expert.Param `json:"params"`
}

// The operations an adapter may serve.
const (
	ReferenceSearch = "search"
	ReferenceFetch  = "fetch"
)

// ReferenceOffer is what an adapter serves.
type ReferenceOffer struct {
	// Operations are ReferenceSearch, ReferenceFetch or both.
	Operations []string
	// Kinds are the kinds of document the service holds, such as solution
	// and article, in its own words: the engine holds no service's
	// vocabulary, so a lookup names a kind from this list.
	Kinds []string
}

// ReferenceQuery is a search's parameters.
type ReferenceQuery struct {
	Service string
	// Text is the query, redacted before the adapter sees it.
	Text string
	// Kinds are the kinds of document wanted, such as solution and
	// article; empty means every kind. The engine holds no service's
	// vocabulary: the adapter maps them.
	Kinds []string
	// Rows is how many results, 1 to 100; 0 means 10.
	Rows int
}

// SearchOutcome is how a search ended.
type SearchOutcome string

const (
	// SearchComplete is a search that returned without error; with no
	// documents it is a search that found nothing, never a failure.
	SearchComplete SearchOutcome = "complete"
	// SearchTruncated is a complete search that matched more than it
	// returned: Total is the service's count.
	SearchTruncated SearchOutcome = "truncated"
	// SearchDenied is a credential the service refused.
	SearchDenied SearchOutcome = "denied"
	// SearchFailed is a transport error, an invalid response, or an error
	// that is not the credential's; the query may or may not have reached
	// the service.
	SearchFailed SearchOutcome = "failed"
	// SearchUnavailable is a credential that could not be had: nothing was
	// sent.
	SearchUnavailable SearchOutcome = "unavailable"
)

// ReferenceSearchResult is what a search returned.
type ReferenceSearchResult struct {
	// Total is the service's own count of matches.
	Total     int
	Documents []SearchHit
	Outcome   SearchOutcome
	// QuerySent is the text as sent, for the record.
	QuerySent string
}

// SearchHit is one document a search returned, its states verbatim, empty
// where the service gave none. A hit carries no body: fetch it before
// citing it.
type SearchHit struct {
	ID                   string
	Kind                 string
	Title                string
	URL                  string
	State                string
	ModerationState      string
	HasPublishedRevision string
}

// ReferenceID identifies a document to fetch.
type ReferenceID struct {
	Service string
	ID      string
	Kind    string
}

// FetchOutcome is how a fetch of one page ended.
type FetchOutcome string

const (
	// FetchComplete is a page read whole, the document's last.
	FetchComplete FetchOutcome = "complete"
	// FetchMorePages is a page read whole with more to follow.
	FetchMorePages FetchOutcome = "more_pages"
	// FetchTruncated is a page cut to the engine's bound.
	FetchTruncated FetchOutcome = "truncated"
	// FetchDenied is a credential the service refused.
	FetchDenied FetchOutcome = "denied"
	// FetchFailed is a transport, response or service error.
	FetchFailed FetchOutcome = "failed"
	// FetchUnavailable is a credential that could not be had: nothing was
	// sent.
	FetchUnavailable FetchOutcome = "unavailable"
)

// ReferenceDocument is one page of a fetched document. A field the service
// did not give is nil, or zero for Modified.
type ReferenceDocument struct {
	ID       string
	Kind     string
	Title    *string
	URL      *string
	Language *string
	// State is the service's own state, verbatim, such as verified.
	State    *string
	Modified time.Time
	// Body is the page's text, sections joined for a solution.
	Body      string
	Page      int
	HasMore   bool
	Truncated bool
	Outcome   FetchOutcome
}

// referencePageBytes is the most of a page's text the engine stores; a
// longer page is cut and marked truncated.
const referencePageBytes = 64 << 10

// referenceTimeout is how long a lookup may take; past it, the lookup is
// cancelled and recorded as failed.
const referenceTimeout = 30 * time.Second

// collectedByReference is the provenance of everything a lookup stored.
const collectedByReference = "reference"

// lookupPolicy redacts what a lookup sends, which always leaves the host.
var lookupPolicy = redact.For("internal_network")

// referencesByService indexes a deployment's adapters, refusing a nil one,
// one with no service ID and two with the same.
func referencesByService(adapters []ReferenceAdapter) (map[string]ReferenceAdapter, error) {
	out := map[string]ReferenceAdapter{}
	for i, a := range adapters {
		if a == nil {
			return nil, fmt.Errorf("engine: reference adapter %d is nil", i)
		}
		id := a.ServiceID()
		if id == "" {
			return nil, fmt.Errorf("engine: reference adapter %d has no service ID", i)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("engine: two reference adapters serve %q", id)
		}
		offer := a.Offer()
		if len(offer.Kinds) == 0 {
			return nil, fmt.Errorf("engine: reference adapter %q names no kind of document", id)
		}
		for _, op := range offer.Operations {
			if op != ReferenceSearch && op != ReferenceFetch {
				return nil, fmt.Errorf("engine: reference adapter %q offers %q, which is neither search nor fetch", id, op)
			}
		}
		out[id] = a
	}
	return out, nil
}

// lookupText is the text a lookup sends: redacted, and refused if a secret
// the policy names is still in it, as a request is.
func lookupText(text string) (string, error) {
	sent := lookupPolicy.String(text)
	if kind, ok := lookupPolicy.Holds(sent); ok {
		return "", fmt.Errorf("the lookup still holds a %s after redaction, and was not sent", kind)
	}
	return sent, nil
}

// lookupAllowed refuses a lookup whose query derives from the case's
// content for a case whose models are barred, failing closed as a model
// call does: that text leaves the host.
func (e *Engine) lookupAllowed(ctx context.Context, c *openCase, fromCase bool) error {
	if !fromCase {
		return nil
	}
	if d := e.barredDecision(ctx, c); d.refused {
		return fmt.Errorf("%s: %s", companion.OutcomeBarred, d.reason)
	}
	return nil
}

// adapterFor is the registered adapter of a service.
func (e *Engine) adapterFor(service string) (ReferenceAdapter, error) {
	a, ok := e.references[service]
	if !ok {
		return nil, fmt.Errorf("no reference adapter serves %q", service)
	}
	return a, nil
}

// lookupSearch runs a search through its service's adapter and stores
// what came back as evidence, whatever its outcome, before anything can
// disclose it. fromCase says the query derives from the case's content.
func (e *Engine) lookupSearch(ctx context.Context, c *openCase, q ReferenceQuery, fromCase bool) (evidence.Item, error) {
	it, _, err := e.search(ctx, c, q, fromCase)
	return it, err
}

// search is lookupSearch, saying whether the search read anything: it
// completed, finding nothing included, or was truncated.
func (e *Engine) search(ctx context.Context, c *openCase, q ReferenceQuery, fromCase bool) (evidence.Item, bool, error) {
	a, err := e.adapterFor(q.Service)
	if err != nil {
		return evidence.Item{}, false, err
	}
	if err := e.lookupAllowed(ctx, c, fromCase); err != nil {
		return evidence.Item{}, false, err
	}
	if q.Text, err = lookupText(q.Text); err != nil {
		return evidence.Item{}, false, err
	}
	lctx, cancel := context.WithTimeout(ctx, referenceTimeout)
	defer cancel()
	res, err := a.Search(lctx, q)
	if errors.Is(ctx.Err(), context.Canceled) {
		return evidence.Item{}, false, ctx.Err()
	}
	if err != nil && res.Outcome == "" {
		res = ReferenceSearchResult{Outcome: SearchFailed}
	}
	if res.Outcome == "" {
		res.Outcome = SearchComplete
	}
	if res.Outcome == SearchComplete && res.Total > len(res.Documents) {
		res.Outcome = SearchTruncated
	}
	if res.Outcome != SearchComplete && res.Outcome != SearchTruncated {
		res.Documents = nil
	}
	res.QuerySent = q.Text
	it := searchItem(q, res, lookupFailure(err), c.now().UTC())
	if err := c.ev.Add(it); err != nil {
		return evidence.Item{}, false, err
	}
	return it, res.Outcome == SearchComplete || res.Outcome == SearchTruncated, nil
}

// lookupFailure is an adapter's error as the record keeps it, redacted, since an
// error may quote what it was sent.
func lookupFailure(err error) string {
	if err == nil {
		return ""
	}
	return lookupPolicy.String(err.Error())
}

// searchItem is a search's result as evidence: its query as the location,
// the hits as its text, and in its note the service, the query and the
// outcome, a search that did not complete saying so.
func searchItem(q ReferenceQuery, res ReferenceSearchResult, why string, at time.Time) evidence.Item {
	var b strings.Builder
	fmt.Fprintf(&b, "service: %s\nquery: %s\ntime: %s\noutcome: %s\ntotal: %d · returned: %d\n", q.Service, res.QuerySent, at.Format(time.RFC3339), res.Outcome, res.Total, len(res.Documents))
	for i, h := range res.Documents {
		fmt.Fprintf(&b, "%d. %s %s: %s", i+1, h.Kind, h.ID, h.Title)
		if h.URL != "" {
			b.WriteString(" — " + h.URL)
		}
		var states []string
		for _, s := range [][2]string{{"state", h.State}, {"moderation", h.ModerationState}, {"published revision", h.HasPublishedRevision}} {
			if s[1] != "" {
				states = append(states, s[0]+" "+s[1])
			}
		}
		if len(states) > 0 {
			b.WriteString(" (" + strings.Join(states, "; ") + ")")
		}
		b.WriteString("\n")
	}
	var note string
	switch res.Outcome {
	case SearchComplete:
		note = fmt.Sprintf("%s search for %q: complete, %d found", q.Service, res.QuerySent, len(res.Documents))
		if len(res.Documents) == 0 {
			note += ": the search found nothing, which describes the search, not the case"
		}
	case SearchTruncated:
		if res.Total > 0 {
			note = fmt.Sprintf("%s search for %q: %d of %d returned", q.Service, res.QuerySent, len(res.Documents), res.Total)
		} else {
			note = fmt.Sprintf("%s search for %q: %d returned (more available)", q.Service, res.QuerySent, len(res.Documents))
		}
	default:
		note = fmt.Sprintf("%s search for %q: %s, so it says nothing of what the service holds", q.Service, res.QuerySent, res.Outcome)
	}
	if why != "" {
		note += "; " + why
	}
	note += ". A hit is a lead to test, never a match."
	it := item(evidence.TypeReferenceSearch, q.Service, res.QuerySent, b.String())
	it.Ref.Provenance.CollectedBy = collectedByReference
	it.Note = note
	return it
}

// lookupFetch fetches one page of a document through its service's
// adapter, bounds it and stores it as evidence, whatever its outcome,
// before anything can disclose it.
func (e *Engine) lookupFetch(ctx context.Context, c *openCase, ref ReferenceID, page int, fromCase bool) (evidence.Item, error) {
	it, _, err := e.fetch(ctx, c, ref, page, fromCase)
	return it, err
}

// fetch is lookupFetch, saying whether a page was read.
func (e *Engine) fetch(ctx context.Context, c *openCase, ref ReferenceID, page int, fromCase bool) (evidence.Item, bool, error) {
	a, err := e.adapterFor(ref.Service)
	if err != nil {
		return evidence.Item{}, false, err
	}
	if err := e.lookupAllowed(ctx, c, fromCase); err != nil {
		return evidence.Item{}, false, err
	}
	if page < 1 {
		page = 1
	}
	lctx, cancel := context.WithTimeout(ctx, referenceTimeout)
	defer cancel()
	doc, err := a.Fetch(lctx, ref, page)
	if errors.Is(ctx.Err(), context.Canceled) {
		return evidence.Item{}, false, ctx.Err()
	}
	if err != nil && doc.Outcome == "" {
		doc = ReferenceDocument{Outcome: FetchFailed}
	}
	doc.ID, doc.Page = ref.ID, page
	if doc.Kind == "" {
		doc.Kind = ref.Kind
	}
	if doc.Outcome == "" {
		doc.Outcome = FetchComplete
		if doc.HasMore {
			doc.Outcome = FetchMorePages
		}
	}
	switch doc.Outcome {
	case FetchComplete, FetchMorePages, FetchTruncated:
	default:
		doc.Body, doc.HasMore = "", false
	}
	if len(doc.Body) > referencePageBytes {
		cut := referencePageBytes
		for cut > 0 && !utf8.RuneStart(doc.Body[cut]) {
			cut--
		}
		doc.Body, doc.Truncated = doc.Body[:cut], true
	}
	if doc.Truncated && doc.Outcome == FetchComplete {
		doc.Outcome = FetchTruncated
	}
	it := documentItem(ref.Service, doc, lookupFailure(err))
	if err := c.ev.Add(it); err != nil {
		return evidence.Item{}, false, err
	}
	read := doc.Outcome == FetchComplete || doc.Outcome == FetchMorePages || doc.Outcome == FetchTruncated
	return it, read, nil
}

// documentItem is a page as evidence: its body as the text, its URL as
// the location, and in its note the service, the document, its state and
// time, the page and the outcome.
func documentItem(service string, doc ReferenceDocument, why string) evidence.Item {
	location := fmt.Sprintf("%s %s %s", service, doc.Kind, doc.ID)
	if doc.URL != nil && *doc.URL != "" {
		location = *doc.URL
	}
	parts := []string{fmt.Sprintf("%s %s %s, page %d", service, doc.Kind, doc.ID, doc.Page)}
	if doc.Title != nil {
		parts = append(parts, fmt.Sprintf("%q", *doc.Title))
	}
	state := "state unknown"
	if doc.State != nil {
		state = "state " + *doc.State
	}
	parts = append(parts, state)
	if !doc.Modified.IsZero() {
		parts = append(parts, "modified "+doc.Modified.UTC().Format(time.RFC3339))
	}
	switch doc.Outcome {
	case FetchComplete:
		parts = append(parts, "complete")
	case FetchMorePages:
		parts = append(parts, "more pages follow")
	case FetchTruncated:
		parts = append(parts, "truncated: cut to its bound")
	default:
		parts = append(parts, string(doc.Outcome)+": nothing was read from it")
	}
	if doc.HasMore && doc.Outcome != FetchMorePages {
		parts = append(parts, "more pages follow")
	}
	if why != "" {
		parts = append(parts, why)
	}
	// Its identity covers the page and the outcome, so a page refused and
	// one that failed are two items.
	it := item(evidence.TypeReferenceDocument, service, location, fmt.Sprintf("page %d\n%s\n%s", doc.Page, doc.Outcome, doc.Body))
	it.Content = []byte(doc.Body)
	it.Size = *evidence.SizeOf(doc.Body, doc.Truncated)
	it.Ref.Provenance.CollectedBy = collectedByReference
	it.Note = strings.Join(parts, "; ")
	return it
}

// anyKind is the search parameter's value for every kind of document.
const anyKind = "any"

// lookupOffers are the lookups a request offers: each registered
// service's operations, as its adapter serves them, with typed
// parameters, by service. With no adapter, none.
func (e *Engine) lookupOffers() []LookupOffer {
	services := make([]string, 0, len(e.references))
	for id := range e.references {
		services = append(services, id)
	}
	sort.Strings(services)
	var out []LookupOffer
	for _, id := range services {
		offer := e.references[id].Offer()
		kinds := make([]any, 0, len(offer.Kinds))
		for _, k := range offer.Kinds {
			kinds = append(kinds, k)
		}
		for _, op := range offer.Operations {
			switch op {
			case ReferenceSearch:
				out = append(out, LookupOffer{Service: id, Operation: ReferenceSearch,
					Description: "Search " + id + " for documents: literal error strings, or the exact product and component with the behavior described, in English. A hit is a lead to test, never a match; fetch a document before citing it.",
					Params: map[string]expert.Param{
						"text": {Type: "string", MaxLength: 500, Description: "the query, as sent; secrets are redacted before it leaves the host"},
						"kind": {Type: "string", Enum: append([]any{anyKind}, kinds...), Default: anyKind, Description: "the kind of document wanted"},
						"rows": {Type: "integer", Minimum: num(1), Maximum: num(100), Default: 10, Description: "how many results"},
					}})
			case ReferenceFetch:
				out = append(out, LookupOffer{Service: id, Operation: ReferenceFetch,
					Description: "Fetch one page of a " + id + " document by its identifier or key, as a search hit or a case comment names it.",
					Params: map[string]expert.Param{
						"id":   {Type: "string", MaxLength: 128, Description: "the document's identifier or key"},
						"kind": {Type: "string", Enum: kinds, Description: "the kind of document"},
						"page": {Type: "integer", Minimum: num(1), Default: 1, Description: "the page, from 1; a page says whether more follow"},
					}})
			}
		}
	}
	return out
}

func num(f float64) *float64 { return &f }

// runLookup carries out a lookup the companion asked for,
// its parameters already validated against the offer, and stores what
// came back. fromCase says its query may derive from the case's content.
//
// read says whether it read anything: a search that completed, or a page.
func (e *Engine) runLookup(ctx context.Context, c *openCase, l Lookup, fromCase bool) (it evidence.Item, read bool, err error) {
	text, _ := l.Params["text"].(string)
	kind, _ := l.Params["kind"].(string)
	switch l.Operation {
	case ReferenceSearch:
		q := ReferenceQuery{Service: l.Service, Text: text, Rows: intParam(l.Params, "rows", 0)}
		if kind != "" && kind != anyKind {
			q.Kinds = []string{kind}
		}
		return e.search(ctx, c, q, fromCase)
	case ReferenceFetch:
		id, _ := l.Params["id"].(string)
		ref := ReferenceID{Service: l.Service, ID: id, Kind: kind}
		return e.fetch(ctx, c, ref, intParam(l.Params, "page", 1), fromCase)
	}
	return evidence.Item{}, false, fmt.Errorf("a lookup is a search or a fetch, not %q", l.Operation)
}

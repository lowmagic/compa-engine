// Package evidence keeps a case's evidence in its case store: each item's
// provenance — its source and location — its size, and its content as a
// blob. Evidence is immutable and identified by content, so storing an
// item twice stores it once.
//
// It is also the one place a collection becomes evidence a request can
// carry, so evidence offered fresh from the runner and evidence read back
// from the store reach a model as the same text.
package evidence

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/store"
)

const (
	TypeTestResult    = "test_result"
	TypeCommandOutput = "command_output"
	// TypeIngested is what the engineer brought in themselves: a file, or
	// what they piped in.
	TypeIngested = "ingested"
	// TypeReferenceSearch is one knowledge-base search's result list, and
	// TypeReferenceDocument one page of a document fetched by its ID,
	// each collected by a deployment's reference adapter.
	TypeReferenceSearch   = "reference_search"
	TypeReferenceDocument = "reference_document"
)

var ErrNotFound = errors.New("evidence: not found")

type Item struct {
	Ref Ref
	// Location is where in the case's artifacts the evidence came from:
	// for a collection, the paths it read, one per line.
	Location string
	Size     Size
	// Collection is the record of the collection that produced the item,
	// without its output, where one did.
	Collection *collect.Record
	Content    []byte
	// Note is the engineer's description of evidence they ingested, or a
	// knowledge-base lookup's service, what it asked and its outcome.
	Note      string
	CreatedAt time.Time
}

func FromRecord(rec collect.Record) Item {
	ref := Ref{ID: rec.ID, Type: TypeCommandOutput, Provenance: Provenance{Source: "command", CollectedBy: "engine"}}
	if rec.What.TestID != "" {
		ref.Type = TypeTestResult
		ref.Provenance.Source = rec.What.TestID + "@" + rec.What.TestVersion
	}
	meta := rec
	meta.Output = nil
	return Item{
		Ref:        ref,
		Location:   strings.Join(rec.PathsRead, "\n"),
		Size:       *SizeOf(string(rec.Output), rec.Truncated),
		Collection: &meta,
		Content:    rec.Output,
	}
}

// Input offers the item to a request, inlined. A collection that did not
// run says why, so a model does not take its empty output for a finding;
// one that stopped early says how far it got and what it never reached,
// so a model does not take a search that stopped for one that found all
// there is.
//
// What it says before the text is the evidence's note, not lines of its
// text, so the text's lines keep their stored numbers.
func (it Item) Input() Input {
	var note []string
	if c := it.Collection; c != nil && c.Status != collect.StatusOK {
		note = append(note, fmt.Sprintf("status: %s; reason: %s", c.Status, c.Reason))
	}
	if c := it.Collection; c != nil && c.Stopped != nil {
		note = append(note, c.Stopped.Summary())
	}
	// A lookup's note says what it asked and how it ended, so a model
	// never reads a failed search as one that found nothing.
	if (it.Ref.Type == TypeReferenceSearch || it.Ref.Type == TypeReferenceDocument) && it.Note != "" {
		note = append(note, it.Note)
	}
	size := it.Size
	ref := it.Ref
	ref.Note = strings.Join(note, "\n")
	return Input{Ref: ref, Text: string(it.Content), Inline: true, Size: &size}
}

// Store is the runner's Sink.
type Store struct {
	s *store.Store
	// Now stamps what is stored; injectable for tests.
	Now func() time.Time
}

func New(s *store.Store) *Store {
	return &Store{s: s, Now: func() time.Time { return time.Now().UTC() }}
}

// Put stores a collection; storing the same one again changes nothing.
func (e *Store) Put(rec collect.Record) error { return e.Add(FromRecord(rec)) }

// Add keeps an item already stored under the same ID as it was: an ID is
// the item's identity.
func (e *Store) Add(it Item) error {
	if it.Ref.ID == "" {
		return errors.New("evidence: an item needs an ID")
	}
	digest, err := e.s.PutBlob(it.Content)
	if err != nil {
		return err
	}
	return e.AddStored(it, digest)
}

// AddStored is Add for content already in the store as a blob, as streamed
// content is; the item's Content is ignored.
func (e *Store) AddStored(it Item, digest string) error {
	if it.Ref.ID == "" {
		return errors.New("evidence: an item needs an ID")
	}
	var collection any
	if it.Collection != nil {
		j, err := json.Marshal(it.Collection)
		if err != nil {
			return fmt.Errorf("evidence: %w", err)
		}
		collection = string(j)
	}
	_, err := e.s.DB().Exec(`INSERT INTO evidence
		(id, type, scope, source, location, collected_by, blob, bytes, lines, lower_bound, collection, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		it.Ref.ID, it.Ref.Type, it.Ref.Scope, it.Ref.Provenance.Source, it.Location, it.Ref.Provenance.CollectedBy,
		digest, it.Size.Bytes, it.Size.Lines, it.Size.LowerBound, collection, it.Note, e.Now().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	return nil
}

func (e *Store) Exists(id string) bool {
	var one int
	return e.s.DB().QueryRow(`SELECT 1 FROM evidence WHERE id = ?`, id).Scan(&one) == nil
}

func (e *Store) Get(id string) (Item, error) {
	it, digest, err := e.describe(id)
	if err != nil {
		return Item{}, err
	}
	if it.Content, err = e.s.Blob(digest); err != nil {
		return Item{}, err
	}
	return it, nil
}

// Describe returns an item without its content, which may be large.
func (e *Store) Describe(id string) (Item, error) {
	it, _, err := e.describe(id)
	return it, err
}

// Open streams an item's content, checked against its digest at the end.
func (e *Store) Open(id string) (io.ReadCloser, error) {
	_, digest, err := e.describe(id)
	if err != nil {
		return nil, err
	}
	return e.s.OpenBlob(digest)
}

func (e *Store) describe(id string) (Item, string, error) {
	var it Item
	var digest, created string
	var collection sql.NullString
	err := e.s.DB().QueryRow(`SELECT id, type, scope, source, location, collected_by, blob, bytes, lines, lower_bound, collection, note, created_at
		FROM evidence WHERE id = ?`, id).Scan(
		&it.Ref.ID, &it.Ref.Type, &it.Ref.Scope, &it.Ref.Provenance.Source, &it.Location, &it.Ref.Provenance.CollectedBy,
		&digest, &it.Size.Bytes, &it.Size.Lines, &it.Size.LowerBound, &collection, &it.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Item{}, "", fmt.Errorf("evidence: %w", err)
	}
	if collection.Valid {
		it.Collection = &collect.Record{}
		if err := json.Unmarshal([]byte(collection.String), it.Collection); err != nil {
			return Item{}, "", fmt.Errorf("evidence: %s: %w", id, err)
		}
	}
	if it.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Item{}, "", fmt.Errorf("evidence: %s: %w", id, err)
	}
	return it, digest, nil
}

// List returns every item without its content, oldest first, ties by ID.
func (e *Store) List() ([]Item, error) {
	rows, err := e.s.DB().Query(`SELECT id, type, scope, source, location, collected_by, bytes, lines, lower_bound, created_at
		FROM evidence ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var created string
		if err := rows.Scan(&it.Ref.ID, &it.Ref.Type, &it.Ref.Scope, &it.Ref.Provenance.Source, &it.Location, &it.Ref.Provenance.CollectedBy,
			&it.Size.Bytes, &it.Size.Lines, &it.Size.LowerBound, &created); err != nil {
			return nil, fmt.Errorf("evidence: %w", err)
		}
		if it.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("evidence: %s: %w", it.Ref.ID, err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

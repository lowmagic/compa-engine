// Package sources records what a case's sources report exists — for now
// its attachments — so the engine knows what it was told and when, apart
// from the evidence it made from it.
package sources

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lowmagic/compa-engine/internal/store"
)

const KindAttachment = "attachment"

type Source struct {
	Origin, Name, Kind string
	Path, Dir          string
	Size               int64
	State              string
	// Reported is what the source says the item is, such as a must-gather.
	Reported            string
	FirstSeen, LastSeen time.Time
}

type Store struct {
	s *store.Store
	// Now stamps what is stored; injectable for tests.
	Now func() time.Time
}

func New(s *store.Store) *Store {
	return &Store{s: s, Now: func() time.Time { return time.Now().UTC() }}
}

// Record stores what a source reports now. An item it reported before keeps
// its first sighting; one it no longer reports keeps its last.
func (st *Store) Record(items []Source) error {
	tx, err := st.s.DB().Begin()
	if err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	defer tx.Rollback()
	now := st.Now().Format(time.RFC3339Nano)
	for _, it := range items {
		if _, err := tx.Exec(`INSERT INTO sources (origin, name, kind, path, dir, size, state, reported, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (origin, name) DO UPDATE SET kind = excluded.kind, path = excluded.path, dir = excluded.dir,
				size = excluded.size, state = excluded.state, reported = excluded.reported, last_seen = excluded.last_seen`,
			it.Origin, it.Name, it.Kind, it.Path, it.Dir, it.Size, it.State, it.Reported, now, now); err != nil {
			return fmt.Errorf("sources: %s: %w", it.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	return nil
}

// List returns every recorded item, by origin and name.
func (st *Store) List() ([]Source, error) {
	rows, err := st.s.DB().Query(`SELECT origin, name, kind, path, dir, size, state, reported, first_seen, last_seen FROM sources ORDER BY origin, name`)
	if err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var it Source
		var first, last string
		if err := rows.Scan(&it.Origin, &it.Name, &it.Kind, &it.Path, &it.Dir, &it.Size, &it.State, &it.Reported, &first, &last); err != nil {
			return nil, fmt.Errorf("sources: %w", err)
		}
		it.FirstSeen, _ = time.Parse(time.RFC3339Nano, first)
		it.LastSeen, _ = time.Parse(time.RFC3339Nano, last)
		out = append(out, it)
	}
	return out, rows.Err()
}

// Acquisition is what one reading of a case's sources found current: the
// evidence that is the case now, and the attachments the artifact source
// reported, by name, an empty list meaning none.
type Acquisition struct {
	Evidence    []string
	Origin      string
	Attachments []string
	At          time.Time
}

// Acquired records an acquisition, so what is current is read from it
// rather than from when evidence or a sighting was first stored.
func (st *Store) Acquired(a Acquisition) error {
	ev, err := json.Marshal(nonNil(a.Evidence))
	if err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	atts, err := json.Marshal(nonNil(a.Attachments))
	if err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	if _, err := st.s.DB().Exec(`INSERT INTO acquisitions (evidence, origin, attachments, created_at) VALUES (?, ?, ?, ?)`,
		string(ev), a.Origin, string(atts), st.Now().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	return nil
}

// Latest returns the latest acquisition; false when none was recorded, as
// in a store from before acquisitions were.
func (st *Store) Latest() (Acquisition, bool, error) {
	var a Acquisition
	var ev, atts, at string
	err := st.s.DB().QueryRow(`SELECT evidence, origin, attachments, created_at FROM acquisitions ORDER BY n DESC LIMIT 1`).Scan(&ev, &a.Origin, &atts, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, fmt.Errorf("sources: %w", err)
	}
	if err := json.Unmarshal([]byte(ev), &a.Evidence); err != nil {
		return a, false, fmt.Errorf("sources: %w", err)
	}
	if err := json.Unmarshal([]byte(atts), &a.Attachments); err != nil {
		return a, false, fmt.Errorf("sources: %w", err)
	}
	a.At, _ = time.Parse(time.RFC3339Nano, at)
	return a, true, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

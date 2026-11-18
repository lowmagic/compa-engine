package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func metaKeys(t *testing.T, s *Store, keys ...string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, k := range keys {
		var v string
		got[k] = s.DB().QueryRow(`SELECT value FROM meta WHERE key = ?`, k).Scan(&v) == nil
	}
	return got
}

// Within a transaction, a package's own transaction is a savepoint: rolled
// back alone, it undoes only its own writes; committed, its writes still
// wait on the outer transaction, and an outer failure undoes them too.
func TestTxNestsPackageTransactions(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), DirName), "0420", Options{})
	defer s.Close()
	put := func(db Querier, k string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, 'x')`, k); err != nil {
			t.Fatal(err)
		}
	}
	nested := func(b *Store, k string, commit bool) {
		t.Helper()
		tx, err := b.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		put(tx, k)
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Tx(t.Context(), func(b *Store) error {
		nested(b, "rolled-back", false)
		nested(b, "nested", true)
		put(b.DB(), "outer")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := metaKeys(t, s, "rolled-back", "nested", "outer"); got["rolled-back"] || !got["nested"] || !got["outer"] {
		t.Errorf("after commit: %v", got)
	}

	failed := errors.New("failed")
	if err := s.Tx(t.Context(), func(b *Store) error {
		nested(b, "nested-2", true)
		put(b.DB(), "outer-2")
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("a failing transaction: %v", err)
	}
	if got := metaKeys(t, s, "nested-2", "outer-2"); got["nested-2"] || got["outer-2"] {
		t.Errorf("after a failure: %v", got)
	}
	if err := s.Tx(t.Context(), func(b *Store) error { return b.Close() }); err == nil {
		t.Error("a transaction's store closed")
	}
}

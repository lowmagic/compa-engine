// Package store is a case's durable store: a SQLite database in WAL mode
// for state and indexes, and content-addressed, zstd-compressed blobs for
// large payloads. One store holds one case and nothing is shared between
// cases. One process holds a case at a time: a lock refuses a second
// process, or a second open in this one, and says who holds it. The store
// is its maker's alone, its directory readable by no one else.
//
// A store lives in the case folder's .compa directory, or, where there is no
// case folder, under the engineer's home directory, where Sweep removes it
// once it has gone unused for the retention period (docs/SPEC.md §2).
package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const DirName = ".compa"

// DefaultMaxBlobBytes bounds one blob, before compression.
const DefaultMaxBlobBytes = 1 << 30

// ErrLocked means another process, or another open in this one, holds the
// case; the error wrapping it says which, where the holder wrote it.
var ErrLocked = errors.New("store: the case is open elsewhere")

// Options left zero take the defaults.
type Options struct {
	// Now stamps the store's use; injectable for tests.
	Now func() time.Time
	// MaxBlobBytes bounds one blob before compression, and so what one
	// blob may decompress to.
	MaxBlobBytes int64
}

type Store struct {
	dir string
	db  *sql.DB
	// q runs the packages' statements: db, or within Tx, its transaction,
	// bound true.
	q       DB
	bound   bool
	lock    *os.File
	now     func() time.Time
	maxBlob int64
	enc     *zstd.Encoder
	dec     *zstd.Decoder
}

var caseIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Location is where a case's store lives: the case folder's .compa directory,
// or, with no case folder, a directory for the case under the engineer's
// home directory, which Sweep may remove. inHome reports the second.
func Location(caseDir, home, caseID string) (dir string, inHome bool, err error) {
	if caseDir != "" {
		return filepath.Join(caseDir, DirName), false, nil
	}
	if !caseIDPattern.MatchString(caseID) {
		return "", false, fmt.Errorf("store: case ID %q cannot name a directory", caseID)
	}
	return filepath.Join(homeCases(home), caseID), true, nil
}

func homeCases(home string) string { return filepath.Join(home, DirName, "cases") }

// Open opens the store in dir for caseID, creating it if needed, and holds
// the case until Close. A store created for one case refuses another.
func Open(dir, caseID string, o Options) (*Store, error) {
	s, err := open(dir, o)
	if err != nil {
		return nil, err
	}
	if err := s.claim(caseID); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// open locks dir and opens its database, migrated, without marking it
// used, so Sweep can read a store without keeping it alive.
func open(dir string, o Options) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if who, _ := io.ReadAll(io.LimitReader(lock, 512)); len(bytes.TrimSpace(who)) > 0 {
				return nil, fmt.Errorf("%w: held by %s", ErrLocked, bytes.TrimSpace(who))
			}
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("store: locking %s: %w", dir, err)
	}
	// The holder writes who it is, for a process refused to report.
	if err := lock.Truncate(0); err == nil {
		_, _ = lock.WriteAt([]byte(holder()+"\n"), 0)
	}
	s := &Store{dir: dir, lock: lock, now: o.Now, maxBlob: o.MaxBlobBytes}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.maxBlob <= 0 {
		s.maxBlob = DefaultMaxBlobBytes
	}
	// Exclusive locking keeps SQLite's wal-index in memory rather than in a
	// shared-memory file, which WAL cannot rely on over NFS, where a
	// deployment may keep its case folders; the flock above already gives
	// this process the case alone. Measured live it also made commits ten
	// times faster. WAL is switched on afterwards, not in the
	// DSN: as a DSN pragma, on a database already in WAL mode, the driver
	// created the shared-memory file anyway.
	dsn := "file:" + filepath.Join(dir, "case.db") +
		"?_pragma=locking_mode(EXCLUSIVE)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	if s.db, err = sql.Open("sqlite", dsn); err != nil {
		s.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	// One connection: the case is held by this process alone, and a
	// single writer cannot meet itself busy.
	s.db.SetMaxOpenConns(1)
	s.q = rootDB{s.db}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		s.Close()
		return nil, fmt.Errorf("store: setting WAL mode: got %q, %v", mode, err)
	}
	if err := migrate(s.db, migrations); err != nil {
		s.Close()
		return nil, err
	}
	if s.enc, err = zstd.NewWriter(nil); err != nil {
		s.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	if s.dec, err = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(uint64(s.maxBlob))); err != nil {
		s.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return s, nil
}

// holder is this process as a refused one reports it: its command, process
// ID, user and host, and since when.
func holder() string {
	cmd := filepath.Base(os.Args[0])
	if len(os.Args) > 1 {
		cmd += " " + strings.Join(os.Args[1:min(len(os.Args), 3)], " ")
	}
	who := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s (pid %d), %s on %s, since %s", cmd, os.Getpid(), who, host, time.Now().UTC().Format(time.RFC3339))
}

// claim records the case the store holds, or checks it is the same, and
// marks the store used now.
func (s *Store) claim(caseID string) error {
	var got string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'case_id'`).Scan(&got); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('case_id', ?), ('created_at', ?)`, caseID, s.stamp()); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	case err != nil:
		return fmt.Errorf("store: %w", err)
	case got != caseID:
		return fmt.Errorf("store: %s holds case %s, not %s", s.dir, got, caseID)
	}
	return s.touch()
}

func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

func (s *Store) touch() error {
	if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('last_used', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, s.stamp()); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func (s *Store) Dir() string { return s.dir }

// Meta reads one of the store's own values; ok is false for a key never
// set.
func (s *Store) Meta(ctx context.Context, key string) (value string, ok bool, err error) {
	switch err := s.q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: %w", err)
	}
	return value, true, nil
}

// SetMeta sets one of the store's own values.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if _, err := s.q.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Changes counts the rows this store has written, inserted, updated or
// deleted, since it was opened: SQLite's own count for the one connection
// every writer shares, so no writer has to report its writes, and a write
// whose journal entry was lost still counts. It only grows while the
// store is open; a connection opened again starts it afresh, so a reader
// compares values for equality, never order.
func (s *Store) Changes() (int64, error) {
	var n int64
	if err := s.q.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return n, nil
}

// DB is the store's database, or within Tx, its transaction.
func (s *Store) DB() DB { return s.q }

// Close marks the store used, closes it and releases the case. The store
// Tx binds is closed by nothing: it ends with its transaction.
func (s *Store) Close() error {
	if s.bound {
		return errors.New("store: a transaction's store is not closed")
	}
	var errs []error
	if s.db != nil {
		errs = append(errs, s.touch(), s.db.Close())
	}
	if s.enc != nil {
		errs = append(errs, s.enc.Close())
	}
	if s.dec != nil {
		s.dec.Close()
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close()) // closing releases the lock
	}
	return errors.Join(errs...)
}

// lastUsed is when the store was last opened or closed.
func (s *Store) lastUsed() (time.Time, error) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'last_used'`).Scan(&v); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, v)
}

// Sweep removes the case stores under the engineer's home directory that
// have gone unused for longer than retention, and returns the directories
// it removed. A store that is open, or whose use cannot be read, is kept.
// Stores in case folders are never swept: they follow the case's own
// retention.
func Sweep(home string, retention time.Duration, now time.Time) ([]string, error) {
	root := homeCases(home)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "case.db")); err != nil {
			continue // not a store
		}
		s, err := open(dir, Options{})
		if err != nil {
			continue
		}
		last, err := s.lastUsed()
		expired := err == nil && now.Sub(last) > retention
		// Release without touching, so reading a store does not renew it.
		s.db.Close()
		s.db = nil
		if expired {
			if err := os.RemoveAll(dir); err != nil {
				s.Close()
				return removed, fmt.Errorf("store: %w", err)
			}
			removed = append(removed, dir)
		}
		s.Close()
	}
	return removed, nil
}

// Move moves a case's store from one directory into another holding none,
// as when the case folder appears for a case first opened without it. The
// source is held while it is copied: its files first, then its database,
// whole and consistent, through VACUUM INTO, renamed into place last, so a
// database at to means a complete store. Then from is removed.
func Move(from, to, caseID string, o Options) error {
	if err := copyStore(from, to, caseID, o); err != nil {
		return err
	}
	if err := os.RemoveAll(from); err != nil {
		return fmt.Errorf("store: the store is moved to %s, but removing %s: %w", to, from, err)
	}
	return nil
}

// Snapshot copies a case's store into a directory holding none, as Move
// does, and leaves the original where it is: for working on a copy, as
// measuring a case's store does, adding nothing to its investigation.
func Snapshot(from, to, caseID string, o Options) error {
	return copyStore(from, to, caseID, o)
}

// copyStore copies a store whole, holding the source while it does.
func copyStore(from, to, caseID string, o Options) error {
	if _, err := os.Stat(filepath.Join(to, "case.db")); err == nil {
		return fmt.Errorf("store: %s already holds a store", to)
	}
	src, err := Open(from, caseID, o)
	if err != nil {
		return err
	}
	moved := false
	defer func() {
		if !moved {
			src.Close()
		}
	}()
	if err := os.MkdirAll(to, 0o700); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := copyFiles(from, to); err != nil {
		return err
	}
	tmp := filepath.Join(to, "case.db.moving")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := src.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("store: copying the database: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(to, "case.db")); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	moved = true
	return src.Close()
}

// copyFiles copies everything in a store's directory but its database and
// lock: blobs and expert snapshots.
func copyFiles(from, to string) error {
	skip := map[string]bool{"case.db": true, "case.db-wal": true, "case.db-shm": true, "case.db-journal": true, "lock": true}
	return filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil || rel == "." {
			return err
		}
		if filepath.Dir(rel) == "." && skip[rel] {
			return nil
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, dst)
	})
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("store: %w", err)
	}
	return out.Close()
}

// Keep marks a home store used, so Sweep leaves it while its case is
// still being opened from elsewhere.
func Keep(dir, caseID string) error {
	s, err := Open(dir, caseID, Options{})
	if err != nil {
		return err
	}
	return s.Close()
}

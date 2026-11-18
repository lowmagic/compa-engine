package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func clock(t *time.Time) Options { return Options{Now: func() time.Time { return *t }} }

func mustOpen(t *testing.T, dir, caseID string, o Options) *Store {
	t.Helper()
	s, err := Open(dir, caseID, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLocation(t *testing.T) {
	if dir, inHome, err := Location("/cases/0420", "/home/e", "0420"); err != nil || inHome || dir != "/cases/0420/.compa" {
		t.Errorf("case folder: %s %v %v", dir, inHome, err)
	}
	if dir, inHome, err := Location("", "/home/e", "0420"); err != nil || !inHome || dir != "/home/e/.compa/cases/0420" {
		t.Errorf("home: %s %v %v", dir, inHome, err)
	}
	for _, id := range []string{"", "..", "../x", "a/b", ".hidden"} {
		if _, _, err := Location("", "/home/e", id); err == nil {
			t.Errorf("case ID %q must not name a directory", id)
		}
	}
}

func TestReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	now := t0
	s := mustOpen(t, dir, "0420", clock(&now))
	digest, err := s.PutBlob([]byte("must-gather excerpt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(time.Hour)
	s = mustOpen(t, dir, "0420", clock(&now))
	defer s.Close()
	got, err := s.Blob(digest)
	if err != nil || string(got) != "must-gather excerpt" {
		t.Errorf("blob after reopen: %q %v", got, err)
	}
	var created string
	if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key = 'created_at'`).Scan(&created); err != nil || created != t0.Format(time.RFC3339Nano) {
		t.Errorf("created_at %q %v", created, err)
	}
	if last, err := s.lastUsed(); err != nil || !last.Equal(now) {
		t.Errorf("last used %v %v", last, err)
	}
	var mode, locking string
	s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	s.DB().QueryRow(`PRAGMA locking_mode`).Scan(&locking)
	if mode != "wal" || locking != "exclusive" {
		t.Errorf("journal mode %s, locking mode %s", mode, locking)
	}
	if _, err := os.Stat(filepath.Join(dir, "case.db-shm")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a shared-memory wal-index exists: %v", err)
	}
}

// A store made before exclusive locking, whose database is already in WAL
// mode, opens with no shared-memory wal-index too.
func TestExistingWALStoreOpensExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	s := mustOpen(t, dir, "0420", Options{})
	s.Close()
	old, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "case.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO meta (key, value) VALUES ('old', 'x')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	os.Remove(filepath.Join(dir, "case.db-shm"))

	s = mustOpen(t, dir, "0420", Options{})
	defer s.Close()
	if _, err := s.PutBlob([]byte("after")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "case.db-shm")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a shared-memory wal-index exists: %v", err)
	}
}

// Two cases share nothing, and a store refuses a case it was not made for.
func TestCasesAreIsolated(t *testing.T) {
	root := t.TempDir()
	a := mustOpen(t, filepath.Join(root, "a", DirName), "case-a", Options{})
	defer a.Close()
	b := mustOpen(t, filepath.Join(root, "b", DirName), "case-b", Options{})
	defer b.Close()
	digest, err := a.PutBlob([]byte("only in a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Blob(digest); err == nil {
		t.Error("case b reads case a's blob")
	}
	b.Close()
	if _, err := Open(filepath.Join(root, "b", DirName), "case-a", Options{}); err == nil || !strings.Contains(err.Error(), "holds case case-b") {
		t.Errorf("opening b's store as case a: %v", err)
	}
}

// A second open is refused, saying which process holds the case; once the
// first closes, the case opens again.
func TestLocked(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, "0420", Options{})
	_, err := Open(dir, "0420", Options{})
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), fmt.Sprintf("(pid %d)", os.Getpid())) {
		t.Errorf("second open: %v", err)
	}
	s.Close()
	mustOpen(t, dir, "0420", Options{}).Close()
}

func TestBlobs(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, "0420", Options{MaxBlobBytes: 1024})
	defer s.Close()
	data := bytes.Repeat([]byte("router-default 0/1 Pending\n"), 30)
	d1, err := s.PutBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.PutBlob(append([]byte(nil), data...))
	if err != nil || d1 != d2 {
		t.Fatalf("same content, digests %s %s %v", d1, d2, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "blobs", "sha256", "*", "*"))
	var rows, size, stored int64
	s.DB().QueryRow(`SELECT count(*), sum(size), sum(stored_size) FROM blobs`).Scan(&rows, &size, &stored)
	if len(files) != 1 || rows != 1 || size != int64(len(data)) || stored >= size {
		t.Errorf("stored once and compressed: files %v rows %d size %d stored %d", files, rows, size, stored)
	}

	if _, err := s.PutBlob(make([]byte, 1025)); err == nil {
		t.Error("a blob over the limit was stored")
	}
	if _, err := s.Blob("../../etc/passwd"); err == nil {
		t.Error("a path was accepted as a digest")
	}

	// A damaged blob is an error, not wrong content.
	if err := os.WriteFile(files[0], s.enc.EncodeAll([]byte("tampered"), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blob(d1); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("damaged blob: %v", err)
	}

	// A blob that would decompress past the limit is refused.
	big := bytes.Repeat([]byte{'x'}, 64<<10)
	sum := sha256.Sum256(big)
	digest := hex.EncodeToString(sum[:])
	enc, _ := zstd.NewWriter(nil)
	os.MkdirAll(filepath.Dir(s.blobPath(digest)), 0o700)
	if err := os.WriteFile(s.blobPath(digest), enc.EncodeAll(big, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Blob(digest); err == nil {
		t.Error("a blob larger than the limit was decompressed")
	}
}

func schemaOf(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, sqlText string
		rows.Scan(&typ, &name, &sqlText)
		b.WriteString(typ + " " + name + ": " + sqlText + "\n")
	}
	return b.String()
}

func memDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// Every migration applies to an empty database and to one at each earlier
// version, ending at the same schema; a database from a newer engine is
// refused. Two later steps stand in for the store's future migrations, so
// the stepping is exercised before there are many real ones.
func TestMigrations(t *testing.T) {
	steps := append(append([]string{}, migrations...),
		`CREATE TABLE later (id INTEGER PRIMARY KEY)`,
		`ALTER TABLE later ADD COLUMN note TEXT`)
	fresh := memDB(t)
	if err := migrate(fresh, steps); err != nil {
		t.Fatal(err)
	}
	want := schemaOf(t, fresh)
	for k := 0; k <= len(steps); k++ {
		db := memDB(t)
		if err := migrate(db, steps[:k]); err != nil {
			t.Fatalf("to version %d: %v", k, err)
		}
		if err := migrate(db, steps); err != nil {
			t.Fatalf("from version %d: %v", k, err)
		}
		if got := schemaOf(t, db); got != want {
			t.Errorf("from version %d the schema differs:\n%s\nwant\n%s", k, got, want)
		}
		var v int
		db.QueryRow(`PRAGMA user_version`).Scan(&v)
		if v != len(steps) {
			t.Errorf("from version %d: user_version %d", k, v)
		}
	}
	if err := migrate(fresh, migrations); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("a newer database must be refused: %v", err)
	}

	db := memDB(t)
	if err := migrate(db, append(append([]string{}, migrations...), `CREATE TABLE broken (`)); err == nil {
		t.Fatal("a broken step applied")
	}
	var v int
	db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("after a failed step the version is %d, want %d", v, len(migrations))
	}
}

// TestMigration41KeepsConversations: migration 41 makes the companion's
// tables again to allow the editor's conversations and the correction and
// revise messages, keeping every row and rowid; a kind it does not know is
// still refused.
func TestMigration41KeepsConversations(t *testing.T) {
	db := memDB(t)
	if err := migrate(db, migrations[:40]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO companion_conversations (rowid, id, kind, created_at) VALUES (7, 'conv-1', 'main', 'synthetic');
		INSERT INTO companion_messages (rowid, conversation_id, seq, kind, text, created_at) VALUES (9, 'conv-1', 1, 'model', 'A synthetic answer.', 'synthetic')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, migrations[:41]); err != nil {
		t.Fatal(err)
	}
	var conv, text string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM companion_conversations WHERE rowid = 7`).Scan(&conv); err != nil || conv != "conv-1" {
		t.Errorf("the conversation after 41: %q, %v", conv, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT text FROM companion_messages WHERE rowid = 9`).Scan(&text); err != nil || text != "A synthetic answer." {
		t.Errorf("the message after 41: %q, %v", text, err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO companion_conversations (id, kind, created_at) VALUES ('editor-1', 'editor', 'synthetic');
		INSERT INTO companion_messages (conversation_id, seq, kind, text, created_at) VALUES ('conv-1', 2, 'correction', 'A synthetic correction.', 'synthetic'), ('conv-1', 3, 'revise', 'A synthetic ask.', 'synthetic'), ('conv-1', 4, 'shown', 'A synthetic answer as shown.', 'synthetic')`); err != nil {
		t.Errorf("the new kinds: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO companion_messages (conversation_id, seq, kind, text, created_at) VALUES ('conv-1', 5, 'review', 'x', 'synthetic')`); err == nil {
		t.Error("a message of a kind 41 does not know was kept")
	}
}

// TestMigration42KeepsRows: [E3] migration 42 makes the companion's tables
// and checks again to allow the reviewer's conversations, review messages
// and outcomes, keeping every row and rowid, an editor's check included,
// with no name or statements for it; a kind or an outcome it does not
// know is still refused.
func TestMigration42KeepsRows(t *testing.T) {
	db := memDB(t)
	if err := migrate(db, migrations[:41]); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_conversations (rowid, id, kind, created_at) VALUES (7, 'conv-1', 'main', 'synthetic'), (8, 'editor-1', 'editor', 'synthetic');
		INSERT INTO companion_messages (rowid, conversation_id, seq, kind, text, invocation_id, created_at) VALUES (9, 'conv-1', 1, 'model', 'A synthetic answer.', 'call-1', 'synthetic'), (10, 'conv-1', 2, 'correction', 'A synthetic correction.', '', 'synthetic');
		INSERT INTO checks (rowid, id, investigation_id, role, conversation_id, checked_invocation_id, check_conversation_id, invocation_id, schema_id, findings, dropped, outcome, error, created_at)
			VALUES (11, 'check-1', 'case-1', 'editor', 'conv-1', 'call-1', 'editor-1', 'call-2', 'schema', '{"findings":[]}', '[]', 'corrected', '', 'synthetic')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, migrations[:42]); err != nil {
		t.Fatal(err)
	}
	var kind, text, invocation, outcome, name string
	var statements int
	if err := db.QueryRowContext(ctx, `SELECT kind FROM companion_conversations WHERE rowid = 8`).Scan(&kind); err != nil || kind != "editor" {
		t.Errorf("the conversation after 42: %q, %v", kind, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT text, invocation_id FROM companion_messages WHERE rowid = 9`).Scan(&text, &invocation); err != nil || text != "A synthetic answer." || invocation != "call-1" {
		t.Errorf("the message after 42: %q %q, %v", text, invocation, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT outcome, checked_name, statements FROM checks WHERE rowid = 11 AND id = 'check-1'`).Scan(&outcome, &name, &statements); err != nil || outcome != "corrected" || name != "" || statements != 0 {
		t.Errorf("the check after 42: %q %q %d, %v", outcome, name, statements, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_conversations (id, kind, created_at) VALUES ('review-1', 'review', 'synthetic');
		INSERT INTO companion_messages (conversation_id, seq, kind, text, invocation_id, created_at) VALUES ('conv-1', 3, 'review', 'A synthetic review.', 'call-3', 'synthetic');
		INSERT INTO checks (id, investigation_id, role, conversation_id, checked_invocation_id, checked_name, statements, check_conversation_id, invocation_id, schema_id, findings, dropped, outcome, error, created_at)
			VALUES ('check-2', 'case-1', 'reviewer', 'conv-1', '', 'i1', 3, 'review-1', 'call-3', 'schema', '{}', '[]', 'reviewed', '', 'synthetic'),
			('check-3', 'case-1', 'reviewer', 'conv-1', 'call-1', 'a1', 1, 'review-1', '', 'schema', '{}', '[]', 'failed', 'synthetic failure', 'synthetic')`); err != nil {
		t.Errorf("the new kinds and outcomes: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO checks (id, investigation_id, role, conversation_id, checked_invocation_id, check_conversation_id, invocation_id, schema_id, findings, dropped, outcome, error, created_at)
		VALUES ('check-4', 'case-1', 'reviewer', 'conv-1', '', 'review-1', '', 'schema', '{}', '[]', 'verified', '', 'synthetic')`); err == nil {
		t.Error("a check of an outcome 42 does not know was kept")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_messages (conversation_id, seq, kind, text, created_at) VALUES ('conv-1', 9, 'verdict', 'x', 'synthetic')`); err == nil {
		t.Error("a message of a kind 42 does not know was kept")
	}
}

// TestMigration43KeepsRows: [E4] migration 43 makes the companion's
// messages again to allow the engineer's verdicts, keeping every row and
// rowid, and adds span_verdicts; a kind or a verdict it does not know is
// still refused, and a span is judged once per answer.
func TestMigration43KeepsRows(t *testing.T) {
	db := memDB(t)
	if err := migrate(db, migrations[:42]); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_conversations (rowid, id, kind, created_at) VALUES (7, 'conv-1', 'main', 'synthetic');
		INSERT INTO companion_messages (rowid, conversation_id, seq, kind, text, invocation_id, cache_break, created_at) VALUES
			(9, 'conv-1', 1, 'model', 'A synthetic answer.', 'call-1', 1, 'synthetic'), (12, 'conv-1', 2, 'review', 'A synthetic review.', 'call-2', 0, 'synthetic')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, migrations[:43]); err != nil {
		t.Fatal(err)
	}
	var text, invocation, kind string
	var brk int
	if err := db.QueryRowContext(ctx, `SELECT text, invocation_id, cache_break FROM companion_messages WHERE rowid = 9 AND conversation_id = 'conv-1' AND seq = 1`).Scan(&text, &invocation, &brk); err != nil ||
		text != "A synthetic answer." || invocation != "call-1" || brk != 1 {
		t.Errorf("the answer after 43: %q %q %d, %v", text, invocation, brk, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT kind FROM companion_messages WHERE rowid = 12`).Scan(&kind); err != nil || kind != "review" {
		t.Errorf("the review after 43: %q, %v", kind, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_messages (conversation_id, seq, kind, text, created_at) VALUES ('conv-1', 3, 'verdict', 'A synthetic verdict.', 'synthetic');
		INSERT INTO span_verdicts (conversation_id, answer_seq, answer_name, span, statement, evidence_id, evidence_name, line, line_text, verdict, created_at)
			VALUES ('conv-1', 1, 'a1', 'worker-1 Ready', 'worker-1 was Ready.', 'ev-1', 'e3', 2, 'worker-1 NotReady', 'wrong', 'synthetic')`); err != nil {
		t.Errorf("the new kind and a verdict: %v", err)
	}
	for what, stmt := range map[string]string{
		"a message of a kind 43 does not know": `INSERT INTO companion_messages (conversation_id, seq, kind, text, created_at) VALUES ('conv-1', 4, 'note', 'x', 'synthetic')`,
		"a verdict 43 does not know": `INSERT INTO span_verdicts (conversation_id, answer_seq, answer_name, span, statement, evidence_id, evidence_name, line, line_text, verdict, created_at)
			VALUES ('conv-1', 1, 'a1', 'other', 's', 'ev-1', 'e3', 2, 'l', 'maybe', 'synthetic')`,
		"a span judged twice": `INSERT INTO span_verdicts (conversation_id, answer_seq, answer_name, span, statement, evidence_id, evidence_name, line, line_text, verdict, created_at)
			VALUES ('conv-1', 1, 'a1', 'worker-1 Ready', 's', 'ev-1', 'e3', 2, 'l', 'stands', 'synthetic')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s was kept", what)
		}
	}
}

// TestMigration44MapsVerdicts: [E4] migration 44 keeps every verdict row
// and rowid, stands as supported and wrong as incorrect, each line as the
// verdict's one range; it takes the four kinds and several ranges, and
// still refuses a kind it does not know, and the messages are untouched.
func TestMigration44MapsVerdicts(t *testing.T) {
	db := memDB(t)
	if err := migrate(db, migrations[:43]); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_messages (rowid, conversation_id, seq, kind, text, created_at) VALUES
			(9, 'conv-1', 2, 'verdict', 'The engineer checked a1''s `+"`worker-1 Ready`"+` against e3:2: it is wrong.', 'synthetic');
		INSERT INTO span_verdicts (rowid, conversation_id, answer_seq, answer_name, span, statement, evidence_id, evidence_name, line, line_text, verdict, created_at) VALUES
			(4, 'conv-1', 1, 'a1', 'worker-1 Ready', 'worker-1 was Ready.', 'ev-1', 'e3', 2, 'worker-1 NotReady', 'wrong', 'synthetic-1'),
			(5, 'conv-1', 1, 'a1', 'worker-2 Ready', 'worker-2 was Ready.', 'ev-2', 'e4', 7, 'worker-2 Ready', 'stands', 'synthetic-2')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, migrations[:44]); err != nil {
		t.Fatal(err)
	}
	type row struct {
		rowid                  int
		span, verdict, at      string
		id, name, text         string
		ord, from, to, answers int
	}
	var got []row
	rows, err := db.QueryContext(ctx, `SELECT v.rowid, v.span, v.verdict, v.created_at, l.evidence_id, l.evidence_name, l.text, l.ord, l.from_line, l.to_line, v.answer_seq
		FROM span_verdicts v JOIN span_verdict_lines l USING (conversation_id, answer_seq, span) ORDER BY v.rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rowid, &r.span, &r.verdict, &r.at, &r.id, &r.name, &r.text, &r.ord, &r.from, &r.to, &r.answers); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{4, "worker-1 Ready", "incorrect", "synthetic-1", "ev-1", "e3", "worker-1 NotReady", 1, 2, 2, 1},
		{5, "worker-2 Ready", "supported", "synthetic-2", "ev-2", "e4", "worker-2 Ready", 1, 7, 7, 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the verdicts after 44:\n got %+v\nwant %+v", got, want)
	}
	var text string
	if err := db.QueryRowContext(ctx, `SELECT text FROM companion_messages WHERE rowid = 9`).Scan(&text); err != nil || !strings.HasSuffix(text, "it is wrong.") {
		t.Errorf("the verdict message after 44: %q, %v", text, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO span_verdicts (conversation_id, answer_seq, answer_name, span, statement, verdict, created_at) VALUES
			('conv-1', 1, 'a1', 'a', 's', 'inferred', 'synthetic'), ('conv-1', 1, 'a1', 'b', 's', 'unsupported', 'synthetic');
		INSERT INTO span_verdict_lines (conversation_id, answer_seq, span, ord, evidence_id, evidence_name, from_line, to_line, text) VALUES
			('conv-1', 1, 'a', 1, 'ev-1', 'e3', 1, 3, 'x'), ('conv-1', 1, 'a', 2, 'ev-2', 'e4', 5, 5, 'y')`); err != nil {
		t.Errorf("the new kinds and two ranges: %v", err)
	}
	for what, stmt := range map[string]string{
		"a verdict 44 does not know":      `INSERT INTO span_verdicts (conversation_id, answer_seq, answer_name, span, statement, verdict, created_at) VALUES ('conv-1', 1, 'a1', 'c', 's', 'stands', 'synthetic')`,
		"a range ending before it begins": `INSERT INTO span_verdict_lines (conversation_id, answer_seq, span, ord, evidence_id, evidence_name, from_line, to_line, text) VALUES ('conv-1', 1, 'a', 3, 'ev-1', 'e3', 4, 3, 'z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s was kept", what)
		}
	}
}

// TestMigration45MarksExposureUnknown: migration 45 leaves every
// conversation made before it with its exposure unknown and its messages
// untouched, and keeps what messages show and what requests carried,
// refusing lines that are no range.
func TestMigration45MarksExposureUnknown(t *testing.T) {
	db := memDB(t)
	if err := migrate(db, migrations[:44]); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_conversations (rowid, id, kind, created_at) VALUES (7, 'conv-1', 'main', 'synthetic');
		INSERT INTO companion_messages (rowid, conversation_id, seq, kind, text, created_at) VALUES (9, 'conv-1', 1, 'case', 'A synthetic case block.', 'synthetic')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, migrations[:45]); err != nil {
		t.Fatal(err)
	}
	var exposure int
	var text string
	if err := db.QueryRowContext(ctx, `SELECT c.exposure, m.text FROM companion_conversations c JOIN companion_messages m ON m.conversation_id = c.id WHERE c.rowid = 7 AND m.rowid = 9`).Scan(&exposure, &text); err != nil ||
		exposure != 0 || text != "A synthetic case block." {
		t.Errorf("the conversation after 45: exposure %d, %q, %v", exposure, text, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO companion_conversations (id, kind, created_at, exposure) VALUES ('conv-2', 'main', 'synthetic', 1);
		INSERT INTO companion_exposures (conversation_id, seq, evidence_name, from_line, to_line) VALUES ('conv-2', 1, 'e1', 0, 0), ('conv-2', 1, 'e2', 3, 3), ('conv-2', 1, 'e3', 3, 9);
		INSERT INTO companion_requests (invocation_id, conversation_id, through_seq) VALUES ('call-1', 'conv-2', 1)`); err != nil {
		t.Errorf("a record of what was shown: %v", err)
	}
	for what, stmt := range map[string]string{
		"lines ending before they begin": `INSERT INTO companion_exposures (conversation_id, seq, evidence_name, from_line, to_line) VALUES ('conv-2', 1, 'e4', 5, 4)`,
		"a whole item with a last line":  `INSERT INTO companion_exposures (conversation_id, seq, evidence_name, from_line, to_line) VALUES ('conv-2', 1, 'e4', 0, 4)`,
		"an exposure neither 0 nor 1":    `UPDATE companion_conversations SET exposure = 2 WHERE id = 'conv-2'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s was kept", what)
		}
	}
}

func TestSweep(t *testing.T) {
	home := t.TempDir()
	at := func(caseID string, used time.Time) string {
		dir, _, _ := Location("", home, caseID)
		mustOpen(t, dir, caseID, clock(&used)).Close()
		return dir
	}
	old := at("old", t0.Add(-20*24*time.Hour))
	recent := at("recent", t0.Add(-24*time.Hour))
	openDir := at("open", t0.Add(-30*24*time.Hour))
	held := mustOpen(t, openDir, "open", Options{Now: func() time.Time { return t0.Add(-30 * 24 * time.Hour) }})
	defer held.Close()
	notStore := filepath.Join(home, DirName, "cases", "notes")
	os.MkdirAll(notStore, 0o700)
	caseFolder := filepath.Join(t.TempDir(), DirName)
	used := t0.Add(-90 * 24 * time.Hour)
	mustOpen(t, caseFolder, "folder", clock(&used)).Close()

	removed, err := Sweep(home, 14*24*time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Errorf("removed %v, want only %s", removed, old)
	}
	for _, keep := range []string{recent, openDir, notStore, caseFolder} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed", keep)
		}
	}
	s, err := open(recent, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if last, err := s.lastUsed(); err != nil || !last.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("sweeping renewed a store: last used %v %v", last, err)
	}
}

// A moved store keeps its state and blobs, and leaves nothing behind; a
// directory already holding a store, or a store for another case, is
// refused, the source untouched.
func TestMove(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "home", "0420"), filepath.Join(root, "folder", DirName)
	s := mustOpen(t, from, "0420", Options{})
	digest, err := s.PutBlob([]byte("an engineer's capture\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO meta (key, value) VALUES ('note', 'kept')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	os.MkdirAll(filepath.Join(from, "experts", "x"), 0o700)
	os.WriteFile(filepath.Join(from, "experts", "x", "expert.yaml"), []byte("id: x\n"), 0o600)

	if err := Move(from, to, "0421", Options{}); err == nil {
		t.Error("moved a store for another case")
	}
	if err := Move(from, to, "0420", Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(from); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the source is still there: %v", err)
	}
	m := mustOpen(t, to, "0420", Options{})
	var note string
	if err := m.DB().QueryRow(`SELECT value FROM meta WHERE key = 'note'`).Scan(&note); err != nil || note != "kept" {
		t.Errorf("state: %q %v", note, err)
	}
	if b, err := m.Blob(digest); err != nil || string(b) != "an engineer's capture\n" {
		t.Errorf("blob: %q %v", b, err)
	}
	m.Close()
	if b, err := os.ReadFile(filepath.Join(to, "experts", "x", "expert.yaml")); err != nil || string(b) != "id: x\n" {
		t.Errorf("snapshot: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(to, "case.db.moving")); err == nil {
		t.Error("the temporary database was left")
	}

	other := filepath.Join(root, "home", "0422")
	mustOpen(t, other, "0422", Options{}).Close()
	if err := Move(other, to, "0422", Options{}); err == nil {
		t.Error("moved into a directory holding a store")
	}
	if _, err := os.Stat(filepath.Join(other, "case.db")); err != nil {
		t.Errorf("a refused move changed the source: %v", err)
	}
}

// Changes counts every row any writer writes, inside a transaction or not,
// with nothing to report it, and a read changes nothing.
func TestChangesCountsEveryWrite(t *testing.T) {
	s := mustOpen(t, t.TempDir(), "0420", Options{})
	defer s.Close()
	at := func() int64 {
		t.Helper()
		n, err := s.Changes()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	start := at()
	if _, err := s.DB().Exec(`INSERT INTO comment_exclusions (id, kind, value, created_at) VALUES ('x', 'private', '', 'now')`); err != nil {
		t.Fatal(err)
	}
	if got := at(); got != start+1 {
		t.Errorf("after one insert: %d, from %d", got, start)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec(`UPDATE comment_exclusions SET value = 'y'`)
	tx.Exec(`DELETE FROM comment_exclusions`)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := at(); got != start+3 {
		t.Errorf("after a transaction of two writes: %d, from %d", got, start)
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM comment_exclusions`).Scan(&n)
	if got := at(); got != start+3 {
		t.Errorf("a read counted: %d", got)
	}
}

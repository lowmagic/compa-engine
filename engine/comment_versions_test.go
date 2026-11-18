package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// commentVersion is one stored version of a case comment, as the
// comment-versions report reads it: no text of the comment but a hash of
// its body.
type commentVersion struct {
	evidenceID, stored string
	cm                 CaseComment
	bodyHash           string
	read               bool
	items              int
}

// changedFields names what differs between two versions of one comment.
func changedFields(a, b commentVersion) []string {
	var out []string
	for _, f := range []struct {
		name   string
		differ bool
	}{
		{"body", a.bodyHash != b.bodyHash},
		{"updated", !a.cm.Updated.Equal(b.cm.Updated)},
		{"visibility", a.cm.Visibility != b.cm.Visibility},
		{"author", a.cm.Author != b.cm.Author},
		{"author kind", a.cm.AuthorKind != b.cm.AuthorKind},
		{"automated", a.cm.Automated != b.cm.Automated},
		{"created", !a.cm.Created.Equal(b.cm.Created)},
	} {
		if f.differ {
			out = append(out, f.name)
		}
	}
	return out
}

var blobDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readBlob reads a blob from a store's directory without opening the
// store, which would lock it and run its migrations: the file is the
// store's layout, and its content is checked against its digest.
func readBlob(dir, digest string) ([]byte, error) {
	if !blobDigest.MatchString(digest) {
		return nil, fmt.Errorf("%q is not a blob digest", digest)
	}
	compressed, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", digest[:2], digest+".zst"))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	data, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("blob %s does not match its digest", digest)
	}
	return data, nil
}

// eachRow runs each over the rows a query returns.
func eachRow(ctx context.Context, db *sql.DB, query string, each func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// storedCommentVersions reads every version of every comment a store
// holds, oldest first, with whether the ledger read each and how many of
// its items cite it.
func storedCommentVersions(ctx context.Context, db *sql.DB, dir string) (map[string][]commentVersion, error) {
	read := map[string]bool{}
	if err := eachRow(ctx, db, `SELECT evidence_id FROM ledger_read`, func(r *sql.Rows) error {
		var id string
		err := r.Scan(&id)
		read[id] = true
		return err
	}); err != nil {
		return nil, err
	}
	items := map[string]int{}
	if err := eachRow(ctx, db, `SELECT comment_id, COUNT(*) FROM ledger_items GROUP BY comment_id`, func(r *sql.Rows) error {
		var id string
		var n int
		err := r.Scan(&id, &n)
		items[id] = n
		return err
	}); err != nil {
		return nil, err
	}
	out := map[string][]commentVersion{}
	if err := eachRow(ctx, db, `SELECT id, blob, created_at FROM evidence WHERE type = ?`, func(r *sql.Rows) error {
		var v commentVersion
		var digest string
		if err := r.Scan(&v.evidenceID, &digest, &v.stored); err != nil {
			return err
		}
		data, err := readBlob(dir, digest)
		if err != nil {
			return fmt.Errorf("%s: %w", v.evidenceID, err)
		}
		v.cm = commentOf(v.evidenceID, data)
		sum := sha256.Sum256([]byte(v.cm.Body))
		v.bodyHash = hex.EncodeToString(sum[:])[:12]
		v.read, v.items = read[v.evidenceID], items[v.evidenceID]
		out[v.cm.ID] = append(out[v.cm.ID], v)
		return nil
	}, TypeCaseComment); err != nil {
		return nil, err
	}
	for _, vs := range out {
		sort.Slice(vs, func(i, j int) bool {
			if vs[i].stored != vs[j].stored {
				return vs[i].stored < vs[j].stored
			}
			return vs[i].cm.Updated.Before(vs[j].cm.Updated)
		})
	}
	return out, nil
}

// commentVersionsReport says, for each comment stored in more than one
// version, what changed between versions and what the ledger made of
// each: IDs, times, field names, hashes and counts, never a comment's text.
func commentVersionsReport(all map[string][]commentVersion) string {
	var ids []string
	changes, bodyChanged, bodySame := 0, 0, 0
	sameFields := map[string]int{}
	for id, vs := range all {
		if len(vs) < 2 {
			continue
		}
		ids = append(ids, id)
		for i := 1; i < len(vs); i++ {
			changes++
			fs := changedFields(vs[i-1], vs[i])
			if strings.Contains(strings.Join(fs, ","), "body") {
				bodyChanged++
				continue
			}
			bodySame++
			for _, f := range fs {
				sameFields[f]++
			}
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	fmt.Fprintf(&b, "comments stored: %d, in more than one version: %d\n", len(all), len(ids))
	fmt.Fprintf(&b, "changes between versions: %d; body changed in %d, body unchanged in %d", changes, bodyChanged, bodySame)
	if bodySame > 0 {
		var fs []string
		for f, n := range sameFields {
			fs = append(fs, fmt.Sprintf("%s %d", f, n))
		}
		sort.Strings(fs)
		fmt.Fprintf(&b, " (what changed instead: %s)", strings.Join(fs, ", "))
	}
	b.WriteString("\n")
	for _, id := range ids {
		fmt.Fprintf(&b, "comment %s:\n", id)
		vs := all[id]
		for i, v := range vs {
			updated := "-"
			if !v.cm.Updated.IsZero() {
				updated = v.cm.Updated.UTC().Format(time.RFC3339)
			}
			ledger := "no"
			if v.read {
				ledger = "yes"
			}
			fmt.Fprintf(&b, "  v%d %s stored %s updated %s visibility %s body %s; ledger read %s, items %d",
				i+1, v.evidenceID, v.stored, updated, v.cm.Visibility, v.bodyHash, ledger, v.items)
			if i > 0 {
				fs := changedFields(vs[i-1], v)
				if len(fs) == 0 {
					fs = []string{"nothing read here"}
				}
				fmt.Fprintf(&b, "; changed: %s", strings.Join(fs, ", "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestCommentVersionsOnStores reports, opt-in, how a real case store's
// comments changed between the versions it holds: for each store named
// in COMPA_COMMENT_VERSIONS_STORES, separated by ':', read-only, IDs, times,
// field names, body hashes and counts only. With compa not running on
// those cases:
//
//	COMPA_COMMENT_VERSIONS_STORES=$HOME/<case>/.compa \
//	  go test ./engine -run TestCommentVersionsOnStores -count=1 -v
//
// -count=1 because the test cache does not see the store change: the
// SQLite driver opens it outside the os package the cache watches.
//
// What it shows: whether the case source gives a comment a new version
// with its body unchanged, which would make the ledger read it again
// though nothing in it changed; and, for an edited comment, whether the
// ledger read the new version and how many items still cite the old one.
// It says nothing
// of what the ledger's model made of either version.
func TestCommentVersionsOnStores(t *testing.T) {
	paths := os.Getenv("COMPA_COMMENT_VERSIONS_STORES")
	if paths == "" {
		t.Skip("set COMPA_COMMENT_VERSIONS_STORES to report on real case stores")
	}
	for i, p := range strings.Split(paths, ":") {
		dir := p
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			dir = filepath.Dir(p)
		}
		db, err := openReadOnly(t.Context(), p)
		if err != nil {
			t.Fatalf("store %d: %v", i+1, err)
		}
		all, err := storedCommentVersions(t.Context(), db, dir)
		db.Close()
		if err != nil {
			t.Fatalf("store %d: %v", i+1, err)
		}
		t.Logf("store %d\n%s", i+1, commentVersionsReport(all))
	}
}

// The comment-versions reader reads a store's comment versions without
// writing to it: an edited comment's body change, a change to its updated
// time alone, what the ledger read and the items citing each version, and
// nothing of the comments' text in the report.
func TestCommentVersionsReadsAStore(t *testing.T) {
	_, c, ids := summaryCase(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	// An older store holds the old ledger's reads and items,
	// which the reader reports.
	if _, err := c.st.DB().ExecContext(t.Context(), `INSERT INTO ledger_read (investigation_id, evidence_id) VALUES (?, ?)`, "inv-1", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.DB().ExecContext(t.Context(), `INSERT INTO ledger_items
		(investigation_id, id, n, kind, who, text, comment_id, due, status, status_by, status_note, corrected, made_by, created_at)
		VALUES ('inv-1', 'l1', 1, 'reports', 'customer', 'the upgrade started it', ?, '', 'open', '', '', 'added by the engineer', 'engineer', ?)`,
		ids[1], at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	for _, cm := range []Comment{
		{ID: "c1", Visibility: VisibilityPublic, Created: at, Updated: at.Add(2 * time.Hour), Body: "Started after the upgrade to 4.16, edited."},
		{ID: "c2", Visibility: VisibilityPrivate, Created: at.Add(time.Hour), Updated: at.Add(3 * time.Hour), Body: "Asked for a must-gather."},
	} {
		if err := c.ev.Add(commentItem("stub", "00000001", cm)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := c.st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(t.Context(), c.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	all, err := storedCommentVersions(t.Context(), db, c.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all["c1"]) != 2 || len(all["c2"]) != 2 {
		t.Fatalf("versions: c1 %d, c2 %d, want 2 each", len(all["c1"]), len(all["c2"]))
	}
	if got := changedFields(all["c1"][0], all["c1"][1]); strings.Join(got, ",") != "body,updated" {
		t.Errorf("c1 changed %v, want body and updated", got)
	}
	if got := changedFields(all["c2"][0], all["c2"][1]); strings.Join(got, ",") != "updated" {
		t.Errorf("c2 changed %v, want updated alone", got)
	}
	if v := all["c1"][0]; !v.read || v.items != 1 || v.evidenceID != ids[1] {
		t.Errorf("c1's first version: read %v, %d items, %s", v.read, v.items, v.evidenceID)
	}
	if v := all["c1"][1]; v.read || v.items != 0 {
		t.Errorf("c1's edit: read %v, %d items", v.read, v.items)
	}
	report := commentVersionsReport(all)
	if !strings.Contains(report, "body changed in 1, body unchanged in 1 (what changed instead: updated 1)") {
		t.Errorf("report's counts:\n%s", report)
	}
	for _, text := range []string{"Started after the upgrade", "4.16", "must-gather", "the upgrade started it"} {
		if strings.Contains(report, text) {
			t.Errorf("the report holds the case's text %q:\n%s", text, report)
		}
	}
}

// openReadOnly opens a case store's database for reading only: compa must
// not have the case open, since the store is locked while it does.
func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "case.db")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	return db, db.PingContext(ctx)
}

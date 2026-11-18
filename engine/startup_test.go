package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// heldCases is a case source whose comments wait until released.
type heldCases struct {
	release chan struct{}
	asked   chan struct{}
}

func (heldCases) Name() string { return "held" }
func (heldCases) Case(_ context.Context, id string) (Case, error) {
	return Case{ID: id, Subject: "Volumes will not attach"}, nil
}
func (h heldCases) Comments(ctx context.Context, id string) ([]Comment, error) {
	h.asked <- struct{}{}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []Comment{{ID: "c1", Author: Party{Name: "J. Doe"}, Body: "sosreport attached", Created: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}}, nil
}

// startupCase is a case with an artifact directory holding one extracted
// sosreport, its comments held by the case source.
func startupCase(t *testing.T) (*Engine, heldCases, *snapshotArtifacts) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeTree(t, root, "0010-sosreport", 3)
	cases := heldCases{release: make(chan struct{}), asked: make(chan struct{}, 16)}
	arts := &snapshotArtifacts{root: root, atts: []Attachment{{Name: "0010-sosreport", Dir: "0010-sosreport", Size: -1, State: AttachmentExtracted, Kind: "sosreport"}}}
	e, err := New(Options{Cases: cases, Artifacts: arts, Home: t.TempDir(), Config: []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n")})
	if err != nil {
		t.Fatal(err)
	}
	return e, cases, arts
}

func writeTree(t *testing.T, root, dir string, files int) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, dir, "var", "log"), 0o700)
	for i := range files {
		os.WriteFile(filepath.Join(root, dir, "var", "log", string(rune('a'+i))+".log"), []byte("line\n"), 0o600)
	}
}

// A case starts once its details and attachment list are read: before
// its comments come back and before any walk. Then, in the background,
// the walk ends while the comments are still held.
func TestCaseStartsBeforeCommentsAndWalk(t *testing.T) {
	e, cases, _ := startupCase(t)
	ctx := context.Background()
	s, err := e.startCase(ctx, "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.c.Close()
	if s.c.walked != 0 {
		t.Errorf("starting walked %d attachments", s.c.walked)
	}
	if ids, _ := readingOf(s.c, TypeArtifactOverview); len(ids) != 1 {
		t.Fatalf("no attachment list on starting: %v", ids)
	}
	sess := e.startedSession(s)
	commentsRead := make(chan error, 1)
	go func() { commentsRead <- sess.ReadComments(ctx) }()
	<-cases.asked
	if err := sess.ReadCounts(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-commentsRead:
		t.Fatalf("the comments were read before they were released: %v", err)
	default:
	}
	if ids, _ := readingOf(s.c, TypeArtifactCounts); len(ids) != 1 || s.c.walked != 1 {
		t.Errorf("counts %v, %d walked", ids, s.c.walked)
	}
	close(cases.release)
	if err := <-commentsRead; err != nil {
		t.Fatal(err)
	}
	if ids, _ := readingOf(s.c, TypeCaseComment); len(ids) != 1 {
		t.Errorf("the comments read: %v", ids)
	}
}

// A reopened case whose attachments are unchanged walks nothing; a new
// attachment is walked alone. The attachment list, and so what the case
// block shows, does not change when only a count does; the counts are an
// item beside it.
func TestReopenWalksOnlyWhatChanged(t *testing.T) {
	e, cases, arts := startupCase(t)
	close(cases.release)
	open := func() *openCase {
		t.Helper()
		c, err := e.open(context.Background(), "00000001", true)
		if err != nil {
			t.Fatal(err)
		}
		kase, _ := cases.Case(context.Background(), "00000001")
		cms, _ := cases.Comments(context.Background(), "00000001")
		if _, err := e.acquire(context.Background(), c, kase, cms); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	ctx1, _ := companionItems(c)
	counts1, _ := readingOf(c, TypeArtifactCounts)
	if c.walked != 1 || len(counts1) != 1 {
		t.Fatalf("first open: %d walked, counts %v", c.walked, counts1)
	}
	c.Close()

	c = open()
	if c.walked != 0 {
		t.Errorf("an unchanged reopen walked %d attachments", c.walked)
	}
	if counts, _ := readingOf(c, TypeArtifactCounts); len(counts) != 1 || counts[0] != counts1[0] {
		t.Errorf("counts after an unchanged reopen: %v", counts)
	}
	// Only a count changes: the attachment as its source reports it is the
	// same, its contents counted afresh.
	writeTree(t, arts.root, "0010-sosreport", 5)
	if _, err := replaceInReading(c, TypeArtifactCounts, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.readCounts(context.Background(), c, arts.atts); err != nil {
		t.Fatal(err)
	}
	ctx2, _ := companionItems(c)
	counts2, _ := readingOf(c, TypeArtifactCounts)
	if strings.Join(ctx2, " ") != strings.Join(ctx1, " ") || counts2[0] == counts1[0] {
		t.Errorf("a count changed the case's context: %v, %v; counts %v, %v", ctx1, ctx2, counts1, counts2)
	}
	for _, id := range ctx2 {
		if id == counts2[0] {
			t.Error("the case block shows the counts")
		}
	}
	c.Close()

	writeTree(t, arts.root, "0020-must-gather", 2)
	arts.atts = append(arts.atts, Attachment{Name: "0020-must-gather", Dir: "0020-must-gather", Size: -1, State: AttachmentExtracted})
	c = open()
	defer c.Close()
	if c.walked != 1 {
		t.Errorf("a new attachment beside an unchanged one: %d walked", c.walked)
	}
	counts, _ := readingOf(c, TypeArtifactCounts)
	it, _ := c.ev.Get(counts[0])
	if !strings.Contains(string(it.Content), "attachment: 0010-sosreport") || !strings.Contains(string(it.Content), "attachment: 0020-must-gather") {
		t.Errorf("the counts:\n%s", it.Content)
	}
}

// A walk cancelled, as quitting cancels it, records no counts: the
// reading is as it was.
func TestCancelledWalkLeavesNothing(t *testing.T) {
	e, cases, arts := startupCase(t)
	close(cases.release)
	s, err := e.startCase(context.Background(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.c.Close()
	before, _, _ := s.c.src.Latest()
	items, _ := s.c.ev.List()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.readCounts(ctx, s.c, arts.atts); err == nil {
		t.Fatal("a cancelled walk succeeded")
	}
	after, _, _ := s.c.src.Latest()
	still, _ := s.c.ev.List()
	if strings.Join(after.Evidence, " ") != strings.Join(before.Evidence, " ") || len(still) != len(items) {
		t.Errorf("a cancelled walk left %d items, reading %v (was %v)", len(still)-len(items), after.Evidence, before.Evidence)
	}
}

// unreadableComments is a case source whose comments cannot be read.
type unreadableComments struct{ heldCases }

func (unreadableComments) Comments(context.Context, string) ([]Comment, error) {
	return nil, errors.New("gateway timeout")
}

// Comments that could not be read are said to the caller, what was read
// before standing.
func TestUnreadableCommentsAreSaid(t *testing.T) {
	_, held, arts := startupCase(t)
	e, err := New(Options{Cases: unreadableComments{held}, Artifacts: arts, Home: t.TempDir(), Config: []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.startCase(context.Background(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.c.Close()
	want := "the case's comments could not be read: gateway timeout; what was read before stands"
	if err := e.startedSession(s).ReadComments(context.Background()); err == nil || err.Error() != want {
		t.Errorf("ReadComments() = %v, want %q", err, want)
	}
}

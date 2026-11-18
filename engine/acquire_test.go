package engine

import (
	"context"
	"strings"
	"testing"
)

// snapshotArtifacts reports whatever attachments it is set to.
type snapshotArtifacts struct {
	root string
	atts []Attachment
}

func (s *snapshotArtifacts) Name() string                                 { return "snapshot" }
func (s *snapshotArtifacts) Root(context.Context, string) (string, error) { return s.root, nil }
func (s *snapshotArtifacts) Attachments(context.Context, string) ([]Attachment, error) {
	return s.atts, nil
}

// What the case block offers is what the case is now: case metadata that
// returns to an earlier version is that version, and a comment gone from
// the source is gone from the case's items.
func TestCaseContextIsTheLatestAcquisition(t *testing.T) {
	e, c := actionCase(t, "")
	e.cases, e.artifacts, c.root = nopCases{}, nopArtifacts{}, ""
	open, closed := Case{ID: c.id, Subject: "case", Status: "Open"}, Case{ID: c.id, Subject: "case", Status: "Closed"}
	cm := Comment{ID: "c1", Body: "This comment will be deleted."}
	for _, step := range []struct {
		kase     Case
		comments []Comment
	}{{open, []Comment{cm}}, {closed, []Comment{cm}}, {open, nil}} {
		if _, err := e.acquire(context.Background(), c, step.kase, step.comments); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := companionItems(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("the context holds %v, want only the metadata", ids)
	}
	if it, err := c.ev.Get(ids[0]); err != nil || !strings.Contains(string(it.Content), "status: Open") {
		t.Errorf("the metadata offered is not the current one: %s %v", it.Content, err)
	}
}

// Attachments A, then A and B, then A again: the overview offered is A's.
func TestAttachmentsAreTheLatestAcquisition(t *testing.T) {
	e, c := actionCase(t, "")
	c.root = t.TempDir()
	s := &snapshotArtifacts{root: c.root}
	e.cases, e.artifacts = nopCases{}, s
	a := Attachment{Name: "a.log", Path: "a.log", Size: 1, State: AttachmentPresent}
	b := Attachment{Name: "b.log", Path: "b.log", Size: 1, State: AttachmentPresent}
	kase := Case{ID: c.id, Subject: "case"}
	for _, snapshot := range [][]Attachment{{a}, {a, b}, {a}} {
		s.atts = snapshot
		if _, err := e.acquire(context.Background(), c, kase, nil); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := companionItems(c)
	if err != nil {
		t.Fatal(err)
	}
	var overview string
	for _, id := range ids {
		if it, _ := c.ev.Get(id); it.Ref.Type == TypeArtifactOverview {
			overview = string(it.Content)
		}
	}
	if !strings.Contains(overview, "attachment: a.log") || strings.Contains(overview, "b.log") {
		t.Errorf("the overview offered is not the current one:\n%s", overview)
	}

}

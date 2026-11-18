package engine

import (
	"strings"
	"testing"
	"time"
)

func TestEvidenceIDsFollowContentAndOrigin(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	c := Comment{ID: "c1", Author: Party{Name: "customer"}, Visibility: VisibilityPublic, Created: at, Body: "hello"}
	a := commentItem("stub", "00000001", c)
	if b := commentItem("stub", "00000001", c); b.Ref.ID != a.Ref.ID || string(b.Content) != string(a.Content) {
		t.Error("the same comment rendered twice differs")
	}
	edited := c
	edited.Body = "hello again"
	moved := c
	moved.ID = "c2"
	for name, other := range map[string]string{
		"edited body":    commentItem("stub", "00000001", edited).Ref.ID,
		"another case":   commentItem("stub", "00000002", c).Ref.ID,
		"another ID":     commentItem("stub", "00000001", moved).Ref.ID,
		"another source": commentItem("other", "00000001", c).Ref.ID,
	} {
		if other == a.Ref.ID {
			t.Errorf("%s keeps the ID %s", name, a.Ref.ID)
		}
	}
	want := "case: 00000001\ncomment: c1\nauthor: customer\nvisibility: public\ncreated: 2026-10-01T09:00:00Z\n\nhello\n"
	if string(a.Content) != want || a.Ref.Type != TypeCaseComment || a.Ref.Provenance.Source != "stub" || a.Location != "case 00000001 comment c1" {
		t.Errorf("comment item = %+v\n%s", a.Ref, a.Content)
	}
	// A field's newline would read as another field; the body keeps its own.
	k := caseItem("stub", Case{ID: "00000001", Subject: "two\nlines", Description: "body\nlines"})
	if !strings.Contains(string(k.Content), "subject: two lines\n") || !strings.HasSuffix(string(k.Content), "\nbody\nlines\n") {
		t.Errorf("case item:\n%s", k.Content)
	}
}

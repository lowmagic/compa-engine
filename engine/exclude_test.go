package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/sources"
)

// excludeCase is the summary case with a third comment, a bot's, and the
// author kinds the source gives: c1 the customer's, public; c2 support's,
// private; c3 support's, public.
func excludeCase(t *testing.T) *openCase {
	t.Helper()
	_, c, _ := summaryCase(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, cm := range []Comment{
		{ID: "c1", Author: Party{Name: "Jane Customer"}, AuthorKind: AuthorCustomer, Visibility: VisibilityPublic, Created: at, Body: "Started after the upgrade."},
		{ID: "c2", Author: Party{Name: "Sam Support"}, AuthorKind: AuthorSupport, Visibility: VisibilityPrivate, Created: at.Add(time.Hour), Body: "Asked for a must-gather."},
		{ID: "c3", Author: Party{Name: "Bot"}, AuthorKind: AuthorSupport, Visibility: VisibilityPublic, Created: at.Add(3 * time.Hour), Body: "Automated reminder.\nSecond line."},
	} {
		time.Sleep(2 * time.Millisecond)
		if err := c.ev.Add(commentItem("stub", "00000001", cm)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// readComments are the comments the case block gives models, newest
// first.
func readComments(t *testing.T, c *openCase) string {
	t.Helper()
	got, err := companionItems(c)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, id := range got {
		if it, _ := c.ev.Get(id); it.Ref.Type == TypeCaseComment {
			out = append(out, commentOf(id, it.Content).ID)
		}
	}
	return strings.Join(out, ",")
}

// Rules leave comments out of what models read, and removed, read them
// again. A rule already held is not recorded twice. A comment whose author
// kind the source does not give is left out by neither the support rule
// nor the customer one.
func TestExcludeComments(t *testing.T) {
	c := excludeCase(t)
	if got := readComments(t, c); got != "c3,c2,c1" {
		t.Fatalf("read before any rule: %s", got)
	}
	for _, tc := range []struct{ kind, value, want string }{
		{ExcludeComment, "c2", "c3,c1"},
		{ExcludeAuthor, "Bot", "c2,c1"},
		{ExcludePrivate, "", "c3,c1"},
		{ExcludePublic, "", "c2"},
		{ExcludeSupport, "", "c1"},
		{ExcludeCustomer, "", "c3,c2"},
		{ExcludeOlder, "c2", "c3"},
		{ExcludeAll, "", ""},
	} {
		r, err := excludeComments(c, tc.kind, tc.value)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if again, err := excludeComments(c, tc.kind, tc.value); err != nil || again.ID != r.ID {
			t.Errorf("%s again: rule %s, %v; want the one held, %s", tc.kind, again.ID, err, r.ID)
		}
		if got := readComments(t, c); got != tc.want {
			t.Errorf("%s %q: models read %s, want %s", tc.kind, tc.value, got, tc.want)
		}
		if err := includeComments(c, r.ID); err != nil {
			t.Fatalf("%s removed: %v", tc.kind, err)
		}
		if got := readComments(t, c); got != "c3,c2,c1" {
			t.Errorf("after %s removed: %s", tc.kind, got)
		}
	}
	// With no author kind, neither group rule touches a comment.
	c.ev.Add(commentItem("stub", "00000001", Comment{ID: "c4", Visibility: VisibilityPublic, Created: time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC), Body: "Unknown."}))
	for _, kind := range []string{ExcludeSupport, ExcludeCustomer} {
		if _, err := excludeComments(c, kind, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := readComments(t, c); got != "c4" {
		t.Errorf("both kinds left out: %s", got)
	}
}

// The comments listed are the versions models are given: those the latest
// acquisition holds. A comment edited to B and back to A shows A, though
// B is the newest version stored.
func TestCommentsListTheAcquiredVersion(t *testing.T) {
	c := excludeCase(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	meta := caseItem("stub", Case{ID: "00000001", Subject: "s"})
	a := commentItem("stub", "00000001", Comment{ID: "c1", Visibility: VisibilityPublic, Created: at, Body: "Version A."})
	b := commentItem("stub", "00000001", Comment{ID: "c1", Visibility: VisibilityPublic, Created: at, Body: "Version B."})
	for _, it := range []evidence.Item{meta, a, b} {
		time.Sleep(2 * time.Millisecond)
		c.ev.Add(it)
	}
	for _, ids := range [][]string{{meta.Ref.ID, a.Ref.ID}, {meta.Ref.ID, b.Ref.ID}, {meta.Ref.ID, a.Ref.ID}} {
		time.Sleep(2 * time.Millisecond)
		if err := c.src.Acquired(sources.Acquisition{Evidence: ids, Origin: "stub", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	all := caseCommentsAll(c)
	if len(all) != 1 || all[0].Body != "Version A." {
		t.Errorf("listed %+v", all)
	}
}

// An author's rule holds the name as the comment showed it then, with the
// author's ID where the source gives one; the same author whose name the
// source now writes otherwise — decoded, or renamed — is still left out.
func TestAuthorRuleFollowsTheAuthorsID(t *testing.T) {
	rules := []Exclusion{{Kind: ExcludeAuthor, Value: "Ann O&#39;Neil (005A)"}}
	if ExcludedBy(rules, CaseComment{ID: "c1", Author: "Ann O'Neil (005A)"}) == nil {
		t.Error("the same author, the name decoded, is read")
	}
	if ExcludedBy(rules, CaseComment{ID: "c2", Author: "Ann O'Neil (005B)"}) != nil {
		t.Error("another author of the same name is left out")
	}
	if ExcludedBy([]Exclusion{{Kind: ExcludeAuthor, Value: "Bot"}}, CaseComment{ID: "c3", Author: "Bot"}) == nil {
		t.Error("a name with no ID no longer matches itself")
	}

}

func TestSupportExclusionCutover(t *testing.T) {
	c := excludeCase(t)
	if _, err := excludeComments(c, ExcludeKind, ""); err == nil {
		t.Error("a rule by author kind was taken without a kind")
	}
	if _, err := excludeComments(c, "support", ""); err != nil {
		t.Fatal(err)
	}
	if got := readComments(t, c); got != "c1" {
		t.Errorf("support exclusion reads %q, want c1", got)
	}
}

// TestAuthorKindOfTheSources: an author kind the engine has no word for
// is a source's to report. A rule leaves its comments out and no other's,
// and it is shown under its own name.
func TestAuthorKindOfTheSources(t *testing.T) {
	rules := []Exclusion{{Kind: ExcludeKind, Value: "partner"}}
	partner := CaseComment{ID: "c1", AuthorName: "Pat Synthetic", AuthorKind: "partner", Visibility: string(VisibilityPublic)}
	if ExcludedBy(rules, partner) == nil {
		t.Error("a partner's comment is read under a rule leaving partners out")
	}
	for _, cm := range []CaseComment{{ID: "c2", AuthorKind: AuthorCustomer}, {ID: "c3", AuthorKind: AuthorSupport}, {ID: "c4"}} {
		if ExcludedBy(rules, cm) != nil {
			t.Errorf("%s, author kind %q, is left out by a rule for partners", cm.ID, cm.AuthorKind)
		}
	}
	if got := strings.Join(commentFacts(partner, time.UTC, "Agent"), " · "); !strings.Contains(got, "Pat Synthetic · Partner · Public") {
		t.Errorf("the ledger's header = %q, want the author, Partner and Public", got)
	}
}

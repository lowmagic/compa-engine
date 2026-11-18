package stubcases_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/sourcetest"
	"github.com/lowmagic/compa-engine/engine/stubcases"
)

const fixtures = "../../testdata/cases"

// A case's routing inputs are read as written; the field routing keys had
// before routingKeys is refused, as any unknown field is.
func TestRoutingInputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00000001.yaml"), []byte("id: '00000001'\nroutingKeys: [a]\ncomponents: [csi-driver]\ntags: [volume-attach]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := stubcases.New(dir).Case(context.Background(), "00000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RoutingKeys) != 1 || c.RoutingKeys[0] != "a" || len(c.Components) != 1 || c.Components[0] != "csi-driver" || len(c.Tags) != 1 || c.Tags[0] != "volume-attach" {
		t.Fatalf("source lost routing inputs: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(dir, "00000002.yaml"), []byte("id: '00000002'\nsbrs: [a]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := stubcases.New(dir).Case(context.Background(), "00000002"); err == nil || !strings.Contains(err.Error(), "field sbrs not found") {
		t.Errorf("the old field: %+v, %v; want it refused as an unknown field", c, err)
	}
}

func TestConformance(t *testing.T) {
	for _, id := range []string{"00000001", "00000002", "00000004"} {
		t.Run(id, func(t *testing.T) {
			sourcetest.TestCaseSource(t, stubcases.New(fixtures), id, "99999999")
		})
	}
}

func TestFixture(t *testing.T) {
	ctx := context.Background()
	src := stubcases.New(fixtures)
	c, err := src.Case(ctx, "00000001")
	if err != nil {
		t.Fatal(err)
	}
	if c.ProductVersion != "4.16" || len(c.RoutingKeys) != 1 || c.RoutingKeys[0] != "openshift-networking" || c.Created.IsZero() ||
		!c.WaitingOnSupport || c.ClusterVersion != "4.16.3" || c.Owner.Name != "support-engineer" {
		t.Errorf("case = %+v", c)
	}
	comments, err := src.Comments(ctx, "00000001")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, cm := range comments {
		ids = append(ids, cm.ID)
	}
	if got := strings.Join(ids, ","); got != "c1,c2,c3" {
		t.Errorf("comment order = %s, want c1,c2,c3 (oldest first, ties by ID)", got)
	}
	if comments[2].Visibility != engine.VisibilityPrivate || comments[0].Visibility != engine.VisibilityPublic {
		t.Errorf("visibility: c1 %q, c3 %q", comments[0].Visibility, comments[2].Visibility)
	}
	if comments[2].Author.Name != "support-engineer" || comments[1].Updated.IsZero() {
		t.Errorf("c3 author %+v, c2 updated %v", comments[2].Author, comments[1].Updated)
	}
	if none, err := src.Comments(ctx, "00000002"); err != nil || len(none) != 0 {
		t.Errorf("Comments(00000002) = %v, %v; want none", none, err)
	}
}

// The comment viewer's fixture loads with each comment's author kind and
// automation mark; synthetic is the fixture's own mark and reaches no
// comment.
func TestViewerFixture(t *testing.T) {
	ctx := context.Background()
	src := stubcases.New(fixtures)
	c, err := src.Case(ctx, "00000004")
	if err != nil {
		t.Fatal(err)
	}
	comments, err := src.Comments(ctx, "00000004")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 18 || c.Owner.ID != "user-1" {
		t.Fatalf("%d comments, owner %+v", len(comments), c.Owner)
	}
	kinds := map[string]int{}
	var automated []string
	for _, cm := range comments {
		kinds[cm.AuthorKind]++
		if cm.Automated {
			automated = append(automated, cm.ID)
		}
	}
	if kinds[engine.AuthorSupport] != 10 || kinds[engine.AuthorCustomer] != 8 || len(kinds) != 2 {
		t.Errorf("author kinds %v", kinds)
	}
	if strings.Join(automated, ",") != "c5" {
		t.Errorf("automated: %v", automated)
	}
	if c17 := comments[16]; c17.ID != "c17" || c17.Updated.IsZero() || c17.Author.ID != "user-5" {
		t.Errorf("c17: %+v", c17)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	write := func(id, body string) {
		if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("unknown-field", "id: unknown-field\ncolour: blue\n")
	write("wrong-id", "id: other\n")
	write("repeated", "id: repeated\ncomments:\n  - {id: a, visibility: public}\n  - {id: a, visibility: public}\n")
	write("visibility", "id: visibility\ncomments:\n  - {id: a, visibility: internal}\n")
	src := stubcases.New(dir)

	for id, want := range map[string]string{
		"unknown-field": "colour",
		"wrong-id":      `says its ID is "other"`,
		"repeated":      `repeated ID "a"`,
		"visibility":    `visibility "internal"`,
		"absent":        filepath.Join(dir, "absent.yaml"),
	} {
		_, err := src.Case(ctx, id)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), id) {
			t.Errorf("Case(%q) = %v, want an error naming the case and %q", id, err, want)
		}
	}
	if _, err := src.Case(ctx, "absent"); !errors.Is(err, engine.ErrCaseNotFound) {
		t.Errorf("absent case: %v", err)
	}
}

// An author kind is the source's own word: the two the engine has labels
// for and any other reach the comment as written.
func TestAuthorKindIsTheSources(t *testing.T) {
	for _, kind := range []string{"support", "customer", "partner", "vendor"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			body := "id: synthetic\ncomments:\n  - {id: c1, visibility: public, authorKind: " + kind + "}\n"
			if err := os.WriteFile(filepath.Join(dir, "synthetic.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			comments, err := stubcases.New(dir).Comments(context.Background(), "synthetic")
			if err != nil || len(comments) != 1 || comments[0].AuthorKind != kind {
				t.Errorf("Comments authorKind %q = %+v, %v", kind, comments, err)
			}
		})
	}
}

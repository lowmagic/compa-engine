package cli

import (
	"testing"

	"github.com/lowmagic/compa-engine/engine"
)

// An author kind the engine has no word for is listed under the source's
// own name, and a comment whose source gives no kind under none.
func TestAuthorKindIsListedUnderItsOwnName(t *testing.T) {
	partner := engine.CaseComment{ID: "c1", AuthorName: "Pat Synthetic", AuthorKind: "partner", Visibility: string(engine.VisibilityPublic)}
	if got := kindLabel(partner, "Agent"); got != "PARTNER" {
		t.Errorf("the listing's label = %q, want PARTNER", got)
	}
	if got := kindLabel(engine.CaseComment{ID: "c4"}, "Agent"); got != "" {
		t.Errorf("a comment whose source gives no kind is labelled %q", got)
	}
}

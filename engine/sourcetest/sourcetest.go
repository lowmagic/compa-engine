// Package sourcetest holds conformance tests that every CaseSource and
// ArtifactSource passes, the engine's own and any a deployment supplies.
package sourcetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/lowmagic/compa-engine/engine"
)

// invalidIDs cannot name a directory; every source refuses them, including
// one that does not map IDs to paths, so a case ID is valid everywhere or
// nowhere.
var invalidIDs = []string{"", ".", "..", "a/b", `a\b`, "-rf", "a\x00b", "a\nb"}

// TestCaseSource checks src against a case it has, existing, and one it
// does not, missing. A comment's author kind, its automation mark, its
// author's ID and its edit time are optional: a source that has none of
// them passes, and one that gives them gives them in the engine's terms.
func TestCaseSource(t *testing.T, src engine.CaseSource, existing, missing string) {
	ctx := context.Background()
	if src.Name() == "" {
		t.Error("Name() is empty; provenance needs it")
	}
	t.Run("existing", func(t *testing.T) {
		c, err := src.Case(ctx, existing)
		if err != nil {
			t.Fatalf("Case(%q): %v", existing, err)
		}
		if c.ID != existing {
			t.Errorf("Case(%q).ID = %q", existing, c.ID)
		}
		again, err := src.Case(ctx, existing)
		if err != nil || !reflect.DeepEqual(c, again) {
			t.Errorf("Case(%q) differs between two calls: %v", existing, err)
		}
		comments, err := src.Comments(ctx, existing)
		if err != nil {
			t.Fatalf("Comments(%q): %v", existing, err)
		}
		seen := map[string]bool{}
		for i, cm := range comments {
			if cm.ID == "" || seen[cm.ID] {
				t.Errorf("comment %d has an empty or repeated ID %q", i, cm.ID)
			}
			seen[cm.ID] = true
			switch cm.Visibility {
			case engine.VisibilityPrivate, engine.VisibilityPartner, engine.VisibilityPublic:
			default:
				t.Errorf("comment %q has visibility %q, want one of the engine's three", cm.ID, cm.Visibility)
			}
			// Any kind a source has is its own to report; it is one word,
			// since it is shown in a column and named in a rule.
			if strings.ContainsFunc(cm.AuthorKind, unicode.IsSpace) {
				t.Errorf("comment %q has author kind %q, want one word or none", cm.ID, cm.AuthorKind)
			}
			if !cm.Updated.IsZero() && cm.Updated.Before(cm.Created) {
				t.Errorf("comment %q was edited at %v, before it was written at %v", cm.ID, cm.Updated, cm.Created)
			}
			if i > 0 {
				prev := comments[i-1]
				if cm.Created.Before(prev.Created) || (cm.Created.Equal(prev.Created) && cm.ID < prev.ID) {
					t.Errorf("comments %q and %q are not oldest first, ties by ID", prev.ID, cm.ID)
				}
			}
		}
		again2, err := src.Comments(ctx, existing)
		if err != nil || !reflect.DeepEqual(comments, again2) {
			t.Errorf("Comments(%q) differs between two calls: %v", existing, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := src.Case(ctx, missing); !errors.Is(err, engine.ErrCaseNotFound) {
			t.Errorf("Case(%q) = %v, want ErrCaseNotFound", missing, err)
		}
		if _, err := src.Comments(ctx, missing); !errors.Is(err, engine.ErrCaseNotFound) {
			t.Errorf("Comments(%q) = %v, want ErrCaseNotFound", missing, err)
		}
	})
	t.Run("invalid IDs", func(t *testing.T) {
		for _, id := range invalidIDs {
			if _, err := src.Case(ctx, id); !errors.Is(err, engine.ErrInvalidCaseID) {
				t.Errorf("Case(%q) = %v, want ErrInvalidCaseID", id, err)
			}
			if _, err := src.Comments(ctx, id); !errors.Is(err, engine.ErrInvalidCaseID) {
				t.Errorf("Comments(%q) = %v, want ErrInvalidCaseID", id, err)
			}
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		done, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := src.Case(done, existing); !errors.Is(err, context.Canceled) {
			t.Errorf("Case with a cancelled context = %v, want context.Canceled", err)
		}
		if _, err := src.Comments(done, existing); !errors.Is(err, context.Canceled) {
			t.Errorf("Comments with a cancelled context = %v, want context.Canceled", err)
		}
	})
}

// TestArtifactSource checks src against a case it has artifacts for,
// existing, and one it does not, missing. A source that serves one
// directory for every case has no missing case; pass "" to skip that check.
func TestArtifactSource(t *testing.T, src engine.ArtifactSource, existing, missing string) {
	ctx := context.Background()
	t.Run("existing", func(t *testing.T) {
		root, err := src.Root(ctx, existing)
		if err != nil {
			t.Fatalf("Root(%q): %v", existing, err)
		}
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			t.Errorf("Root(%q) = %q, want an absolute, clean path", existing, root)
		}
		if real, err := filepath.EvalSymlinks(root); err != nil || real != root {
			t.Errorf("Root(%q) = %q still has a symlink in it (resolves to %q, %v)", existing, root, real, err)
		}
		if _, err := os.ReadDir(root); err != nil {
			t.Errorf("Root(%q) = %q, which cannot be read: %v", existing, root, err)
		}
		if again, err := src.Root(ctx, existing); err != nil || again != root {
			t.Errorf("Root(%q) differs between two calls: %q, %v", existing, again, err)
		}
	})
	if src.Name() == "" {
		t.Error("Name() is empty; provenance needs it")
	}
	t.Run("attachments", func(t *testing.T) {
		root, err := src.Root(ctx, existing)
		if err != nil {
			t.Fatal(err)
		}
		atts, err := src.Attachments(ctx, existing)
		if err != nil {
			t.Fatalf("Attachments(%q): %v", existing, err)
		}
		for i, a := range atts {
			if a.Name == "" || (i > 0 && a.Name <= atts[i-1].Name) {
				t.Errorf("attachment %d: names must be non-empty, unique and sorted (%q after %q)", i, a.Name, atts[max(i-1, 0)].Name)
			}
			if !a.State.Valid() {
				t.Errorf("attachment %q: state %q", a.Name, a.State)
			}
			if a.Size < -1 {
				t.Errorf("attachment %q: size %d", a.Name, a.Size)
			}
			for field, rel := range map[string]string{"Path": a.Path, "Dir": a.Dir} {
				if rel == "" {
					continue
				}
				if filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
					t.Errorf("attachment %q: %s %q must be a clean path inside the root", a.Name, field, rel)
					continue
				}
				info, err := os.Stat(filepath.Join(root, rel))
				if err != nil {
					t.Errorf("attachment %q: %s %q: %v", a.Name, field, rel, err)
				} else if (field == "Dir") != info.IsDir() {
					t.Errorf("attachment %q: %s %q is the wrong kind of entry", a.Name, field, rel)
				}
			}
			if a.State == engine.AttachmentExtracted && a.Dir == "" {
				t.Errorf("attachment %q is extracted but names no directory", a.Name)
			}
		}
		again, err := src.Attachments(ctx, existing)
		if err != nil || !reflect.DeepEqual(atts, again) {
			t.Errorf("Attachments(%q) differs between two calls: %v", existing, err)
		}
	})
	if missing != "" {
		t.Run("missing", func(t *testing.T) {
			if _, err := src.Root(ctx, missing); !errors.Is(err, engine.ErrNoArtifacts) {
				t.Errorf("Root(%q) = %v, want ErrNoArtifacts", missing, err)
			}
			if _, err := src.Attachments(ctx, missing); !errors.Is(err, engine.ErrNoArtifacts) {
				t.Errorf("Attachments(%q) = %v, want ErrNoArtifacts", missing, err)
			}
		})
	}
	t.Run("invalid IDs", func(t *testing.T) {
		for _, id := range invalidIDs {
			if _, err := src.Root(ctx, id); !errors.Is(err, engine.ErrInvalidCaseID) {
				t.Errorf("Root(%q) = %v, want ErrInvalidCaseID", id, err)
			}
			if _, err := src.Attachments(ctx, id); !errors.Is(err, engine.ErrInvalidCaseID) {
				t.Errorf("Attachments(%q) = %v, want ErrInvalidCaseID", id, err)
			}
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		done, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := src.Root(done, existing); !errors.Is(err, context.Canceled) {
			t.Errorf("Root with a cancelled context = %v, want context.Canceled", err)
		}
	})
}

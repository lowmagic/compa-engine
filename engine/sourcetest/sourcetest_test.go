package sourcetest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/sourcetest"
)

// bare is a case source that gives only what every source must: no author
// kind, no automation mark, no author ID and no edit time.
type bare struct{}

func (bare) Name() string { return "bare" }

func (bare) Case(ctx context.Context, id string) (engine.Case, error) {
	if err := check(ctx, id); err != nil {
		return engine.Case{}, err
	}
	return engine.Case{ID: id}, nil
}

func (bare) Comments(ctx context.Context, id string) ([]engine.Comment, error) {
	if err := check(ctx, id); err != nil {
		return nil, err
	}
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return []engine.Comment{
		{ID: "a", Author: engine.Party{Name: "someone"}, Visibility: engine.VisibilityPublic, Created: at, Body: "first"},
		{ID: "b", Visibility: engine.VisibilityPrivate, Created: at.Add(time.Hour), Body: "second"},
	}, nil
}

func check(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := engine.CheckCaseID(id); err != nil {
		return err
	}
	if id != "00000001" {
		return fmt.Errorf("case %s: %w", id, engine.ErrCaseNotFound)
	}
	return nil
}

// The comment fields added for the comment viewer are optional: a source
// without them conforms.
func TestOptionalCommentFields(t *testing.T) {
	sourcetest.TestCaseSource(t, bare{}, "00000001", "99999999")
}

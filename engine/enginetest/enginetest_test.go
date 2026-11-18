package enginetest_test

import (
	"context"
	"testing"

	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// A fixture session is the stub case as a client finds it: its comments
// and experts read, and no model, so a turn refuses and nothing is sent.
func TestSessionIsTheStubCase(t *testing.T) {
	s := enginetest.Session(t, "00000001")
	ctx := context.Background()
	experts, err := s.Experts(ctx)
	if s.CaseID() != "00000001" || len(s.Comments()) == 0 || err != nil || len(experts) == 0 {
		t.Fatalf("case %s, %d comments, %d experts, %v", s.CaseID(), len(s.Comments()), len(experts), err)
	}
	if _, err := s.Talk(ctx, "What failed?", nil); err == nil {
		t.Error("a turn ran with no model")
	}
}

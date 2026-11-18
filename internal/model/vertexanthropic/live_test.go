package vertexanthropic_test

import (
	"context"
	"testing"

	"github.com/lowmagic/compa-engine/internal/model/modeltest"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
)

// TestLiveTwoToolTurn runs one turn with two tools on Vertex Claude. What
// it shows about the engine: tool use is allowed
// for the model, the provider streams free text and tool calls, every
// keyword in Keywords is accepted, and the second call reads the prefix
// the first call's breakpoints cached. Whether the model calls both tools
// is the model's.
func TestLiveTwoToolTurn(t *testing.T) {
	project, location := modeltest.Vertex(t)
	p, err := vertexanthropic.New(context.Background(), vertexanthropic.Options{Project: project, Location: location})
	if err != nil {
		t.Fatal(err)
	}
	modeltest.TwoToolTurn(t, p, modeltest.Model("COMPA_LIVE_CLAUDE_MODEL", "claude-sonnet-4-6"), "", vertexanthropic.Keywords)
}

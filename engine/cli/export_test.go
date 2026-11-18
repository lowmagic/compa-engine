package cli

import (
	"context"
	"io"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/chat"
)

// AsTerminal makes the command line take its input and output for the
// engineer's terminal, as yes says, until the test ends.
func AsTerminal(t *testing.T, yes bool) {
	t.Helper()
	was := interactive
	interactive = func(io.Reader, io.Writer) bool { return yes }
	t.Cleanup(func() { interactive = was })
}

// RunChat makes the command line run run in place of the chat until the
// test ends.
func RunChat(t *testing.T, run func(context.Context, *engine.Session, chat.Options, io.Reader, io.Writer) error) {
	t.Helper()
	was := runChat
	runChat = run
	t.Cleanup(func() { runChat = was })
}

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

// InRequest is what ParseIn makes of compa in's arguments.
type InRequest struct {
	ID, Note, Socket string
	Files            []string
}

// ErrUsage is the error for a command line used wrongly.
var ErrUsage = errUsage

// ParseIn reads compa in's arguments as the command line does, in a
// compa shell for shellCase and shellSocket where they are set.
func ParseIn(args []string, shellCase, shellSocket string) (InRequest, error) {
	r, err := parseIn(args, shellCase, shellSocket)
	return InRequest{ID: r.id, Note: r.note, Socket: r.socket, Files: r.files}, err
}

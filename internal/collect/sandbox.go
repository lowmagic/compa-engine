package collect

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
)

// SandboxSpec describes one sandboxed process. The contract every Sandbox
// must meet (checked by the contract tests, engine/sandboxtest): ReadOnly
// paths can be read and not written, except beneath Deny paths, which
// cannot be read or listed; Scratch is the only writable location;
// nothing else holding user data can be read; there is no network unless
// Network is set; the environment is exactly Env. A mechanism that can
// bound memory holds the process's address space to MemoryBytes, where it
// is set, and says so in its Name.
type SandboxSpec struct {
	Argv        []string // Argv[0] is an absolute path
	Scratch     string
	ReadOnly    []string
	Deny        []string
	Network     bool
	Env         []string
	MemoryBytes int64
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// Sandbox prepares commands to run under a containment mechanism. The
// mechanism is a deployment choice; the properties in SandboxSpec are the
// contract.
type Sandbox interface {
	Name() string
	Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error)
}

// ErrNoSandbox is returned by DefaultSandbox where no mechanism is
// implemented yet. Commands are then unavailable; primitives still run.
var ErrNoSandbox = errors.New("no sandbox mechanism is available on this platform")

// denyPaths returns p, under its parent's real path, and where p leads if
// it is a link. p need not exist: a store made later is still denied.
func denyPaths(p string) ([]string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return nil, err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return nil, err
	}
	lit := filepath.Join(parent, filepath.Base(p))
	out := []string{lit}
	if target, err := filepath.EvalSymlinks(lit); err == nil && target != lit {
		out = append(out, target)
	}
	return out, nil
}

func sortedStrings(s []string) []string {
	sort.Strings(s)
	return s
}

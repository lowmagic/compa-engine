package collect

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// systemReadPaths are what a dynamically linked system binary needs to start.
var systemReadPaths = []string{"/usr/lib", "/usr/share", "/System", "/bin", "/usr/bin", "/usr/sbin", "/sbin", "/private/var/db/dyld", "/Library/Apple", "/private/etc"}

// seatbelt runs commands under macOS sandbox-exec with a deny-by-default
// profile generated per command.
type seatbelt struct{}

func DefaultSandbox() (Sandbox, error) {
	if _, err := exec.LookPath(sandboxExec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSandbox, err)
	}
	return seatbelt{}, nil
}

func (seatbelt) Name() string { return "sandbox-exec" }

func (seatbelt) Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error) {
	profile, err := seatbeltProfile(spec)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-p", profile, "--"}, spec.Argv...)
	cmd := exec.CommandContext(ctx, sandboxExec, args...)
	cmd.Dir = spec.Scratch
	cmd.Env = spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	return cmd, nil
}

func seatbeltProfile(spec SandboxSpec) (string, error) {
	real := func(p string) (string, error) {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		return filepath.Abs(r)
	}
	scratch, err := real(spec.Scratch)
	if err != nil {
		return "", err
	}
	binDir, err := real(filepath.Dir(spec.Argv[0]))
	if err != nil {
		return "", err
	}
	read := append([]string{binDir, scratch}, systemReadPaths...)
	for _, p := range spec.ReadOnly {
		r, err := real(p)
		if err != nil {
			return "", err
		}
		read = append(read, r)
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-exec* process-fork)\n(allow signal (target self))\n(allow sysctl-read)\n")
	// Metadata lets path resolution work; it reveals that a path exists,
	// not its content.
	b.WriteString("(allow file-read-metadata)\n")
	b.WriteString("(allow file-read*")
	for _, p := range read {
		fmt.Fprintf(&b, " (subpath %s)", quoteSB(p))
	}
	// dyld must read the root directory itself (its entries' names, not
	// what is beneath them) to start any process.
	b.WriteString(` (literal "/") (literal "/dev/null") (literal "/dev/urandom") (literal "/dev/random") (literal "/dev/zero"))` + "\n")
	fmt.Fprintf(&b, "(allow file-write* (subpath %s) (literal \"/dev/null\"))\n", quoteSB(scratch))
	// Later rules win, so these override the reads allowed above, metadata
	// included. The kernel checks resolved paths, so a link into a denied
	// path is denied too; a denied path that is itself a link is denied at
	// its target as well.
	for _, p := range spec.Deny {
		paths, err := denyPaths(p)
		if err != nil {
			return "", err
		}
		for _, d := range paths {
			fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", quoteSB(d))
		}
	}
	if spec.Network {
		b.WriteString("(allow network*)\n(allow system-socket)\n")
	}
	return b.String(), nil
}

func quoteSB(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

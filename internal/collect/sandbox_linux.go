package collect

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The fixed paths of the mechanisms, so no PATH lookup decides what runs.
const (
	bwrapPath   = "/usr/bin/bwrap"
	prlimitPath = "/usr/bin/prlimit"
	envPath     = "/usr/bin/env"
)

// systemPaths are what a dynamically linked system binary needs to start:
// /usr bound read-only, and the top-level links into it that merged-/usr
// systems keep, recreated as links.
var systemPaths = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}

// etcPaths are the few files under /etc that common tools read to start or
// to name users and zones; /etc as a whole is not shown, since a host may
// keep files there that collection has no business reading.
var etcPaths = []string{"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/alternatives", "/etc/localtime", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf"}

// The empty, unreadable nodes, made in each run's scratch, that are bound
// over denied directories and files.
const (
	deniedDir  = ".compa-denied-dir"
	deniedFile = ".compa-denied-file"
)

// bubblewrap runs commands under bwrap in new namespaces, user, network
// and mount included, over a root holding only the system's binaries and
// libraries, the read-only paths and scratch; with prlimit, it bounds
// their address space.
type bubblewrap struct {
	bwrap, prlimit string
}

// DefaultSandbox is bubblewrap at its fixed path, bounding memory with
// prlimit where that is installed too; with no bwrap, ErrNoSandbox.
func DefaultSandbox() (Sandbox, error) {
	if !executable(bwrapPath) {
		return nil, fmt.Errorf("%w: %s is not installed", ErrNoSandbox, bwrapPath)
	}
	b := bubblewrap{bwrap: bwrapPath}
	if executable(prlimitPath) {
		b.prlimit = prlimitPath
	}
	return b, nil
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func (b bubblewrap) Name() string {
	if b.prlimit != "" {
		return "bwrap+prlimit"
	}
	return "bwrap"
}

func (b bubblewrap) Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error) {
	args, err := bwrapArgs(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, "--")
	if b.prlimit != "" && spec.MemoryBytes > 0 {
		args = append(args, b.prlimit, "--as="+strconv.FormatInt(spec.MemoryBytes, 10), "--")
	}
	// bwrap sets PWD in the environment it passes on; env takes it back
	// out, so the environment is exactly the one given.
	if !slices.ContainsFunc(spec.Env, func(v string) bool { return strings.HasPrefix(v, "PWD=") }) {
		args = append(args, envPath, "-u", "PWD", "--")
	}
	cmd := exec.CommandContext(ctx, b.bwrap, append(args, spec.Argv...)...)
	cmd.Dir = spec.Scratch
	cmd.Env = spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	return cmd, nil
}

func bwrapArgs(spec SandboxSpec) ([]string, error) {
	resolved := func(p string) (string, error) {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		return filepath.Abs(r)
	}
	scratch, err := resolved(spec.Scratch)
	if err != nil {
		return nil, err
	}
	args := []string{"--unshare-all", "--die-with-parent", "--new-session"}
	if spec.Network {
		args = append(args, "--share-net")
	}
	for _, p := range systemPaths {
		fi, err := os.Lstat(p)
		switch {
		case err != nil:
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil, err
			}
			args = append(args, "--symlink", target, p)
		default:
			args = append(args, "--ro-bind", p, p)
		}
	}
	for _, p := range etcPaths {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	// The binary's own directory, where it is not under /usr, and where it
	// leads if it is a link.
	dirs := []string{filepath.Dir(spec.Argv[0])}
	if r, err := resolved(spec.Argv[0]); err == nil {
		dirs = append(dirs, filepath.Dir(r))
	}
	for _, d := range dirs {
		args = append(args, "--ro-bind-try", d, d)
	}
	for _, p := range spec.ReadOnly {
		r, err := resolved(p)
		if err != nil {
			return nil, err
		}
		args = append(args, "--ro-bind", r, r)
		if abs, err := filepath.Abs(p); err == nil && abs != r {
			args = append(args, "--symlink", r, abs)
		}
	}
	args = append(args, "--bind", scratch, scratch, "--chdir", scratch)
	// Later mounts cover earlier ones: each denied path that exists is
	// covered by an empty node no one may read or list. One that does not
	// exist when the command starts has nothing to show.
	if len(spec.Deny) > 0 {
		dir, file := filepath.Join(scratch, deniedDir), filepath.Join(scratch, deniedFile)
		if err := os.Mkdir(dir, 0); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if err := os.WriteFile(file, nil, 0); err != nil && !os.IsExist(err) {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range spec.Deny {
			paths, err := denyPaths(p)
			if err != nil {
				return nil, err
			}
			for _, d := range paths {
				fi, err := os.Stat(d)
				if err != nil || seen[d] {
					continue
				}
				seen[d] = true
				cover := file
				if fi.IsDir() {
					cover = dir
				}
				args = append(args, "--ro-bind", cover, d)
			}
		}
	}
	return args, nil
}

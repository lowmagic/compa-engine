// Package sandboxtest holds the contract every Sandbox keeps, the
// platform's own and any a deployment passes in engine.Options.Sandbox.
package sandboxtest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/engine"
)

// The markers of what no sandboxed process may read.
const (
	storeMarker   = "STORE-MARKER"
	fileMarker    = "EXCLUDED-MARKER"
	outsideMarker = "OUTSIDE-MARKER"
)

// TestSandbox runs commands under sb against an artifact root it makes: it
// can be read and not written, except its denied entries, which cannot be
// read or listed; scratch is the only place written; nothing outside the
// root can be read; there is no network unless allowed, the positive
// control showing the same command reaching it when allowed; and the
// environment is exactly the one given. It needs cat, ls, touch and env;
// the network cases need curl and are skipped without it.
func TestSandbox(t *testing.T, sb engine.Sandbox) {
	if sb.Name() == "" {
		t.Error("Name() is empty; a collection's record names its sandbox")
	}
	root := realDir(t)
	write(t, filepath.Join(root, "data.txt"), "visible\n")
	write(t, filepath.Join(root, ".compa", "case.db"), storeMarker)
	write(t, filepath.Join(root, "excluded.txt"), fileMarker)
	outside := realDir(t)
	write(t, filepath.Join(outside, "secret.txt"), outsideMarker)
	deny := []string{filepath.Join(root, ".compa"), filepath.Join(root, "excluded.txt"), filepath.Join(root, "made-later")}

	type result struct {
		out, stderr string
		err         error
		scratch     string
	}
	run := func(t *testing.T, network bool, argv ...string) result {
		t.Helper()
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			t.Skipf("%s not installed", argv[0])
		}
		if bin, err = filepath.Abs(bin); err != nil {
			t.Fatal(err)
		}
		scratch := realDir(t)
		var out, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd, err := sb.Command(ctx, engine.SandboxSpec{
			Argv: append([]string{bin}, argv[1:]...), Scratch: scratch, ReadOnly: []string{root}, Deny: deny, Network: network,
			Env: env(scratch), Stdin: strings.NewReader(""), Stdout: &out, Stderr: &stderr,
		})
		if err != nil {
			t.Fatalf("Command(%v): %v", argv, err)
		}
		err = cmd.Run()
		return result{out.String(), stderr.String(), err, scratch}
	}
	leaks := func(r result) bool {
		return strings.Contains(r.out, storeMarker) || strings.Contains(r.out, fileMarker) || strings.Contains(r.out, outsideMarker)
	}

	t.Run("reads the read-only root", func(t *testing.T) {
		if r := run(t, false, "cat", filepath.Join(root, "data.txt")); r.err != nil || r.out != "visible\n" {
			t.Fatalf("cat: %v %q %s", r.err, r.out, r.stderr)
		}
	})
	t.Run("cannot read or list what is denied", func(t *testing.T) {
		for _, argv := range [][]string{
			{"cat", filepath.Join(root, ".compa", "case.db")},
			{"ls", "-a", filepath.Join(root, ".compa")},
			{"cat", filepath.Join(root, "excluded.txt")},
		} {
			if r := run(t, false, argv...); r.err == nil || leaks(r) {
				t.Errorf("%v: %v, output %q", argv, r.err, r.out)
			}
		}
	})
	t.Run("cannot read outside the root", func(t *testing.T) {
		if r := run(t, false, "cat", filepath.Join(outside, "secret.txt")); r.err == nil || leaks(r) {
			t.Errorf("read outside: %v, output %q", r.err, r.out)
		}
		if home, err := os.UserHomeDir(); err == nil {
			// bubblewrap makes the directories leading to each path it
			// mounts, so where the root, scratch or a binary lies under
			// home, home lists that path's next name and nothing else.
			given := []string{root}
			if ls, err := exec.LookPath("ls"); err == nil {
				given = append(given, filepath.Dir(ls))
				if target, err := filepath.EvalSymlinks(ls); err == nil {
					given = append(given, filepath.Dir(target))
				}
			}
			if r := run(t, false, "ls", "-A", home); r.err == nil {
				for _, name := range strings.Fields(r.out) {
					if !OnPathTo(filepath.Join(home, name), append(given, r.scratch)...) {
						t.Errorf("listed %s in the home directory, on the way to nothing it was given: %q", name, r.out)
					}
				}
			}
		}
	})
	t.Run("writes only to scratch", func(t *testing.T) {
		r := run(t, false, "touch", "made-in-scratch")
		if r.err != nil {
			t.Errorf("scratch write: %v %s", r.err, r.stderr)
		} else if _, err := os.Stat(filepath.Join(r.scratch, "made-in-scratch")); err != nil {
			t.Errorf("the scratch write is not in scratch: %v", err)
		}
		for _, target := range []string{filepath.Join(outside, "written"), filepath.Join(root, "planted")} {
			if r := run(t, false, "touch", target); r.err == nil {
				t.Errorf("wrote %s", target)
			}
			if _, err := os.Stat(target); err == nil {
				t.Errorf("%s exists", target)
			}
		}
	})
	t.Run("the environment is exactly the one given", func(t *testing.T) {
		t.Setenv("COMPA_SANDBOXTEST_SECRET", "leak")
		r := run(t, false, "env")
		if r.err != nil {
			t.Fatal(r.err, r.stderr)
		}
		got := strings.Split(strings.TrimSpace(r.out), "\n")
		want := env(r.scratch)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("env = %q, want %q", got, want)
		}
	})
	t.Run("no network unless allowed", func(t *testing.T) {
		reached := func(network bool) bool {
			t.Helper()
			ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Skip(err)
			}
			defer ln.Close()
			accepted := make(chan struct{}, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					accepted <- struct{}{}
					c.Close()
				}
			}()
			run(t, network, "curl", "-s", "-m", "3", fmt.Sprintf("http://%s/", ln.Addr()))
			select {
			case <-accepted:
				return true
			case <-time.After(200 * time.Millisecond):
				return false
			}
		}
		if reached(false) {
			t.Error("the sandboxed command reached the network")
		}
		if !reached(true) {
			t.Error("with network allowed, curl did not connect; the case above proves nothing")
		}
	})
}

func env(scratch string) []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + scratch, "LANG=C"}
}

// OnPathTo reports whether p is one of the given paths or a directory on
// the way to one: what a sandbox shows of the host when it mounts them.
// Each path is taken as given and with its links resolved.
func OnPathTo(p string, given ...string) bool {
	var all []string
	for _, g := range given {
		all = append(all, g)
		if target, err := filepath.EvalSymlinks(g); err == nil {
			all = append(all, target)
		}
	}
	ps := []string{p}
	if target, err := filepath.EvalSymlinks(p); err == nil {
		ps = append(ps, target)
	}
	for _, q := range ps {
		for _, g := range all {
			if g == q || strings.HasPrefix(g, q+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// realDir is a temporary directory by its real path, as a sandbox resolves
// one.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

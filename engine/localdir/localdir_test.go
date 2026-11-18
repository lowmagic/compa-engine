package localdir_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/sourcetest"
)

// hostLayout is a layout a deployment may have: case folders under cases/,
// and a home directory holding a symlink per case to its folder.
func hostLayout(t *testing.T) (home, cases string) {
	t.Helper()
	tmp := t.TempDir()
	home, cases = filepath.Join(tmp, "home"), filepath.Join(tmp, "cases")
	for _, dir := range []string{home, filepath.Join(cases, "00000001", "must-gather"), filepath.Join(cases, "00000002")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(cases, "00000001"), filepath.Join(home, "00000001")); err != nil {
		t.Fatal(err)
	}
	return home, cases
}

func TestConformance(t *testing.T) {
	home, cases := hostLayout(t)
	t.Run("base of folders", func(t *testing.T) {
		sourcetest.TestArtifactSource(t, localdir.New(cases), "00000002", "99999999")
	})
	t.Run("base of links", func(t *testing.T) {
		sourcetest.TestArtifactSource(t, localdir.New(home), "00000001", "00000002")
	})
	t.Run("explicit directory", func(t *testing.T) {
		sourcetest.TestArtifactSource(t, localdir.Dir("../../testdata/artifacts"), "00000001", "")
	})
}

func TestLinkResolvesToTheCaseFolder(t *testing.T) {
	home, cases := hostLayout(t)
	root, err := localdir.New(home).Root(context.Background(), "00000001")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(cases, "00000001"))
	if root != want {
		t.Errorf("Root = %q, want the case folder %q", root, want)
	}
}

// TestErrors checks each failure is one error naming the case and the path,
// so the engineer can see what to fix.
func TestErrors(t *testing.T) {
	ctx := context.Background()
	home, cases := hostLayout(t)
	if err := os.Symlink(filepath.Join(cases, "gone"), filepath.Join(home, "broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src := localdir.New(home)
	for _, c := range []struct {
		id, want string
		is       error
	}{
		{"00000002", filepath.Join(home, "00000002") + " does not exist", engine.ErrNoArtifacts},
		{"broken", filepath.Join(home, "broken") + " does not exist or is a broken link", engine.ErrNoArtifacts},
		{"file", "is not a directory", engine.ErrNoArtifacts},
		{"a/b", `contains '/'`, engine.ErrInvalidCaseID},
	} {
		_, err := src.Root(ctx, c.id)
		if !errors.Is(err, c.is) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Root(%q) = %v, want %v containing %q", c.id, err, c.is, c.want)
		}
	}
}

func TestUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	_, cases := hostLayout(t)
	dir := filepath.Join(cases, "00000002")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	_, err := localdir.New(cases).Root(context.Background(), "00000002")
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "cannot read artifact directory") || !strings.Contains(err.Error(), "00000002") {
		t.Errorf("Root of an unreadable directory = %v", err)
	}
}

package benchcase

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func digestTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		data, _ := os.ReadFile(p)
		h.Write([]byte(rel))
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func TestGenerateIsDeterministic(t *testing.T) {
	o := Options{Bytes: 4 << 20, Files: 300, Seed: 7}
	a, b := t.TempDir(), t.TempDir()
	sa, err := Generate(a, o)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := Generate(b, o)
	if sa != sb || digestTree(t, a) != digestTree(t, b) {
		t.Errorf("two generations differ: %+v %+v", sa, sb)
	}
	if sa.Files != 303 || sa.Bytes < o.Bytes*9/10 || sa.Bytes > o.Bytes*11/10 {
		t.Errorf("stats %+v for %+v", sa, o)
	}
	c := t.TempDir()
	Generate(c, Options{Bytes: 4 << 20, Files: 300, Seed: 8})
	if digestTree(t, c) == digestTree(t, a) {
		t.Error("another seed gave the same tree")
	}
	if info, err := os.Stat(filepath.Join(a, sa.Largest)); err != nil || info.Size() != sa.LargestBytes {
		t.Errorf("largest %s: %v", sa.Largest, err)
	}
}

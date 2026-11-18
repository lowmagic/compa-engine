package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install copies fixture packages from testdata/catalogs into root.
func install(t *testing.T, root string, fixtures ...string) {
	t.Helper()
	for _, f := range fixtures {
		// A fixture is in the shared testdata/catalogs, or, named by a
		// path, in this package's own testdata.
		src := filepath.Join("../testdata/catalogs", f)
		if strings.Contains(f, "/") {
			src = f
		}
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			dst := filepath.Join(root, filepath.Base(f), rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

package engine_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/lowmagic/compa-engine"

// required reads the module paths go.mod requires, direct and indirect.
func required(t *testing.T, gomod string) []string {
	t.Helper()
	raw, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	var mods []string
	block := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case block && fields[0] == ")":
			block = false
		case block:
			mods = append(mods, fields[0])
		case fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			block = true
		case fields[0] == "require" && len(fields) >= 3:
			mods = append(mods, fields[1])
		}
	}
	return mods
}

// importAllowed reports whether a package of this module may import path:
// the standard library, this module, and the modules go.mod requires.
func importAllowed(path string, required []string) bool {
	if first, _, _ := strings.Cut(path, "/"); !strings.Contains(first, ".") {
		return true
	}
	for _, mod := range append([]string{module}, required...) {
		if path == mod || strings.HasPrefix(path, mod+"/") {
			return true
		}
	}
	return false
}

// TestEngineImportsOnlyDeclaredDependencies parses every Go file of the
// module and fails on an import of anything but the standard library, the
// module itself and the modules go.mod requires. A deployment and an
// expert repository depend on the engine, never the other way, and a
// workspace spanning their checkouts would let such an import build.
// Parsing files rather than running go list keeps the result tied to the
// files, so the test cache cannot go stale.
func TestEngineImportsOnlyDeclaredDependencies(t *testing.T) {
	root := ".."
	mods := required(t, filepath.Join(root, "go.mod"))
	if len(mods) == 0 {
		t.Fatal("go.mod requires nothing; the reader is wrong")
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			name := d.Name()
			if rel != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if !importAllowed(imported, mods) {
				t.Errorf("%s imports %s, which is not this module, the standard library or a module go.mod requires", filepath.ToSlash(rel), imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImportAllowed(t *testing.T) {
	mods := []string{"go.yaml.in/yaml/v3", "example.com/dep"}
	for path, want := range map[string]bool{
		"os":                                 true,
		"io/fs":                              true,
		module + "/engine":                   true,
		module + "/internal/collect":         true,
		"go.yaml.in/yaml/v3":                 true,
		"example.com/dep/sub":                true,
		"example.com/dependent":              false,
		"github.com/lowmagic/compa":          false,
		"github.com/lowmagic/compa/cmd":      false,
		"github.com/lowmagic/compa-experts":  false,
		module + "-extra/engine":             false,
		"example.com/a-deployment/casestore": false,
	} {
		if got := importAllowed(path, mods); got != want {
			t.Errorf("importAllowed(%q) = %v, want %v", path, got, want)
		}
	}
}

package engine_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// kernel are the engine's kernel packages: what the engine keeps and how
// it collects. None may depend on a concrete model provider, at compile
// time, directly or through another package (docs/TECH_STACK.md
// §Constraints).
var kernel = []string{"store", "evidence", "invocations", "collect", "expert", "sources"}

// providerDirs hold the concrete model providers.
var providerDirs = []string{"model/vertexgemini", "model/vertexanthropic", "model/openai", "model/fixture"}

// TestKernelDoesNotDependOnProviders builds the import graph of the
// engine's internal packages from their non-test files and fails on any
// path from a kernel package to a provider package, naming it.
func TestKernelDoesNotDependOnProviders(t *testing.T) {
	root := filepath.Join("..", "internal")
	prefix := module + "/internal/"
	imports := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		pkg, _ := filepath.Rel(root, filepath.Dir(path))
		pkg = filepath.ToSlash(pkg)
		if _, ok := imports[pkg]; !ok {
			imports[pkg] = nil
		}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if rel, ok := strings.CutPrefix(imported, prefix); ok && !slices.Contains(imports[pkg], rel) {
				imports[pkg] = append(imports[pkg], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	isProvider := func(pkg string) bool {
		for _, d := range providerDirs {
			if pkg == d || strings.HasPrefix(pkg, d+"/") {
				return true
			}
		}
		return false
	}
	for _, start := range kernel {
		if _, ok := imports[start]; !ok {
			t.Errorf("kernel package internal/%s has no source; the list is stale", start)
			continue
		}
		// Breadth first, keeping how each package was reached.
		via := map[string]string{start: ""}
		queue := []string{start}
		for len(queue) > 0 {
			pkg := queue[0]
			queue = queue[1:]
			if isProvider(pkg) {
				chain := []string{pkg}
				for p := via[pkg]; p != ""; p = via[p] {
					chain = append([]string{p}, chain...)
				}
				t.Errorf("internal/%s depends on a model provider: %s", start, strings.Join(chain, " -> "))
				break
			}
			for _, next := range imports[pkg] {
				if _, seen := via[next]; !seen {
					via[next] = pkg
					queue = append(queue, next)
				}
			}
		}
	}
}

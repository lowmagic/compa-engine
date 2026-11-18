package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
)

// Tool definitions by capture kind: a catalog may say which kinds
// of capture its tools read, such as must-gather or sosreport, and the
// names that mark one. A diagnosis offers a tool whose kinds no attachment
// has in one line with an eligibility note naming the missing kind, and
// in full once such an attachment is there. The marks are the experts'
// knowledge; the engine only looks for them.

// Bounds of the look for marks: how deep under the artifact root, and how
// many entries at most, so a large case costs a bounded walk.
const (
	captureDepth   = 5
	captureEntries = 50000
)

// captureKinds are the kinds among those declared that some attachment
// under root has, found by their marks.
func captureKinds(root string, decl []expert.Capture) map[string]bool {
	found := map[string]bool{}
	if root == "" || len(decl) == 0 {
		return found
	}
	marks := map[string][]string{}
	for _, d := range decl {
		for _, m := range d.Marks {
			marks[m] = append(marks[m], d.Kind)
		}
	}
	seen := 0
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if seen++; seen > captureEntries {
				return true
			}
			if depth == 0 && (e.Name() == collect.StoreDir || strings.HasPrefix(e.Name(), ".")) {
				continue
			}
			for _, k := range marks[e.Name()] {
				found[k] = true
			}
			if len(found) == len(decl) {
				return true
			}
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 && depth+1 < captureDepth {
				if walk(filepath.Join(dir, e.Name()), depth+1) {
					return true
				}
			}
		}
		return false
	}
	walk(root, 0)
	return found
}

// offeredByCapture is the expert's tests as a diagnosis offers them, with
// eligibility notes: a catalog tool whose kinds no attachment has is
// described in one line, and its note says which kind is missing.
func offeredByCapture(root string, r expert.Resolved) ([]expert.Test, map[string]string) {
	var decl []expert.Capture
	for _, rc := range r.Catalogs {
		if rc.Package != nil {
			decl = append(decl, rc.Package.Manifest.Spec.Captures...)
		}
	}
	have := captureKinds(root, decl)
	out := append([]expert.Test(nil), r.Package.Tests...)
	notes := map[string]string{}
	for _, rc := range r.Catalogs {
		var kinds []string
		missing := true
		if rc.Package != nil {
			for _, c := range rc.Package.Manifest.Spec.Captures {
				kinds = append(kinds, c.Kind)
				missing = missing && !have[c.Kind]
			}
		}
		sort.Strings(kinds)
		for _, t := range rc.Tools {
			if len(kinds) > 0 && missing {
				t.Description = oneLine(t)
				notes[t.ID] = fmt.Sprintf("reads a %s, and no attachment was identified as one", strings.Join(kinds, " or "))
			}
			out = append(out, t)
		}
	}
	return out, notes
}

// oneLine is a test's summary, else the first sentence of its description.
func oneLine(t expert.Test) string {
	if t.Summary != "" {
		return t.Summary
	}
	d := strings.TrimSpace(t.Description)
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1]
	}
	if i := strings.IndexByte(d, '\n'); i >= 0 {
		d = d[:i]
	}
	return d
}

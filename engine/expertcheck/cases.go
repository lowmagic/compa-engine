package expertcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/schema"
)

// CasesFile is the name of a test's fixture cases, in the directory its
// fixtures field names.
const CasesFile = "cases.yaml"

// cases are one test's fixture cases (compa.lowmag.cc/expert/cases/v1alpha2).
type cases struct {
	Tool        string        `json:"tool"`
	Root        string        `json:"root"`
	Modified    time.Time     `json:"modified"`
	Requires    []string      `json:"requires"`
	Credentials []string      `json:"credentials"`
	Cases       []fixtureCase `json:"cases"`
}

type fixtureCase struct {
	Name      string         `json:"name"`
	Covers    []string       `json:"covers"`
	Params    map[string]any `json:"params"`
	Status    string         `json:"status"`
	Truncated bool           `json:"truncated"`
	Reason    string         `json:"reason"`
	Output    string         `json:"output"`
}

// CaseFailure is a fixture case that did not return what its file says,
// or, with no Case, a cases file that could not be run.
type CaseFailure struct {
	Tool    string `json:"tool"`
	Case    string `json:"case,omitempty"`
	Problem string `json:"problem"`
}

// CaseResults are what running one or more cases files found.
type CaseResults struct {
	Run      int           `json:"run"`
	Failures []CaseFailure `json:"failures"`
}

func (r *CaseResults) add(o CaseResults) {
	r.Run += o.Run
	r.Failures = append(r.Failures, o.Failures...)
}

// RunCases runs the cases of file, a cases file whose root may lie outside
// any package, such as a repository's own boundary cases over a root that
// holds symlinks, by the rules a package's cases follow; its tool is the
// highest usable version that defines it. With update, each case's status,
// reason, truncation and output are written from its run instead of
// compared, the rest of the file kept.
func (p *Packages) RunCases(ctx context.Context, file string, update bool) (CaseResults, error) {
	raw, c, err := readCases(file)
	if err != nil {
		return CaseResults{}, err
	}
	pkg := p.definer(c.Tool)
	if pkg == nil {
		return CaseResults{}, fmt.Errorf("%s: no package defines %s", file, c.Tool)
	}
	return runCases(ctx, pkg, file, raw, c, filepath.Join(filepath.Dir(file), filepath.FromSlash(c.Root)), update)
}

// packageCases runs the cases of every test of pkg that has fixtures.
func packageCases(ctx context.Context, pkg *expert.Package, update bool) CaseResults {
	r := CaseResults{Failures: []CaseFailure{}}
	for _, t := range pkg.Tests {
		if t.Fixtures == "" {
			continue
		}
		file := filepath.Join(pkg.Dir, filepath.FromSlash(t.Fixtures), CasesFile)
		fail := func(err error) {
			r.Failures = append(r.Failures, CaseFailure{Tool: t.ID, Problem: err.Error()})
		}
		raw, c, err := readCases(file)
		if errors.Is(err, fs.ErrNotExist) {
			fail(fmt.Errorf("%s/%s: no cases: a test with fixtures has its cases there", t.Fixtures, CasesFile))
			continue
		}
		if err != nil {
			fail(err)
			continue
		}
		if c.Tool != t.ID {
			fail(fmt.Errorf("%s: tool %s, but the file is %s's fixtures", file, c.Tool, t.ID))
			continue
		}
		root := filepath.Join(filepath.Dir(file), filepath.FromSlash(c.Root))
		if rel, err := filepath.Rel(pkg.Dir, root); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			fail(fmt.Errorf("%s: root %s is outside the package", file, c.Root))
			continue
		}
		res, err := runCases(ctx, pkg, file, raw, c, root, update)
		if err != nil {
			fail(err)
		}
		r.add(res)
	}
	return r
}

// readCases reads and validates a cases file.
func readCases(file string) ([]byte, cases, error) {
	var c cases
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, c, err
	}
	doc, err := schema.DecodeYAML(raw)
	if err == nil {
		err = schema.Validate(schema.ExpertCases, doc)
	}
	if err != nil {
		return nil, c, fmt.Errorf("%s: %w", file, err)
	}
	j, err := json.Marshal(doc)
	if err == nil {
		err = json.Unmarshal(j, &c)
	}
	if err != nil {
		return nil, c, fmt.Errorf("%s: %w", file, err)
	}
	return raw, c, nil
}

// runCases runs c's cases over a copy of root, its entries' times set to
// c.Modified, and compares each result with the case, or with update
// writes it into the file.
func runCases(ctx context.Context, pkg *expert.Package, file string, raw []byte, c cases, root string, update bool) (CaseResults, error) {
	r := CaseResults{Failures: []CaseFailure{}}
	fail := func(name, format string, args ...any) {
		r.Failures = append(r.Failures, CaseFailure{Tool: c.Tool, Case: name, Problem: fmt.Sprintf(format, args...)})
	}
	covered := map[string]bool{}
	seen := map[string]bool{}
	for _, fc := range c.Cases {
		if seen[fc.Name] {
			fail(fc.Name, "the name is given to another case of the file")
		}
		seen[fc.Name] = true
		for _, l := range fc.Covers {
			covered[l] = true
		}
	}
	for _, l := range c.Requires {
		if !covered[l] {
			fail("", "no case covers %s, which the file requires", l)
		}
	}
	tmp, err := os.MkdirTemp("", "compa-cases-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(tmp)
	copied := filepath.Join(tmp, "root")
	if err := copyRoot(root, copied, c.Modified); err != nil {
		return r, fmt.Errorf("%s: root %s: %w", file, c.Root, err)
	}
	runner := &collect.Runner{Limits: collect.DefaultLimits, CredentialFiles: c.Credentials}
	got := make([]Result, len(c.Cases))
	for i, fc := range c.Cases {
		rec := runner.RunTest(ctx, copied, pkg, c.Tool, fc.Params)
		got[i] = Result{Status: rec.Status, Reason: rec.Reason, Truncated: rec.Truncated, Output: string(rec.Output)}
		r.Run++
		if update {
			continue
		}
		want := Result{Status: fc.Status, Reason: fc.Reason, Truncated: fc.Truncated, Output: fc.Output}
		if problem := differs(got[i], want); problem != "" {
			fail(fc.Name, "%s", problem)
		}
	}
	if update {
		if err := writeResults(file, raw, got); err != nil {
			return r, err
		}
	}
	return r, nil
}

// differs says how got differs from want, or nothing.
func differs(got, want Result) string {
	switch {
	case got.Status != want.Status:
		return fmt.Sprintf("status %s, want %s (reason %q)", got.Status, want.Status, got.Reason)
	case got.Reason != want.Reason:
		return fmt.Sprintf("reason %q, want %q", got.Reason, want.Reason)
	case got.Truncated != want.Truncated:
		return fmt.Sprintf("truncated %v, want %v", got.Truncated, want.Truncated)
	case got.Output != want.Output:
		g, w := strings.Split(got.Output, "\n"), strings.Split(want.Output, "\n")
		for i := range min(len(g), len(w)) {
			if g[i] != w[i] {
				return fmt.Sprintf("output line %d is %q, want %q", i+1, g[i], w[i])
			}
		}
		return fmt.Sprintf("output has %d lines, want %d", len(g), len(w))
	}
	return ""
}

// copyRoot copies the tree at from to to, keeping symlinks as links, then
// sets every entry's times to t but a link's own, which the standard
// library cannot set.
func copyRoot(from, to string, t time.Time) error {
	var stamped []string
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, dst)
		case d.IsDir():
			if err := os.Mkdir(dst, 0o755); err != nil {
				return err
			}
		default:
			if err := copyFile(p, dst); err != nil {
				return err
			}
		}
		stamped = append(stamped, dst)
		return nil
	})
	if err != nil {
		return err
	}
	// Deepest first: writing an entry changes its directory's time.
	for _, p := range slices.Backward(stamped) {
		if err := os.Chtimes(p, t, t); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// writeResults writes each case's status, reason, truncation and output
// into the file from got, in the cases' order, keeping every other node,
// its style and its comments: a reason is left out where empty and
// truncated where false.
func writeResults(file string, raw []byte, got []Result) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	list := value(doc.Content[0], "cases")
	if list == nil || len(list.Content) != len(got) {
		return fmt.Errorf("%s: the cases changed while they ran", file)
	}
	for i, c := range list.Content {
		g := got[i]
		set(c, "status", g.Status, false, "truncated", "reason", "output")
		if g.Reason != "" {
			set(c, "reason", g.Reason, false, "truncated", "output")
		} else {
			unset(c, "reason")
		}
		if g.Truncated {
			set(c, "truncated", "true", false, "output")
		} else {
			unset(c, "truncated")
		}
		set(c, "output", g.Output, true)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(file, b.Bytes(), 0o644)
}

// value is the node under key in a mapping, or nil.
func value(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// set sets key in mapping m to a scalar, leaving a node that already holds
// v as it is written. A key it adds goes before the first of before the
// mapping has, or last. text is a string a block of lines is written as.
func set(m *yaml.Node, key, v string, text bool, before ...string) {
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"}
	switch {
	case key == "truncated":
		n.Tag = "!!bool"
	case text && strings.Contains(v, "\n") && m.Style&yaml.FlowStyle == 0:
		n.Style = yaml.LiteralStyle
	}
	if old := value(m, key); old != nil {
		if old.Kind != yaml.ScalarNode || old.Value != v {
			*old = *n
		}
		return
	}
	at := len(m.Content)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if slices.Contains(before, m.Content[i].Value) {
			at = i
			break
		}
	}
	m.Content = slices.Insert(m.Content, at, &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}, n)
}

func unset(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

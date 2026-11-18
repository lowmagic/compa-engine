// Package expertcheck checks expert and catalog packages and runs their
// tests against fixture artifacts, through the engine's own loader and
// runner, so the experts repository needs no copy of either. It imports
// no deployment code and none of the terminal libraries, the case store or the
// model providers (TestDependencies).
package expertcheck

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/escape"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/schema"
	"github.com/lowmagic/compa-engine/internal/semver"
)

var loader = expert.Loader{CheckStage: collect.CheckStage}

var (
	// ErrUsage is a mistake in the command line: exit status 2.
	ErrUsage = errors.New("usage")
	// ErrFailed is a check that ran and did not pass: exit status 1. Each
	// command names itself before it: "expert check failed: …".
	ErrFailed = errors.New("failed")
)

// Command is compa expert check, given the arguments after "expert check":
// it discovers each root, runs every usable package's fixture cases,
// prints the report to out, as text or with --json in
// compa.lowmag.cc/expert/check/v1alpha2, and fails on any error, a case
// failing among them, or on any warning with --fail-on-warning. With
// --update it writes each case's result into its file in place of
// comparing it.
func Command(ctx context.Context, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("expert check", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	failOnWarning := fl.Bool("fail-on-warning", false, "")
	asJSON := fl.Bool("json", false, "")
	update := fl.Bool("update", false, "")
	roots, err := parseInterleaved(fl, args)
	if err != nil || len(roots) == 0 {
		return fmt.Errorf("%w: expert check [--fail-on-warning] [--json] [--update] <root> [root …]", ErrUsage)
	}
	r := checkPackages(ctx, roots, *update)
	if *asJSON {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		out.Write(append(data, '\n'))
	} else {
		r.writeText(out)
	}
	if r.Totals.Errors > 0 || *failOnWarning && r.Totals.Warnings > 0 {
		return fmt.Errorf("expert check %w: %s, %s", ErrFailed, plural(r.Totals.Errors, "error"), plural(r.Totals.Warnings, "warning"))
	}
	return nil
}

// parseInterleaved lets flags follow the roots; the standard library's
// flag stops at the first positional argument.
func parseInterleaved(fl *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, err
		}
		if fl.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fl.Arg(0))
		args = fl.Args()[1:]
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// checkReport is compa expert check's report, in the schema
// compa.lowmag.cc/expert/check/v1alpha2. Slices are never nil, so the JSON
// carries empty lists rather than nulls.
type checkReport struct {
	Schema    string         `json:"schema"`
	EngineAPI string         `json:"engine_api"`
	Roots     []string       `json:"roots"`
	Packages  []checkPackage `json:"packages"`
	Failed    []checkFailure `json:"failed"`
	Errors    []string       `json:"errors"`
	Warnings  []string       `json:"warnings"`
	Totals    checkTotals    `json:"totals"`
}

type checkPackage struct {
	Kind     string         `json:"kind"`
	ID       string         `json:"id"`
	Version  string         `json:"version"`
	Digest   string         `json:"digest"`
	Dir      string         `json:"dir"`
	Usable   bool           `json:"usable"`
	Tests    []string       `json:"tests"`
	Catalogs []checkCatalog `json:"catalogs,omitempty"`
	Cases    CaseResults    `json:"cases"`
	Warnings []string       `json:"warnings"`
}

type checkCatalog struct {
	ID      string   `json:"id"`
	Range   string   `json:"range"`
	Version string   `json:"version,omitempty"`
	Tools   []string `json:"tools"`
	Unmet   string   `json:"unmet,omitempty"`
}

type checkFailure struct {
	Dir      string   `json:"dir"`
	Problems []string `json:"problems"`
}

type checkTotals struct {
	Packages int `json:"packages"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Cases    int `json:"cases"`
}

// checkPackages discovers the roots as local roots, so the same ID and
// version in two of them is an error rather than an override, reports
// what the loader found, and runs each usable package's fixture cases. It
// writes nothing but, with update, the cases' results into their files.
func checkPackages(ctx context.Context, args []string, update bool) checkReport {
	r := checkReport{Schema: string(schema.ExpertCheck), EngineAPI: expert.EngineAPIVersion, Roots: []string{}, Packages: []checkPackage{}, Failed: []checkFailure{}, Errors: []string{}, Warnings: []string{}}
	var roots []expert.Root
	for _, a := range args {
		p := filepath.Clean(a)
		r.Roots = append(r.Roots, p)
		// Discovery warns about a missing configured root; a root named to
		// be checked must exist.
		if info, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			r.Errors = append(r.Errors, fmt.Sprintf("expert root %s does not exist", p))
			continue
		} else if err == nil && !info.IsDir() {
			r.Errors = append(r.Errors, fmt.Sprintf("expert root %s is not a directory", p))
			continue
		}
		roots = append(roots, expert.Root{Path: p, Source: expert.Local})
	}
	idx := loader.Discover(roots)

	for _, err := range idx.Errors {
		var le *expert.LoadError
		if errors.As(err, &le) {
			r.Failed = append(r.Failed, checkFailure{Dir: le.Dir, Problems: le.Problems})
			continue
		}
		r.Errors = append(r.Errors, err.Error())
	}
	r.Warnings = append(r.Warnings, idx.IndexWarnings...)
	for _, e := range idx.Loaded {
		p := checkEntry(idx, e)
		p.Cases = CaseResults{Failures: []CaseFailure{}}
		if p.Usable {
			p.Cases = packageCases(ctx, e.Package, update)
		}
		r.Packages = append(r.Packages, p)
	}
	sort.Slice(r.Packages, func(i, j int) bool {
		a, b := r.Packages[i], r.Packages[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if c := semver.MustParse(a.Version).Compare(semver.MustParse(b.Version)); c != 0 {
			return c < 0
		}
		return a.Dir < b.Dir
	})
	sort.Slice(r.Failed, func(i, j int) bool { return r.Failed[i].Dir < r.Failed[j].Dir })

	r.Totals = checkTotals{Packages: len(r.Packages), Errors: len(r.Failed) + len(r.Errors), Warnings: len(r.Warnings)}
	for _, p := range r.Packages {
		r.Totals.Warnings += len(p.Warnings)
		r.Totals.Errors += len(p.Cases.Failures)
		r.Totals.Cases += p.Cases.Run
	}
	return r
}

func checkEntry(idx *expert.Index, e expert.Entry) checkPackage {
	p := checkPackage{
		Kind: strings.ToLower(e.Manifest.Kind), ID: e.ID(), Version: e.Version(), Digest: e.Digest, Dir: e.Dir,
		Tests: []string{}, Warnings: append([]string{}, e.Warnings...),
	}
	if used, ok := idx.Describe(e.ID(), e.Version()); ok && used.Dir == e.Dir {
		p.Usable = true
	}
	for _, t := range e.Tests {
		p.Tests = append(p.Tests, t.ID)
	}
	if e.IsCatalog() {
		return p
	}
	res := idx.ResolveEntry(e)
	p.Warnings = append(p.Warnings, res.Warnings...)
	for _, rc := range res.Catalogs {
		c := checkCatalog{ID: rc.Ref.ID, Range: rc.Ref.Version, Tools: []string{}, Unmet: rc.Unresolved}
		if rc.Package != nil {
			c.Version = rc.Package.Version()
		}
		for _, t := range rc.Tools {
			c.Tools = append(c.Tools, t.ID)
		}
		p.Catalogs = append(p.Catalogs, c)
	}
	return p
}

// writeText prints the report for a person. Package content is untrusted,
// and problems quote it, so everything printed is escaped.
func (r checkReport) writeText(out io.Writer) {
	w := func(format string, args ...any) { io.WriteString(out, escape.Terminal(fmt.Sprintf(format, args...))) }
	for _, p := range r.Packages {
		state := ""
		if !p.Usable {
			state = " (not used; see the errors)"
		}
		w("%s %s %s%s\n  dir: %s\n  digest: %s\n", p.Kind, p.ID, p.Version, state, p.Dir, p.Digest)
		if len(p.Tests) > 0 {
			label := "tests"
			if p.Kind == "catalog" {
				label = "tools"
			}
			w("  %s: %s\n", label, strings.Join(p.Tests, ", "))
		}
		for _, c := range p.Catalogs {
			resolved := "unresolved"
			if c.Version != "" {
				resolved = c.Version
			}
			offered := "none"
			if len(c.Tools) > 0 {
				offered = strings.Join(c.Tools, ", ")
			}
			w("  catalog %s %s: %s, offering %s\n", c.ID, c.Range, resolved, offered)
		}
		if p.Cases.Run > 0 || len(p.Cases.Failures) > 0 {
			w("  cases: %d run, %d failed\n", p.Cases.Run, len(p.Cases.Failures))
		}
		for _, f := range p.Cases.Failures {
			if f.Case == "" {
				w("  error: %s: %s\n", f.Tool, f.Problem)
			} else {
				w("  error: %s, case %q: %s\n", f.Tool, f.Case, f.Problem)
			}
		}
		for _, warning := range p.Warnings {
			w("  warning: %s\n", warning)
		}
	}
	for _, f := range r.Failed {
		w("failed %s\n", f.Dir)
		for _, problem := range f.Problems {
			w("  error: %s\n", problem)
		}
	}
	for _, e := range r.Errors {
		w("error: %s\n", e)
	}
	for _, warning := range r.Warnings {
		w("warning: %s\n", warning)
	}
	w("\n%s, %d failed to load; %s, %s; %s run\n", plural(r.Totals.Packages, "package"), len(r.Failed), plural(r.Totals.Errors, "error"), plural(r.Totals.Warnings, "warning"), plural(r.Totals.Cases, "case"))
}

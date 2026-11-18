package expert

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/semver"
)

// Source says where a root's packages come from. Local roots take
// precedence over the bundled set, with a warning.
type Source int

const (
	Bundled Source = iota
	Local
	// Snapshot is a pinned package taken from the snapshot store because
	// its installed copy is missing or changed.
	Snapshot
)

func (s Source) String() string {
	switch s {
	case Local:
		return "local"
	case Snapshot:
		return "snapshot"
	}
	return "bundled"
}

// Root is a directory whose immediate subdirectories are expert packages
// and tool catalogs, told apart by manifest file.
type Root struct {
	Path   string
	Source Source
}

type Entry struct {
	*Package
	Source Source
}

// Index is the set of experts and catalogs discovered across roots.
// Packages that failed to load are reported in Errors and are not
// resolvable.
type Index struct {
	byID     map[string][]Entry // sorted by version, highest first
	Errors   []error
	Warnings []string
	// Loaded is every package that loaded, in discovery order, including
	// those not resolvable because of a duplicate or an ID clash.
	Loaded []Entry
	// IndexWarnings are the warnings about roots and precedence rather
	// than one package's content. They are in Warnings too.
	IndexWarnings []string
}

func (c *Index) indexWarning(w string) {
	c.Warnings = append(c.Warnings, w)
	c.IndexWarnings = append(c.IndexWarnings, w)
}

// Discover loads every package under the given roots. It reads files only;
// nothing is executed and nothing is fetched.
func (l Loader) Discover(roots []Root) *Index {
	c := &Index{byID: map[string][]Entry{}}
	type key struct{ id, version string }
	seen := map[key]Entry{}
	rejected := map[key]bool{}
	// Every loaded package's directory by ID and kind, recorded before
	// duplicates are filtered, so a clash between kinds is found whatever
	// happened to the duplicates.
	dirs := map[string]*kindDirs{}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				c.indexWarning(fmt.Sprintf("expert root %s (%s) does not exist", root.Path, root.Source))
				continue
			}
			c.Errors = append(c.Errors, fmt.Errorf("expert root %s: %w", root.Path, err))
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name()[0] == '.' {
				continue
			}
			dir := filepath.Join(root.Path, e.Name())
			if !hasManifest(dir) {
				continue
			}
			pkg, err := l.Load(dir)
			if err != nil {
				c.Errors = append(c.Errors, err)
				continue
			}
			c.Loaded = append(c.Loaded, Entry{pkg, root.Source})
			for _, w := range pkg.Warnings {
				c.Warnings = append(c.Warnings, fmt.Sprintf("%s %s %s at %s: %s", pkg.kind(), pkg.ID(), pkg.Version(), pkg.Dir, w))
			}
			d := dirs[pkg.ID()]
			if d == nil {
				d = &kindDirs{}
				dirs[pkg.ID()] = d
			}
			if pkg.IsCatalog() {
				d.catalogs = append(d.catalogs, pkg.Dir)
			} else {
				d.experts = append(d.experts, pkg.Dir)
			}
			k := key{pkg.ID(), pkg.Version()}
			if rejected[k] {
				continue
			}
			prev, dup := seen[k]
			switch {
			case !dup:
				seen[k] = Entry{pkg, root.Source}
			case prev.IsCatalog() != pkg.IsCatalog():
				// The ID is dropped below, with the clash reported.
			case prev.Source == root.Source:
				c.Errors = append(c.Errors, fmt.Errorf("%s %s %s is defined twice in %s roots (%s and %s); neither is used", pkg.kind(), k.id, k.version, root.Source, prev.Dir, pkg.Dir))
				delete(seen, k)
				rejected[k] = true
			case root.Source == Local:
				c.indexWarning(fmt.Sprintf("local %s %s %s at %s overrides the bundled copy at %s", pkg.kind(), k.id, k.version, pkg.Dir, prev.Dir))
				seen[k] = Entry{pkg, root.Source}
			default:
				c.indexWarning(fmt.Sprintf("local %s %s %s at %s overrides the bundled copy at %s", pkg.kind(), k.id, k.version, prev.Dir, pkg.Dir))
			}
		}
	}
	for k, e := range seen {
		c.byID[k.id] = append(c.byID[k.id], e)
	}
	c.dropKindClashes(dirs)
	for _, list := range c.byID {
		sort.Slice(list, func(i, j int) bool {
			return semver.MustParse(list[i].Version()).Compare(semver.MustParse(list[j].Version())) > 0
		})
	}
	c.dropWithoutCatalogs()
	c.Warnings = append(c.Warnings, c.unmetDependencies()...)
	return c
}

// dropWithoutCatalogs refuses every expert version that declares a
// catalog which is not installed at all, or whose ID is an expert's: a
// set composed without a catalog is found when it loads, not when a case
// needs the tools. A catalog installed at no version in range, or without
// a listed tool, still leaves the expert usable, with a warning.
func (c *Index) dropWithoutCatalogs() {
	ids := make([]string, 0, len(c.byID))
	for id := range c.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var kept []Entry
		for _, e := range c.byID[id] {
			var missing []string
			for _, ref := range e.Manifest.Spec.Catalogs {
				if why := c.absent(ref); why != "" {
					missing = append(missing, why)
				}
			}
			if len(missing) == 0 {
				kept = append(kept, e)
				continue
			}
			c.Errors = append(c.Errors, &LoadError{Dir: e.Dir, Problems: []string{fmt.Sprintf("expert %s %s is not used: %s", e.ID(), e.Version(), strings.Join(missing, "; "))}})
		}
		if len(kept) == 0 {
			delete(c.byID, id)
		} else {
			c.byID[id] = kept
		}
	}
}

func hasManifest(dir string) bool {
	for _, f := range []string{ManifestFile, CatalogFile} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

type kindDirs struct{ experts, catalogs []string }

// dropKindClashes removes every ID used by both an expert and a catalog, in
// any versions. IDs are unique across the two kinds, and neither package
// can be trusted to be the one meant.
func (c *Index) dropKindClashes(dirs map[string]*kindDirs) {
	ids := make([]string, 0, len(dirs))
	for id := range dirs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := dirs[id]
		if len(d.experts) == 0 || len(d.catalogs) == 0 {
			continue
		}
		sort.Strings(d.experts)
		sort.Strings(d.catalogs)
		c.Errors = append(c.Errors, fmt.Errorf("%s is the ID of an expert (%s) and of a catalog (%s); IDs are unique across experts and catalogs, so neither is used", id, strings.Join(d.experts, ", "), strings.Join(d.catalogs, ", ")))
		delete(c.byID, id)
	}
}

type Descriptor struct {
	Kind, ID, Version, Title, Summary, EngineAPI, Digest string
	Source                                               Source
	Tests, Binaries                                      []string
}

func describe(e Entry) Descriptor {
	m := e.Manifest
	return Descriptor{
		Kind: m.Kind, ID: m.Metadata.ID, Version: m.Metadata.Version, Title: m.Metadata.Title, Summary: m.Metadata.Summary,
		EngineAPI: m.Spec.EngineAPI, Digest: e.Digest, Source: e.Source,
		Tests: m.Spec.Tests, Binaries: m.Spec.Binaries,
	}
}

// List returns every resolvable package, by ID and then highest version.
func (c *Index) List() []Descriptor {
	ids := make([]string, 0, len(c.byID))
	for id := range c.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Descriptor
	for _, id := range ids {
		for _, e := range c.byID[id] {
			out = append(out, describe(e))
		}
	}
	return out
}

// Describe returns the package with the exact ID and version.
func (c *Index) Describe(id, version string) (Entry, bool) {
	for _, e := range c.byID[id] {
		if e.Version() == version {
			return e, true
		}
	}
	return Entry{}, false
}

// Resolve returns the highest version of the expert id within the
// constraint range, with the catalogs it uses. An empty constraint accepts
// any release version. An unmet catalog dependency does not fail it.
func (c *Index) Resolve(id, constraint string) (Resolved, error) {
	list, ok := c.byID[id]
	if !ok {
		return Resolved{}, fmt.Errorf("expert %s: not installed", id)
	}
	if constraint == "" {
		constraint = ">=0.0.0"
	}
	r, err := semver.ParseRange(constraint)
	if err != nil {
		return Resolved{}, fmt.Errorf("expert %s: %w", id, err)
	}
	if list[0].IsCatalog() {
		return Resolved{}, fmt.Errorf("expert %s: is a tool catalog, which is used through an expert's spec.catalogs and never selected itself", id)
	}
	e, ok := highest(list, r)
	if !ok {
		return Resolved{}, fmt.Errorf("expert %s: no installed version satisfies %q", id, constraint)
	}
	return c.ResolveEntry(e), nil
}

func highest(list []Entry, r semver.Range) (Entry, bool) {
	for _, e := range list {
		if r.Contains(semver.MustParse(e.Version())) {
			return e, true
		}
	}
	return Entry{}, false
}

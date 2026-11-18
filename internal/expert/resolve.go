package expert

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/internal/semver"
)

// Resolved is an expert with each catalog it declares resolved to the
// highest installed version within the expert's range. Two experts may
// resolve one catalog to different versions.
type Resolved struct {
	Entry
	Catalogs []ResolvedCatalog
	// Warnings name each unmet catalog dependency and the tools it leaves
	// out, to be shown with the expert.
	Warnings []string
}

// ResolvedCatalog is one of an expert's catalog dependencies. Package is
// nil when no version resolved. Tools holds the listed tools the resolved
// version defines, in the order the expert lists them. Unresolved says what
// is unmet and is empty only when every listed tool is available.
type ResolvedCatalog struct {
	Ref        CatalogRef
	Package    *Package
	Tools      []Test
	Unresolved string
}

// notOffered lists the tools the expert lists that the dependency does not
// provide.
func (rc ResolvedCatalog) notOffered() []string {
	have := map[string]bool{}
	for _, t := range rc.Tools {
		have[t.ID] = true
	}
	var out []string
	for _, id := range rc.Ref.ToolIDs() {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}

func unmetWarning(expert *Package, rc ResolvedCatalog) string {
	return fmt.Sprintf("expert %s %s: %s; not offered: %s", expert.ID(), expert.Version(), rc.Unresolved, strings.Join(rc.notOffered(), ", "))
}

// ResolveEntry resolves the catalogs of one discovered expert version,
// which need not be the highest installed.
func (c *Index) ResolveEntry(e Entry) Resolved {
	r := Resolved{Entry: e}
	for _, ref := range e.Manifest.Spec.Catalogs {
		rc := c.resolveCatalog(ref)
		if rc.Unresolved != "" {
			r.Warnings = append(r.Warnings, unmetWarning(e.Package, rc))
		}
		r.Catalogs = append(r.Catalogs, rc)
	}
	return r
}

// absent says why a catalog reference names no installed catalog, or is
// empty where some version of the catalog is installed.
func (c *Index) absent(ref CatalogRef) string {
	list := c.byID[ref.ID]
	switch {
	case len(list) == 0:
		return fmt.Sprintf("catalog %s (%s) is not installed", ref.ID, ref.Version)
	case !list[0].IsCatalog():
		return fmt.Sprintf("%s (%s) is an expert, not a catalog", ref.ID, ref.Version)
	}
	return ""
}

func (c *Index) resolveCatalog(ref CatalogRef) ResolvedCatalog {
	rc := ResolvedCatalog{Ref: ref}
	if rc.Unresolved = c.absent(ref); rc.Unresolved != "" {
		return rc
	}
	list := c.byID[ref.ID]
	r, err := semver.ParseRange(ref.Version)
	if err != nil {
		rc.Unresolved = err.Error()
		return rc
	}
	e, ok := highest(list, r)
	if !ok {
		installed := make([]string, len(list))
		for i, e := range list {
			installed[i] = e.Version()
		}
		rc.Unresolved = fmt.Sprintf("catalog %s has no installed version in %s (installed: %s)", ref.ID, ref.Version, strings.Join(installed, ", "))
		return rc
	}
	rc.Package = e.Package
	var missing []string
	for _, id := range ref.ToolIDs() {
		if t, ok := e.Test(id); ok {
			rc.Tools = append(rc.Tools, t)
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		rc.Unresolved = fmt.Sprintf("catalog %s %s, resolved from %s, does not define %s", ref.ID, e.Version(), ref.Version, strings.Join(missing, ", "))
	}
	return rc
}

// unmetDependencies warns about every unmet catalog dependency of every
// discovered expert version, by expert ID and then highest version.
func (c *Index) unmetDependencies() []string {
	ids := make([]string, 0, len(c.byID))
	for id := range c.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		for _, e := range c.byID[id] {
			out = append(out, c.ResolveEntry(e).Warnings...)
		}
	}
	return out
}

// Pin records the exact expert an investigation uses, and the catalogs it
// uses, so its behavior does not change mid-case.
type Pin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	// EngineAPI is the Expert API version the pin was made under.
	EngineAPI string            `json:"engine_api"`
	Tests     map[string]string `json:"tests"`
	// Catalogs is absent for an expert that declares none.
	Catalogs []CatalogPin `json:"catalogs,omitempty"`
}

// CatalogPin is one catalog dependency as pinned. Version, Digest and Tools
// are empty when no version resolved; Tools maps each tool used to its
// version.
type CatalogPin struct {
	ID         string            `json:"id"`
	Range      string            `json:"range"`
	Version    string            `json:"version,omitempty"`
	Digest     string            `json:"digest,omitempty"`
	Tools      map[string]string `json:"tools,omitempty"`
	Unresolved string            `json:"unresolved,omitempty"`
}

// pin identifies the package alone. Only Resolved.Pin is public, so an
// expert is never pinned without its catalogs.
func (p *Package) pin() Pin {
	tests := map[string]string{}
	for _, t := range p.Tests {
		tests[t.ID] = t.Version
	}
	return Pin{ID: p.ID(), Version: p.Version(), Digest: p.Digest, EngineAPI: EngineAPIVersion, Tests: tests}
}

func (r Resolved) Pin() Pin {
	p := r.Package.pin()
	for _, rc := range r.Catalogs {
		cp := CatalogPin{ID: rc.Ref.ID, Range: rc.Ref.Version, Unresolved: rc.Unresolved}
		if rc.Package != nil {
			cp.Version, cp.Digest = rc.Package.Version(), rc.Package.Digest
			if len(rc.Tools) > 0 {
				cp.Tools = map[string]string{}
				for _, t := range rc.Tools {
					cp.Tools[t.ID] = t.Version
				}
			}
		}
		p.Catalogs = append(p.Catalogs, cp)
	}
	return p
}

// Pinned returns the expert and catalogs an investigation pinned. Each
// package's installed copy is used while it matches its pinned digest;
// otherwise its snapshot in store is used and a warning says so. A
// dependency the pin records as unresolved stays unresolved, with its
// warning. Upgrading is an explicit, separate step: resolve again and pin
// the result.
func (c *Index) Pinned(pin Pin, store *Store) (Resolved, []string, error) {
	var warnings []string
	e, warning, err := c.pinnedPackage("expert", pin, store)
	if err != nil {
		return Resolved{}, nil, err
	}
	if warning != "" {
		warnings = append(warnings, warning)
	}
	r := Resolved{Entry: e}
	refs := map[string]CatalogRef{}
	for _, ref := range e.Manifest.Spec.Catalogs {
		refs[ref.ID] = ref
	}
	for _, cp := range pin.Catalogs {
		ref, ok := refs[cp.ID]
		if !ok || ref.Version != cp.Range {
			return Resolved{}, nil, fmt.Errorf("expert %s %s: pinned catalog %s (%s) is not one the expert declares", pin.ID, pin.Version, cp.ID, cp.Range)
		}
		rc := ResolvedCatalog{Ref: ref, Unresolved: cp.Unresolved}
		if cp.Version != "" {
			ce, warning, err := c.pinnedPackage("catalog", Pin{ID: cp.ID, Version: cp.Version, Digest: cp.Digest}, store)
			if err != nil {
				return Resolved{}, nil, fmt.Errorf("expert %s %s: %w", pin.ID, pin.Version, err)
			}
			if warning != "" {
				warnings = append(warnings, warning)
			}
			rc.Package = ce.Package
			for _, id := range ref.ToolIDs() {
				if _, used := cp.Tools[id]; !used {
					continue
				}
				t, ok := ce.Test(id)
				if !ok {
					return Resolved{}, nil, fmt.Errorf("expert %s %s: pinned tool %s is not in catalog %s %s", pin.ID, pin.Version, id, cp.ID, cp.Version)
				}
				rc.Tools = append(rc.Tools, t)
			}
		}
		if rc.Unresolved != "" {
			w := unmetWarning(e.Package, rc)
			r.Warnings = append(r.Warnings, w)
			warnings = append(warnings, w)
		}
		r.Catalogs = append(r.Catalogs, rc)
	}
	return r, warnings, nil
}

func (c *Index) pinnedPackage(kind string, pin Pin, store *Store) (Entry, string, error) {
	if e, ok := c.Describe(pin.ID, pin.Version); ok && e.Digest == pin.Digest {
		return e, "", nil
	}
	if store == nil {
		return Entry{}, "", fmt.Errorf("%s %s %s (%s): installed copy is missing or changed, and there is no snapshot store", kind, pin.ID, pin.Version, pin.Digest)
	}
	pkg, err := store.Load(pin)
	if err != nil {
		return Entry{}, "", fmt.Errorf("%s %s %s: installed copy is missing or changed, and the snapshot failed: %w", kind, pin.ID, pin.Version, err)
	}
	return Entry{pkg, Snapshot}, fmt.Sprintf("%s %s %s: installed copy is missing or changed; using the pinned snapshot", kind, pin.ID, pin.Version), nil
}

// Offered returns the tests a request offers for the expert: its own,
// then the tools of each catalog it uses that resolved.
func (r Resolved) Offered() []Test {
	out := append([]Test(nil), r.Package.Tests...)
	for _, rc := range r.Catalogs {
		out = append(out, rc.Tools...)
	}
	return out
}

// PackageOf returns the package that defines a test the expert offers: the
// expert itself, or the catalog version it resolved. A catalog's other
// tools are not offered, and not found.
func (r Resolved) PackageOf(testID string) (*Package, bool) {
	if _, ok := r.Test(testID); ok {
		return r.Package, true
	}
	for _, rc := range r.Catalogs {
		for _, t := range rc.Tools {
			if t.ID == testID {
				return rc.Package, true
			}
		}
	}
	return nil, false
}

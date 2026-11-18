package engine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/routing"
)

// installedExperts are the experts that can be opened, the highest
// version of each, catalogs left out.
func (e *Engine) installedExperts() []expert.Descriptor {
	var out []expert.Descriptor
	seen := map[string]bool{}
	for _, d := range e.experts().idx.List() {
		if d.Kind == expert.KindCatalog || seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		out = append(out, d)
	}
	return out
}

// route ranks the installed experts for a case from what its source
// reports, against the routing library. A case's routing key is a strong signal but
// not a conclusive one: the result suggests, and the engineer chooses,
// here and as the investigation moves to other routing keys. The second
// result is the experts the library names that are not installed, whose
// entries routing ignores.
func (e *Engine) route(kase Case) (routing.Result, []string, error) {
	path := e.config.RoutingLibraryPath()
	if path == "" {
		return routing.Result{}, nil, errors.New("no routing library is configured (routingLibrary)")
	}
	lib, err := routing.LoadLibrary(path)
	if err != nil {
		return routing.Result{}, nil, err
	}
	var pkgs []*expert.Package
	for _, d := range e.installedExperts() {
		if en, ok := e.experts().idx.Describe(d.ID, d.Version); ok {
			pkgs = append(pkgs, en.Package)
		}
	}
	r, err := routing.NewRouter(lib, pkgs)
	if err != nil {
		return routing.Result{}, nil, err
	}
	return r.Route(routingFacts(kase), routing.Override{}), r.Uninstalled, nil
}

// routed describes a routing result in a line.
func (e *Engine) routed(kase Case) string {
	installed := e.installedExperts()
	var names []string
	for _, d := range installed {
		names = append(names, d.ID+" "+d.Version)
	}
	problems := ""
	if errs := e.experts().Errors; len(errs) > 0 {
		problems = fmt.Sprintf("; %s could not be loaded: %s", text.Plural(len(errs), "package"), text.Safe(strings.Join(errs, "; ")))
	}
	if len(installed) == 0 {
		return "experts: none installed" + problems
	}
	res, uninstalled, err := e.route(kase)
	if err != nil {
		return fmt.Sprintf("experts: %s installed; not routed: %v%s", strings.Join(names, ", "), err, problems)
	}
	var shown []string
	for _, cand := range res.Candidates {
		// The manifest and the library may each give the same reason.
		var why []string
		seen := map[string]bool{}
		for _, m := range cand.Matches {
			r := m.Kind + " " + m.Value
			if m.Relationship != "" && m.Relationship != "primary" {
				r += ", " + m.Relationship
			}
			if !seen[r] {
				seen[r] = true
				why = append(why, r)
			}
		}
		shown = append(shown, fmt.Sprintf("%s %s (%s)", cand.ExpertID, cand.Version, strings.Join(why, ", ")))
	}
	desc := "suggested " + strings.Join(shown, "; ")
	if len(shown) == 0 {
		desc = fmt.Sprintf("no expert suggested for routing keys %q", kase.RoutingKeys)
	}
	if len(res.UnknownKeys) > 0 {
		desc += fmt.Sprintf("; routing keys the library %s does not know: %q", res.RegistryRevision, res.UnknownKeys)
	}
	if n := len(uninstalled); n > 0 {
		// A library compiled over more experts than are installed: said
		// once, briefly, since it may name dozens.
		named := uninstalled
		more := ""
		if n > 5 {
			named, more = uninstalled[:5], fmt.Sprintf(" and %d more", n-5)
		}
		desc += fmt.Sprintf("; the routing library %s names %s not installed, ignored: %s%s", res.RegistryRevision, text.Plural(n, "expert"), strings.Join(named, ", "), more)
	}
	return fmt.Sprintf("experts: %s installed; %s%s", strings.Join(names, ", "), text.Safe(desc), problems)
}

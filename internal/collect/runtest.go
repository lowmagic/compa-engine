package collect

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// ResolveParams applies defaults and checks supplied values against the
// test's parameter schema. Unknown names are refused.
func ResolveParams(t expert.Test, supplied map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for name := range supplied {
		if _, ok := t.Params[name]; !ok {
			return nil, fmt.Errorf("test %s has no parameter %q", t.ID, name)
		}
	}
	names := make([]string, 0, len(t.Params))
	for n := range t.Params {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p := t.Params[name]
		v, ok := supplied[name]
		if !ok {
			if p.Default == nil {
				return nil, fmt.Errorf("test %s: parameter %q is required", t.ID, name)
			}
			v = p.Default
		}
		if err := expert.CheckParamValue(p, v); err != nil {
			return nil, fmt.Errorf("test %s: parameter %q: %v", t.ID, name, err)
		}
		out[name] = v
	}
	return out, nil
}

// RunTest runs an expert's test or a catalog's tool against an artifact
// root. A test whose
// parameters are invalid, whose preconditions fail, or whose binaries are
// not allowlisted reports unavailable and runs nothing.
func (r *Runner) RunTest(ctx context.Context, artifactRoot string, pkg *expert.Package, testID string, args map[string]any) Record {
	t, ok := pkg.Test(testID)
	what := What{TestID: testID}
	kind := "expert"
	if pkg.IsCatalog() {
		kind, what.Catalog = "catalog", pkg.ID()+"@"+pkg.Version()
	} else {
		what.Expert = pkg.ID() + "@" + pkg.Version()
	}
	if !ok {
		return r.unavailableRecord(what, fmt.Sprintf("%s %s has no test %s", kind, pkg.ID(), testID))
	}
	what.TestVersion, what.Pipeline = t.Version, t.Run
	vars, err := ResolveParams(t, args)
	if err != nil {
		return r.unavailableRecord(what, err.Error())
	}
	what.Params = vars
	if len(t.Preconditions) > 0 {
		root, err := openArtifactRoot(artifactRoot, r.exclusions(), r.CredentialFiles)
		if err != nil {
			return r.unavailableRecord(what, "artifact root: "+err.Error())
		}
		defer root.Close()
		for _, pre := range t.Preconditions {
			p := pre.Exists
			if m := wholeTemplate.FindStringSubmatch(p); m != nil {
				p = argString(vars[m[1]])
			}
			if _, err := root.stat(p); errors.Is(err, errExcluded) || errors.Is(err, errCredentials) {
				return r.unavailableRecord(what, "precondition: "+err.Error())
			} else if err != nil {
				return r.unavailableRecord(what, fmt.Sprintf("precondition: %s does not exist in the artifacts", p))
			}
		}
	}
	return r.Run(ctx, Request{ArtifactRoot: artifactRoot, What: what, Vars: vars})
}

func (r *Runner) unavailableRecord(what What, reason string) Record {
	return Unavailable(what, reason)
}

func Unavailable(what What, reason string) Record {
	rec := Record{What: what, Status: StatusUnavailable, Reason: reason}
	rec.ID = evidenceID(rec)
	return rec
}

// WithID gives a record made outside the runner, as the engine's own tools
// make theirs, the ID the runner would: one derived from what ran and what
// it produced.
func WithID(rec Record) Record {
	rec.ID = evidenceID(rec)
	return rec
}

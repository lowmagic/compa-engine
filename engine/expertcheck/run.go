package expertcheck

import (
	"context"
	"errors"
	"fmt"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
)

// Packages are the packages discovered under some roots.
type Packages struct {
	idx *expert.Index
	// CredentialFiles name the files Run refuses as holding only
	// credentials, as a deployment names them to the engine; none unless
	// set.
	CredentialFiles []string
}

// Load discovers the packages under the roots as local roots, as Command
// does. Any package that fails to load, or any other discovery error,
// fails it: a fixture run over a broken package would test nothing.
func Load(roots ...string) (*Packages, error) {
	var rs []expert.Root
	for _, r := range roots {
		rs = append(rs, expert.Root{Path: r, Source: expert.Local})
	}
	idx := loader.Discover(rs)
	if err := errors.Join(idx.Errors...); err != nil {
		return nil, err
	}
	return &Packages{idx: idx}, nil
}

// Result is the part of a collection's record a fixture checks.
type Result struct {
	Status    string
	Reason    string
	Truncated bool
	Output    string
}

// Run runs the test or catalog tool testID against artifactRoot with the
// given parameters, through the engine's runner at its default limits. No
// sandbox and no binary is allowed, so a step that runs a command reports
// unavailable; the case store, .compa, is never read, nor the credential
// files named in CredentialFiles.
func (p *Packages) Run(ctx context.Context, artifactRoot, testID string, params map[string]any) Result {
	pkg := p.definer(testID)
	if pkg == nil {
		return Result{Status: collect.StatusUnavailable, Reason: fmt.Sprintf("no package defines %s", testID)}
	}
	r := &collect.Runner{Limits: collect.DefaultLimits, CredentialFiles: p.CredentialFiles}
	rec := r.RunTest(ctx, artifactRoot, pkg, testID, params)
	return Result{Status: rec.Status, Reason: rec.Reason, Truncated: rec.Truncated, Output: string(rec.Output)}
}

// definer is the highest usable version of the package that defines
// testID. Test IDs are unique across packages.
func (p *Packages) definer(testID string) *expert.Package {
	for _, d := range p.idx.List() {
		e, ok := p.idx.Describe(d.ID, d.Version)
		if !ok {
			continue
		}
		if _, ok := e.Test(testID); ok {
			return e.Package
		}
	}
	return nil
}

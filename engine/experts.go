package engine

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
)

// loader is the engine's expert loader: every stage of every test is
// checked against the collection primitives.
func loader() expert.Loader { return expert.Loader{CheckStage: collect.CheckStage} }

// installed is the configured expert roots, discovered once per command.
// Errors and Warnings are what discovery found, for the engineer to see as
// compa expert check shows them.
type installed struct {
	idx      *expert.Index
	Errors   []string
	Warnings []string
}

var errNoExperts = errors.New("no expert roots are configured (experts.bundled, experts.local)")

func (e *Engine) experts() *installed {
	e.lazy.Lock()
	defer e.lazy.Unlock()
	if e.installed != nil {
		return e.installed
	}
	var bundleErr error
	if bundleErr = e.bundled(); bundleErr != nil {
		bundleErr = fmt.Errorf("%w; working without them", bundleErr)
	}
	roots, warnings := e.config.ExpertRoots()
	in := &installed{idx: loader().Discover(roots), Warnings: warnings}
	if bundleErr != nil {
		in.Errors = append(in.Errors, bundleErr.Error())
	}
	for _, err := range in.idx.Errors {
		in.Errors = append(in.Errors, err.Error())
	}
	in.Warnings = append(in.Warnings, in.idx.Warnings...)
	e.installed = in
	return in
}

// snapshots is where a case keeps the packages its conversations pinned: in its
// store, so the case folder carries them, unless configuration names
// another directory.
func (e *Engine) snapshots(c *openCase) *expert.Store {
	dir := e.config.SnapshotDir()
	if dir == "" {
		dir = filepath.Join(c.storeDir, "experts")
	}
	return &expert.Store{Dir: dir, Loader: loader()}
}

func (e *Engine) resolve(expertID, constraint string) (expert.Resolved, error) {
	roots, _ := e.config.ExpertRoots()
	if len(roots) == 0 {
		return expert.Resolved{}, errNoExperts
	}
	return e.experts().idx.Resolve(expertID, constraint)
}

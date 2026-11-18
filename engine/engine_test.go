package engine_test

import (
	"context"
	"testing"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/stubcases"
	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/routing"
)

func TestNewRequiresSources(t *testing.T) {
	cases, artifacts := stubcases.New("../testdata/cases"), localdir.New("../testdata")
	for name, o := range map[string]engine.Options{
		"no case source":     {Artifacts: artifacts},
		"no artifact source": {Cases: cases},
	} {
		if _, err := engine.New(o); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	if _, err := engine.New(engine.Options{Cases: cases, Artifacts: artifacts}); err != nil {
		t.Error(err)
	}
	cfg := []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\ncollection: {exclude: [.statedir]}\n")
	if _, err := engine.New(engine.Options{Cases: cases, Artifacts: artifacts, Config: cfg}); err != nil {
		t.Errorf("valid configuration: %v", err)
	}
	if _, err := engine.New(engine.Options{Cases: cases, Artifacts: artifacts, Config: []byte("kind: Config\nharness: pi\n")}); err == nil {
		t.Error("New accepted an invalid configuration")
	}
}

// TestStubCaseRoutesByKey routes from case metadata alone; no artifact
// source is consulted.
func TestStubCaseRoutesByKey(t *testing.T) {
	ix := expert.Loader{CheckStage: collect.CheckStage}.Discover([]expert.Root{{Path: "../testdata/experts", Source: expert.Bundled}})
	if len(ix.Errors) > 0 {
		t.Fatal(ix.Errors)
	}
	var pkgs []*expert.Package
	for _, id := range []string{"openshift.networking", "openshift.platform"} {
		r, err := ix.Resolve(id, ">=0.1.0 <0.2.0")
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, r.Package)
	}
	lib, err := routing.LoadLibrary("../testdata/routing/library.yaml")
	if err != nil {
		t.Fatal(err)
	}
	router, err := routing.NewRouter(lib, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	src := stubcases.New("../testdata/cases")
	for id, want := range map[string]string{"00000001": "openshift.networking", "00000002": "openshift.platform"} {
		c, err := src.Case(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		got := router.Route(engine.RoutingFacts(c), routing.Override{})
		if len(got.Candidates) == 0 || got.Candidates[0].ExpertID != want {
			t.Errorf("case %s routes to %+v, want %s first", id, got.Candidates, want)
			continue
		}
		if m := got.Candidates[0].Matches; len(m) == 0 || m[0].Kind != routing.MatchKey {
			t.Errorf("case %s: top match %+v, want a routing key match", id, m)
		}
	}
}

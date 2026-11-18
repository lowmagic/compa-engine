package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/expert"
)

func fixtures(t *testing.T) (*Library, []*expert.Package) {
	t.Helper()
	lib, err := LoadLibrary("../../testdata/routing/library.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []*expert.Package
	for _, name := range []string{"platform", "networking"} {
		p, err := expert.Loader{}.Load(filepath.Join("../../testdata/experts", name))
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return lib, pkgs
}

func router(t *testing.T) *Router {
	lib, pkgs := fixtures(t)
	r, err := NewRouter(lib, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) > 0 {
		t.Fatalf("problems: %v", r.Problems)
	}
	return r
}

func ids(res Result) string {
	var out []string
	for _, c := range res.Candidates {
		out = append(out, c.ExpertID)
	}
	return strings.Join(out, " ")
}

func TestLibraryLookups(t *testing.T) {
	lib, _ := fixtures(t)
	if lib.Revision() != "fixture-1" {
		t.Errorf("revision = %q", lib.Revision())
	}
	for _, name := range []string{"openshift", "OpenShift", "ocp"} {
		if s, ok := lib.Canonical(name); !ok || s.ID != "openshift" {
			t.Errorf("Canonical(%q) = %v, %v", name, s.ID, ok)
		}
	}
	if _, ok := lib.Canonical("rhel"); ok {
		t.Error("unknown routing key resolved")
	}
	if got := lib.Experts("openshift-networking"); len(got) != 1 || got[0].ExpertID != "openshift.networking" {
		t.Errorf("forward lookup = %v", got)
	}
	if got := lib.KeysForComponent("OVN-Kubernetes"); !reflect.DeepEqual(got, []string{"openshift-networking"}) {
		t.Errorf("reverse component lookup = %v", got)
	}
	if got := lib.KeysForTag("openshift"); !reflect.DeepEqual(got, []string{"openshift", "openshift-networking"}) {
		t.Errorf("reverse tag lookup = %v", got)
	}
}

func TestLibraryRejectsConflictingNames(t *testing.T) {
	_, err := NewLibrary("r", []Key{{ID: "a", Aliases: []string{"x"}}, {ID: "b", Aliases: []string{"X"}}})
	if err == nil || !strings.Contains(err.Error(), "names both a and b") {
		t.Fatalf("got %v", err)
	}
	if _, err := NewLibrary("r", []Key{{ID: "a"}, {ID: "a"}}); err == nil || !strings.Contains(err.Error(), "routing key a is defined twice") {
		t.Fatalf("want error for a duplicate routing key, got %v", err)
	}
}

const libraryBody = `revision: r1
%s:
  - id: openshift
    experts: [{id: openshift.platform}]
`

func writeLibrary(t *testing.T, header, list string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "library.yaml")
	if err := os.WriteFile(path, []byte(header+fmt.Sprintf(libraryBody, list)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The library is read in one form only: the routing form loads, and each
// part of the form it replaced is refused on its own.
func TestLoadLibraryForm(t *testing.T) {
	const current = "apiVersion: compa.lowmag.cc/routing/v1alpha1\nkind: RoutingLibrary\n"
	path := writeLibrary(t, current, "keys")
	lib, err := LoadLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.Experts("openshift"); lib.Revision() != "r1" || len(got) != 1 || got[0].ExpertID != "openshift.platform" || got[0].Relationship != "primary" {
		t.Errorf("revision %q, experts %+v", lib.Revision(), got)
	}
	for _, tc := range []struct {
		name, header, list, want string
	}{
		{"old kind", "apiVersion: compa.lowmag.cc/routing/v1alpha1\nkind: SBRLibrary\n", "keys", "/kind"},
		{"old apiVersion", "apiVersion: compa.lowmag.cc/sbr/v1alpha1\nkind: RoutingLibrary\n", "keys", "/apiVersion"},
		{"old list", current, "sbrs", "sbrs"},
		{"old document", "apiVersion: compa.lowmag.cc/sbr/v1alpha1\nkind: SBRLibrary\n", "sbrs", "/kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLibrary(t, tc.header, tc.list)
			_, err := LoadLibrary(path)
			if err == nil || !strings.HasPrefix(err.Error(), "routing library "+path+": ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRouteByKey(t *testing.T) {
	r := router(t)
	res := r.Route(Facts{Keys: []string{"OpenShift Networking"}}, Override{})
	if ids(res) != "openshift.networking" || res.Ambiguous {
		t.Fatalf("got %s (ambiguous %v)", ids(res), res.Ambiguous)
	}
	m := res.Candidates[0].Matches[0]
	if m.Kind != MatchKey || m.Key != "openshift-networking" || m.Relationship != "primary" {
		t.Errorf("match = %+v", m)
	}

	// The openshift routing key is primary for platform (registry) and secondary for
	// networking (its manifest), so both are offered, platform first.
	res = r.Route(Facts{Keys: []string{"openshift"}}, Override{})
	if ids(res) != "openshift.platform openshift.networking" {
		t.Fatalf("got %s", ids(res))
	}
	if res.Candidates[1].Score >= res.Candidates[0].Score {
		t.Errorf("a secondary relationship must rank below a primary one: %+v", res.Candidates)
	}
	if res.RegistryRevision != "fixture-1" {
		t.Errorf("revision = %q", res.RegistryRevision)
	}
}

// Where the library and an installed manifest both relate an expert to a
// routing key, the manifest decides: a library naming networking primary for openshift
// does not outweigh networking's own manifest, which names it secondary,
// and the match keeps the library's provenance.
func TestManifestDecidesItsOwnRelationship(t *testing.T) {
	_, pkgs := fixtures(t)
	lib, err := NewLibrary("r", []Key{{ID: "openshift", Aliases: []string{"OpenShift"}, Experts: []Relationship{
		{ExpertID: "openshift.platform", Relationship: "primary"},
		{ExpertID: "openshift.networking", Relationship: "primary", Provenance: []Provenance{{Source: "example-source"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRouter(lib, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	res := r.Route(Facts{Keys: []string{"openshift"}}, Override{})
	if ids(res) != "openshift.platform openshift.networking" || res.Candidates[1].Score >= res.Candidates[0].Score {
		t.Fatalf("candidates %+v", res.Candidates)
	}
	ms := res.Candidates[1].Matches
	if len(ms) != 1 || ms[0].Relationship != "secondary" || ms[0].Source != "manifest" || len(ms[0].Provenance) != 1 || ms[0].Provenance[0].Source != "example-source" {
		t.Errorf("networking's matches %+v", ms)
	}
}

func TestRouteOwnershipOutweighsSignals(t *testing.T) {
	r := router(t)
	res := r.Route(Facts{
		Keys:   []string{"openshift"},
		Values: map[string]any{"cluster.network_type": "OVNKubernetes", "product.name": "OpenShift"},
	}, Override{})
	if ids(res) != "openshift.platform openshift.networking" {
		t.Fatalf("got %s", ids(res))
	}
}

func TestRouteBySignalAndComponent(t *testing.T) {
	r := router(t)
	res := r.Route(Facts{Values: map[string]any{"cluster.network_type": "OVNKubernetes"}}, Override{})
	if ids(res) != "openshift.networking" || res.Candidates[0].Matches[0].Kind != MatchSignal {
		t.Fatalf("signal: got %+v", res.Candidates)
	}
	res = r.Route(Facts{Components: []string{"coredns"}}, Override{})
	if ids(res) != "openshift.networking" {
		t.Fatalf("component: got %s", ids(res))
	}
	kinds := map[string]bool{}
	for _, m := range res.Candidates[0].Matches {
		kinds[m.Kind] = true
	}
	if !kinds[MatchComponent] || !kinds[MatchComponentKey] {
		t.Errorf("want a direct and a registry component match, got %+v", res.Candidates[0].Matches)
	}
}

func TestRouteAmbiguousAndTags(t *testing.T) {
	r := router(t)
	// The openshift tag points to both routing keys; tags only suggest.
	res := r.Route(Facts{Tags: []string{"openshift"}}, Override{})
	if ids(res) != "openshift.networking openshift.platform" {
		t.Fatalf("got %s", ids(res))
	}
	for _, c := range res.Candidates {
		if c.Score >= weights[MatchSignal] {
			t.Errorf("a tag must weigh less than a signal: %+v", c)
		}
	}
	if res.Ambiguous {
		t.Errorf("unequal scores flagged ambiguous: %+v", res.Candidates)
	}

	// A routing key shared by two experts as primary is ambiguous, and the tie
	// breaks by expert ID.
	_, pkgs := fixtures(t)
	shared, err := NewLibrary("r", []Key{{ID: "shared", Experts: []Relationship{{ExpertID: "openshift.platform"}, {ExpertID: "openshift.networking"}}}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewRouter(shared, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	res = r2.Route(Facts{Keys: []string{"shared"}}, Override{})
	if !res.Ambiguous || ids(res) != "openshift.networking openshift.platform" {
		t.Errorf("got %s (ambiguous %v)", ids(res), res.Ambiguous)
	}
}

func TestRouteContradictedSignal(t *testing.T) {
	r := router(t)
	res := r.Route(Facts{Keys: []string{"openshift-networking"}, Values: map[string]any{"cluster.network_type": "OpenShiftSDN"}}, Override{})
	if ids(res) != "openshift.networking" {
		t.Fatalf("got %s", ids(res))
	}
	if got := res.Candidates[0].Contradicted; len(got) != 1 || !strings.Contains(got[0], "cluster.network_type") {
		t.Errorf("contradicted = %v", got)
	}
}

func TestRouteOverride(t *testing.T) {
	r := router(t)
	res := r.Route(Facts{Keys: []string{"openshift"}}, Override{Include: []string{"openshift.networking", "missing.expert"}, Exclude: []string{"openshift.platform"}})
	if ids(res) != "openshift.networking" {
		t.Fatalf("got %s", ids(res))
	}
	if res.Candidates[0].Matches[0].Kind != MatchOverride {
		t.Errorf("matches = %+v", res.Candidates[0].Matches)
	}
	want := []Exclusion{
		{ExpertID: "missing.expert", Reason: "engineer included it, but it is not installed"},
		{ExpertID: "openshift.platform", Reason: "excluded by the engineer"},
	}
	if !reflect.DeepEqual(res.Exclusions, want) {
		t.Errorf("exclusions = %+v", res.Exclusions)
	}
}

func TestRouteUnknownKey(t *testing.T) {
	res := router(t).Route(Facts{Keys: []string{"rhel-kernel"}}, Override{})
	if len(res.Candidates) != 0 || !reflect.DeepEqual(res.UnknownKeys, []string{"rhel-kernel"}) {
		t.Fatalf("got %+v", res)
	}
}

func TestRouteIsDeterministic(t *testing.T) {
	lib, pkgs := fixtures(t)
	facts := Facts{Keys: []string{"openshift"}, Components: []string{"kubelet", "coredns"}, Tags: []string{"openshift"}, Values: map[string]any{"cluster.network_type": "OVNKubernetes"}}
	var first []byte
	for i := 0; i < 20; i++ {
		order := pkgs
		if i%2 == 1 {
			order = []*expert.Package{pkgs[1], pkgs[0]}
		}
		r, err := NewRouter(lib, order)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(r.Route(facts, Override{}))
		if first == nil {
			first = got
		} else if string(got) != string(first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, got, first)
		}
	}
}

func TestRouterReportsUnknownManifestKey(t *testing.T) {
	_, pkgs := fixtures(t)
	strict, err := NewLibrary("r", []Key{{ID: "openshift"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRouter(strict, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], `declares routing key "openshift-networking"`) {
		t.Fatalf("problems = %v", r.Problems)
	}
	if _, err := NewRouter(strict, append(pkgs, pkgs[0])); err == nil {
		t.Fatal("want error for an expert given twice")
	}
}

func TestSameValue(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
	}{{3.0, 3, true}, {3, "3", false}, {"a", "a", true}, {true, true, true}, {true, "true", false}} {
		if got := sameValue(c.a, c.b); got != c.want {
			t.Errorf("sameValue(%#v, %#v) = %v", c.a, c.b, got)
		}
	}
}

// A fact neither observed nor known absent is unknown: missing collection
// is not absence, so present: false must not match it.
func TestSignalPresenceIsThreeValued(t *testing.T) {
	yes, no := true, false
	observed := Facts{Values: map[string]any{"f.x": "v"}}
	absent := Facts{Absent: map[string]bool{"f.x": true}}
	unknown := Facts{}
	for _, tc := range []struct {
		name string
		s    expert.Signal
		f    Facts
		want signalResult
	}{
		{"present:true, observed", expert.Signal{Fact: "f.x", Present: &yes}, observed, signalMatch},
		{"present:true, absent", expert.Signal{Fact: "f.x", Present: &yes}, absent, signalContradicted},
		{"present:true, unknown", expert.Signal{Fact: "f.x", Present: &yes}, unknown, signalAbsent},
		{"present:false, observed", expert.Signal{Fact: "f.x", Present: &no}, observed, signalContradicted},
		{"present:false, absent", expert.Signal{Fact: "f.x", Present: &no}, absent, signalMatch},
		{"present:false, unknown", expert.Signal{Fact: "f.x", Present: &no}, unknown, signalAbsent},
		{"equals, absent", expert.Signal{Fact: "f.x", Equals: "v"}, absent, signalAbsent},
		{"equals, unknown", expert.Signal{Fact: "f.x", Equals: "v"}, unknown, signalAbsent},
		{"equals, observed", expert.Signal{Fact: "f.x", Equals: "v"}, observed, signalMatch},
	} {
		if got := evaluate(tc.s, tc.f); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestRouterIgnoresAndReportsUninstalledExperts: a library compiled over
// more experts than are installed still routes: an entry naming an expert
// that is not installed is ignored, the installed ones are suggested as
// before, and the router says which experts it ignored.
func TestRouterIgnoresAndReportsUninstalledExperts(t *testing.T) {
	lib, pkgs := fixtures(t)
	r, err := NewRouter(lib, pkgs[1:]) // openshift.networking alone
	if err != nil {
		t.Fatalf("a library naming an expert not installed failed the router: %v", err)
	}
	if strings.Join(r.Uninstalled, ",") != "openshift.platform" {
		t.Errorf("Uninstalled = %q, want openshift.platform", r.Uninstalled)
	}
	res := r.Route(Facts{Keys: []string{"openshift"}}, Override{})
	if len(res.Candidates) != 1 || res.Candidates[0].ExpertID != "openshift.networking" {
		t.Errorf("Route(openshift) = %+v, want the installed secondary alone", res.Candidates)
	}
	if all := router(t); len(all.Uninstalled) != 0 {
		t.Errorf("with every expert installed, Uninstalled = %q", all.Uninstalled)
	}
}

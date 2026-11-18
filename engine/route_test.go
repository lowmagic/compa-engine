package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/config"
)

// TestRoutingSuggests: a case's routing key ranks the installed experts, a
// new key suggests anew, a key no expert answers says so, and with no
// routing library configured the line says which setting names one.
func TestRoutingSuggests(t *testing.T) {
	experts, _ := filepath.Abs("../testdata/experts")
	lib, _ := filepath.Abs("../testdata/routing/library.yaml")
	cfg, err := config.Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {bundled: "+experts+"}\nroutingLibrary: "+lib+"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{config: cfg, home: t.TempDir()}
	line := e.routed(Case{ID: "00000001", RoutingKeys: []string{"openshift-networking"}})
	if !strings.Contains(line, "suggested openshift.networking 0.1.0 (key openshift-networking)") {
		t.Errorf("line %q", line)
	}
	if line := e.routed(Case{ID: "00000001", RoutingKeys: []string{"openshift"}}); !strings.Contains(line, "suggested openshift.platform") {
		t.Errorf("after the routing key changed: %q", line)
	}
	if line := e.routed(Case{ID: "00000001", RoutingKeys: []string{"ceph"}}); !strings.Contains(line, `no expert suggested for routing keys ["ceph"]`) {
		t.Errorf("line %q", line)
	}
	bare, err := config.Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {bundled: "+experts+"}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	unrouted := &Engine{config: bare, home: t.TempDir()}
	if line := unrouted.routed(Case{ID: "00000001", RoutingKeys: []string{"openshift"}}); !strings.Contains(line, "not routed: no routing library is configured (routingLibrary)") {
		t.Errorf("line %q", line)
	}
}

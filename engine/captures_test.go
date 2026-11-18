package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/expert"
)

var gatherKinds = []expert.Capture{
	{Kind: "must-gather", Marks: []string{"cluster-scoped-resources"}},
	{Kind: "sosreport", Marks: []string{"sos_commands"}},
}

func layout(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A kind is present when an attachment holds one of its marks, however
// deep its own layout puts it within the bound; the case store and hidden
// entries are not looked in.
func TestCaptureKinds(t *testing.T) {
	root := layout(t, "0010-must-gather.tar.gz/must-gather.local.1/quay-io-image/cluster-scoped-resources/core", ".compa/cluster-scoped-resources")
	got := captureKinds(root, gatherKinds)
	if !got["must-gather"] || got["sosreport"] {
		t.Errorf("kinds %v", got)
	}
	if got := captureKinds(layout(t, ".compa/sos_commands", ".hidden/sos_commands"), gatherKinds); len(got) != 0 {
		t.Errorf("kinds in the store or hidden: %v", got)
	}
	if got := captureKinds(layout(t, "a/b/c/d/e/f/sos_commands"), gatherKinds); got["sosreport"] {
		t.Error("a mark past the depth bound was found")
	}
}

// A tool of a catalog that reads a kind the case lacks is offered in
// one line with a note naming the kind; once an attachment of that kind
// is there, in full. A catalog that declares no kinds is always offered in
// full, as are the expert's own tests.
func TestOfferedByCapture(t *testing.T) {
	full := "Reads the cluster operators' conditions. Each line is an operator, its condition and its message."
	r := expert.Resolved{
		Entry: expert.Entry{Package: &expert.Package{Tests: []expert.Test{{ID: "x.own", Description: "Its own test. Long."}}}},
		Catalogs: []expert.ResolvedCatalog{
			{Package: &expert.Package{Manifest: expert.Manifest{Spec: expert.Spec{Captures: gatherKinds[:1]}}},
				Tools: []expert.Test{{ID: "openshift.gather.operators", Description: full}, {ID: "openshift.gather.nodes", Description: "Node conditions. More.", Summary: "Each node's conditions."}}},
			{Package: &expert.Package{}, Tools: []expert.Test{{ID: "common.files.grep", Description: "Searches files. More."}}},
		},
	}
	tests, notes := offeredByCapture(layout(t, "sosreport-1/sos_commands"), r)
	desc := map[string]string{}
	for _, t := range tests {
		desc[t.ID] = t.Description
	}
	if desc["openshift.gather.operators"] != "Reads the cluster operators' conditions." || desc["openshift.gather.nodes"] != "Each node's conditions." ||
		desc["common.files.grep"] != "Searches files. More." || desc["x.own"] != "Its own test. Long." || len(tests) != 4 {
		t.Errorf("offered %v", desc)
	}
	if n := notes["openshift.gather.operators"]; !strings.Contains(n, "reads a must-gather, and no attachment was identified as one") || notes["common.files.grep"] != "" {
		t.Errorf("notes %v", notes)
	}
	tests, notes = offeredByCapture(layout(t, "mg/cluster-scoped-resources"), r)
	if tests[1].Description != full || len(notes) != 0 {
		t.Errorf("with a must-gather: %q %v", tests[1].Description, notes)
	}
}

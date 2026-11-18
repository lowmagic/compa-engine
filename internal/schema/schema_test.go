package schema

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var fixtureSchemas = map[string]ID{
	"manifest-v1alpha3": ExpertManifestV1alpha3,
	"tests-v1alpha2":    ExpertTestsV1alpha2,
	"catalog":           ExpertCatalog,
	"identity":          ExpertIdentity,
	"check":             ExpertCheck,
	"cases":             ExpertCases,
	"routing":           RoutingLibrary,
	"config":            Config,
	"ledger":            LedgerUpdate,
	"findings":          CheckFindings,
}

// TestFixtures validates every file under testdata/<kind>/valid and
// testdata/<kind>/invalid. An invalid fixture's first line is
// "# want: <text>", which must appear in one of the reported problems.
func TestFixtures(t *testing.T) {
	for dir, id := range fixtureSchemas {
		for _, outcome := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join("testdata", dir, outcome, "*.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) == 0 {
				t.Errorf("no %s fixtures for %s", outcome, dir)
			}
			for _, file := range files {
				t.Run(filepath.ToSlash(file), func(t *testing.T) {
					data, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					doc, err := DecodeYAML(data)
					if err != nil {
						t.Fatal(err)
					}
					err = Validate(id, doc)
					if outcome == "valid" {
						if err != nil {
							t.Fatalf("want valid, got %v", err)
						}
						return
					}
					want, ok := strings.CutPrefix(strings.SplitN(string(data), "\n", 2)[0], "# want: ")
					if !ok {
						t.Fatal(`invalid fixture must start with "# want: "`)
					}
					var se *Error
					if !errors.As(err, &se) {
						t.Fatalf("want *Error containing %q, got %v", want, err)
					}
					for _, p := range se.Problems {
						if strings.Contains(p, want) {
							return
						}
					}
					t.Fatalf("no problem contains %q:\n%s", want, strings.Join(se.Problems, "\n"))
				})
			}
		}
	}
}

func TestEverySchemaCompiles(t *testing.T) {
	for _, id := range All {
		if err := Validate(id, map[string]any{}); err == nil {
			t.Errorf("%s accepted an empty document", id)
		} else if !errors.As(err, new(*Error)) {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestUnknownSchema(t *testing.T) {
	if err := Validate("compa.lowmag.cc/nope/nope/v1", map[string]any{}); err == nil {
		t.Fatal("want error for unknown schema")
	}
}

func TestLedgerVersionCutover(t *testing.T) {
	const current ID = "compa.lowmag.cc/ledger/update/v1alpha2"
	const legacy ID = "compa.lowmag.cc/ledger/update/v1alpha1"
	doc := map[string]any{"items": []any{}, "updates": []any{}}
	if LedgerUpdate != current {
		t.Fatalf("LedgerUpdate = %q, want %q", LedgerUpdate, current)
	}
	if err := Validate(current, doc); err != nil {
		t.Fatalf("current ledger schema unavailable: %v", err)
	}
	if err := Validate(legacy, doc); err == nil || err.Error() != "unknown schema "+string(legacy) {
		t.Fatalf("want unregistered legacy ledger schema, got %v", err)
	}
	if _, err := embedded(legacy); err == nil {
		t.Fatal("legacy ledger schema is still embedded")
	}
}

func TestRoutingSchemaCutover(t *testing.T) {
	if RoutingLibrary != "compa.lowmag.cc/routing/library/v1alpha1" || RoutingTerms != "compa.lowmag.cc/routing/terms/v1alpha1" {
		t.Fatalf("routing schemas = %q, %q", RoutingLibrary, RoutingTerms)
	}
	terms := map[string]any{"apiVersion": "compa.lowmag.cc/routing/v1alpha1", "kind": "TermDictionary", "revision": "r1",
		"terms": []any{map[string]any{"term": "pod network", "keys": []any{"openshift-networking"}}}}
	if err := Validate(RoutingTerms, terms); err != nil {
		t.Fatalf("routing term dictionary: %v", err)
	}
	for _, legacy := range []ID{"compa.lowmag.cc/sbr/library/v1alpha1", "compa.lowmag.cc/sbr/terms/v1alpha1"} {
		if err := Validate(legacy, map[string]any{}); err == nil || err.Error() != "unknown schema "+string(legacy) {
			t.Errorf("want unregistered schema %s, got %v", legacy, err)
		}
		if _, err := embedded(legacy); err == nil {
			t.Errorf("schema %s is still embedded", legacy)
		}
	}
}

func TestDecodeYAMLRejectsMultipleDocuments(t *testing.T) {
	if _, err := DecodeYAML([]byte("a: 1\n---\nb: 2\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestDecodeYAMLRejectsDuplicateKeys(t *testing.T) {
	if _, err := DecodeYAML([]byte("a: 1\na: 2\n")); err == nil {
		t.Fatal("want error")
	}
}

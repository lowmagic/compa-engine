package ledger_test

import (
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/api"
	"github.com/lowmagic/compa-engine/internal/ledger"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// vc is a synthetic request: e1 to e3 given, oldest first, e3 an edit of
// ev-old; the ledger holds l1, an ask from ev-held, l2, what ev-held
// provided, l3, a report from ev-old, and l4, an ask already answered.
func vc() ledger.Context {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return ledger.Context{
		Names: map[string]string{"e1": "ev-1", "e2": "ev-2", "e3": "ev-3", "c9": "ev-2"},
		Written: map[string]time.Time{
			"ev-held": t0, "ev-old": t0.Add(time.Hour),
			"ev-1": t0.Add(2 * time.Hour), "ev-2": t0.Add(3 * time.Hour), "ev-3": t0.Add(time.Hour),
		},
		Held: []ledger.Entry{
			{ID: "l1", Kind: ledger.Asks, Status: ledger.Open, Comment: "ev-held"},
			{ID: "l2", Kind: ledger.Provides, Comment: "ev-held"},
			{ID: "l3", Kind: ledger.Reports, Comment: "ev-old"},
			{ID: "l4", Kind: ledger.Asks, Status: ledger.Answered, Comment: "ev-held"},
		},
		Earlier: map[string][]string{"ev-3": {"ev-old"}},
	}
}

func args(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// An update's elements are each checked against the request and the
// ledger: what passes is kept, comments as evidence IDs, and what fails is
// dropped with why.
func TestValidate(t *testing.T) {
	u, dropped, err := ledger.Validate(vc(), args(t, `{
		"items": [
			{"kind": "provides", "who": "customer", "text": "A must-gather.", "comment": "e1", "attachment": "must-gather.tar.gz", "status": "superseded", "because": "e2"},
			{"kind": "asks", "who": "support", "text": "Send a fresh must-gather.", "comment": "c9", "status": "open"},
			{"kind": "reports", "who": "customer", "text": "It started after the upgrade.", "comment": "e1", "attachment": "x.log", "status": "none"},
			{"kind": "reports", "who": "customer", "text": "Gone.", "comment": "e9", "status": "none"},
			{"kind": "reports", "who": "customer", "text": "Gone.", "comment": "e1", "status": "superseded", "because": "e2"},
			{"kind": "asks", "who": "customer", "text": "Why?", "comment": "e2", "status": "answered", "because": "e1"}
		],
		"updates": [
			{"item": "l1", "status": "answered", "because": "e1"},
			{"item": "l2", "status": "superseded", "because": "e2", "note": "a newer capture was sent"},
			{"item": "l3", "status": "withdrawn", "because": "e3"},
			{"item": "l4", "status": "ruled_out", "because": "e2"},
			{"item": "l1", "status": "ruled_out", "because": "e2"},
			{"item": "l9", "status": "done", "because": "e2"},
			{"item": "l4", "status": "withdrawn", "because": "e1"}
		],
		"none": [{"comment": "e3", "why": "nothing_new"}, {"comment": "e1", "why": "courtesy"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := ledger.Update{
		Items: []ledger.Item{
			{Kind: "provides", Who: "customer", Text: "A must-gather.", Comment: "ev-1", Attachment: "must-gather.tar.gz", Status: "superseded", Because: "ev-2"},
			{Kind: "asks", Who: "support", Text: "Send a fresh must-gather.", Comment: "ev-2", Status: "open"},
			{Kind: "reports", Who: "customer", Text: "It started after the upgrade.", Comment: "ev-1"},
		},
		Updates: []ledger.Change{
			{Item: "l1", Status: "answered", Because: "ev-1"},
			{Item: "l2", Status: "superseded", Because: "ev-2", Note: "a newer capture was sent"},
			{Item: "l3", Status: "withdrawn", Because: "ev-3"},
		},
		None: []ledger.None{{Comment: "ev-3", Why: "nothing_new"}},
	}
	if d := cmp.Diff(want, u); d != "" {
		t.Errorf("kept (-want +got):\n%s", d)
	}
	var got []string
	for _, d := range dropped {
		got = append(got, d.String())
	}
	wantDropped := []string{
		"items[2].attachment: a reports item names no attachment",
		"items[3]: e9 is no comment the request gave",
		"items[4]: a reports item cannot be superseded",
		"items[5]: e1 was not written after ev-2",
		"updates[3]: l4 is already answered",
		"updates[4]: l1 is moved twice",
		"updates[5]: the ledger holds no item l9",
		"updates[6]: e1 is no edited version of l4's comment",
		"none[1]: e1 gave items",
	}
	if d := cmp.Diff(wantDropped, got); d != "" {
		t.Errorf("dropped (-want +got):\n%s", d)
	}
}

// An update the schema refuses is refused whole, saying why, so the model
// can call again.
func TestValidateRefusesWhole(t *testing.T) {
	for _, s := range []string{
		`{"items": []}`,
		`{"items": [{"kind": "asks", "who": "support", "text": "x", "comment": "e1", "status": "closed"}], "updates": []}`,
		`{"items": [], "updates": [], "summary": "the case"}`,
	} {
		if _, _, err := ledger.Validate(vc(), args(t, s)); err == nil {
			t.Errorf("%s was accepted", s)
		}
	}
}

// The ledger files an item under one of two sides; a who that is neither
// is not a side.
func TestValidateRejectsUnknownWho(t *testing.T) {
	_, _, err := ledger.Validate(vc(), args(t, `{"items": [{"kind": "asks", "who": "nobody", "text": "Send logs.", "comment": "e1", "status": "open"}], "updates": []}`))
	if err == nil || !strings.Contains(err.Error(), "/items/0/who") {
		t.Fatalf("want schema rejection of a who that is no side, got %v", err)
	}
}

// ledger_update's parameters use only the keywords both providers accepted
// live, and offer the same kinds, statuses and reasons as the public
// schema.
func TestDeclMatchesSchema(t *testing.T) {
	accepted := map[string]bool{}
	for _, k := range vertexgemini.Keywords {
		if slices.Contains(vertexanthropic.Keywords, k) {
			accepted[k] = true
		}
	}
	d := ledger.Decl()
	for _, k := range model.Keywords(d.Parameters) {
		if !accepted[k] {
			t.Errorf("ledger_update uses %q, which no live call accepted on both providers", k)
		}
	}
	raw, err := fs.ReadFile(api.Schemas, "ledger/v1alpha2/update.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var sch map[string]any
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatal(err)
	}
	enums := func(s map[string]any) map[string][]any {
		out := map[string][]any{}
		var walk func(path string, v any)
		walk = func(path string, v any) {
			m, ok := v.(map[string]any)
			if !ok {
				return
			}
			if e, ok := m["enum"].([]any); ok {
				out[path] = e
			}
			for k, sub := range m {
				walk(path+"/"+k, sub)
			}
		}
		walk("", s)
		return out
	}
	// The declaration nests what the schema defines once: compare by the
	// last two path parts, as properties/<name>.
	byName := func(m map[string][]any) map[string][]any {
		out := map[string][]any{}
		for p, e := range m {
			parts := strings.Split(p, "/")
			key := parts[len(parts)-1]
			for _, q := range parts {
				if q == "update" || q == "updates" {
					key = "update." + key
				}
			}
			out[key] = e
		}
		return out
	}
	declEnums := byName(enums(d.Parameters))
	schemaEnums := byName(enums(sch))
	if diff := cmp.Diff(schemaEnums, declEnums); diff != "" {
		t.Errorf("the declaration's enums differ from the schema's (-schema +declaration):\n%s", diff)
	}
	if !strings.Contains(ledger.Prompt(), ledger.ToolName) {
		t.Error("the prompt does not name the tool")
	}
}

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The inventory's captures are synthetic: each test writes a capture's
// mark and the identity line the fixture catalog's tool passes on
// (engine/testdata/catalogs/captures), so these tests hold the engine's
// rules of currency, not any capture's format, which the experts
// repository's identity tools and their fixtures hold.

// capture is one synthetic capture: where it is, the mark of its kind and
// its identity file.
type capture struct {
	kind, path string // kind is must-gather or sosreport
	identity   string // the identity file's content
}

// identity is a synthetic identity line, null for an empty value.
func identity(capture, targetKind, target, at, local, raw string, problems ...string) string {
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	if problems == nil {
		problems = []string{}
	}
	b, _ := json.Marshal(map[string]any{"capture": capture, "target_kind": targetKind, "target": null(target), "time": null(at),
		"local_time": null(local), "raw_time": null(raw), "problems": problems})
	return string(b) + "\n"
}

// inventoryCase is a case whose artifact directory holds the captures,
// each attachment named by its directory unless listed, with the fixture
// identity catalog installed.
func inventoryCase(t *testing.T, atts []Attachment, captures ...capture) (*Session, *snapshotArtifacts) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	for _, c := range captures {
		mark, file := "cluster-scoped-resources", "gather-identity.json"
		if c.kind == "sosreport" {
			mark, file = "sos_commands", "sos-identity.json"
		}
		if err := os.MkdirAll(filepath.Join(root, c.path, mark), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, c.path, file), []byte(c.identity), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	experts := t.TempDir()
	install(t, experts, "testdata/catalogs/captures")
	arts := &snapshotArtifacts{root: root, atts: atts}
	e, err := New(Options{Cases: nopCases{}, Artifacts: arts, Home: t.TempDir(),
		Config: []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {local: [" + experts + "]}\n")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.StartSession(context.Background(), "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, arts
}

func extracted(dirs ...string) []Attachment {
	var out []Attachment
	for _, d := range dirs {
		out = append(out, Attachment{Name: d, Dir: d, Size: 1000, State: AttachmentExtracted, Kind: "archive"})
	}
	return out
}

// byCapture is every capture the inventory lists, by its path, the first
// where two lines name one path, and each the inventory could not place by
// its attachment's name.
func byCapture(t *testing.T, inv Inventory) map[string]InventoryCapture {
	t.Helper()
	out := map[string]InventoryCapture{}
	for _, ia := range inv.Attachments {
		for _, c := range ia.Captures {
			k := c.Capture
			if k == "" {
				k = ia.Attachment.Name
			}
			if _, ok := out[k]; !ok {
				out[k] = c
			}
		}
	}
	return out
}

func readInventory(t *testing.T, s *Session) Inventory {
	t.Helper()
	inv, err := s.ReadInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// Two must-gathers of one cluster: the later is current and the earlier
// outdated, superseded by it, whatever the attachments' numbers say;
// another cluster's must-gather is current too; a document is listed with
// its kind and size and no currency.
func TestInventoryCurrentAndOutdated(t *testing.T) {
	const a, b = "00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000b"
	atts := append(extracted("0010-must-gather", "0020-must-gather", "0030-must-gather"),
		Attachment{Name: "0040-notes.pdf", Path: "0040-notes.pdf", Size: 2048, State: AttachmentPresent, Kind: "document"})
	s, _ := inventoryCase(t, atts,
		// The lower number is the later capture.
		capture{"must-gather", "0010-must-gather/must-gather.local.2", identity("0010-must-gather/must-gather.local.2", "cluster", a, "2026-09-24T12:00:00+02:00", "", "2026-09-24 10:00:00.5 +0000 UTC m=+0.40")},
		capture{"must-gather", "0020-must-gather/must-gather.local.1", identity("0020-must-gather/must-gather.local.1", "cluster", a, "2026-09-24T09:59:59Z", "", "")},
		capture{"must-gather", "0030-must-gather/mg", identity("0030-must-gather/mg", "cluster", b, "2026-09-01T00:00:00Z", "", "")},
	)
	inv := readInventory(t, s)
	got := byCapture(t, inv)
	if c := got["0010-must-gather/must-gather.local.2"]; c.Currency != CaptureCurrent || c.Target != a {
		t.Errorf("the later capture: %+v", c)
	}
	if c := got["0020-must-gather/must-gather.local.1"]; c.Currency != CaptureOutdated || c.SupersededBy != "0010-must-gather/must-gather.local.2" {
		t.Errorf("the earlier capture: %+v", c)
	}
	if c := got["0030-must-gather/mg"]; c.Currency != CaptureCurrent {
		t.Errorf("another cluster's capture: %+v", c)
	}
	if len(got) != 3 || len(inv.Attachments) != 4 {
		t.Fatalf("captures %v, %d attachments", got, len(inv.Attachments))
	}
	doc := inv.Attachments[3]
	if doc.Attachment.Name != "0040-notes.pdf" || len(doc.Captures) != 0 || len(doc.Runs) != 0 {
		t.Errorf("the document: %+v", doc)
	}
	for _, ia := range inv.Attachments[:3] {
		if len(ia.Runs) != 1 || ia.Runs[0].Tool != "fixture.captures.gather_identity@0.1.0" || ia.Runs[0].Status != "ok" || strings.Join(ia.Kinds, " ") != "must-gather" {
			t.Errorf("%s: runs %+v, kinds %v", ia.Attachment.Name, ia.Runs, ia.Kinds)
		}
	}
	txt := inv.Text()
	for _, want := range []string{
		"Artifact inventory: 4 attachments\n",
		"- 0040-notes.pdf (document, 2048 bytes, present): no capture identified, no currency\n",
		"  - must-gather 0020-must-gather/must-gather.local.1 · cluster " + a + " · captured 2026-09-24T09:59:59Z · outdated, superseded by 0010-must-gather/must-gather.local.2\n",
		"; identity in " + inv.Attachments[0].Runs[0].Evidence + "\n",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("the inventory's text lacks %q:\n%s", want, txt)
		}
	}
	// One stored item, current in the case's reading, and each identity
	// run stored as evidence.
	ids, _ := readingOf(s.c, TypeArtifactInventory)
	if len(ids) != 1 || ids[0] != inv.Evidence {
		t.Fatalf("inventory in the reading %v, want %s", ids, inv.Evidence)
	}
	it, err := s.c.ev.Get(inv.Evidence)
	if err != nil || !strings.Contains(string(it.Content), "currency: none: no capture identified") {
		t.Errorf("the item: %v\n%s", err, it.Content)
	}
	run, err := s.c.ev.Get(inv.Attachments[0].Runs[0].Evidence)
	if err != nil || run.Collection == nil || run.Collection.What.Params["dir"] != "0010-must-gather" {
		t.Errorf("the identity run's evidence: %v %+v", err, run.Collection)
	}
}

// One sosreport extracted twice is one report: the second is a duplicate
// of the first; an earlier report of the same host is outdated. Their
// times are local, with no zone, so they are ordered by local time.
func TestInventorySosreportExtractedTwice(t *testing.T) {
	const host = "host-a.example.test"
	noZone := "the date file gives no zone"
	s, _ := inventoryCase(t, extracted("0010-sosreport", "0020-sosreport", "0030-sosreport"),
		capture{"sosreport", "0010-sosreport/sosreport-host-a", identity("0010-sosreport/sosreport-host-a", "host", host, "", "2026-09-24T10:18:29", "2026-09-24 10:18:29", noZone)},
		capture{"sosreport", "0020-sosreport/sosreport-host-a", identity("0020-sosreport/sosreport-host-a", "host", host, "", "2026-09-24T10:18:29", "2026-09-24 10:18:29", noZone)},
		capture{"sosreport", "0030-sosreport/sosreport-host-a", identity("0030-sosreport/sosreport-host-a", "host", host, "", "2026-09-20T08:00:00", "2026-09-20 08:00:00", noZone)},
	)
	got := byCapture(t, readInventory(t, s))
	if c := got["0010-sosreport/sosreport-host-a"]; c.Currency != CaptureCurrent || len(c.Problems) != 1 {
		t.Errorf("the first extraction: %+v", c)
	}
	if c := got["0020-sosreport/sosreport-host-a"]; c.Currency != CaptureDuplicate || c.DuplicateOf != "0010-sosreport/sosreport-host-a" {
		t.Errorf("the second extraction: %+v", c)
	}
	if c := got["0030-sosreport/sosreport-host-a"]; c.Currency != CaptureOutdated || c.SupersededBy != "0010-sosreport/sosreport-host-a" {
		t.Errorf("the earlier report: %+v", c)
	}
}

// A capture whose identity cannot be established is uncertain, saying
// why: an unread target, an identity tool that failed, a line that is not
// a valid identity or names a capture outside the attachment, two lines
// that disagree, and times of one target that cannot be compared because
// one states its zone and another does not.
func TestInventoryUncertain(t *testing.T) {
	const cluster, host = "00000000-0000-4000-8000-00000000000c", "host-b.example.test"
	s, _ := inventoryCase(t, extracted("0010-nested", "0020-unreadable", "0030-invalid", "0040-outside", "0050-mixed", "0060-mixed", "0070-conflict"),
		capture{"must-gather", "0010-nested/mg", identity("0010-nested/mg", "cluster", cluster, "2026-09-24T10:00:00Z", "", "")},
		capture{"must-gather", "0010-nested/mg/inner", identity("0010-nested/mg/inner", "cluster", "", "", "", "", "no ClusterVersion beneath this root")},
		capture{"sosreport", "0020-unreadable/sosreport-x", "{not json\n"},
		capture{"sosreport", "0030-invalid/sosreport-y", identity("0030-invalid/sosreport-y", "pod", host, "", "2026-09-24T10:18:29", "")},
		capture{"sosreport", "0040-outside/sosreport-z", identity("0010-nested/mg", "host", host, "", "2026-09-24T10:18:29", "")},
		capture{"sosreport", "0050-mixed/sosreport-b", identity("0050-mixed/sosreport-b", "host", host, "2026-09-24T10:18:29+02:00", "2026-09-24T10:18:29", "")},
		capture{"sosreport", "0060-mixed/sosreport-b", identity("0060-mixed/sosreport-b", "host", host, "", "2026-09-25T10:18:29", "", "the date file gives no zone")},
		capture{"must-gather", "0070-conflict/mg", identity("0070-conflict/mg", "cluster", "00000000-0000-4000-8000-00000000000d", "2026-09-24T10:00:00Z", "", "") +
			identity("0070-conflict/mg", "cluster", "00000000-0000-4000-8000-00000000000e", "2026-09-24T10:00:00Z", "", "")},
	)
	got := byCapture(t, readInventory(t, s))
	for k, want := range map[string]string{
		"0010-nested/mg/inner":   "its cluster ID could not be read",
		"0020-unreadable":        "its identity could not be read: fixture.captures.sos_identity@0.1.0 failed",
		"0030-invalid":           "line 1 of fixture.captures.sos_identity@0.1.0 is not a valid identity: does not conform to compa.lowmag.cc/expert/identity/v1alpha2: /target_kind",
		"0040-outside":           `capture "0010-nested/mg" is not beneath the attachment's directory 0040-outside`,
		"0050-mixed/sosreport-b": "its capture time cannot be compared with that of 0060-mixed/sosreport-b: one states its zone and the other does not",
		"0060-mixed/sosreport-b": "its capture time cannot be compared with that of 0050-mixed/sosreport-b",
		"0070-conflict/mg":       "conflicting identities: must-gather of cluster ID 00000000-0000-4000-8000-00000000000d at 2026-09-24T10:00:00Z; must-gather of cluster ID 00000000-0000-4000-8000-00000000000e",
	} {
		if c := got[k]; c.Currency != CaptureUncertain || !strings.Contains(c.Why, want) {
			t.Errorf("%s: %+v, want uncertain: %s", k, c, want)
		}
	}
	if c := got["0010-nested/mg/inner"]; strings.Join(c.Problems, "") != "no ClusterVersion beneath this root" {
		t.Errorf("the nested root's problems: %+v", c)
	}
	if c := got["0010-nested/mg"]; c.Currency != CaptureCurrent {
		t.Errorf("the identified root beside the nested one: %+v", c)
	}
}

// The inventory is one deterministic item, made again only when the
// attachments or their identity change: an attachment its source reports
// unchanged keeps the identity read before, and one reported changed is
// identified again. A cancelled reading records nothing.
func TestInventoryRemadeOnlyOnChange(t *testing.T) {
	const cluster = "00000000-0000-4000-8000-00000000000f"
	s, arts := inventoryCase(t, extracted("0010-must-gather"),
		capture{"must-gather", "0010-must-gather/mg", identity("0010-must-gather/mg", "cluster", cluster, "2026-09-24T10:00:00Z", "", "")})
	first := readInventory(t, s)
	if again := readInventory(t, s); again.Evidence != first.Evidence {
		t.Errorf("an unchanged case made a new inventory: %s, then %s", first.Evidence, again.Evidence)
	}
	// What the identity tool would now read changes, but the attachment as
	// its source reports it does not: the runs before stand.
	file := filepath.Join(arts.root, "0010-must-gather", "mg", "gather-identity.json")
	if err := os.WriteFile(file, []byte(identity("0010-must-gather/mg", "cluster", cluster, "2026-09-25T10:00:00Z", "", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if again := readInventory(t, s); again.Evidence != first.Evidence {
		t.Errorf("an unchanged attachment was identified again")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.atts[0].Size = 2000
	if _, err := s.ReadInventory(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled reading: %v", err)
	}
	if ids, _ := readingOf(s.c, TypeArtifactInventory); len(ids) != 1 || ids[0] != first.Evidence {
		t.Errorf("a cancelled reading changed the inventory: %v", ids)
	}
	changed := readInventory(t, s)
	if changed.Evidence == first.Evidence || byCapture(t, changed)["0010-must-gather/mg"].Time != "2026-09-25T10:00:00Z" {
		t.Errorf("a changed attachment was not identified again: %+v", changed)
	}
}

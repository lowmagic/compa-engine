package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

func writeRefExpert(t *testing.T, root, dir, id, version string, refs map[string]string, summaries map[string]string, analyst string) {
	t.Helper()
	pkgDir := filepath.Join(root, dir)
	var b strings.Builder
	b.WriteString("apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata:\n")
	b.WriteString("  id: " + id + "\n  version: " + version + "\n  summary: Synthetic reference expert\n")
	b.WriteString("spec:\n  engineApi: '>=0.2.0 <0.3.0'\n  analyst: analyst.md\n")
	if len(refs) > 0 {
		b.WriteString("  references:\n")
		keys := make([]string, 0, len(refs))
		for k := range refs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			b.WriteString("    - path: " + k + "\n      summary: " + summaries[k] + "\n")
		}
	}
	files := map[string]string{"expert.yaml": b.String(), "analyst.md": analyst}
	for k, v := range refs {
		files[k] = v
	}
	for rel, content := range files {
		p := filepath.Join(pkgDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExpertListsReferences: loading an expert that declares references
// lists them with their summaries; loading one with none prints no
// references section.
func TestExpertListsReferences(t *testing.T) {
	td := t.TempDir()
	writeRefExpert(t, td, "refs", "fixture.refs", "0.1.0",
		map[string]string{
			"references/alpha.md": "# Alpha\n\nAlpha body.\n",
			"references/beta.md":  "# Beta\n\nBeta body.\n",
		},
		map[string]string{
			"references/alpha.md": "Alpha lookup table",
			"references/beta.md":  "Beta protocol limits",
		},
		"Check `references/alpha.md` and `references/beta.md` as needed.\n",
	)
	s, f := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "load_expert", map[string]any{"id": "fixture.refs"}),
			call("c2", "load_expert", map[string]any{"id": "openshift.platform"}),
		),
		modelfixture.Text("Loaded."),
	), td)
	if _, err := s.Talk(context.Background(), "Load both experts.", nil); err != nil {
		t.Fatal(err)
	}
	res := lastResults(f.Requests()[1])
	withRefs := res["c1"].Text
	wantSection := "## Its references, read with read_reference\n\n- `references/alpha.md`: Alpha lookup table\n- `references/beta.md`: Beta protocol limits\n"
	if res["c1"].IsError || !strings.Contains(withRefs, wantSection) {
		t.Errorf("expert with references:\n%s\nwant %q", withRefs, wantSection)
	}
	withoutRefs := res["c2"].Text
	if res["c2"].IsError || strings.Contains(withoutRefs, "Its references") {
		t.Errorf("expert without references printed a references section:\n%s", withoutRefs)
	}
}

// TestReadReferencePinnedAndRepeat: read_reference returns the pinned
// package's text under its heading, stores no evidence item, records no
// exposure, answers repeat reads in the same conversation cleanly, keeps
// the pinned snapshot across an installed upgrade, and reads fresh after
// compaction.
func TestReadReferencePinnedAndRepeat(t *testing.T) {
	td := t.TempDir()
	writeRefExpert(t, td, "refs", "fixture.refs", "0.1.0",
		map[string]string{
			"references/first.md":  "# First 0.1.0\n\nFirst rule 0.1.0.\n",
			"references/second.md": "# Second 0.1.0\n\nSecond rule 0.1.0.\n",
		},
		map[string]string{
			"references/first.md":  "First reference",
			"references/second.md": "Second reference",
		},
		"Read `references/first.md` and `references/second.md`.\n",
	)
	var step atomic.Int64
	var s *Session
	s, f := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		switch step.Add(1) {
		case 1:
			return modelfixture.Calling(call("c1", "load_expert", map[string]any{"id": "fixture.refs"})), nil
		case 2:
			return modelfixture.Text("Loaded."), nil
		case 3:
			return modelfixture.Calling(call("c2", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/first.md"})), nil
		case 4:
			return modelfixture.Calling(call("c3", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/first.md"})), nil
		case 5:
			// Upgrade the installed expert on disk before reading the second reference.
			writeRefExpert(t, td, "refs", "fixture.refs", "0.2.0",
				map[string]string{
					"references/first.md":  "# First 0.2.0\n\nUpgraded first rule.\n",
					"references/second.md": "# Second 0.2.0\n\nUpgraded second rule.\n",
				},
				map[string]string{
					"references/first.md":  "First reference",
					"references/second.md": "Second reference",
				},
				"Read `references/first.md` and `references/second.md`.\n",
			)
			s.e.installed = nil
			return modelfixture.Calling(call("c4", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/second.md"})), nil
		case 6:
			return modelfixture.Text("First and second read."), nil
		case 7:
			return modelfixture.Text("Handoff: fixture.refs read."), nil
		case 8:
			return modelfixture.Calling(call("c5", "load_expert", map[string]any{"id": "fixture.refs"})), nil
		case 9:
			return modelfixture.Calling(call("c6", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/first.md"})), nil
		default:
			return modelfixture.Text("Upgraded read."), nil
		}
	}, td)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Load the expert.", nil); err != nil {
		t.Fatal(err)
	}
	evBefore, err := s.c.ev.List()
	if err != nil {
		t.Fatal(err)
	}
	namesBefore, err := evidenceNames(s.c)
	if err != nil {
		t.Fatal(err)
	}
	xBefore, err := s.exposure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Read the references.", nil); err != nil {
		t.Fatal(err)
	}
	evAfter, err := s.c.ev.List()
	if err != nil {
		t.Fatal(err)
	}
	namesAfter, err := evidenceNames(s.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(evAfter) != len(evBefore) || len(namesAfter) != len(namesBefore) {
		t.Errorf("read_reference stored evidence: before (%d, %d), after (%d, %d)", len(evBefore), len(namesBefore), len(evAfter), len(namesAfter))
	}
	xAfter, err := s.exposure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k := range xAfter.shown {
		if !xBefore.shown[k] {
			t.Errorf("unexpected new exposure %q", k)
		}
	}

	if _, err := s.Compact(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Talk(ctx, "Read again after compact.", nil); err != nil {
		t.Fatal(err)
	}

	reqs := f.Requests()
	if len(reqs) != 10 {
		t.Fatalf("%d model calls, want 10", len(reqs))
	}
	r3 := lastResults(reqs[3])
	if r3["c2"].IsError || r3["c2"].Text != "# Reference fixture.refs 0.1.0 references/first.md\n\n# First 0.1.0\n\nFirst rule 0.1.0.\n" {
		t.Errorf("first read = %+v", r3["c2"])
	}
	r4 := lastResults(reqs[4])
	if r4["c3"].IsError || !strings.Contains(r4["c3"].Text, "already read in this conversation") || strings.Contains(r4["c3"].Text, "First rule 0.1.0") {
		t.Errorf("repeat read = %+v", r4["c3"])
	}
	r5 := lastResults(reqs[5])
	if r5["c4"].IsError || r5["c4"].Text != "# Reference fixture.refs 0.1.0 references/second.md\n\n# Second 0.1.0\n\nSecond rule 0.1.0.\n" {
		t.Errorf("read after upgrade = %+v, want pinned 0.1.0", r5["c4"])
	}
	r9 := lastResults(reqs[9])
	if r9["c6"].IsError || r9["c6"].Text != "# Reference fixture.refs 0.2.0 references/first.md\n\n# First 0.2.0\n\nUpgraded first rule.\n" {
		t.Errorf("read after compact = %+v, want upgraded 0.2.0", r9["c6"])
	}
}

// TestReadReferenceRepeatInTurn: a second read_reference of the same
// expert and path in a later call of the same conversation returns the
// already-read note rather than resending the file text.
func TestReadReferenceRepeatInTurn(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"})),
		modelfixture.Calling(call("c2", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"})),
		modelfixture.Text("Done."),
	))
	if _, err := s.Talk(context.Background(), "Read OVN ref twice.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d calls, want 3", len(reqs))
	}
	first := lastResults(reqs[1])["c1"]
	if first.IsError || !strings.Contains(first.Text, "OVN reference excerpt (fixture).") {
		t.Errorf("first read = %+v", first)
	}
	second := lastResults(reqs[2])["c2"]
	if second.IsError || !strings.Contains(second.Text, "already read in this conversation") || strings.Contains(second.Text, "OVN reference excerpt") {
		t.Errorf("repeat read = %+v", second)
	}
}

// TestReadReferenceRefusals: read_reference refuses an expert not loaded,
// an undeclared path, a path escaping the package, and a file past the
// hard size bound.
func TestReadReferenceRefusals(t *testing.T) {
	td := t.TempDir()
	writeRefExpert(t, td, "big", "fixture.big", "0.1.0",
		map[string]string{
			"references/small.md": "Small reference.\n",
			"references/huge.md":  strings.Repeat("x", expert.ReferenceMaxBytes+1),
		},
		map[string]string{
			"references/small.md": "Small",
			"references/huge.md":  "Oversized",
		},
		"Read `references/small.md` and `references/huge.md`.\n",
	)
	s, f := companionSession(t, script(
		modelfixture.Calling(call("l1", "load_expert", map[string]any{"id": "fixture.big"})),
		modelfixture.Calling(
			call("r1", readReferenceTool, map[string]any{"expert": "openshift.platform", "path": "references/ovn.md"}),
			call("r2", readReferenceTool, map[string]any{"expert": "fixture.big", "path": "analyst.md"}),
			call("r3", readReferenceTool, map[string]any{"expert": "fixture.big", "path": "references/missing.md"}),
			call("r4", readReferenceTool, map[string]any{"expert": "fixture.big", "path": "../outside.md"}),
			call("r5", readReferenceTool, map[string]any{"expert": "fixture.big", "path": "/etc/passwd"}),
			call("r6", readReferenceTool, map[string]any{"expert": "fixture.big", "path": `references\small.md`}),
			call("r7", readReferenceTool, map[string]any{"expert": "fixture.big", "path": "references/huge.md"}),
			call("r8", readReferenceTool, map[string]any{"expert": "fixture.big", "path": "references/small.md"}),
		),
		modelfixture.Text("Handled."),
	), td)
	if _, err := s.Talk(context.Background(), "Test refusals.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	loadRes := lastResults(reqs[1])["l1"]
	_, refSec, hasSec := strings.Cut(loadRes.Text, "## Its references, read with read_reference\n\n")
	if loadRes.IsError || !hasSec || !strings.Contains(refSec, "- `references/small.md`: Small") || strings.Contains(refSec, "- `references/huge.md`") {
		t.Errorf("load_expert with oversized reference = %+v, want small.md listed and huge.md omitted", loadRes)
	}
	res := lastResults(reqs[2])
	for id, want := range map[string]string{
		"r1": "not loaded",
		"r2": "not a declared reference",
		"r3": "not a declared reference",
		"r4": "escapes the package",
		"r5": "escapes the package",
		"r6": "escapes the package",
		"r7": "limit",
	} {
		if r := res[id]; !r.IsError || !strings.Contains(r.Text, want) {
			t.Errorf("%s = %+v, want error containing %q", id, r, want)
		}
	}
	if r := res["r8"]; r.IsError || !strings.Contains(r.Text, "Small reference.") {
		t.Errorf("valid read r8 = %+v", r)
	}
}

// TestReadReferenceConsult: a consult reads its own expert's reference from
// the pinned snapshot (even if the installed package upgrades while it
// runs) and is refused another expert's reference even when loaded in the
// main conversation.
func TestReadReferenceConsult(t *testing.T) {
	td := t.TempDir()
	writeRefExpert(t, td, "refs", "fixture.refs", "0.1.0",
		map[string]string{"references/guide.md": "Guide 0.1.0."},
		map[string]string{"references/guide.md": "Guide summary"},
		"See `references/guide.md`.\n",
	)
	var step atomic.Int64
	var s *Session
	s, f := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		switch step.Add(1) {
		case 1:
			return modelfixture.Calling(call("c1", "consult", map[string]any{"expert": "fixture.refs", "question": "Check the guide."})), nil
		case 2:
			// Upgrade on disk after the consult pinned 0.1.0, before read_reference runs.
			writeRefExpert(t, td, "refs", "fixture.refs", "0.2.0",
				map[string]string{"references/guide.md": "Guide 0.2.0."},
				map[string]string{"references/guide.md": "Guide summary"},
				"See `references/guide.md`.\n",
			)
			s.e.installed = nil
			return modelfixture.Calling(
				call("k1", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/guide.md"}),
				call("k2", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"}),
			), nil
		case 3:
			return modelfixture.Text("Consult read guide."), nil
		default:
			return modelfixture.Text("Main done."), nil
		}
	}, td)
	if _, err := s.Talk(context.Background(), "Ask fixture.refs.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d calls, want 4", len(reqs))
	}
	res := lastResults(reqs[2])
	if res["k1"].IsError || res["k1"].Text != "# Reference fixture.refs 0.1.0 references/guide.md\n\nGuide 0.1.0.\n" {
		t.Errorf("consult own reference = %+v", res["k1"])
	}
	if !res["k2"].IsError || !strings.Contains(res["k2"].Text, "not loaded") {
		t.Errorf("consult other expert reference = %+v, want refused", res["k2"])
	}
}

// TestReadReferenceAbsentWhenNoneInstalled: read_reference is omitted from
// the tool table when no installed expert declares a reference.
func TestReadReferenceAbsentWhenNoneInstalled(t *testing.T) {
	e, c := actionCaseWith(t, "", "core-0.1.1", "storage")
	for _, tl := range e.companionTools(context.Background(), c) {
		if tl.Decl().Name == readReferenceTool {
			t.Fatal("read_reference is offered when no installed expert declares a reference")
		}
	}
}

// TestReadReferenceSameBatch: calling read_reference twice for the same
// expert and path in one concurrent batch sends the text once, answers the
// duplicate cleanly, and counts the guidance tokens once in Trajectory.
func TestReadReferenceSameBatch(t *testing.T) {
	var step atomic.Int64
	s, f := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		var a *modelfixture.Answer
		if step.Add(1) == 1 {
			a = modelfixture.Calling(
				call("c1", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"}),
				call("c2", readReferenceTool, map[string]any{"expert": "openshift.networking", "path": "references/ovn.md"}),
			)
		} else {
			a = modelfixture.Text("Done.")
		}
		a.Usage = cachedUsage
		return a, nil
	})
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Read OVN ref twice at once.", nil); err != nil {
		t.Fatal(err)
	}
	res := lastResults(f.Requests()[1])
	var full, repeat int
	for _, id := range []string{"c1", "c2"} {
		r := res[id]
		if r.IsError {
			t.Fatalf("%s returned error: %+v", id, r)
		}
		switch {
		case strings.HasPrefix(r.Text, "# Reference openshift.networking ") && strings.Contains(r.Text, "OVN reference excerpt (fixture)."):
			full++
		case strings.Contains(r.Text, "already read in this conversation"):
			repeat++
		default:
			t.Errorf("%s unexpected result: %+v", id, r)
		}
	}
	if full != 1 || repeat != 1 {
		t.Errorf("batch full=%d repeat=%d, want 1 and 1", full, repeat)
	}
	traj, err := s.Trajectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(traj.Turns) != 1 || len(traj.Turns[0].References) != 2 {
		t.Fatalf("unexpected references in trajectory: %+v", traj)
	}
	var firstReads, repeatedReads int
	for _, rr := range traj.Turns[0].References {
		if rr.Refused {
			t.Errorf("unexpected refusal in batch: %+v", rr)
		}
		if rr.Repeated {
			repeatedReads++
		} else {
			firstReads++
		}
	}
	if firstReads != 1 || repeatedReads != 1 || traj.Totals.References != 1 {
		t.Errorf("first=%d repeated=%d total=%d, want 1/1/1", firstReads, repeatedReads, traj.Totals.References)
	}
	var refGuidance int
	for _, g := range traj.Turns[0].Guidance.Experts {
		if g.ID == "openshift.networking" && g.Path == "references/ovn.md" {
			refGuidance++
		}
	}
	if refGuidance != 1 {
		t.Errorf("reference GuidanceLoad count = %d, want 1 (%+v)", refGuidance, traj.Turns[0].Guidance.Experts)
	}
}

// TestReadReferenceOfferedForPinnedSnapshot: read_reference remains offered
// and readable from the conversation's pinned snapshot even when no currently
// installed expert declares any references.
func TestReadReferenceOfferedForPinnedSnapshot(t *testing.T) {
	tdRefs := t.TempDir()
	writeRefExpert(t, tdRefs, "refs", "fixture.refs", "0.1.0",
		map[string]string{"references/pinned.md": "# Pinned\n\nPinned content.\n"},
		map[string]string{"references/pinned.md": "Pinned ref"},
		"See `references/pinned.md`.\n",
	)
	tdPlain := t.TempDir()
	writeRefExpert(t, tdPlain, "plain", "fixture.plain", "0.1.0", nil, nil, "No references here.\n")
	var step atomic.Int64
	var s *Session
	s, f := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		switch step.Add(1) {
		case 1:
			return modelfixture.Calling(call("l1", "load_expert", map[string]any{"id": "fixture.refs"})), nil
		case 2:
			return modelfixture.Text("Loaded."), nil
		case 3:
			return modelfixture.Calling(call("r1", readReferenceTool, map[string]any{"expert": "fixture.refs", "path": "references/pinned.md"})), nil
		default:
			return modelfixture.Text("Read from snapshot."), nil
		}
	}, tdRefs)
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Load pinned expert.", nil); err != nil {
		t.Fatal(err)
	}
	// Replace installed roots so no installed expert declares any references.
	s.e.config.Experts.Local = []string{tdPlain}
	s.e.installed = nil
	if !slices.ContainsFunc(s.e.companionTools(ctx, s.c), func(tl companion.Tool) bool {
		return tl.Decl().Name == readReferenceTool
	}) {
		t.Fatal("read_reference omitted despite pinned expert declaring a reference")
	}
	if _, err := s.Talk(ctx, "Read pinned reference.", nil); err != nil {
		t.Fatal(err)
	}
	res := lastResults(f.Requests()[3])["r1"]
	if res.IsError || res.Text != "# Reference fixture.refs 0.1.0 references/pinned.md\n\n# Pinned\n\nPinned content.\n" {
		t.Errorf("read from pinned snapshot = %+v", res)
	}
}

type externalFixture struct{ model.Provider }

func (f externalFixture) Describe() model.Descriptor {
	d := f.Provider.Describe()
	d.EgressClass = "external_provider"
	return d
}

// TestReadReferenceRedacted: secrets inside a reference file are redacted
// before reaching the provider.
func TestReadReferenceRedacted(t *testing.T) {
	td := t.TempDir()
	writeRefExpert(t, td, "sec", "fixture.secret", "0.1.0",
		map[string]string{"references/secret.md": "Example token:\nDB_PASSWORD=hunter2hunter2\n"},
		map[string]string{"references/secret.md": "Example config"},
		"See `references/secret.md`.\n",
	)
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "load_expert", map[string]any{"id": "fixture.secret"})),
		modelfixture.Calling(call("c2", readReferenceTool, map[string]any{"expert": "fixture.secret", "path": "references/secret.md"})),
		modelfixture.Text("Done."),
	), td)
	s.e.config.Reasoning.MaxEgressClass = "external_provider"
	s.e.models["fixture"] = externalFixture{f}
	if _, err := s.Talk(context.Background(), "Read secret ref.", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	res := lastResults(reqs[2])["c2"]
	if res.IsError || strings.Contains(res.Text, "hunter2hunter2") || !strings.Contains(res.Text, "REDACTED") {
		t.Errorf("reference sent to provider = %+v, want redacted", res)
	}
	// Verify stored result kept the original text unredacted.
	msgs, err := companion.Store{DB: s.c.st.DB()}.Messages(context.Background(), mustLatest(t, s).ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	for _, m := range msgs {
		for _, r := range m.Results {
			if r.CallID == "c2" {
				stored = r.Text
			}
		}
	}
	if !strings.Contains(stored, "hunter2hunter2") {
		t.Errorf("stored reference lost original content: %q", stored)
	}
}

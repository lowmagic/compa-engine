package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// rankedQueue answers the companion's calls with the answers given so
// far, in order; past them, "done". Tests add answers once they know the
// evidence names to cite.
type rankedQueue struct {
	mu      sync.Mutex
	answers []*modelfixture.Answer
}

func (q *rankedQueue) add(answers ...*modelfixture.Answer) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.answers = append(q.answers, answers...)
}

func (q *rankedQueue) respond(context.Context, model.Request) (*modelfixture.Answer, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.answers) == 0 {
		return modelfixture.Text("done"), nil
	}
	a := q.answers[0]
	q.answers = q.answers[1:]
	return a, nil
}

func rankedSession(t *testing.T) (*Session, *modelfixture.Provider, *rankedQueue) {
	t.Helper()
	q := &rankedQueue{}
	s, f := companionSession(t, q.respond)
	return s, f, q
}

// rankedItem is a synthetic item the engineer brought in, named.
func rankedItem(t *testing.T, s *Session, location string) string {
	t.Helper()
	it, _, err := s.Ingest(strings.NewReader(strings.Repeat("a synthetic line\n", 20)), location, "")
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.NameEvidence(it.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func rankedTalk(t *testing.T, s *Session, words string) {
	t.Helper()
	if _, err := s.Talk(t.Context(), words, nil); err != nil {
		t.Fatal(err)
	}
}

func readRanked(t *testing.T, s *Session, limit int) []RankedEvidence {
	t.Helper()
	r, err := s.RankedEvidence(t.Context(), limit)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rankedNames(r []RankedEvidence) []string {
	var out []string
	for _, e := range r {
		out = append(out, e.Name)
	}
	return out
}

// storeRows counts every table's rows, to show reading wrote nothing.
func storeRows(t *testing.T, s *Session) map[string]int {
	t.Helper()
	tables := func() []string {
		rows, err := s.c.st.DB().QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			names = append(names, n)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return names
	}()
	out := map[string]int{}
	for _, n := range tables {
		var c int
		if err := s.c.st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM "`+n+`"`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

// TestRankedEvidenceWeights: in one turn, lines the engineer pointed at
// outrank a pinned answer's citation, which outranks the model's other
// citation, which outranks what a tool returned; the pinned answer's
// citation counts as the pin's, not again as an answer's. Reading the
// list calls no model and changes nothing in the store, a name no
// evidence has included.
func TestRankedEvidenceWeights(t *testing.T) {
	s, f, q := rankedSession(t)
	pointed, pinned, cited := rankedItem(t, s, "pointed.txt"), rankedItem(t, s, "pinned.txt"), rankedItem(t, s, "cited.txt")
	q.add(
		&modelfixture.Answer{Pieces: []string{"Looking at [" + cited + ":5] and [e999:1]."},
			Calls: []model.ToolCall{call("c1", "grep", map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"})}},
		modelfixture.Text("Those lines are ["+pinned+":1-2]."),
	)
	rankedTalk(t, s, pointsAt+pointed+":3-4\n\nWhat do these lines mean?")
	if _, err := s.Pin(t.Context()); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	returned := ExcerptEvidence(lastResults(reqs[1])["c1"].Text)
	if len(returned) != 1 {
		t.Fatalf("grep's result names %v", returned)
	}
	before := storeRows(t, s)
	got := readRanked(t, s, 0)
	ids := map[string]string{}
	for _, n := range []string{pointed, pinned, cited, returned[0]} {
		id, err := evidenceByName(s.c, n)
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = id
	}
	want := []RankedEvidence{
		{Name: pointed, EvidenceID: ids[pointed], Score: rankPointed, Ranges: []LineRange{{3, 4}}, Why: []string{"pointed at by the engineer"}},
		{Name: pinned, EvidenceID: ids[pinned], Score: rankPinned, Ranges: []LineRange{{1, 2}}, Why: []string{"cited in i1"}},
		{Name: cited, EvidenceID: ids[cited], Score: rankCited, Ranges: []LineRange{{5, 5}}, Why: []string{"cited by the model"}},
		{Name: returned[0], EvidenceID: ids[returned[0]], Score: rankReturned, Why: []string{"returned by grep"}},
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("ranked (-want +got):\n%s", d)
	}
	if d := cmp.Diff(before, storeRows(t, s)); d != "" {
		t.Errorf("ranking changed the store (-before +after):\n%s", d)
	}
	if n := len(f.Requests()); n != 2 {
		t.Errorf("ranking made %d model calls", n-2)
	}
	if top := readRanked(t, s, 2); !cmp.Equal(rankedNames(top), []string{pointed, pinned}) {
		t.Errorf("limit 2 gave %v", rankedNames(top))
	}
	render := RenderRanked(got)
	for _, line := range []string{
		pointed + " · 5.00 · lines 3-4 · pointed at by the engineer · this turn\n",
		returned[0] + " · 1.00 · returned by grep · this turn\n",
	} {
		if !strings.Contains(render, line) {
			t.Errorf("the listing lacks %q:\n%s", line, render)
		}
	}
	if n := strings.Count(render, "\n"); n != len(got) {
		t.Errorf("the listing has %d lines for %d items:\n%s", n, len(got), render)
	}
}

// TestRankedEvidenceDecays: an old citation ranks below a recent one of
// the same kind, and items of equal score and recency keep the order of
// their names, the same on every reading.
func TestRankedEvidenceDecays(t *testing.T) {
	s, _, q := rankedSession(t)
	old, recent, also := rankedItem(t, s, "old.txt"), rankedItem(t, s, "recent.txt"), rankedItem(t, s, "also.txt")
	q.add(
		modelfixture.Text("Early: ["+old+":2]."),
		modelfixture.Text("Nothing cited."),
		modelfixture.Text("Now: ["+also+"] and ["+recent+":7]."),
	)
	rankedTalk(t, s, "First question.")
	rankedTalk(t, s, "Second question.")
	rankedTalk(t, s, "Third question.")
	got := readRanked(t, s, 0)
	if d := cmp.Diff([]string{recent, also, old}, rankedNames(got)); d != "" {
		t.Fatalf("order (-want +got):\n%s", d)
	}
	if got[0].Score != got[1].Score || got[0].LastTurn != 0 || got[2].LastTurn != 2 || got[2].Score >= got[0].Score {
		t.Errorf("scores and turns: %+v", got)
	}
	if !cmp.Equal(got[0].Why, []string{"cited in a3"}) || !cmp.Equal(got[2].Why, []string{"cited in a1"}) {
		t.Errorf("why: %v, %v", got[0].Why, got[2].Why)
	}
	if !strings.Contains(RenderRanked(got), old+" · 2.17 · lines 2 · cited in a1 · 2 turns ago\n") {
		t.Errorf("the listing:\n%s", RenderRanked(got))
	}
	for range 3 {
		if d := cmp.Diff(got, readRanked(t, s, 0)); d != "" {
			t.Fatalf("a second reading differs (-first +second):\n%s", d)
		}
	}
}

// TestRankedEvidenceLeavesOutCaseBlock: the case's details, overview and
// comments, named once the case block shows them, are left out however
// they were used.
func TestRankedEvidenceLeavesOutCaseBlock(t *testing.T) {
	s, _, q := rankedSession(t)
	own := rankedItem(t, s, "own.txt")
	rankedTalk(t, s, "Hello.")
	evs, err := s.Evidences()
	if err != nil {
		t.Fatal(err)
	}
	var caseNames []string
	for _, e := range evs {
		if caseBlockTypes[e.Item.Ref.Type] {
			caseNames = append(caseNames, e.Name)
		}
	}
	if len(caseNames) < 3 {
		t.Fatalf("case items %v, want details, overview and comments", caseNames)
	}
	var cites []string
	for _, n := range caseNames {
		cites = append(cites, "["+n+":1]")
	}
	q.add(modelfixture.Text("The case says " + strings.Join(cites, ", ") + "; see [" + own + ":1]."))
	rankedTalk(t, s, pointsAt+strings.Join(caseNames, ", ")+"\n\nWhat do these say?")
	if _, err := s.Pin(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := rankedNames(readRanked(t, s, 0)); !cmp.Equal(got, []string{own}) {
		t.Errorf("ranked %v, want only %s", got, own)
	}
}

// TestRankedEvidenceMergesRanges: an item's ranges from every use are
// sorted, overlapping and adjacent ones merged; a whole item cited has
// none.
func TestRankedEvidenceMergesRanges(t *testing.T) {
	s, _, q := rankedSession(t)
	lines, whole := rankedItem(t, s, "lines.txt"), rankedItem(t, s, "whole.txt")
	q.add(modelfixture.Text("See [" + lines + ":4-6, 10] and [" + lines + ":1], [" + lines + ":7] and [" + whole + "]."))
	rankedTalk(t, s, pointsAt+lines+":3-4, "+lines+":12\n\nAnd these?")
	got := readRanked(t, s, 0)
	if len(got) != 2 {
		t.Fatalf("ranked %+v", got)
	}
	if d := cmp.Diff([]LineRange{{1, 1}, {3, 7}, {10, 10}, {12, 12}}, got[0].Ranges); d != "" {
		t.Errorf("%s's ranges (-want +got):\n%s", lines, d)
	}
	if got[1].Name != whole || got[1].Ranges != nil {
		t.Errorf("the whole item: %+v", got[1])
	}
	if !cmp.Equal(got[0].Why, []string{"cited in a1", "pointed at by the engineer"}) {
		t.Errorf("why %v", got[0].Why)
	}
}

// TestRankedEvidenceLineage: citations in a compacted conversation count,
// a consult's do not.
func TestRankedEvidenceLineage(t *testing.T) {
	s, f, q := rankedSession(t)
	early, private, late := rankedItem(t, s, "early.txt"), rankedItem(t, s, "private.txt"), rankedItem(t, s, "late.txt")
	q.add(
		modelfixture.Text("Early finding ["+early+":2]."),
		modelfixture.Text("The handoff."),
		modelfixture.Calling(call("c1", "consult", map[string]any{"expert": "openshift.platform", "question": "What about this?"})),
		modelfixture.Text("The consult's answer ["+private+":1]."),
		modelfixture.Text("The main answer ["+late+":3]."),
	)
	rankedTalk(t, s, "First question.")
	if _, err := s.Compact(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	rankedTalk(t, s, "Ask the platform expert.")
	if n := len(f.Requests()); n != 5 {
		t.Fatalf("%d model calls, want 5", n)
	}
	got := readRanked(t, s, 0)
	if d := cmp.Diff([]string{late, early}, rankedNames(got)); d != "" {
		t.Errorf("ranked (-want +got):\n%s", d)
	}
}

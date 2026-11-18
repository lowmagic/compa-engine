package engine

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The span verdicts' tests run on the synthetic replay's evidence
// (testdata/replay/epistemic), the answer synthetic.

// spanAnswer states four spans no evidence holds as written: the OSD's
// stop with a different epoch, its statement citing nothing; kubelet's
// pid with a restart, cited to the later of the kubelet log's two greps,
// and again citing nothing; and words no evidence holds at all.
const spanAnswer = "The OSD logged `osd.3 1206 prepare_to_stop starting shutdown`. " +
	"The kubelet logged `kubelet[2211] restarted` [{later}:1]. It logged `kubelet[2211] restarted twice`. " +
	"Nothing logged `zqxj vbnm`."

// verdictSession is a session whose first turn greps the replay's kubelet
// log twice and its OSD log, then answers with answer, the evidence names
// filled in; every later call answers "done". names are the names the
// greps were given, by role, and the kubelet greps' as earlier and later.
func verdictSession(t *testing.T, answer string) (replay, *modelfixture.Provider) {
	t.Helper()
	var mu sync.Mutex
	names := map[string]string{}
	roles := map[string]string{"c1": "evict", "c2": "csi", "c3": "osd"}
	var n atomic.Int64
	respond := func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Calling(
				call("c1", "grep", map[string]any{"pattern": "Eviction manager", "path": "nodes/worker-1/kubelet.log"}),
				call("c2", "grep", map[string]any{"pattern": "connection refused", "path": "nodes/worker-1/kubelet.log"}),
				call("c3", "grep", map[string]any{"pattern": "Terminated|shutdown", "path": "nodes/worker-1/var/log/ceph/ceph-osd.3.log"}),
			), nil
		case 2:
			mu.Lock()
			defer mu.Unlock()
			for id, res := range lastResults(r) {
				if ev := ExcerptEvidence(res.Text); len(ev) == 1 {
					names[roles[id]] = ev[0]
				}
			}
			if nameNumber(names["evict"]) < nameNumber(names["csi"]) {
				names["earlier"], names["later"] = names["evict"], names["csi"]
			} else {
				names["earlier"], names["later"] = names["csi"], names["evict"]
			}
			return modelfixture.Text(replay{names: names}.fill(answer)), nil
		}
		return modelfixture.Text("done"), nil
	}
	s, f := companionSession(t, respond)
	td, err := filepath.Abs("../testdata/replay/epistemic")
	if err != nil {
		t.Fatal(err)
	}
	s.c.root = td
	if _, err := s.Talk(context.Background(), "What happened on worker-1?", nil); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		if names[role] == "" {
			t.Fatalf("the greps stored %v; want one item each", names)
		}
	}
	return replay{s: s, names: names}, f
}

// lineOf is the number, from 1, of the first line of the item named
// holding want, and that line.
func lineOf(t *testing.T, s *Session, name, want string) EvidenceLine {
	t.Helper()
	it, err := s.EvidenceNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range strings.Split(string(it.Content), "\n") {
		if strings.Contains(l, want) {
			return EvidenceLine{Name: name, Line: i + 1, Text: l}
		}
	}
	t.Fatalf("%s holds no %q", name, want)
	return EvidenceLine{}
}

// spanFlags are a text's span flags by span.
func spanFlags(t *testing.T, s *Session, text string) map[string]AnswerFlag {
	t.Helper()
	flags, err := s.AnswerFlags(context.Background(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]AnswerFlag{}
	for _, f := range flags[0] {
		if f.Span != "" {
			out[f.Span] = f
		}
	}
	return out
}

// TestSpanCheckSkipsConditions: a value in a statement opening as a
// condition is what the condition supposes, not a claim of what was
// seen, so it is not looked for; the same kind of value stated as seen,
// or in a statement opening with a time — when, once — is.
func TestSpanCheckSkipsConditions(t *testing.T) {
	s, _ := companionSession(t, script(modelfixture.Text("done")))
	if _, err := s.Talk(context.Background(), "Where does it stand?", nil); err != nil {
		t.Fatal(err)
	}
	text := "- If the customer reports the disks are `persistent` VMDKs, check the datastore permissions.\n" +
		"- Unless `multi-writer` sharing is set, the disks are not shared.\n" +
		"- **Should** `scsi1:0.sharing` be set, the bus is shared.\n" +
		"- The disk mode is `independent-nonpersistent`.\n" +
		"- When the node rebooted, the kubelet logged `zz-marker-1`.\n"
	flags := spanFlags(t, s, text)
	var got []string
	for sp := range flags {
		got = append(got, sp)
	}
	slices.Sort(got)
	if want := []string{"independent-nonpersistent", "zz-marker-1"}; !slices.Equal(got, want) {
		t.Errorf("spans flagged %q; want %q", got, want)
	}
}

// TestSpanClosestLine: [E4] a span found in no evidence carries its
// statement and the line of the evidence read sharing the most of its
// words: a value the model changed is checked against the line it was
// changed from, not an earlier line sharing fewer words; on a tie, a line
// of an item the statement cites, else the earliest item's first; a span
// sharing no word with any line has none, and is unsupported.
func TestSpanClosestLine(t *testing.T) {
	r, _ := verdictSession(t, spanAnswer)
	flags := spanFlags(t, r.s, r.fill(spanAnswer))
	if len(flags) != 4 {
		t.Fatalf("span flags %+v; want four", flags)
	}
	stop := flags["osd.3 1206 prepare_to_stop starting shutdown"]
	if want := lineOf(t, r.s, r.names["osd"], "prepare_to_stop starting shutdown"); stop.Closest == nil || *stop.Closest != want {
		t.Errorf("the changed value's closest line %+v; want %+v", stop.Closest, want)
	}
	if stop.Statement != "The OSD logged `osd.3 1206 prepare_to_stop starting shutdown`." || stop.Unsupported() || stop.Arithmetic {
		t.Errorf("the changed value's flag %+v", stop)
	}
	cited := flags["kubelet[2211] restarted"]
	if want := lineOf(t, r.s, r.names["later"], "kubelet[2211]"); cited.Closest == nil || *cited.Closest != want {
		t.Errorf("the cited statement's closest line %+v; want the cited item's %+v", cited.Closest, want)
	}
	uncited := flags["kubelet[2211] restarted twice"]
	if want := lineOf(t, r.s, r.names["earlier"], "kubelet[2211]"); uncited.Closest == nil || *uncited.Closest != want {
		t.Errorf("the uncited statement's closest line %+v; want the earliest item's %+v", uncited.Closest, want)
	}
	if none := flags["zqxj vbnm"]; none.Closest != nil || !none.Unsupported() || none.Arithmetic {
		t.Errorf("a span sharing no word: %+v", none)
	}
}

// rangeOf is lines from to to of the item named, as a verdict keeps them.
func rangeOf(t *testing.T, s *Session, name string, from, to int) EvidenceRange {
	t.Helper()
	it, err := s.EvidenceNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(it.Content), "\n")
	if to > len(lines) {
		t.Fatalf("%s has %d lines; want %d", name, len(lines), to)
	}
	return EvidenceRange{Name: name, From: from, To: to, Lines: lines[from-1 : to]}
}

// TestSpanVerdictIncorrectReadByModel: [E4] the engineer's verdict that a
// span is incorrect, against the lines they marked, is kept, appended once
// to the main conversation after its last message, every earlier message
// byte for byte, naming the answer, the span and the lines, quoted as
// stored; the main model's next call is given it, and history reads it
// beneath the answer. It is no answer: the answers as shown, a reload's
// last exchange, the ranked evidence, /pin and the next answer's number
// are as they were. The same span is not judged twice.
func TestSpanVerdictIncorrectReadByModel(t *testing.T) {
	r, f := verdictSession(t, spanAnswer)
	ctx := context.Background()
	answer := r.fill(spanAnswer)
	flag := spanFlags(t, r.s, answer)["osd.3 1206 prepare_to_stop starting shutdown"]
	before, was := mainMessages(t, r.s), stateOf(t, r.s)
	marked := rangeOf(t, r.s, r.names["osd"], 1, 2)
	v, err := r.s.JudgeSpan(ctx, "a1", flag, VerdictIncorrect, []EvidenceRange{{Name: marked.Name, From: 1, To: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.JudgeSpan(ctx, "a1", flag, VerdictSupported, nil); err == nil {
		t.Error("a span was judged twice")
	}
	want := "The engineer checked a1's `osd.3 1206 prepare_to_stop starting shutdown` against " + marked.Place() + ": it is incorrect.\n\n" +
		marked.Place() + ":\n```\n1  " + marked.Lines[0] + "\n2  " + marked.Lines[1] + "\n```"
	after := mainMessages(t, r.s)
	if len(after) != len(before)+1 {
		t.Fatalf("%d messages after the verdict, %d before; want one more", len(after), len(before))
	}
	if d := cmp.Diff(before, after[:len(before)]); d != "" {
		t.Errorf("the earlier messages changed (-before +after):\n%s", d)
	}
	if last := after[len(after)-1]; last.Text != want || last.Kind != companion.KindVerdict || last.Role != model.RoleUser {
		t.Errorf("the verdict kept as %s %s %q; want %q", last.Kind, last.Role, last.Text, want)
	}
	if d := cmp.Diff(was, stateOf(t, r.s), cmp.AllowUnexported(reviewState{})); d != "" {
		t.Errorf("the verdict changed the answers, the last exchange, the ranked uses or the items (-before +after):\n%s", d)
	}
	if it, err := r.s.Pin(ctx); err != nil || it.Text != answer {
		t.Errorf("/pin kept %q, %v", it.Text, err)
	}
	if _, err := r.s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if next := reqs[len(reqs)-1].Messages; !slices.ContainsFunc(next, func(m model.Message) bool { return m.Text == want && m.Role == model.RoleUser }) {
		t.Error("the main model's next call is not given the verdict")
	}
	history, err := r.s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	views := AnswerViews(history)
	if len(views) != 2 || views[1].Label != "a2" {
		t.Errorf("the answers after the verdict and another turn: %+v", views)
	}
	got := history[views[0].Original].Verdicts
	if len(got) != 1 || cmp.Diff(v, got[0]) != "" || got[0].Kind != VerdictIncorrect || got[0].Answer != "a1" ||
		cmp.Diff([]EvidenceRange{marked}, got[0].Evidence) != "" || got[0].Statement != "The OSD logged `osd.3 1206 prepare_to_stop starting shutdown`." {
		t.Errorf("the verdict as history reads it: %+v, want %+v", got, v)
	}
}

// TestSpanVerdictsToldTheModel: [E4] of the four verdicts, only
// unsupported and incorrect reach the main model's next request, each
// worded as the engineer's, an incorrect one with the closest line where
// none is marked; supported and inferred are kept and never sent, inferred
// resting on the lines marked; every verdict reads back with its kind and
// its lines.
func TestSpanVerdictsToldTheModel(t *testing.T) {
	r, f := verdictSession(t, spanAnswer)
	ctx := context.Background()
	flags := spanFlags(t, r.s, r.fill(spanAnswer))
	stop, restarted, twice, none := flags["osd.3 1206 prepare_to_stop starting shutdown"], flags["kubelet[2211] restarted"],
		flags["kubelet[2211] restarted twice"], flags["zqxj vbnm"]
	inferredFrom := rangeOf(t, r.s, r.names["later"], 1, 2)
	for _, j := range []struct {
		flag   AnswerFlag
		kind   VerdictKind
		marked []EvidenceRange
	}{
		{restarted, VerdictSupported, nil},
		{twice, VerdictInferred, []EvidenceRange{{Name: inferredFrom.Name, From: 1, To: 2}}},
		{none, VerdictUnsupported, nil},
		{stop, VerdictIncorrect, nil},
	} {
		if _, err := r.s.JudgeSpan(ctx, "a1", j.flag, j.kind, j.marked); err != nil {
			t.Fatalf("%s %s: %v", j.kind, j.flag.Span, err)
		}
	}
	if _, err := r.s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	var told []string
	for _, m := range reqs[len(reqs)-1].Messages {
		if strings.HasPrefix(m.Text, "The engineer ") {
			told = append(told, m.Text)
		}
	}
	want := []string{
		"The engineer found no evidence for a1's `zqxj vbnm`: treat it as unsupported.",
		"The engineer checked a1's `osd.3 1206 prepare_to_stop starting shutdown` against " + stop.Closest.Place() + ": it is incorrect.\n\n" +
			stop.Closest.Place() + ":\n```\n" + strconv.Itoa(stop.Closest.Line) + "  " + stop.Closest.Text + "\n```",
	}
	if d := cmp.Diff(want, told); d != "" {
		t.Errorf("what the model's next call is told (-want +got):\n%s", d)
	}
	history, err := r.s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[VerdictKind][]EvidenceRange{}
	for _, v := range history[AnswerViews(history)[0].Original].Verdicts {
		kept[v.Kind] = v.Evidence
	}
	closest := func(l *EvidenceLine) []EvidenceRange {
		return []EvidenceRange{{Name: l.Name, From: l.Line, To: l.Line, Lines: []string{l.Text}}}
	}
	wantKept := map[VerdictKind][]EvidenceRange{
		VerdictSupported: closest(restarted.Closest), VerdictInferred: {inferredFrom}, VerdictUnsupported: nil, VerdictIncorrect: closest(stop.Closest),
	}
	if d := cmp.Diff(wantKept, kept); d != "" {
		t.Errorf("the verdicts as history reads them (-want +got):\n%s", d)
	}
}

// TestSpanVerdictMarkedLines: [E4] the lines a verdict rests on are read
// again from the stored evidence: lines past an item's end, of an item the
// conversation did not see, or a range ending before it begins are
// refused, as is inferred with no lines marked, and nothing is kept.
func TestSpanVerdictMarkedLines(t *testing.T) {
	r, _ := verdictSession(t, spanAnswer)
	ctx := context.Background()
	flag := spanFlags(t, r.s, r.fill(spanAnswer))["zqxj vbnm"]
	before := mainMessages(t, r.s)
	for what, j := range map[string]struct {
		kind   VerdictKind
		marked []EvidenceRange
	}{
		"inferred with nothing marked": {VerdictInferred, nil},
		"lines past the item's end":    {VerdictIncorrect, []EvidenceRange{{Name: r.names["osd"], From: 1, To: 400}}},
		"an item never seen":           {VerdictIncorrect, []EvidenceRange{{Name: "e999", From: 1, To: 1}}},
		"a range ending before it":     {VerdictSupported, []EvidenceRange{{Name: r.names["osd"], From: 2, To: 1}}},
	} {
		if v, err := r.s.JudgeSpan(ctx, "a1", flag, j.kind, j.marked); err == nil {
			t.Errorf("%s was kept: %+v", what, v)
		}
	}
	if _, err := r.s.JudgeSpan(ctx, "a1", flag, VerdictInferred, nil); !errors.Is(err, ErrInferredUnmarked) {
		t.Errorf("inferred with nothing marked: %v", err)
	}
	if d := cmp.Diff(before, mainMessages(t, r.s)); d != "" {
		t.Errorf("a refused verdict changed the conversation (-before +after):\n%s", d)
	}
}

// TestSpanVerdictCarriedByReload: [E4] a verdict the model is told, not
// yet read by any model call, is carried into the conversation a
// suggested reload begins, so the next call reads it; one the model has
// read is not carried again.
func TestSpanVerdictCarriedByReload(t *testing.T) {
	r, f := verdictSession(t, spanAnswer)
	ctx := context.Background()
	flags := spanFlags(t, r.s, r.fill(spanAnswer))
	v, err := r.s.JudgeSpan(ctx, "a1", flags["osd.3 1206 prepare_to_stop starting shutdown"], VerdictIncorrect, nil)
	if err != nil {
		t.Fatal(err)
	}
	was := mainMessages(t, r.s)
	if _, err := r.s.Reload(ctx, ReloadSuggested, nil); err != nil {
		t.Fatal(err)
	}
	if cmp.Diff(was, mainMessages(t, r.s)) == "" {
		t.Fatal("the reload began no conversation")
	}
	if _, err := r.s.Talk(ctx, "And now?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if n := countText(reqs[len(reqs)-1].Messages, verdictText(v)); n != 1 {
		t.Errorf("the call after the reload holds the verdict %d times; want once", n)
	}
	if _, err := r.s.Reload(ctx, ReloadSuggested, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Talk(ctx, "And then?", nil); err != nil {
		t.Fatal(err)
	}
	reqs = f.Requests()
	if n := countText(reqs[len(reqs)-1].Messages, verdictText(v)); n != 0 {
		t.Errorf("a verdict the model had read was carried again, %d times", n)
	}
}

// countText counts the messages whose text is text.
func countText(msgs []model.Message, text string) int {
	n := 0
	for _, m := range msgs {
		if m.Text == text {
			n++
		}
	}
	return n
}

// TestSpanVerdictLineMustBeWhole: [E4] a verdict is kept only against the
// closest line as stored, whole: a line cut short, or not as stored, as a
// stale or forged flag might carry it, is refused, and nothing is kept.
func TestSpanVerdictLineMustBeWhole(t *testing.T) {
	r, _ := verdictSession(t, spanAnswer)
	ctx := context.Background()
	flag := spanFlags(t, r.s, r.fill(spanAnswer))["osd.3 1206 prepare_to_stop starting shutdown"]
	cut, forged := flag, flag
	cut.Closest = &EvidenceLine{Name: flag.Closest.Name, Line: flag.Closest.Line, Text: flag.Closest.Text[:len(flag.Closest.Text)/2]}
	forged.Closest = &EvidenceLine{Name: flag.Closest.Name, Line: flag.Closest.Line, Text: "osd.3 1206 prepare_to_stop starting shutdown"}
	before := mainMessages(t, r.s)
	for _, f := range []AnswerFlag{cut, forged} {
		if v, err := r.s.JudgeSpan(ctx, "a1", f, VerdictIncorrect, nil); err == nil {
			t.Errorf("a verdict against a line not as stored was kept: %+v", v)
		}
	}
	if d := cmp.Diff(before, mainMessages(t, r.s)); d != "" {
		t.Errorf("a refused verdict changed the conversation (-before +after):\n%s", d)
	}
	v, err := r.s.JudgeSpan(ctx, "a1", flag, VerdictSupported, nil)
	if want := []EvidenceRange{{Name: flag.Closest.Name, From: flag.Closest.Line, To: flag.Closest.Line, Lines: []string{flag.Closest.Text}}}; err != nil || cmp.Diff(want, v.Evidence) != "" {
		t.Errorf("the verdict against the line whole: %+v, %v", v, err)
	}
}

// TestSpanArithmetic: [E4] a span that is only arithmetic, an operator
// between two operands among it, is evaluated, not searched: one that
// holds is not flagged, one that does not is flagged incorrect with no
// closest line, and anything outside the grammar — a comparison of
// versions, of dates written with leading zeros, of bare numbers — is
// searched as any span.
func TestSpanArithmetic(t *testing.T) {
	for span, want := range map[string]struct{ holds, ok bool }{
		"16 GiB / 4 = 4 GiB":            {true, true},
		"16 GiB / 4 = 5 GiB":            {false, true},
		"2 + 2 = 5":                     {false, true},
		"(1 + 2) * 3 = 9":               {true, true},
		"10 / 3 = 3.33":                 {true, true},
		"10 / 3 = 3.4":                  {false, true},
		"1,048,576 kB / 1024 = 1024 kB": {true, true},
		"1 + 1 <= 2 < 3":                {true, true},
		"-2 * -2 >= 4":                  {true, true},
		"2 * 2 != 4":                    {false, true},
		"1 - 1 = 0":                     {true, true},
		"3 < 2":                         {false, false},
		"4 != 4":                        {false, false},
		"-1 < 0":                        {false, false},
		"4.9 < 4.14":                    {false, false},
		"2026-11-06 < 2026-11-08":       {false, false},
		"16 GiB / 4 = 4 GB":             {false, false},
		"2 + 2":                         {false, false},
		"4096":                          {false, false},
		"1 / 0 = 1":                     {false, false},
		"MemTotal = 16 GiB":             {false, false},
		"2026-11-08":                    {false, false},
		"a = b":                         {false, false},
		"x == 1":                        {false, false},
	} {
		if holds, ok := arithmetic(span); holds != want.holds || ok != want.ok {
			t.Errorf("arithmetic(%q) = %v, %v; want %v, %v", span, holds, ok, want.holds, want.ok)
		}
	}
	const answer = "The node has `16 GiB / 4 = 4 GiB` per pod. It has `16 GiB / 4 = 5 GiB` per pod. Nothing logged `zqxj vbnm`."
	r, _ := verdictSession(t, answer)
	flags := spanFlags(t, r.s, answer)
	if _, ok := flags["16 GiB / 4 = 4 GiB"]; ok {
		t.Errorf("arithmetic that holds was flagged: %+v", flags)
	}
	want := AnswerFlag{What: "`16 GiB / 4 = 5 GiB`", Why: "incorrect: the arithmetic does not hold", Span: "16 GiB / 4 = 5 GiB",
		Statement: "It has `16 GiB / 4 = 5 GiB` per pod.", Arithmetic: true}
	if d := cmp.Diff(want, flags["16 GiB / 4 = 5 GiB"]); d != "" {
		t.Errorf("arithmetic that does not hold (-want +got):\n%s", d)
	}
	if f := flags["zqxj vbnm"]; !f.Unsupported() || f.Arithmetic {
		t.Errorf("a span that is no arithmetic is not searched: %+v", f)
	}
}

// TestSpanClosestNeedsHalfItsWords: [E4] a line is close to a span only
// where it shares at least half of the span's distinct words, rounded up:
// one word of five, such as a node's name, is no closeness, three of five
// are, and `MemTotal: 1,048,576 kB` is close to "MemTotal: 2097152 kB",
// two of three.
func TestSpanClosestNeedsHalfItsWords(t *testing.T) {
	data := []byte("# Synthetic lines.\nMemTotal:       2097152 kB\nalpha beta gamma delta epsilon\n")
	spans := []string{"MemTotal: 1,048,576 kB", "alpha zeta eta theta iota", "alpha beta gamma zeta eta"}
	unseen := map[string]bool{}
	for _, sp := range spans {
		unseen[sp] = true
	}
	nr := newNearest(spans)
	nr.read("e1", data, bytes.Split(bytes.ToLower(data), []byte("\n")), unseen)
	want := [][]EvidenceLine{
		{{Name: "e1", Line: 2, Text: "MemTotal:       2097152 kB"}},
		nil,
		{{Name: "e1", Line: 3, Text: "alpha beta gamma delta epsilon"}},
	}
	if d := cmp.Diff(want, nr.best); d != "" {
		t.Errorf("the closest lines (-want +got):\n%s", d)
	}
}

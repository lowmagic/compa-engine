package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// The replay's answers, synthetic, written with the shapes of failure the
// epistemic checks are for; {find} and the like are the evidence names
// the replay's tool calls were given. a1 qualifies what it infers; a2
// drops the qualifiers, cites a listing for where daemons ran, states an
// alert rule no evidence holds, and miswrites two citations, beside three
// statements the evidence supports.
const (
	replayA1 = "- The kubelet's eviction manager ranked pods for eviction under ephemeral-storage pressure [{evict}:3].\n" +
		"- The CSI socket refused connections, likely because the plugin container restarted [{csi}:1].\n" +
		"- Either the OSD shutdown or the disk filling may explain the alerts; the two are hours apart [{osd}:1-2, {enospc}:1].\n"
	replayA2 = "The MONs a, b and c ran on worker-1 [{find}:3-5]. The kubelet evicted csi-rbdplugin-k2x9p [{evict}:3]. " +
		"The CSI socket was removed and the containers crashed [{csi}:1]. The alert `ceph_osd_up == 0` fired with a 300 s threshold. " +
		"The OSD shutdown at 08:16 led to the disk filling at 11:55 [{osd}:1-2, {enospc}:1]. /var, the root filesystem, reached 100% utilization [{enospc}:1].\n\n" +
		"- The namespace's pods place the MONs on worker-2, worker-3 and worker-4 [{pods}:2-11].\n" +
		"- The kubelet's eviction manager ranked `openshift-storage/csi-rbdplugin-k2x9p` for eviction [{evict}:3].\n" +
		"- See {osd}:2 for the shutdown, and ({csi}) for the socket.\n\n" +
		"```\noc get pods -n openshift-storage -o wide # e9:4 (e9) `not a span`\n```\n"
	replayDraft = "Hello, the storage on worker-1 ran out of space after an OSD stopped. We are checking why."
)

// replay is a session over the synthetic replay
// (testdata/replay/epistemic): one turn reading the worker's Ceph
// log directory, the namespace's pods and four searches, answered with
// replayA1; a second answered with replayA2; and a draft for the customer,
// replayDraft, which cites nothing. names are the evidence names its
// calls were given, by role.
type replay struct {
	s     *Session
	names map[string]string
}

func replaySession(t *testing.T) replay {
	t.Helper()
	var mu sync.Mutex
	names := map[string]string{}
	fill := func(s string) string {
		mu.Lock()
		defer mu.Unlock()
		for role, name := range names {
			s = strings.ReplaceAll(s, "{"+role+"}", name)
		}
		return s
	}
	roles := map[string]string{"c1": "find", "c2": "pods", "c3": "evict", "c4": "csi", "c5": "osd", "c6": "enospc"}
	var n atomic.Int64
	respond := func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		switch n.Add(1) {
		case 1:
			return modelfixture.Calling(
				call("c1", "find", map[string]any{"path": "nodes/worker-1/var/log/ceph"}),
				call("c2", "read", map[string]any{"path": "namespaces/openshift-storage/pods.yaml"}),
				call("c3", "grep", map[string]any{"pattern": "Eviction manager", "path": "nodes/worker-1/kubelet.log"}),
				call("c4", "grep", map[string]any{"pattern": "connection refused", "path": "nodes/worker-1/kubelet.log"}),
				call("c5", "grep", map[string]any{"pattern": "Terminated|shutdown", "path": "nodes/worker-1/var/log/ceph/ceph-osd.3.log"}),
				call("c6", "grep", map[string]any{"pattern": "no space left", "path": "nodes/worker-1/journal.log"}),
			), nil
		case 2:
			mu.Lock()
			for id, res := range lastResults(r) {
				if ev := ExcerptEvidence(res.Text); len(ev) == 1 {
					names[roles[id]] = ev[0]
				}
			}
			mu.Unlock()
			return modelfixture.Text(fill(replayA1)), nil
		case 3:
			return modelfixture.Text(fill(replayA2)), nil
		case 4:
			return modelfixture.Text(replayDraft), nil
		}
		return modelfixture.Text("done"), nil
	}
	s, _ := companionSession(t, respond)
	td, err := filepath.Abs("../testdata/replay/epistemic")
	if err != nil {
		t.Fatal(err)
	}
	s.c.root = td
	s.e.config.Comments.Intentions = []config.Intention{{ID: "customer-update", Label: "Customer update", Audience: "customer", Purpose: "Tell the customer what was found."}}
	ctx := context.Background()
	if _, err := s.Talk(ctx, "What happened on worker-1?", nil); err != nil {
		t.Fatal(err)
	}
	if len(names) != len(roles) {
		t.Fatalf("the replay's calls stored %v; want one item each", names)
	}
	if _, err := s.Talk(ctx, "Summarize where it stands.", nil); err != nil {
		t.Fatal(err)
	}
	return replay{s: s, names: names}
}

// fill is text with the replay's evidence names in place.
func (r replay) fill(s string) string {
	for role, name := range r.names {
		s = strings.ReplaceAll(s, "{"+role+"}", name)
	}
	return s
}

// statementOf is the ID of the request's statement beginning with prefix.
func statementOf(t *testing.T, c Checked, prefix string) string {
	t.Helper()
	for _, st := range c.Statements {
		if strings.HasPrefix(st.Text, prefix) {
			return st.ID
		}
	}
	t.Fatalf("no statement begins %q", prefix)
	return ""
}

func candidatesOf(c Checked, id string) []CheckCandidate {
	var out []CheckCandidate
	for _, cand := range c.Candidates {
		if cand.Statement == id {
			out = append(out, cand)
		}
	}
	return out
}

// TestCheckListingIsLabelled: the find listing a statement cites for where
// daemons ran is quoted under its kind, listing, and that statement — and
// only a statement resting on listings alone — is the editor's candidate;
// statements the evidence supports are none. A finding on lines the
// request did not quote, past the listing's end, is dropped.
func TestCheckListingIsLabelled(t *testing.T) {
	r := replaySession(t)
	req, err := r.s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mons := statementOf(t, req.Checked, "The MONs a, b and c ran on worker-1")
	cands := candidatesOf(req.Checked, mons)
	if len(cands) != 1 || cands[0].Why != CandidateListing || !slices.Equal(cands[0].Listings, []string{r.names["find"]}) {
		t.Errorf("the MONs statement's candidates: %+v", cands)
	}
	for _, want := range []string{
		"### " + r.names["find"] + " · listing · find ",
		"- " + mons + " cites only file listings, which name files and say nothing of what ran: " + r.names["find"] + ".",
		"### " + r.names["pods"] + " · read · read ",
	} {
		if !strings.Contains(req.Text, want) {
			t.Errorf("the editor's request lacks %q:\n%s", want, req.Text)
		}
	}
	if !req.Context.Listings[r.names["find"]] || req.Context.Listings[r.names["pods"]] {
		t.Errorf("listings given: %v", req.Context.Listings)
	}
	for _, supported := range []string{"The namespace's pods place the MONs", "The kubelet's eviction manager ranked `openshift-storage"} {
		if c := candidatesOf(req.Checked, statementOf(t, req.Checked, supported)); len(c) != 0 {
			t.Errorf("a supported statement is a candidate: %+v", c)
		}
	}
	// A finding on the listing validates against what the request quoted.
	f, dropped, err := check.Validate(req.Context, map[string]any{
		"findings": []any{
			map[string]any{"statement": mons, "kind": "listing_as_state", "quote": "ran on worker-1", "why": "a listing of log files", "evidence": []any{r.names["find"] + ":3-5"}},
			map[string]any{"statement": mons, "kind": "unsupported", "quote": "ran on worker-1", "why": "lines not given", "evidence": []any{r.names["find"] + ":40-41"}},
		},
		"supported": []any{},
	})
	if err != nil || len(f.Findings) != 1 || len(dropped) != 1 {
		t.Errorf("findings %+v, dropped %v, %v", f.Findings, dropped, err)
	}
}

// TestCheckListingFindingRestsOnStatement: a listing_as_state finding on a
// statement citing only a listing is kept, by the editor and the reviewer,
// though it names no evidence and its quote carries the citation, as a
// model wrote it live; one on a statement citing a file read,
// not a listing, is dropped still.
func TestCheckListingFindingRestsOnStatement(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	editor, err := r.s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := r.s.ReviewRequest(ctx, ReviewAsk{})
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]CheckRequest{"editor": editor, "reviewer": reviewer} {
		mons := statementOf(t, req.Checked, "The MONs a, b and c ran on worker-1")
		pods := statementOf(t, req.Checked, "The namespace's pods place the MONs")
		f, dropped, err := check.Validate(req.Context, map[string]any{
			"findings": []any{
				map[string]any{"statement": mons, "kind": "listing_as_state", "quote": r.fill("The MONs a, b and c ran on worker-1 [{find}:3-5]."), "why": "a listing of log files"},
				map[string]any{"statement": pods, "kind": "listing_as_state", "quote": "place the MONs", "why": "a file, not live state", "evidence": []any{r.names["pods"] + ":2-11"}},
			},
			"supported": []any{},
		})
		if err != nil || len(f.Findings) != 1 || f.Findings[0].Statement != mons || len(dropped) != 1 || dropped[0].Reason != "a listing_as_state finding cites no listing given" {
			t.Errorf("%s: findings %+v, dropped %v, %v", name, f.Findings, dropped, err)
		}
	}
}

// TestCheckSpanNoted: an inline code span found in no evidence the
// conversation saw — the alert rule — is flagged beneath the answer, and a
// span the evidence holds is not; a statement stating it and citing
// nothing is the editor's candidate.
func TestCheckSpanNoted(t *testing.T) {
	r := replaySession(t)
	flags, err := r.s.AnswerFlags(context.Background(), []string{r.fill(replayA2)})
	if err != nil {
		t.Fatal(err)
	}
	var spans []string
	for _, f := range flags[0] {
		if strings.HasPrefix(f.What, "`") {
			spans = append(spans, f.What+" "+f.Why)
		}
	}
	if d := cmp.Diff([]string{"`ceph_osd_up == 0` in no evidence this conversation saw"}, spans); d != "" {
		t.Errorf("span flags (-want +got):\n%s", d)
	}
	req, err := r.s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	alert := statementOf(t, req.Checked, "The alert `ceph_osd_up == 0` fired")
	if c := candidatesOf(req.Checked, alert); len(c) != 1 || c[0].Why != CandidateUncited {
		t.Errorf("the alert's candidates: %+v", c)
	}
	if !strings.Contains(req.Text, "- `ceph_osd_up == 0` is in no evidence this conversation saw.") {
		t.Errorf("the editor's request does not say the span is in no evidence:\n%s", req.Text)
	}
}

// TestCheckSpanCommand: a code span that is a command line to run is no
// claim and is not flagged, nor is one in a fenced block; a stated value,
// and an expression whose first word is no program, are flagged still. A
// product's program is a command only where the configuration names it
// (checks.commands); the engine's own, such as dig, always is.
func TestCheckSpanCommand(t *testing.T) {
	r := replaySession(t)
	answer := "Run `oc -n openshift-dns get pods -o wide` and `dig @172.30.0.10 kubernetes.default` on worker-1. " +
		"The cluster DNS service is `172.30.0.10`, and the alert `ceph_osd_up == 0` fired.\n\n" +
		"```\nceph_osd_up == 1\n```\n"
	spans := func() []string {
		flags, err := r.s.AnswerFlags(context.Background(), []string{answer})
		if err != nil {
			t.Fatal(err)
		}
		var spans []string
		for _, f := range flags[0] {
			spans = append(spans, f.What)
		}
		return spans
	}
	if d := cmp.Diff([]string{"`oc -n openshift-dns get pods -o wide`", "`172.30.0.10`", "`ceph_osd_up == 0`"}, spans()); d != "" {
		t.Errorf("span flags with no program configured (-want +got):\n%s", d)
	}
	r.s.e.config.Checks.Commands = []string{"oc"}
	if d := cmp.Diff([]string{"`172.30.0.10`", "`ceph_osd_up == 0`"}, spans()); d != "" {
		t.Errorf("span flags with oc configured (-want +got):\n%s", d)
	}
}

// TestCheckExcerptLines: a fenced block presented as output is looked for
// line by line: a line the evidence holds is not flagged, one it does not
// is, under the statement presenting it; a bare block after a statement
// citing nothing, such as a query to run, is not looked for.
func TestCheckExcerptLines(t *testing.T) {
	r := replaySession(t)
	answer := r.fill("The kubelet ranked the pods for eviction [{evict}:3]:\n\n" +
		"```\n4: Oct 20 08:10:02 worker-1 kubelet[2211]: I1020 08:10:02.120 eviction_manager.go:395]\n```\n\n" +
		"Then the CSI socket failed [{csi}:1]:\n\n" +
		"```text\nE1020 08:12:41.000 csi_attacher.go:1022] attach timed out on worker-1\n```\n\n" +
		"Check the rate with:\n\n```\nsum(rate(kubelet_evictions_total[5m]))\n```\n")
	flags, err := r.s.AnswerFlags(context.Background(), []string{answer})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range flags[0] {
		got = append(got, f.What+" · "+f.Statement)
	}
	want := []string{"`E1020 08:12:41.000 csi_attacher.go:1022] attach timed out on worker-1` · " + r.fill("Then the CSI socket failed [{csi}:1]:")}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("flags (-want +got):\n%s", d)
	}
}

// TestCheckSpanReworded: a span the model reworded from what it read —
// digit grouping added, spacing, case, quotes or punctuation changed, a
// file and its value written as "key: value", the end cut with an
// ellipsis — is found, as seen live; a different
// value, key or order, or words the evidence never holds, are not.
func TestCheckSpanReworded(t *testing.T) {
	lines := bytes.Split(bytes.ToLower([]byte(strings.Join([]string{
		"MemTotal:       131072000 kB",
		"MemFree:          2097152 kB",
		"allocstall_movable 4242424",
		`  "topologyManagerPolicy": "single-numa-node",`,
		"/sys/fs/cgroup/kubepods.slice/cpuset.mems.effective:0",
		"drwxr-xr-x  0020-inspect.local.5551212/",
		"bytes sent 1,048,576 in total",
	}, "\n"))), []byte("\n"))
	for span, want := range map[string]bool{
		"MemTotal: 131,072,000 kB":                true,
		"MemFree: 2,097,152 kB":                   true,
		"ALLOCSTALL_MOVABLE: 4,242,424":           true,
		"topologyManagerPolicy: single-numa-node": true,
		"cpuset.mems.effective: 0":                true,
		"0020-inspect...":                         true,
		"0020-insp…":                              true,
		"…5551212":                                true,
		"1048576 in total":                        true,
		"MemTotal: 131,072,001 kB":                false,
		"MemFree: 131,072,000 kB":                 false,
		"topologyManagerPolicy: restricted":       false,
		"single-numa-node topologyManagerPolicy":  false,
		"ceph_osd_up == 0":                        false,
		"0021-inspect...":                         false,
		"...":                                     false,
	} {
		if got := spanReworded(lines, span); got != want {
			t.Errorf("%q found %v, want %v", span, got, want)
		}
	}
	r := replaySession(t)
	flags, err := r.s.AnswerFlags(context.Background(), []string{"The OSD logged `No Space Left On Device (ENOSPC)`, not `No Space Left On Device (EIO)`."})
	if err != nil {
		t.Fatal(err)
	}
	var spans []string
	for _, f := range flags[0] {
		spans = append(spans, f.What)
	}
	if d := cmp.Diff([]string{"`No Space Left On Device (EIO)`"}, spans); d != "" {
		t.Errorf("span flags of the replay (-want +got):\n%s", d)
	}
}

// TestCheckSpanLeftOut: a span found only in a comment the engineer's
// rules leave out of what models read is flagged as in no evidence seen,
// as the comment is not read for it.
func TestCheckSpanLeftOut(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	answer := "The customer wrote `Only two workers affected`."
	flagged := func() bool {
		t.Helper()
		flags, err := r.s.AnswerFlags(ctx, []string{answer})
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(flags[0], func(f AnswerFlag) bool { return f.What == "`Only two workers affected`" })
	}
	if flagged() {
		t.Fatal("a span the case's comment holds was flagged")
	}
	if _, err := excludeComments(r.s.c, ExcludeComment, "c1"); err != nil {
		t.Fatal(err)
	}
	if !flagged() {
		t.Error("a span found only in a comment left out of what models read was not flagged")
	}
}

// TestCheckSpanBudget: the span check reads the evidence within its
// bounds, an item's and the total, and a flag says what went unread; a
// cancelled check stops with its context's error, the citation forms
// still returned.
func TestCheckSpanBudget(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	flags, err := r.s.answerFlags(ctx, []string{"See " + r.names["osd"] + ":2 and `no such marker 7f3a`."}, spanReads{Item: 64, Total: 200})
	if err != nil {
		t.Fatal(err)
	}
	why := regexp.MustCompile(`^in no evidence this conversation saw, [1-9]\d* items? of it read only to the first 64 bytes; ` +
		`the 200 bytes the check reads ran out before [1-9]\d* of [1-9]\d* items were read whole$`)
	if len(flags[0]) != 2 || !why.MatchString(flags[0][1].Why) {
		t.Errorf("flags %+v; want the span's saying what went unread", flags[0])
	}
	x, err := r.s.exposure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.s.unseenSpans(cancelled, []string{"no such marker 7f3a"}, x, spanBudget); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled span check returned %v", err)
	}
}

// TestQuoteLongLastLine: a last line with no newline is quoted whole or cut
// on its bound however its length falls across the reader's buffer, one
// exactly the buffer's size included.
func TestQuoteLongLastLine(t *testing.T) {
	long := strings.Repeat("a", 64<<10)
	got, last, partial, err := quoteLines(strings.NewReader(long), 1, 2, 4096)
	if err != nil || got != long[:4096] || last != 1 || !partial {
		t.Errorf("a 64 KiB last line quoted %d bytes to line %d, partial %v, %v; want its first 4096", len(got), last, partial, err)
	}
	got, last, partial, err = quoteLines(strings.NewReader("x\n"+long), 1, 2, 1<<20)
	if err != nil || got != "x\n"+long || last != 2 || partial {
		t.Errorf("a line and a 64 KiB last line quoted %d bytes to line %d, partial %v, %v; want both whole", len(got), last, partial, err)
	}
}

// TestCheckHedgeDropped: a statement citing lines an earlier answer cited
// with a qualifier it no longer has is the editor's candidate, with that
// earlier statement and its answer.
func TestCheckHedgeDropped(t *testing.T) {
	r := replaySession(t)
	req, err := r.s.EditorRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	crashed := candidatesOf(req.Checked, statementOf(t, req.Checked, "The CSI socket was removed"))
	if len(crashed) != 1 || crashed[0].Why != CandidateHedge || crashed[0].EarlierAnswer != "a1" ||
		!strings.HasPrefix(crashed[0].Earlier, "The CSI socket refused connections, likely") || !slices.Equal(crashed[0].Hedges, []string{"likely"}) {
		t.Errorf("the crashed statement's candidates: %+v", crashed)
	}
	// Its "led to" is a certainty candidate too (TestCheckCertaintyCandidate).
	led := candidatesOf(req.Checked, statementOf(t, req.Checked, "The OSD shutdown at 08:16 led to"))
	if len(led) != 2 || led[0].Why != CandidateHedge || !slices.Equal(led[0].Hedges, []string{"either", "may"}) || led[1].Why != CandidateCertainty {
		t.Errorf("the sequence's candidates: %+v", led)
	}
	if !strings.Contains(req.Text, `cites lines that a1 cited with "likely"`) {
		t.Errorf("the editor's request does not give the earlier statement:\n%s", req.Text)
	}
	if c := candidatesOf(req.Checked, statementOf(t, req.Checked, "The kubelet evicted")); len(c) != 0 {
		t.Errorf("a statement whose lines no earlier statement qualified is a candidate: %+v", c)
	}
}

// TestCheckMalformedCitation: names and lines without brackets, or in
// parentheses, are flagged beneath the answer, since Cites reads them as
// nothing; well-formed citations and anything in code are not, nor what
// only looks like a citation: a MAC or IPv6 address, a bracket whose name
// runs on into a word, and a name the conversation never showed.
func TestCheckMalformedCitation(t *testing.T) {
	r := replaySession(t)
	lookalikes := []string{
		"The NIC 3c:fd:fe:e1:22:10 flapped.",
		"Address fe80::e1:5 answered.",
		"Run the [e2e suite](https://example.com/e2e) again.",
		"The emulated NIC (e1000) was chosen.",
		"The driver is [e1000e] here.",
		"In code, `see e1:2 (e1) [e1 2]` is left alone.",
	}
	flags, err := r.s.AnswerFlags(context.Background(), append([]string{r.fill(replayA2), "Fine [e1:2] and [e1:2-3, 4] and [e1:2–3]; bad [e1 2] and [e1:2—3]."}, lookalikes...))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range flags[0] {
		if !strings.HasPrefix(f.What, "`") {
			got = append(got, f.What+" · "+f.Why)
		}
	}
	want := []string{
		r.names["osd"] + ":2 · not read as a citation without brackets: write [" + r.names["osd"] + ":2]",
		"(" + r.names["csi"] + ") · not read as a citation in parentheses: write [" + r.names["csi"] + "]",
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("form flags (-want +got):\n%s", d)
	}
	got = nil
	for _, f := range flags[1] {
		got = append(got, f.What)
	}
	if d := cmp.Diff([]string{"[e1 2]", "[e1:2—3]"}, got); d != "" {
		t.Errorf("bracket form flags (-want +got):\n%s", d)
	}
	for i, text := range lookalikes {
		for _, f := range flags[2+i] {
			if !strings.HasPrefix(f.What, "`") {
				t.Errorf("%q flagged %q: %s", text, f.What, f.Why)
			}
		}
	}
}

// TestQuoteByteForByte: quoted lines are the stored lines' bytes, a line's
// trailing tab and spaces included, under the numbers they are stored at.
func TestQuoteByteForByte(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	for _, c := range []Cite{
		{Text: "[x]", Name: r.names["find"], From: 3, To: 5},
		{Text: "[x]", Name: r.names["csi"], From: 1},
		{Text: "[x]", Name: r.names["pods"]},
	} {
		q, err := r.s.Quote(ctx, []Cite{c}, QuoteBounds{Lines: 100, Bytes: 1 << 20, Total: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		it, err := r.s.EvidenceNamed(c.Name)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(it.Content), "\n")
		from, to := c.From, max(c.To, c.From)
		if from == 0 {
			from, to = 1, int(it.Size.Lines)
		}
		if want := strings.Join(lines[from-1:to], ""); q[0].Text != want || q[0].From != from || q[0].To != to || q[0].Cut != "" {
			t.Errorf("%s:%d-%d quoted %+v; want the stored %q", c.Name, from, to, q[0], want)
		}
	}
	csi, err := r.s.Quote(ctx, []Cite{{Name: r.names["csi"], From: 1}}, QuoteBounds{Lines: 1, Bytes: 4096, Total: 4096})
	if err != nil || !strings.HasSuffix(csi[0].Text, "connection refused\"\t  \n") {
		t.Errorf("the CSI line quoted %q, %v; want its trailing tab and spaces", csi[0].Text, err)
	}
	req, err := r.s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range req.Quotes {
		last := 0
		for _, p := range it.Parts {
			last = max(last, p.To)
		}
		for _, p := range it.Parts {
			for i, line := range strings.SplitAfter(strings.TrimSuffix(p.Text, "\n"), "\n") {
				numbered := fmt.Sprintf("%*d  %s\n", len(strconv.Itoa(last)), p.From+i, strings.TrimSuffix(line, "\n"))
				if !strings.Contains(req.Text, "\n  "+numbered) && !strings.Contains(req.Text, "\n"+citedMark+numbered) {
					t.Errorf("the request does not carry %s's line %d under its number:\n%s", it.Name, p.From+i, req.Text)
				}
			}
		}
	}
}

// TestQuoteBounds: a citation is quoted to its line and byte bounds, a
// line longer than the byte bound cut on it, and past the shared bound
// nothing more is quoted; each cut says so, as does a citation that
// cannot be quoted. A request's quotations take what its bound leaves.
func TestQuoteBounds(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	find, evict := r.names["find"], r.names["evict"]
	content := func(name string) []byte {
		it, err := r.s.EvidenceNamed(name)
		if err != nil {
			t.Fatal(err)
		}
		return it.Content
	}
	lines, err := r.s.Quote(ctx, []Cite{{Name: find, From: 3, To: 5}}, QuoteBounds{Lines: 2, Bytes: 4096, Total: 4096})
	if err != nil || lines[0].To != 4 || lines[0].Cut != "cut at 2 lines: lines 3–4 of lines 3–5 quoted" {
		t.Errorf("the listing quoted %+v, %v; want lines 3 and 4", lines, err)
	}
	findLine3 := strings.SplitAfter(string(content(find)), "\n")[2]
	total := len(findLine3) + 50
	q, err := r.s.Quote(ctx, []Cite{
		{Name: find, From: 3, To: 5},
		{Name: evict, From: 1, To: 3},
		{Name: evict, From: 3},
		{Name: "e999", From: 2},
		{Path: "nodes/worker-1/kubelet.log", From: 2},
	}, QuoteBounds{Lines: 40, Bytes: 50, Total: total})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range q {
		got = append(got, x.Cut)
	}
	want := []string{
		"cut at 50 bytes: line 3 of lines 3–5 quoted",
		"line 1 cut at its first 50 bytes",
		"not quoted: the " + strconv.Itoa(total) + " bytes every quotation shares are spent",
		"not quoted: no evidence e999",
		"not quoted: an artifact path, and only stored evidence is quoted",
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("cuts (-want +got):\n%s", d)
	}
	if q[0].Text != findLine3 || q[1].Text != string(content(evict)[:50]) || q[1].To != 1 {
		t.Errorf("quoted %q and %q to line %d; want the listing's line 3 and the first 50 bytes of the search's line 1", q[0].Text, q[1].Text, q[1].To)
	}
}

// TestCheckRequestBound: a request's bytes — headings, line numbers and
// notes included — stay within its bound: items past it are left out,
// counted in one line, and the lines they would have given are not in
// the context its findings are validated against.
func TestCheckRequestBound(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	c, err := r.s.checked(ctx, r.fill(replayA2), nil)
	if err != nil {
		t.Fatal(err)
	}
	cites := citesOf(c.Statements)
	items, err := r.s.citedItems(ctx, cites)
	if err != nil {
		t.Fatal(err)
	}
	none, err := r.s.checkRequest(ctx, check.Editor, "# Check this answer\n", c, cites, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	prefix, ok := strings.CutSuffix(none.Text, fmt.Sprintf(quotesSpent, len(items)))
	if !ok || len(none.Quotes) != 0 || len(none.Context.Quoted) != 0 {
		t.Fatalf("a request with no room quoted %d items and gave %v:\n%s", len(none.Quotes), none.Context.Quoted, none.Text)
	}
	limit := len(prefix) + 600
	some, err := r.s.checkRequest(ctx, check.Editor, "# Check this answer\n", c, cites, "", limit)
	if err != nil {
		t.Fatal(err)
	}
	n := len(some.Quotes)
	if len(some.Text) > limit || n == 0 || n == len(items) || !strings.HasSuffix(some.Text, fmt.Sprintf(quotesSpent, len(items)-n)) {
		t.Errorf("a request bounded at %d bytes is %d, quoting %d of %d items:\n%s", limit, len(some.Text), n, len(items), some.Text)
	}
	for name := range some.Context.Quoted {
		if !slices.ContainsFunc(some.Quotes, func(q QuotedItem) bool { return q.Name == name && len(q.Parts) > 0 }) {
			t.Errorf("the context gives %s, which the request does not quote", name)
		}
	}
	editor, err := r.s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := r.s.ReviewRequest(ctx, ReviewAsk{})
	if err != nil {
		t.Fatal(err)
	}
	if len(editor.Text) > editorRequestBytes || len(reviewer.Text) > reviewerRequestBytes {
		t.Errorf("the editor's request is %d bytes, the reviewer's %d", len(editor.Text), len(reviewer.Text))
	}
}

// TestCheckRequestsDeterministic: the same conversation makes the same
// requests, byte for byte, asked again and in another session over the
// same replay — but for the evidence names, which follow the order the
// concurrent calls finished in, so are compared by what each names.
func TestCheckRequestsDeterministic(t *testing.T) {
	ctx := context.Background()
	name := regexp.MustCompile(`\be[0-9]+\b`)
	var texts []string
	for range 2 {
		r := replaySession(t)
		roles := map[string]string{}
		for role, n := range r.names {
			roles[n] = "{" + role + "}"
		}
		for range 2 {
			e, err := r.s.EditorRequest(ctx)
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.s.ReviewRequest(ctx, ReviewAsk{})
			if err != nil {
				t.Fatal(err)
			}
			// The answer as written, which the editor is given, holds e9 in
			// a code block as text, not as the name the replay gave.
			both := strings.ReplaceAll(e.Text+"\x00"+v.Text, "# e9:4 (e9)", "# the code block's text")
			texts = append(texts, name.ReplaceAllStringFunc(both, func(n string) string {
				if role, ok := roles[n]; ok {
					return role
				}
				return n
			}))
		}
	}
	for i := 1; i < len(texts); i++ {
		if d := cmp.Diff(texts[0], texts[i]); d != "" {
			t.Errorf("request %d differs from the first (-first +it):\n%s", i, d)
		}
	}
}

// TestReviewRequest: the reviewer is given the text and the items it
// cites, quoted as the editor's are, never the conversation: no earlier
// answer's words and no dropped-qualifier notes. A customer draft citing
// nothing is given the items its source answers cited, said so.
func TestReviewRequest(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	req, err := r.s.ReviewRequest(ctx, ReviewAsk{})
	if err != nil {
		t.Fatal(err)
	}
	if req.Context.Role != check.Reviewer || strings.Contains(req.Text, "likely") || strings.Contains(req.Text, CandidateHedge) ||
		!strings.Contains(req.Text, "The text is the companion's answer a2") {
		t.Errorf("the reviewer's request:\n%s", req.Text)
	}
	if _, _, err := r.s.Draft(ctx, "customer-update", nil); err != nil {
		t.Fatal(err)
	}
	req, err = r.s.ReviewRequest(ctx, ReviewAsk{Item: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"The text is the draft i1 for customer-update, for the customer",
		"It cites no evidence; the items below are those cited by the answers it was written from, a1, a2, quoted without those answers' text.",
		"### " + r.names["find"] + " · listing",
		"### " + r.names["evict"] + " · search",
		"cited as " + r.names["find"] + ":3-5",
	} {
		if !strings.Contains(req.Text, want) {
			t.Errorf("the draft's request lacks %q:\n%s", want, req.Text)
		}
	}
	if strings.Contains(req.Text, "The MONs a, b and c ran") {
		t.Errorf("the draft's request carries an answer's text:\n%s", req.Text)
	}
	if len(req.Quotes) == 0 || !req.Quotes[0].Whole || !strings.Contains(req.Text, "\n"+citedMark) {
		t.Errorf("the draft's request does not quote its first item whole, its cited lines marked:\n%s", req.Text)
	}
	if _, err := r.s.ReviewRequest(ctx, ReviewAsk{Item: "i9"}); err == nil {
		t.Error("a request for no item was made")
	}
}

// TestReviewRequestRanked: asked with Ranked, the reviewer is also given,
// after the items the text cites and quoted the same, the evidence the
// conversation used most that the text does not cite, as a restart
// carries it: each item headed as disclosed to the companion and not cited
// by the text, with why it ranked and the lines cited elsewhere, or its
// first lines where none was named; an item the text cites is not among
// them. Its lines are in the request's context, so a finding resting on
// them is kept, as it is not without Ranked; a text citing everything used
// is told there is none. Synthetic: the replay's evidence.
func TestReviewRequestRanked(t *testing.T) {
	names := map[string]string{}
	roles := map[string]string{"c1": "evict", "c2": "csi", "c3": "find", "c4": "events"}
	answers := []string{
		"- The eviction manager ranked `openshift-storage/csi-rbdplugin-k2x9p` for eviction at 08:10:02 [{evict}:1-3].\n- At 08:12:40 the kubelet's dial to the RBD CSI socket was refused [{csi}:1].\n",
		"- At 08:12:40 the kubelet's dial to the RBD CSI socket was refused, with nothing logged about the plugin's pod before it [{csi}:1].\n",
		"- The kubelet ranked the plugin's pod at 08:10:02 [{evict}:3], its dial refused at 08:12:40 [{csi}:1], six logs listed [{find}:1-6] and an MDS back-off at 08:21:40 [{events}:8-12].\n",
	}
	var n atomic.Int64
	var mu sync.Mutex
	fill := func(s string) string {
		mu.Lock()
		defer mu.Unlock()
		for role, name := range names {
			s = strings.ReplaceAll(s, "{"+role+"}", name)
		}
		return s
	}
	respond := func(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
		i := n.Add(1)
		if i == 1 {
			return modelfixture.Calling(
				call("c1", "grep", map[string]any{"pattern": "Eviction manager", "path": "nodes/worker-1/kubelet.log"}),
				call("c2", "grep", map[string]any{"pattern": "connection refused", "path": "nodes/worker-1/kubelet.log"}),
				call("c3", "find", map[string]any{"path": "nodes/worker-1/var/log/ceph"}),
				call("c4", "read", map[string]any{"path": "namespaces/openshift-storage/events.yaml"}),
			), nil
		}
		if i == 2 {
			mu.Lock()
			for id, res := range lastResults(r) {
				if ev := ExcerptEvidence(res.Text); len(ev) == 1 {
					names[roles[id]] = ev[0]
				}
			}
			mu.Unlock()
		}
		if k := int(i) - 2; k < len(answers) {
			return modelfixture.Text(fill(answers[k])), nil
		}
		return modelfixture.Text("done"), nil
	}
	s, _ := companionSession(t, respond)
	td, err := filepath.Abs("../testdata/replay/epistemic")
	if err != nil {
		t.Fatal(err)
	}
	s.c.root = td
	ctx := context.Background()
	for _, ask := range []string{"What happened on worker-1?", "Did anything warn of the refused dial?"} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(names) != len(roles) {
		t.Fatalf("the calls stored %v; want one item each", names)
	}
	plain, err := s.ReviewRequest(ctx, ReviewAsk{})
	if err != nil {
		t.Fatal(err)
	}
	ranked, err := s.ReviewRequest(ctx, ReviewAsk{Ranked: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Text, uncitedHeading) || len(plain.Context.Quoted) != 1 {
		t.Errorf("the plain request gives the ranked evidence, or quotes %v:\n%s", plain.Context.Quoted, plain.Text)
	}
	before, section, ok := strings.Cut(ranked.Text, "\n"+uncitedHeading+"\n")
	_, plainBody, _ := strings.Cut(plain.Text, "\n## Statements\n")
	if _, body, _ := strings.Cut(before, "\n## Statements\n"); !ok || body != plainBody {
		t.Fatalf("the ranked request's statements and quotations differ from the plain one's, or it has no section:\n%s", ranked.Text)
	}
	evict, find, events := strings.Index(section, "### "+names["evict"]+" · search · "), strings.Index(section, "### "+names["find"]+" · listing · "), strings.Index(section, "### "+names["events"]+" · read · ")
	if evict < 0 || find < evict || events < evict || strings.Contains(section, "### "+names["csi"]+" ") {
		t.Errorf("the section does not give the uncited items, best first, without the cited one:\n%s", section)
	}
	for _, want := range []string{
		"[disclosed to the companion, not cited by the text; cited in a1, returned by grep]",
		`"Eviction manager: pods ranked for eviction" pods=["openshift-storage/csi-rbdplugin-k2x9p","app-demo/web-6f7c9"]`,
		"Its first lines, of 12 lines.",
		"Back-off restarting failed container mds",
		"You are not given the conversation but for that.",
	} {
		if !strings.Contains(ranked.Text, want) {
			t.Errorf("the ranked request lacks %q:\n%s", want, ranked.Text)
		}
	}
	want := map[string][]check.Lines{names["csi"]: {{From: 1, To: 1}}, names["evict"]: {{From: 1, To: 3}}, names["find"]: {{From: 1, To: 6}}, names["events"]: {{From: 1, To: 12}}}
	if d := cmp.Diff(want, ranked.Context.Quoted); d != "" || ranked.Context.Items[names["events"]] != 12 || !ranked.Context.Listings[names["find"]] {
		t.Errorf("the ranked request's context (-want +got):\n%s\nitems %v, listings %v", d, ranked.Context.Items, ranked.Context.Listings)
	}
	// A finding naming the uncited contradiction is kept only where the
	// request gave its lines.
	args := map[string]any{
		"findings": []any{map[string]any{"statement": "s1", "kind": "unsupported", "quote": "with nothing logged about the plugin's pod before it",
			"why": "the eviction manager ranked the plugin's pod at 08:10:02", "evidence": []any{names["evict"] + ":3"},
			"alternative": "the eviction ranking preceded the refusal", "discriminator": "the plugin's own log, not given"}},
		"supported": []any{},
	}
	for _, c := range []struct {
		name string
		req  CheckRequest
		kept int
	}{{"plain", plain, 0}, {"ranked", ranked, 1}} {
		f, _, err := check.Validate(c.req.Context, args)
		if err != nil || len(f.Findings) != c.kept {
			t.Errorf("%s: the finding on the uncited lines: kept %d, want %d, %v", c.name, len(f.Findings), c.kept, err)
		}
	}
	if _, err := s.Talk(ctx, "List what was read.", nil); err != nil {
		t.Fatal(err)
	}
	all, err := s.ReviewRequest(ctx, ReviewAsk{Ranked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(all.Text, "\nNone: the conversation used no evidence the text does not cite.\n") {
		t.Errorf("a text citing every item used is not told there is none:\n%s", all.Text)
	}
}

// TestEditorRequestDraftSources: a draft leaving statements uncited is
// given, after its own, the items cited by the answers it was written
// from, said so, so the editor can cite their lines itself; an answer that
// is no draft is given only what it cites.
func TestEditorRequestDraftSources(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	const source = "It is a draft, and a statement it leaves uncited may rest on the items below its own: those cited by the answers it was written from, a1, a2"
	req, err := r.s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.Text, source) {
		t.Errorf("the answer a2, no draft, is given its sources:\n%s", req.Text)
	}
	if _, _, err := r.s.Draft(ctx, "customer-update", nil); err != nil {
		t.Fatal(err)
	}
	if req, err = r.s.EditorRequest(ctx); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{source, "### " + r.names["find"] + " · listing", "### " + r.names["evict"] + " · search", "cited as " + r.names["find"] + ":3-5"} {
		if !strings.Contains(req.Text, want) {
			t.Errorf("the draft's request lacks %q:\n%s", want, req.Text)
		}
	}
	if !slices.ContainsFunc(req.Quotes, func(q QuotedItem) bool { return q.Name == r.names["evict"] }) {
		t.Errorf("the draft's request quotes %+v; want %s among them, so a citation of it is accepted", req.Quotes, r.names["evict"])
	}
}

// TestCheckNothingYet: a conversation with no answer has nothing to check.
func TestCheckNothingYet(t *testing.T) {
	s, _ := companionSession(t, nil)
	if _, err := s.EditorRequest(context.Background()); err == nil {
		t.Error("an editor's request with no answer was made")
	}
}

// TestCheckCertaintyCandidate: [E2] a statement holding certainty or causal
// wording is the editor's candidate, never shown beneath the answer: the
// synthetic "confirms that … were caused by" and a root cause inside an
// attributed conclusion, whose voice the editor judges, but not "the
// engineer confirmed", a named source's own word.
func TestCheckCertaintyCandidate(t *testing.T) {
	s, _ := companionSession(t, nil)
	ctx := context.Background()
	text := "The investigation confirms that the initial FailedMount errors were caused by root filesystem exhaustion. " +
		"The engineer confirmed the node rebooted at 14:03. Engineering concluded the root cause is the full /var."
	c, err := s.checked(ctx, text, nil)
	if err != nil {
		t.Fatal(err)
	}
	terms := map[string][]string{}
	for _, cand := range c.Candidates {
		if cand.Why == CandidateCertainty {
			terms[cand.Statement] = cand.Terms
		}
	}
	if d := cmp.Diff(map[string][]string{"s1": {"confirms", "caused"}, "s3": {"root cause"}}, terms); d != "" {
		t.Errorf("certainty candidates (-want +got):\n%s", d)
	}
	for _, f := range c.Flags {
		if strings.Contains(f.What, "confirm") || strings.Contains(f.What, "cause") {
			t.Errorf("a certainty candidate is shown beneath the answer: %+v", f)
		}
	}
	req, err := s.checkRequest(ctx, check.Editor, "# Check this answer\n", c, nil, "", editorRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Text, `- s1 holds "confirms", "caused": whether in its own voice`) || strings.Contains(req.Text, "- s2 holds") {
		t.Errorf("the editor's request:\n%s", req.Text)
	}
}

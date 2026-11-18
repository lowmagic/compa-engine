package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/model/modeltest"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// The epistemic checks' benchmark: the fixture plays the main model over
// the synthetic replay
// (testdata/replay/epistemic), reading it with nine tool calls, fetching a
// synthetic knowledge-base solution (testdata/replay/references) and
// consulting an expert, then answering three times, a1, a2 and a3,
// written as a main model writes them, with [eN:lines] citations into
// what it read; the editor's and the reviewer's candidate models check
// them live. Each statement of a2 is seeded with one of the failure
// shapes or is supported by the lines it cites; every statement of a1 and
// of a3, the control, is supported.

// benchStatement is one statement of a replay answer: the failure shape it
// is seeded with, none where its cited lines support it; its text, {find}
// and the like the evidence names; and words — of a seeded statement the
// offending words, which a correction fixing it changes; of a supported
// one words a correction leaving it alone keeps.
type benchStatement struct {
	shape, text, words string
}

// benchShapes are the failure shapes seeded, by number; 7,
// the engine's own citation parser, is no model's to judge.
var benchShapes = []struct{ id, name string }{
	{"1", "listing as runtime state"},
	{"2a", "candidate as outcome: ranked as evicted"},
	{"2b", "candidate as outcome: refused as removed and crashed"},
	{"3", "fact with no evidence: an alert rule and threshold"},
	{"4", "two events hours apart made one"},
	{"5", "measure overstated: ENOSPC as 100%, /var as the root filesystem"},
	{"6", "qualifier dropped against a1"},
	{"8", "certainty and causation in the model's own voice"},
	{"9", "precedent as case evidence: a solution's root cause as this case's"},
	{"10", "absence as evidence of absence: no log, so never deployed"},
	{"11", "events in order told as cause and effect"},
	{"12", "a recommendation stated as a confirmed fix"},
	{"13", "a consult's hedged answer restated as observed"},
	{"14", "a configuration's head read as the whole: a field past it said unset"},
	{"15", "two nodes' captures told as one node's progression"},
	{"16", "a request resting on an unsupported causal premise"},
}

// benchA1 is the first answer: every statement supported, the one
// inference on the ladder's weak wording, "may".
var benchA1 = []benchStatement{
	{"", "The kubelet's eviction manager on worker-1 ranked `openshift-storage/csi-rbdplugin-k2x9p` and `app-demo/web-6f7c9` for eviction at 08:10:02 to reclaim ephemeral-storage [{evict}:1-3].",
		"ranked openshift-storage/csi-rbdplugin-k2x9p and app-demo/web-6f7c9 for eviction"},
	{"", "At 08:12:40 the kubelet's dial to the RBD CSI plugin's socket, `/var/lib/kubelet/plugins/rbd.csi.ceph.com/csi.sock`, was refused; these lines do not show why [{csi}:1].",
		"was refused"},
	{"", "osd.3 logged no heartbeat reply from osd.5 at 08:15:58 and a Terminated signal at 08:16:05; the missed heartbeat may be related to the shutdown, or the two may be unrelated [{osdlog}:2-3].",
		"may be related to the shutdown"},
	{"", "The namespace's pods place the three MONs on worker-2, worker-3 and worker-4, all Running [{pods}:2-11].",
		"on worker-2, worker-3 and worker-4"},
	{"", "The journal shows `rook-ceph-osd` writes to `/var/lib/rook/osd3/log` failing with ENOSPC at 11:55:31 and 11:55:32 [{enospc}:1-2].",
		"failing with enospc"},
}

// benchA2 is the second answer, a summary: the failure shapes beside
// statements the lines support, in no order of either.
var benchA2 = []benchStatement{
	{"", "The namespace's pods place the MONs on worker-2, worker-3 and worker-4 [{pods}:2-11].",
		"place the mons on worker-2, worker-3 and worker-4"},
	{"1", "The MONs a, b and c ran on worker-1, with the OSD, MGR and MDS daemons beside them [{find}:1-6].",
		"ran on worker-1"},
	{"2a", "The kubelet evicted `openshift-storage/csi-rbdplugin-k2x9p` at 08:10:02 to reclaim ephemeral-storage [{evict}:1-3].",
		"kubelet evicted"},
	{"", "At 08:12:40 the kubelet's dial to the RBD CSI socket was refused [{csi}:1].",
		"dial to the rbd csi socket was refused"},
	{"2b", "The CSI socket was then removed and the plugin's containers crashed [{csi}:1].",
		"socket was then removed and the plugin's containers crashed"},
	{"12", "Restarting `openshift-storage/csi-rbdplugin-k2x9p` fixes the refused dials to the RBD CSI socket [{csi}:1].",
		"fixes the refused dials"},
	{"3", "The `CephOSDDown` alert fired on `ceph_osd_up == 0` with a 300 s threshold.",
		"fired on ceph_osd_up == 0 with a 300 s threshold"},
	{"13", "At 08:14:11 the Rook operator deleted the pod `openshift-storage/rook-ceph-osd-3-7d9f8` [{sync}:1].",
		"the rook operator deleted the pod"},
	{"", "osd.3 logged a Terminated signal at 08:16:05 and began its shutdown [{osd}:1-2].",
		"logged a terminated signal at 08:16:05 and began its shutdown"},
	{"10", "osd.5 was never deployed on worker-1: no log of it is in the node's Ceph log directory [{find}:1-6].",
		"osd.5 was never deployed on worker-1"},
	{"14", "The cluster-wide proxy sets no noProxy list, so traffic to the cluster's internal API also goes through the proxy [{proxy}:1-8].",
		"sets no noproxy list"},
	{"4", "The OSD's shutdown at 08:16 and the ENOSPC errors at 11:55 are one continuous failure of osd.3 [{osd}:1-2, {enospc}:1-2].",
		"are one continuous failure"},
	{"", "The journal shows `rook-ceph-osd` writes to `/var/lib/rook/osd3/log` failing with ENOSPC at 11:55:31 and 11:55:32 [{enospc}:1-2].",
		"failing with enospc at 11:55:31 and 11:55:32"},
	{"9", "osd.3's writes failed with ENOSPC because `debug_osd` was left at 20/20 and its log grew until the filesystem holding /var/lib/rook was full [{enospc}:1-2, {kcs}:7-9].",
		"because debug_osd was left at 20/20"},
	{"5", "/var, the root filesystem, reached 100% utilization [{enospc}:1-2].",
		"the root filesystem, reached 100% utilization"},
	{"15", "`/var/lib/containers` on worker-1 filled from 71% at 10:02 to 96% by 12:05 [{df1}:2-4, {df2}:2-4].",
		"filled from 71% at 10:02 to 96% by 12:05"},
	{"6", "osd.3 shut down because it lost its heartbeat to osd.5 [{osdlog}:2-3].",
		"shut down because it lost its heartbeat"},
	{"11", "The MGR's start at 08:20:14 put the MDS into its restart back-off at 08:21:40 [{events}:3-12].",
		"put the mds into its restart back-off"},
	{"16", "Delete the pod `rook-ceph-mds-fs-a-9d0e1` so it is rescheduled off worker-1, whose full disk crashed its `mds` container [{events}:8-12].",
		"whose full disk crashed its mds container"},
	{"8", "This confirms that the ephemeral-storage pressure caused the CSI connection failures [{evict}:1-3, {csi}:1].",
		"confirms that the ephemeral-storage pressure caused"},
}

// benchA3 is the control, the third answer: every statement supported as
// worded, none seeded, so the false findings on it have a denominator of
// their own. Two are what a check may still flag: the one citing only a
// listing, and the solution's root cause, which is the solution's.
var benchA3 = []benchStatement{
	{"", "The namespace's events show the `mgr` container started in the pod `rook-ceph-mgr-a-6b7c8` on worker-1 at 08:20:14 [{events}:3-7].",
		"started in the pod rook-ceph-mgr-a-6b7c8"},
	{"", "At 08:21:40 an event from worker-1's kubelet reported a back-off restarting the failed `mds` container of `rook-ceph-mds-fs-a-9d0e1` [{events}:8-12].",
		"reported a back-off restarting the failed mds container"},
	{"", "At 08:14:11 the kubelet logged an update from the API for the pod `openshift-storage/rook-ceph-osd-3-7d9f8`; the line does not say what changed [{sync}:1].",
		"logged an update from the api for the pod"},
	{"", "The knowledge-base solution gives `debug_osd` left at 20/20 after a troubleshooting session as the root cause in the environment it describes [{kcs}:7-8].",
		"as the root cause in the environment it describes"},
	{"", "The pod list gives `rook-ceph-mon-a-5c6d7`, `rook-ceph-mon-b-8f9a0` and `rook-ceph-mon-c-1b2c3` the phase Running, on worker-2, worker-3 and worker-4 [{pods}:3-11].",
		"the phase running, on worker-2, worker-3 and worker-4"},
	{"", "Worker-1's Ceph log directory lists six log files, named for mds.fs-a, mgr.a, mon.a, mon.b, mon.c and osd.3 [{find}:1-6].",
		"lists six log files"},
}

// benchA4 is the fourth answer, which the reviewer alone reviews, without
// and then with the ranked evidence it does not cite: shape 17, the
// eviction ranking of the CSI plugin's pod, which the conversation read
// and a1 and a2 cited, dismissed as never logged, beside two statements
// its cited lines support. benchA4Against is the role of the evidence
// that contradicts it.
var benchA4 = []benchStatement{
	{"", "At 08:12:40 the kubelet's dial to the RBD CSI plugin's socket, `/var/lib/kubelet/plugins/rbd.csi.ceph.com/csi.sock`, was refused [{csi}:1].",
		"dial to the rbd csi plugin's socket"},
	{"17", "The kubelet logged nothing about the CSI plugin's pod before the refused dial, so the plugin's failure came without warning [{csi}:1].",
		"logged nothing about the csi plugin's pod before the refused dial"},
	{"", "osd.3 logged a Terminated signal at 08:16:05 and began its shutdown [{osd}:1-2].",
		"logged a terminated signal at 08:16:05 and began its shutdown"},
}

const benchA4Against = "evict"

// benchDraftStatement is a statement of the customer draft as written,
// citing nothing, and the roles of the evidence whose lines, cited by a1,
// a2 or a3, hold it; none for the one no evidence holds.
type benchDraftStatement struct {
	text    string
	sources []string
}

// benchDraft is the customer draft written from a1, a2 and a3, citing
// nothing, as drafts in live use do: given the items those answers cite,
// the editor can cite the lines holding each of the first four, in any of
// the items named; the last, which no item holds, it can only anchor or
// flag.
var benchDraft = []benchDraftStatement{
	{"At 08:10:02 the kubelet on worker-1 ranked the CSI plugin's pod for eviction to reclaim ephemeral storage.", []string{"evict"}},
	{"At 08:12:40 the kubelet could not connect to the RBD CSI plugin's socket.", []string{"csi"}},
	{"The osd.3 daemon received a Terminated signal at 08:16:05 and began shutting down.", []string{"osd", "osdlog"}},
	{"Writes to the OSD's log under /var/lib/rook failed with no space left on device at 11:55.", []string{"enospc"}},
	{"The monitoring alert for the OSD fired at 08:17.", nil},
}

// benchDraftAnswer is the draft as the main model writes it.
func benchDraftAnswer() []benchStatement {
	out := make([]benchStatement, len(benchDraft))
	for i, d := range benchDraft {
		out[i] = benchStatement{text: d.text}
	}
	return out
}

const (
	benchAsk1 = "What happened on worker-1?"
	benchAsk2 = "Summarize where it stands for the customer update."
	benchAsk3 = "List what the evidence read shows of the MGR, the MDS and the OSD's pod, as it is written."
	benchAsk4 = "Did anything on worker-1 warn of the refused dial to the CSI socket?"
)

// benchRoles name the replay's reads by their call IDs, as the answers'
// {role} names them; kcs is the knowledge-base solution fetched.
var benchRoles = map[string]string{"c1": "find", "c2": "pods", "c3": "evict", "c4": "csi", "c5": "osd", "c6": "enospc", "c7": "osdlog",
	"c8": "events", "c9": "sync", "c10": "kcs", "c12": "proxy", "c13": "df1", "c14": "df2"}

// benchSolution is the knowledge-base solution the replay fetches, its
// page synthetic: a precedent, about another environment than the case's.
const (
	benchSolutionID   = 7001234
	benchSolutionPage = "../testdata/replay/references/kcs-solution-7001234.txt"
)

// benchConsult is the consulted expert's answer, a model's reading
// qualified as one: shape 13 states it as observed.
const benchConsult = "The update at 08:14:11 may be the Rook operator deleting the pod `openshift-storage/rook-ceph-osd-3-7d9f8`; the kubelet's line says only that the API updated it, not what changed or who changed it."

// benchText is an answer as the main model writes it, a list.
func benchText(ss []benchStatement) string {
	var b strings.Builder
	for _, s := range ss {
		b.WriteString("- " + s.text + "\n")
	}
	return b.String()
}

// benchMain is the main model's part, played by the fixture: the replay's
// reads, the solution's fetch and the consult, then a1, a2 and a3, each
// its turn's, and a4 whenever benchAsk4 is asked; asked for a revision,
// the answer it gave last again, which cannot fulfil an anchor; consulted,
// benchConsult. It counts the revisions asked.
type benchMain struct {
	mu        sync.Mutex
	names     map[string]string
	turn      int
	last      []benchStatement
	revisions int
}

func (m *benchMain) fill(s string) string {
	for role, name := range m.names {
		s = strings.ReplaceAll(s, "{"+role+"}", name)
	}
	return s
}

// isConsult reports whether a request is a consult's, its question last.
func isConsult(r model.Request) bool {
	if len(r.Messages) == 0 {
		return false
	}
	q := r.Messages[len(r.Messages)-1].Text
	return strings.Contains(q, "\n\n## Your guidance: ") && strings.Contains(q, "\n\n## The question\n\n")
}

func (m *benchMain) respond(_ context.Context, r model.Request) (*modelfixture.Answer, error) {
	if isConsult(r) {
		return modelfixture.Text(benchConsult), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if isRevision(r) {
		m.revisions++
		return modelfixture.Text(m.fill(benchText(m.last))), nil
	}
	if strings.Contains(r.Messages[len(r.Messages)-1].Text, benchAsk4) {
		m.last = benchA4
		return modelfixture.Text(m.fill(benchText(m.last))), nil
	}
	m.turn++
	switch m.turn {
	case 1:
		return modelfixture.Calling(
			call("c1", "find", map[string]any{"path": "nodes/worker-1/var/log/ceph"}),
			call("c2", "read", map[string]any{"path": "namespaces/openshift-storage/pods.yaml"}),
			call("c3", "grep", map[string]any{"pattern": "Eviction manager", "path": "nodes/worker-1/kubelet.log"}),
			call("c4", "grep", map[string]any{"pattern": "connection refused", "path": "nodes/worker-1/kubelet.log"}),
			call("c5", "grep", map[string]any{"pattern": "Terminated|shutdown", "path": "nodes/worker-1/var/log/ceph/ceph-osd.3.log"}),
			call("c6", "grep", map[string]any{"pattern": "no space left", "path": "nodes/worker-1/journal.log"}),
			call("c7", "read", map[string]any{"path": "nodes/worker-1/var/log/ceph/ceph-osd.3.log"}),
			call("c8", "read", map[string]any{"path": "namespaces/openshift-storage/events.yaml"}),
			call("c9", "grep", map[string]any{"pattern": "SyncLoop UPDATE", "path": "nodes/worker-1/kubelet.log"}),
			call("c10", "kcs_fetch", map[string]any{"id": float64(benchSolutionID), "kind": "solution"}),
			call("c11", consultTool, map[string]any{"expert": "openshift.platform",
				"question": "What does the kubelet's SyncLoop UPDATE of openshift-storage/rook-ceph-osd-3-7d9f8 at 08:14:11 on worker-1 show?"}),
			call("c12", "read", map[string]any{"path": "cluster-scoped-resources/config.openshift.io/proxies/cluster.yaml", "head": float64(8)}),
			call("c13", "read", map[string]any{"path": "nodes/worker-1/df.txt"}),
			call("c14", "read", map[string]any{"path": "nodes/worker-2/df.txt"}),
		), nil
	case 2:
		for id, res := range lastResults(r) {
			if role, ok := benchRoles[id]; ok {
				if ev := ExcerptEvidence(res.Text); len(ev) == 1 {
					m.names[role] = ev[0]
				}
			}
		}
		m.last = benchA1
	case 3:
		m.last = benchA2
	case 4:
		m.last = benchA3
	case 5:
		m.last = benchDraftAnswer()
	default:
		return modelfixture.Text("done"), nil
	}
	return modelfixture.Text(m.fill(benchText(m.last))), nil
}

// benchCandidate is a model a check is benchmarked on.
type benchCandidate struct {
	provider, model, effort string
}

func (c benchCandidate) String() string {
	if c.effort == "" {
		return c.model
	}
	return c.model + " (" + c.effort + ")"
}

// benchReplay is the stub case on the replay, its knowledge base the
// solution's page, after the first turn: the replay read, the solution
// fetched, the expert consulted and a1 answered, unchecked.
func benchReplay(t *testing.T) (*Session, *benchMain) {
	t.Helper()
	m := &benchMain{names: map[string]string{}}
	s, _ := companionSession(t, m.respond)
	td, err := filepath.Abs("../testdata/replay/epistemic")
	if err != nil {
		t.Fatal(err)
	}
	s.c.root = td
	page, err := os.ReadFile(benchSolutionPage)
	if err != nil {
		t.Fatal(err)
	}
	s.e.references = map[string]ReferenceAdapter{"kcs": &fakeReferences{id: "kcs", pages: map[int]ReferenceDocument{1: {
		Kind: "solution", Title: str("Ceph OSD writes fail with ENOSPC when OSD debug logging is left raised"), State: str("Verified"),
		Modified: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC), Body: string(page)}}}}
	// The calls' records time them on the wall clock, not the stub's.
	s.c.now = time.Now
	if _, err := s.Talk(context.Background(), benchAsk1, nil); err != nil {
		t.Fatalf("the replay's first turn: %v", err)
	}
	if len(m.names) != len(benchRoles) {
		t.Fatalf("the replay's reads stored %v; want one item each", m.names)
	}
	return s, m
}

// benchSession is benchReplay with the live model configured as "live"
// for no task yet.
func benchSession(t *testing.T, c benchCandidate, p model.Provider) (*Session, *benchMain) {
	t.Helper()
	s, m := benchReplay(t)
	s.e.config.Reasoning.MaxEgressClass = "external_provider"
	s.e.config.Reasoning.Models["live"] = config.Model{Provider: c.provider, Model: c.model}
	s.e.models[c.provider] = p
	return s, m
}

// benchResult is one check of one answer as it ended.
type benchResult struct {
	ran bool
	// failed says why the check gave no findings; refused that the model
	// was refused by the project's policy or quota.
	failed  string
	refused bool
	// outcome is how the engine kept the check, where it did.
	outcome  string
	findings check.Findings
	dropped  []check.Rejected
	// raw are the findings as the model wrote them in the call accepted.
	raw            []any
	calls          int
	in, out, think int64
	cost           float64
	unpriced       bool
	dur            time.Duration
	// trace is the check's conversation after its request, in short;
	// refusedCalls counts its calls whose findings were refused whole.
	trace        []string
	refusedCalls int
}

// benchUsage fills r from the records of the check's conversation conv,
// madeBy the call whose findings were accepted.
func benchUsage(t *testing.T, s *Session, conv, madeBy string, r *benchResult) {
	t.Helper()
	if conv == "" {
		return
	}
	rows, err := s.c.st.DB().Query(`SELECT record FROM invocations WHERE task_type IN (?, ?)`, EditorTask, ReviewTask)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	known := func(c model.Count) int64 {
		if c.Known {
			return c.Value
		}
		return 0
	}
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			t.Fatal(err)
		}
		var rec InvocationRecord
		if err := json.Unmarshal([]byte(j), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.ConversationID != conv {
			continue
		}
		r.calls++
		u := rec.Usage
		r.in += known(u.InputTokens) + known(u.CacheReadTokens) + known(u.CacheWriteTokens)
		r.out += known(u.OutputTokens)
		r.think += known(u.ReasoningTokens)
		if rec.Cost.Amount != nil {
			r.cost += *rec.Cost.Amount
		} else {
			r.unpriced = true
		}
		r.dur += rec.Duration
		if rec.InvocationID == madeBy {
			for _, tc := range rec.ToolCalls {
				if fs, ok := tc.Params["findings"].([]any); ok && tc.Test == check.ToolName {
					r.raw = fs
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	msgs, err := (companion.Store{DB: s.c.st.DB()}).Messages(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	short := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > n {
			s = s[:n] + "…"
		}
		return s
	}
	for i, m := range msgs {
		if i == 0 {
			continue
		}
		if strings.TrimSpace(m.Text) != "" {
			r.trace = append(r.trace, fmt.Sprintf("%s: %q", m.Kind, short(m.Text, 300)))
		}
		for _, c := range m.Calls {
			r.trace = append(r.trace, fmt.Sprintf("%s: calls %s", m.Kind, c.Name))
		}
		for _, res := range m.Results {
			what := "result"
			if res.IsError {
				what = "error result"
				if strings.Contains(res.Text, "findings were refused") {
					r.refusedCalls++
				}
			}
			r.trace = append(r.trace, fmt.Sprintf("%s %s: %q", res.Name, what, short(res.Text, 400)))
		}
	}
}

// benchKept is the newest check of role the engine kept.
func benchKept(t *testing.T, s *Session, role check.Role, r *benchResult) {
	t.Helper()
	var conv, madeBy, findings, dropped, outcome, why sql.NullString
	err := s.c.st.DB().QueryRow(`SELECT check_conversation_id, invocation_id, findings, dropped, outcome, error FROM checks
		WHERE role = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, string(role)).Scan(&conv, &madeBy, &findings, &dropped, &outcome, &why)
	if err != nil {
		t.Fatalf("the %s's check was not kept: %v", role, err)
	}
	r.ran, r.outcome = true, outcome.String
	if err := json.Unmarshal([]byte(findings.String), &r.findings); findings.Valid && findings.String != "" && err != nil {
		t.Fatalf("the kept findings: %v", err)
	}
	if err := json.Unmarshal([]byte(dropped.String), &r.dropped); dropped.Valid && dropped.String != "" && err != nil {
		t.Fatalf("the kept dropped elements: %v", err)
	}
	if why.String != "" && (outcome.String == checkUnchecked || outcome.String == checkFailed) {
		r.failed = why.String
	}
	benchUsage(t, s, conv.String, madeBy.String, r)
}

// benchFailure sorts a failed check: refused by the project's policy or
// quota; the model's own failing — no findings the engine took, output
// cut, a bound passed; or else the engine's or the transport's, which
// fails the test.
func benchFailure(t *testing.T, r *benchResult) {
	t.Helper()
	if r.failed == "" {
		return
	}
	for _, w := range []string{"PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "Error 403", "Error 429", "403 Forbidden", "429 Too Many", "quota", "Quota", "organization policy", "constraints/", "NOT_FOUND", "Error 404", "404 Not Found"} {
		if strings.Contains(r.failed, w) {
			r.refused = true
			return
		}
	}
	for _, w := range []string{"without calling " + check.ToolName, "findings were refused", "calls are spent", model.ErrOutputLimit.Error(), model.ErrStopped.Error(), "bound"} {
		if strings.Contains(r.failed, w) {
			return
		}
	}
	t.Errorf("the check failed in the engine or its transport: %s", r.failed)
}

// benchRun is one run of a candidate: the editor's checks of a1, a2, a3
// and the draft, or the reviewer's of a2, a3 and a4, a4 without and then
// with the ranked evidence it does not cite (a4r); revisions are the
// revision calls a2's anchors made, revisions3 a3's; draftAdded the
// citations with lines the draft's revision added; names the replay's
// evidence names, by role.
type benchRun struct {
	cand                       benchCandidate
	n                          int
	a1, a2, a3, draft, a4, a4r benchResult
	revisions, revisions3      int
	draftAdded                 int
	names                      map[string]string
	stopped                    string
}

func benchEditorRun(t *testing.T, c benchCandidate, p model.Provider) benchRun {
	t.Helper()
	ctx := context.Background()
	s, m := benchSession(t, c, p)
	s.e.config.Reasoning.Profiles["editor"] = config.Profile{Model: "live", Effort: c.effort}
	pc := s.e.config.Profile(EditorTask)
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	answers := answersOf(history)
	if len(answers) != 1 {
		t.Fatalf("%d answers after the first turn; want 1", len(answers))
	}
	out := benchRun{cand: c}
	// a1 is checked alone, kept as written, so a2's check is given it as
	// the main model wrote it.
	var run checkRun
	err = s.runEditor(ctx, pc, answers[0], nil, &run, func(TalkEvent) {})
	out.a1.ran = true
	if err != nil {
		out.a1.failed = err.Error()
	}
	out.a1.findings, out.a1.dropped = run.findings, run.dropped
	if strings.TrimSpace(out.a1.findings.Corrected) == strings.TrimSpace(answers[0].text) {
		out.a1.findings.Corrected = ""
	}
	benchUsage(t, s, run.checkConv, run.madeBy, &out.a1)
	benchFailure(t, &out.a1)
	if out.a1.refused {
		return out
	}
	// a2 and then a3 through the engine's flow: the editor, and the
	// revision its anchors cause.
	if out.a2, out.revisions = benchFlow(t, s, m, benchAsk2); !out.a2.ran || out.a2.refused {
		return out
	}
	if out.a3, out.revisions3 = benchFlow(t, s, m, benchAsk3); !out.a3.ran || out.a3.refused {
		return out
	}
	out.draft, out.draftAdded = benchDraftFlow(t, s)
	m.mu.Lock()
	out.names = maps.Clone(m.names)
	m.mu.Unlock()
	return out
}

// benchDraftFlow is the editor's check of the customer draft written from
// a1, a2 and a3, through the engine's flow, as kept, with the citations
// naming lines its revision added.
func benchDraftFlow(t *testing.T, s *Session) (benchResult, int) {
	t.Helper()
	s.e.config.Comments.Intentions = []config.Intention{{ID: "customer-update", Label: "Customer update", Audience: "customer", Purpose: "Tell the customer what was found."}}
	var r benchResult
	_, reply, err := s.Draft(context.Background(), "customer-update", nil)
	if err != nil {
		t.Errorf("the draft failed: %v", err)
		return r, 0
	}
	benchKept(t, s, check.Editor, &r)
	benchFailure(t, &r)
	return r, len(reply.Check.Added)
}

// benchDraftScore is what the editor made of the draft: of the statements
// the answers' cited lines hold, how many its correction cites with the
// item that holds each, anchors, or flags otherwise; and of the last,
// which no item holds, whether it was cited (wrongly), anchored, flagged
// or missed. paired says the correction's statements pair with the
// draft's, without which none is counted cited.
type benchDraftScore struct {
	cited, anchored, flagged int
	last                     string
	paired                   bool
}

func scoreDraft(r benchResult, names map[string]string) benchDraftScore {
	sc := benchDraftScore{last: "-"}
	var corrected []check.Statement
	if r.findings.Corrected != "" {
		corrected = check.Split(r.findings.Corrected)
	}
	sc.paired = len(corrected) == len(benchDraft)
	for i, d := range benchDraft {
		id := "s" + strconv.Itoa(i+1)
		anchored, flagged := false, false
		for _, f := range r.findings.Findings {
			if f.Statement == id {
				anchored = anchored || f.Anchor
				flagged = flagged || !f.Anchor
			}
		}
		cites := []Cite(nil)
		if sc.paired {
			cites = Cites(corrected[i].Text)
		}
		if len(d.sources) == 0 {
			switch {
			case len(cites) > 0:
				sc.last = "cited"
			case anchored:
				sc.last = "a"
			case flagged:
				sc.last = "f"
			}
			continue
		}
		switch {
		case slices.ContainsFunc(cites, func(c Cite) bool {
			return slices.ContainsFunc(d.sources, func(r string) bool { return c.Name == names[r] })
		}):
			sc.cited++
		case anchored:
			sc.anchored++
		case flagged:
			sc.flagged++
		}
	}
	return sc
}

// benchFlow is the editor's check of the answer to ask, through the
// engine's flow, as kept, with the revision calls its anchors made: one
// where it has any.
func benchFlow(t *testing.T, s *Session, m *benchMain, ask string) (benchResult, int) {
	t.Helper()
	m.mu.Lock()
	before := m.revisions
	m.mu.Unlock()
	var r benchResult
	reply, err := s.Talk(context.Background(), ask, nil)
	if err != nil {
		t.Errorf("the turn %q failed: %v", ask, err)
		return r, 0
	}
	benchKept(t, s, check.Editor, &r)
	benchFailure(t, &r)
	m.mu.Lock()
	revisions := m.revisions - before
	m.mu.Unlock()
	anchors := 0
	for _, f := range r.findings.Findings {
		if f.Anchor {
			anchors++
		}
	}
	if want := min(anchors, 1); r.failed == "" && revisions != want {
		t.Errorf("%d anchors made %d revision calls; want %d", anchors, revisions, want)
	}
	if reply.Check.Unchecked != "" && r.failed == "" {
		t.Errorf("the reply says unchecked (%s) but the check was kept as %s", reply.Check.Unchecked, r.outcome)
	}
	return r, revisions
}

func benchReviewerRun(t *testing.T, c benchCandidate, p model.Provider) benchRun {
	t.Helper()
	ctx := context.Background()
	s, m := benchSession(t, c, p)
	if _, err := s.Talk(ctx, benchAsk2, nil); err != nil {
		t.Fatalf("the replay's second turn: %v", err)
	}
	s.e.config.Reasoning.Profiles["reviewer"] = config.Profile{Model: "live", Effort: c.effort}
	out := benchRun{cand: c}
	m.mu.Lock()
	out.names = maps.Clone(m.names)
	m.mu.Unlock()
	if out.a2 = benchReview(t, s, ReviewAsk{}); out.a2.refused {
		return out
	}
	// a3 is answered after a2's review, which the main model reads.
	if _, err := s.Talk(ctx, benchAsk3, nil); err != nil {
		t.Fatalf("the replay's third turn: %v", err)
	}
	if out.a3 = benchReview(t, s, ReviewAsk{}); out.a3.refused {
		return out
	}
	if _, err := s.Talk(ctx, benchAsk4, nil); err != nil {
		t.Fatalf("the replay's fourth turn: %v", err)
	}
	if out.a4 = benchReview(t, s, ReviewAsk{}); out.a4.refused {
		return out
	}
	out.a4r = benchReview(t, s, ReviewAsk{Ranked: true})
	return out
}

// benchReview is the reviewer's review of the last answer, as ask asks
// it, as kept.
func benchReview(t *testing.T, s *Session, ask ReviewAsk) benchResult {
	t.Helper()
	var r benchResult
	_, err := s.Review(context.Background(), ask, nil)
	benchKept(t, s, check.Reviewer, &r)
	if err != nil && r.failed == "" {
		r.failed = err.Error()
	}
	benchFailure(t, &r)
	return r
}

// benchOmission is a4's shape 17 as a review marked it: o, a finding on
// its statement names the evidence that contradicts it, which a4 does not
// cite (the omission named), in its evidence or in its words — a review
// not given that evidence has no name for it; f, a finding names the
// statement otherwise; d, only a finding validation dropped names it; -,
// missed.
func benchOmission(r benchResult, names map[string]string) string {
	id := ""
	for i, st := range benchA4 {
		if st.shape == "17" {
			id = "s" + strconv.Itoa(i+1)
		}
	}
	against := regexp.MustCompile(`\b` + regexp.QuoteMeta(names[benchA4Against]) + `\b`)
	for _, f := range r.findings.Findings {
		if f.Statement != id {
			continue
		}
		for _, ref := range f.Evidence {
			if name, _, _ := strings.Cut(ref, ":"); name == names[benchA4Against] {
				return "o"
			}
		}
		if against.MatchString(f.Why + "\n" + f.Alternative + "\n" + f.Discriminator) {
			return "o"
		}
	}
	return scoreBench(benchA4, r).marks["17"]
}

// benchScore is what a check made of an answer's statements.
type benchScore struct {
	// marks are each seeded shape's: a, an anchor names its statement; f,
	// a finding does; c, the editor's corrected text changes its
	// offending words with no finding naming it; d, only a finding
	// validation dropped names it; -, missed.
	marks  map[string]string
	caught int
	// falses are the supported statements a finding names, falseDropped
	// those only a finding validation dropped names; rewrote those whose
	// words the editor's corrected text changes.
	falses, falseDropped, rewrote []string
	anchors                       int
	// formed counts the reviewer's findings with an alternative and a
	// discriminator, the form a review asks for.
	formed int
}

var benchDroppedFinding = regexp.MustCompile(`^findings\[(\d+)\]$`)

// benchNorm is text as words are looked for in it: lower case, its code
// spans' backticks gone, its spaces single.
func benchNorm(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(s, "`", ""))), " ")
}

func scoreBench(ss []benchStatement, r benchResult) benchScore {
	sc := benchScore{marks: map[string]string{}}
	named := map[string]string{}
	for _, f := range r.findings.Findings {
		mark := "f"
		if f.Anchor {
			mark = "a"
			sc.anchors++
		}
		if named[f.Statement] != "a" {
			named[f.Statement] = mark
		}
		if f.Alternative != "" && f.Discriminator != "" {
			sc.formed++
		}
	}
	dropped := map[string]bool{}
	for _, d := range r.dropped {
		m := benchDroppedFinding.FindStringSubmatch(d.Element)
		if m == nil {
			continue
		}
		if i, _ := strconv.Atoi(m[1]); i < len(r.raw) {
			if f, ok := r.raw[i].(map[string]any); ok {
				if id, ok := f["statement"].(string); ok {
					dropped[id] = true
				}
			}
		}
	}
	corrected := benchNorm(r.findings.Corrected)
	for i, st := range ss {
		id := "s" + strconv.Itoa(i+1)
		changed := corrected != "" && !strings.Contains(corrected, st.words)
		if st.shape == "" {
			switch {
			case named[id] != "":
				sc.falses = append(sc.falses, id)
			case dropped[id]:
				sc.falseDropped = append(sc.falseDropped, id)
			}
			if changed {
				sc.rewrote = append(sc.rewrote, id)
			}
			continue
		}
		mark := "-"
		switch {
		case named[id] != "":
			mark = named[id]
		case changed:
			mark = "c"
		case dropped[id]:
			mark = "d"
		}
		if mark == "a" || mark == "f" || mark == "c" {
			sc.caught++
		}
		sc.marks[st.shape] = mark
	}
	return sc
}

// falseCell is a check's false findings in a table's cell, with those
// validation dropped.
func (sc benchScore) falseCell() string {
	s := strconv.Itoa(len(sc.falses))
	if n := len(sc.falseDropped); n > 0 {
		s += fmt.Sprintf(" (+%d dropped)", n)
	}
	return s
}

// benchSpend keeps the run's estimated spend under its cap: no run starts
// once the spend so far and the dearest run yet would pass it.
type benchSpend struct {
	mu          sync.Mutex
	total, most float64
}

const benchCap = 5.0

func (b *benchSpend) add(run float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += run
	b.most = max(b.most, run)
}

func (b *benchSpend) allows() (bool, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total+b.most <= benchCap, b.total
}

// benchSplits fails t unless each replay answer splits into its
// statements as written, s1 first, in order.
func benchSplits(t *testing.T) {
	t.Helper()
	for _, a := range []struct {
		name string
		ss   []benchStatement
	}{{"a1", benchA1}, {"a2", benchA2}, {"a3", benchA3}, {"a4", benchA4}} {
		got := check.Split(benchText(a.ss))
		if len(got) != len(a.ss) {
			t.Fatalf("%s splits into %d statements; want %d", a.name, len(got), len(a.ss))
		}
		for i, st := range got {
			if st.Text != a.ss[i].text || st.ID != "s"+strconv.Itoa(i+1) {
				t.Fatalf("%s's %s is %q; want %q", a.name, st.ID, st.Text, a.ss[i].text)
			}
		}
	}
}

// TestLiveCheckReplay benchmarks the epistemic checks' candidate models on
// the replay, COMPA_LIVE_RUNS times each (default 1): the editor on
// gemini-3.5-flash-lite and gemini-3.8-flash at low, checking a1 alone
// and then a2 and a3 through the engine's flow, the revision their
// anchors cause answered by the fixture; the reviewer on
// claude-sonnet-4-6, gemini-3.8-flash at low and claude-haiku-4-5,
// reviewing a2, a3, and a4 without and then with the ranked evidence it
// does not cite. COMPA_LIVE_MODELS, a comma-separated list of model IDs,
// keeps only the candidates on them. Scored per candidate and run, one
// table per check logged and written as Markdown to COMPA_LIVE_REPORT where
// set: each shape caught or missed, a4's contradiction named or not,
// false findings on the supported statements of a1, a2 and a4 and, apart,
// on the control a3, anchors, calls, tokens, the estimated cost and the
// time. What it shows about the engine: each candidate's requests and its
// check_findings call are accepted by the provider and validated, kept
// and priced, and an anchor makes exactly one revision call. The scores
// are the models', never a gate: the test fails only on an engine or
// transport error. A model refused by the project's policy or quota is
// recorded and its runs end; no run starts past $5 estimated in all.
func TestLiveCheckReplay(t *testing.T) {
	project, location := modeltest.Vertex(t)
	runs := 1
	if v := os.Getenv("COMPA_LIVE_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("COMPA_LIVE_RUNS=%q: want a count of 1 or more", v)
		}
		runs = n
	}
	benchSplits(t)
	// The bounds are raised so a slow check is still scored; the report
	// says which checks passed the bounds the engine ships with.
	shippedEditor, shippedReviewer := editorBound, reviewerBound
	editorBound, reviewerBound = 3*time.Minute, 5*time.Minute
	t.Cleanup(func() { editorBound, reviewerBound = shippedEditor, shippedReviewer })

	providers := map[string]func(ctx context.Context) (model.Provider, error){
		vertexgemini.ID: func(ctx context.Context) (model.Provider, error) {
			return vertexgemini.New(ctx, vertexgemini.Options{Project: project, Location: location, CacheTTL: 5 * time.Minute})
		},
		vertexanthropic.ID: func(ctx context.Context) (model.Provider, error) {
			return vertexanthropic.New(ctx, vertexanthropic.Options{Project: project, Location: location})
		},
	}
	editors := []benchCandidate{
		{vertexgemini.ID, "gemini-3.5-flash-lite", "low"},
		{vertexgemini.ID, "gemini-3.8-flash", "low"},
	}
	reviewers := []benchCandidate{
		{vertexanthropic.ID, "claude-sonnet-4-6", ""},
		{vertexgemini.ID, "gemini-3.8-flash", "low"},
		{vertexanthropic.ID, "claude-haiku-4-5", ""},
	}
	if v := os.Getenv("COMPA_LIVE_MODELS"); v != "" {
		keep := strings.Split(v, ",")
		only := func(cs []benchCandidate) []benchCandidate {
			return slices.DeleteFunc(cs, func(c benchCandidate) bool { return !slices.Contains(keep, c.model) })
		}
		if editors, reviewers = only(editors), only(reviewers); len(editors)+len(reviewers) == 0 {
			t.Fatalf("COMPA_LIVE_MODELS=%q names no candidate", v)
		}
	}
	var spend benchSpend
	var mu sync.Mutex
	results := map[string][][]benchRun{"editor": make([][]benchRun, len(editors)), "reviewer": make([][]benchRun, len(reviewers))}
	bench := func(t *testing.T, role string, i int, c benchCandidate, one func(*testing.T, benchCandidate, model.Provider) benchRun) {
		t.Helper()
		t.Parallel()
		p, err := providers[c.provider](context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= runs; n++ {
			var r benchRun
			if ok, so := spend.allows(); !ok {
				r = benchRun{cand: c, n: n, stopped: fmt.Sprintf("not run: $%.4f spent, the next run could pass the $%.0f cap", so, benchCap)}
				t.Log(r.stopped)
			} else {
				t.Run("run"+strconv.Itoa(n), func(t *testing.T) {
					r = one(t, c, p)
				})
				r.cand, r.n = c, n
				spend.add(r.a1.cost + r.a2.cost + r.a3.cost + r.draft.cost + r.a4.cost + r.a4r.cost)
			}
			mu.Lock()
			results[role][i] = append(results[role][i], r)
			mu.Unlock()
			if r.a1.refused || r.a2.refused || r.a3.refused || r.a4.refused || r.a4r.refused || r.stopped != "" {
				break
			}
		}
	}
	t.Run("checks", func(t *testing.T) {
		for i, c := range editors {
			t.Run("editor/"+c.model, func(t *testing.T) { bench(t, "editor", i, c, benchEditorRun) })
		}
		for i, c := range reviewers {
			t.Run("reviewer/"+c.model, func(t *testing.T) { bench(t, "reviewer", i, c, benchReviewerRun) })
		}
	})

	var md strings.Builder
	fmt.Fprintf(&md, "# Live check replay\n\n%s · TestLiveCheckReplay · COMPA_LIVE_RUNS=%d · location %s · estimated spend $%.4f\n\n",
		time.Now().Format("2006-01-02 15:04 MST"), runs, location, spend.total)
	md.WriteString("Shapes: ")
	for i, s := range benchShapes {
		if i > 0 {
			md.WriteString("; ")
		}
		md.WriteString(s.id + " " + s.name)
	}
	md.WriteString(". Marks: a, an anchor names the statement; f, a finding names it; c, the editor's corrected text changes its offending words, no finding naming it; d, only a finding validation dropped names it (not caught); -, missed. " +
		"false a1, false a2: supported statements of a1 and a2 a finding names, with (+N dropped) those only a finding validation dropped names. false a3: the same of a3, the control, all " + strconv.Itoa(len(benchA3)) + " of its statements supported and none seeded. " +
		"17 a4, 17 a4 ranked: the reviewer's review of a4 without and with the ranked evidence a4 does not cite, shape 17 marked o where a finding on it rests on the lines that contradict it, else as above; false a4 and false a4 ranked its supported statements a finding names. " +
		"rewrote: supported statements whose words the corrected text changes. dropped: elements validation dropped. s: the check's calls' time as recorded. in: input tokens, cache reads and writes included; out: output; think: reasoning. Cost is the price table's estimate.\n\n")
	md.WriteString(benchEditorTables(results["editor"], shippedEditor))
	md.WriteString(benchDraftTable(results["editor"]))
	md.WriteString(benchReviewerTables(results["reviewer"], shippedReviewer))
	md.WriteString(benchDetails(append(results["editor"], results["reviewer"]...)))
	for _, line := range strings.Split(md.String(), "\n") {
		t.Log(line)
	}
	if path := os.Getenv("COMPA_LIVE_REPORT"); path != "" {
		if err := os.WriteFile(path, []byte(md.String()), 0o644); err != nil {
			t.Errorf("the report: %v", err)
		}
	}
}

// benchOutcome is how a check ended in a table's cell.
func benchOutcome(r benchResult) string {
	switch {
	case r.refused:
		return "refused"
	case r.failed != "":
		return "failed"
	case r.outcome != "":
		return r.outcome
	case r.ran:
		return "ok"
	}
	return "not run"
}

func benchCost(r benchResult) string {
	s := fmt.Sprintf("$%.4f", r.cost)
	if r.unpriced {
		s += "?"
	}
	return s
}

func benchSecs(d time.Duration) string { return fmt.Sprintf("%.1f", d.Seconds()) }

// benchStats is a mean with its least and greatest.
type benchStats struct{ vals []float64 }

func (b *benchStats) add(v float64) { b.vals = append(b.vals, v) }

func (b benchStats) mean() float64 {
	if len(b.vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range b.vals {
		sum += v
	}
	return sum / float64(len(b.vals))
}

func (b benchStats) String() string {
	if len(b.vals) == 0 {
		return "–"
	}
	lo, hi := b.vals[0], b.vals[0]
	for _, v := range b.vals {
		lo, hi = min(lo, v), max(hi, v)
	}
	if lo == hi {
		return strconv.FormatFloat(lo, 'f', -1, 64)
	}
	return fmt.Sprintf("%.2g (%g–%g)", b.mean(), lo, hi)
}

// benchUse accumulates checks' usage, per check.
type benchUse struct {
	checks, over, refused int
	calls, in, out, th    benchStats
	cost, secs            benchStats
}

func (u *benchUse) add(r benchResult, bound time.Duration) {
	if r.calls == 0 {
		return
	}
	u.checks++
	u.refused += r.refusedCalls
	u.calls.add(float64(r.calls))
	u.in.add(float64(r.in))
	u.out.add(float64(r.out))
	u.th.add(float64(r.think))
	u.cost.add(r.cost)
	u.secs.add(r.dur.Seconds())
	if r.dur > bound {
		u.over++
	}
}

func (u benchUse) cells() string {
	if u.checks == 0 {
		return "– | – | – | – | – | – | – |"
	}
	maxSecs := 0.0
	for _, v := range u.secs.vals {
		maxSecs = max(maxSecs, v)
	}
	return fmt.Sprintf("%.1f | %d | %.0f | %.0f | %.0f | $%.4f | %.1f (max %.1f) |", u.calls.mean(), u.refused, u.in.mean(), u.out.mean(), u.th.mean(), u.cost.mean(), u.secs.mean(), maxSecs)
}

func benchShapeHeader() (string, string) {
	var h, sep strings.Builder
	for _, s := range benchShapes {
		h.WriteString(" " + s.id + " |")
		sep.WriteString("---|")
	}
	return h.String(), sep.String()
}

// benchTotal is checks' usage summed: calls, tokens, cost and time, the
// calls refused and the elements dropped.
func benchTotal(rs ...benchResult) benchResult {
	var t benchResult
	for _, r := range rs {
		t.calls += r.calls
		t.in, t.out, t.think = t.in+r.in, t.out+r.out, t.think+r.think
		t.cost += r.cost
		t.unpriced = t.unpriced || r.unpriced
		t.dur += r.dur
		t.refusedCalls += r.refusedCalls
		t.dropped = append(t.dropped, r.dropped...)
	}
	return t
}

// benchScored says a check ran and gave findings to score.
func benchScored(r benchResult) bool { return r.ran && r.failed == "" }

func benchEditorTables(all [][]benchRun, bound time.Duration) string {
	var b strings.Builder
	sh, ss := benchShapeHeader()
	b.WriteString("## Editor\n\nPer run: a1 checked alone; a2 and then a3, the control, through the engine's flow (outcome as kept), their anchors' revision answered by the fixture.\n\n")
	b.WriteString("| model | run | a1 | a2 | a3 |" + sh + " caught | false a1 | false a2 | false a3 | rewrote | anchors a1 | anchors a2 | anchors a3 | revisions a2 | revisions a3 | dropped | refused calls | calls | in | out | think | cost | s a1 | s a2 | s a3 |\n")
	b.WriteString("|---|---|---|---|---|" + ss + strings.Repeat("---|", 20) + "\n")
	var sum strings.Builder
	sum.WriteString("\nPer model, over its runs (a2's catches; false findings and usage per check; over: checks past the shipped " + bound.String() + " bound).\n\n")
	sum.WriteString("| model | runs | caught of " + strconv.Itoa(len(benchShapes)) + " |" + sh + " false a1 | false a2 | false a3, of " + strconv.Itoa(len(benchA3)) + " | false dropped, all runs | rewrote/check | anchors/answer | revisions | calls | refused calls, all runs | in | out | think | cost | s | over |\n")
	sum.WriteString("|---|---|---|" + ss + strings.Repeat("---|", 15) + "\n")
	for _, runs := range all {
		if len(runs) == 0 {
			continue
		}
		var caught, fa1, fa2, fa3, rew, anchors, revs benchStats
		droppedFalse := 0
		var use benchUse
		hits := map[string]int{}
		scored, notes := 0, []string{}
		for _, r := range runs {
			if r.stopped != "" {
				notes = append(notes, r.stopped)
				fmt.Fprintf(&b, "| %s | %d | %s |\n", r.cand, r.n, r.stopped)
				continue
			}
			s1, s2, s3 := scoreBench(benchA1, r.a1), scoreBench(benchA2, r.a2), scoreBench(benchA3, r.a3)
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |", r.cand, r.n, benchOutcome(r.a1), benchOutcome(r.a2), benchOutcome(r.a3))
			for _, s := range benchShapes {
				m := s2.marks[s.id]
				if !benchScored(r.a2) {
					m = "·"
				}
				b.WriteString(" " + m + " |")
			}
			tot := benchTotal(r.a1, r.a2, r.a3)
			fmt.Fprintf(&b, " %d | %s | %s | %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s | %s | %s |\n",
				s2.caught, s1.falseCell(), s2.falseCell(), s3.falseCell(), len(s1.rewrote)+len(s2.rewrote)+len(s3.rewrote),
				s1.anchors, s2.anchors, s3.anchors, r.revisions, r.revisions3,
				len(tot.dropped), tot.refusedCalls, tot.calls, tot.in, tot.out, tot.think,
				benchCost(tot), benchSecs(r.a1.dur), benchSecs(r.a2.dur), benchSecs(r.a3.dur))
			for _, x := range []benchResult{r.a1, r.a2, r.a3} {
				use.add(x, bound)
			}
			if benchScored(r.a1) {
				fa1.add(float64(len(s1.falses)))
				droppedFalse += len(s1.falseDropped)
				rew.add(float64(len(s1.rewrote)))
				anchors.add(float64(s1.anchors))
			}
			if benchScored(r.a2) {
				scored++
				caught.add(float64(s2.caught))
				fa2.add(float64(len(s2.falses)))
				droppedFalse += len(s2.falseDropped)
				rew.add(float64(len(s2.rewrote)))
				anchors.add(float64(s2.anchors))
				revs.add(float64(r.revisions))
				for id, m := range s2.marks {
					if m == "a" || m == "f" || m == "c" {
						hits[id]++
					}
				}
			}
			if benchScored(r.a3) {
				fa3.add(float64(len(s3.falses)))
				droppedFalse += len(s3.falseDropped)
				rew.add(float64(len(s3.rewrote)))
				anchors.add(float64(s3.anchors))
				revs.add(float64(r.revisions3))
			}
			for _, x := range []benchResult{r.a1, r.a2, r.a3} {
				if x.failed != "" {
					notes = append(notes, x.failed)
				}
			}
		}
		fmt.Fprintf(&sum, "| %s | %d of %d scored | %s |", runs[0].cand, scored, len(runs), caught)
		for _, s := range benchShapes {
			fmt.Fprintf(&sum, " %d/%d |", hits[s.id], scored)
		}
		fmt.Fprintf(&sum, " %s | %s | %s | %d | %s | %s | %s | %s %d |\n", fa1, fa2, fa3, droppedFalse, rew, anchors, revs, use.cells(), use.over)
		for _, n := range notes {
			fmt.Fprintf(&sum, "\n%s: %s\n", runs[0].cand, n)
		}
	}
	b.WriteString(sum.String())
	b.WriteString("\n")
	return b.String()
}

// benchDraftTable is the editor's check of the customer draft, a row a
// run and a line a model.
func benchDraftTable(all [][]benchRun) string {
	var b strings.Builder
	held := len(benchDraft) - 1
	fmt.Fprintf(&b, "## Editor on the draft\n\nPer run: the customer draft written from a1, a2 and a3, citing nothing, through the engine's flow, its anchors' revision answered by the fixture. "+
		"Of its %d statements the answers' cited lines hold: cited, those the correction cites with the item that holds it; anchored and flagged otherwise. "+
		"s%d, which no item holds: cited (wrongly), a anchored, f flagged, - missed. added: citations with lines the revision added.\n\n", held, len(benchDraft))
	b.WriteString("| model | run | outcome | cited of " + strconv.Itoa(held) + " | anchored | flagged | s" + strconv.Itoa(len(benchDraft)) + " | added | cost | s |\n|---|---|---|---|---|---|---|---|---|---|\n")
	var sum strings.Builder
	for _, runs := range all {
		var cited benchStats
		var name string
		for _, r := range runs {
			name = r.cand.String()
			if !r.draft.ran {
				continue
			}
			sc := scoreDraft(r.draft, r.names)
			fmt.Fprintf(&b, "| %s | %d | %s | %d | %d | %d | %s | %d | %s | %s |\n", name, r.n, benchOutcome(r.draft), sc.cited, sc.anchored, sc.flagged, sc.last, r.draftAdded, benchCost(r.draft), benchSecs(r.draft.dur))
			if benchScored(r.draft) {
				cited.add(float64(sc.cited))
			}
		}
		if name != "" {
			fmt.Fprintf(&sum, "- %s: cited %s of %d over %d scored runs\n", name, cited, held, len(cited.vals))
		}
	}
	b.WriteString("\n" + sum.String() + "\n")
	return b.String()
}

func benchReviewerTables(all [][]benchRun, bound time.Duration) string {
	var b strings.Builder
	sh, ss := benchShapeHeader()
	b.WriteString("## Reviewer\n\nPer run: a2 reviewed as the engineer was shown it, no editor, then a3, the control, after its turn, then a4 without and then with the ranked evidence it does not cite. 6 is not the reviewer's by design (it is given no conversation); a finding on its statement is scored all the same.\n\n")
	b.WriteString("| model | run | a2 | a3 |" + sh + " caught | false a2 | false a3 | 17 a4 | 17 a4 ranked | false a4 | false a4 ranked | in a4 | in a4 ranked | dropped | refused calls | alt+disc | findings | calls | in | out | think | cost | s a2 | s a3 |\n")
	b.WriteString("|---|---|---|---|" + ss + strings.Repeat("---|", 20) + "\n")
	var sum strings.Builder
	sum.WriteString("\nPer model, over its runs (a2's catches; a4's shape 17 found and its contradiction named, without and with the ranked evidence; false findings and usage per check; over: checks past the shipped " + bound.String() + " bound).\n\n")
	sum.WriteString("| model | runs | caught of " + strconv.Itoa(len(benchShapes)) + " |" + sh + " false a2 | false a3, of " + strconv.Itoa(len(benchA3)) + " | 17 a4 found, named | 17 a4 ranked found, named | false a4 | false a4 ranked | in a4 | in a4 ranked | false dropped, all runs | alt+disc of findings | calls | refused calls, all runs | in | out | think | cost | s | over |\n")
	sum.WriteString("|---|---|---|" + ss + strings.Repeat("---|", 18) + "\n")
	for _, runs := range all {
		if len(runs) == 0 {
			continue
		}
		var caught, fa2, fa3, fa4, fa4r, in4, in4r benchStats
		droppedFalse := 0
		var use benchUse
		hits := map[string]int{}
		scored, formed, findings, notes := 0, 0, 0, []string{}
		// omitted counts, for a4 and then a4r, the runs scored, those
		// finding shape 17 and those naming its contradiction.
		var omitted [2]struct{ scored, found, named int }
		for _, r := range runs {
			if r.stopped != "" {
				notes = append(notes, r.stopped)
				fmt.Fprintf(&b, "| %s | %d | %s |\n", r.cand, r.n, r.stopped)
				continue
			}
			s2, s3, s4, s4r := scoreBench(benchA2, r.a2), scoreBench(benchA3, r.a3), scoreBench(benchA4, r.a4), scoreBench(benchA4, r.a4r)
			fmt.Fprintf(&b, "| %s | %d | %s | %s |", r.cand, r.n, benchOutcome(r.a2), benchOutcome(r.a3))
			for _, s := range benchShapes {
				m := s2.marks[s.id]
				if r.a2.failed != "" {
					m = "·"
				}
				b.WriteString(" " + m + " |")
			}
			var marks [2]string
			for i, x := range []benchResult{r.a4, r.a4r} {
				marks[i] = "·"
				if benchScored(x) {
					marks[i] = benchOmission(x, r.names)
					omitted[i].scored++
					if marks[i] == "o" || marks[i] == "f" {
						omitted[i].found++
					}
					if marks[i] == "o" {
						omitted[i].named++
					}
				}
			}
			tot := benchTotal(r.a2, r.a3, r.a4, r.a4r)
			fmt.Fprintf(&b, " %d | %s | %s | %s | %s | %s | %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s | %s |\n",
				s2.caught, s2.falseCell(), s3.falseCell(), marks[0], marks[1], s4.falseCell(), s4r.falseCell(), r.a4.in, r.a4r.in,
				len(tot.dropped), tot.refusedCalls, s2.formed+s3.formed+s4.formed+s4r.formed,
				len(r.a2.findings.Findings)+len(r.a3.findings.Findings)+len(r.a4.findings.Findings)+len(r.a4r.findings.Findings), tot.calls, tot.in, tot.out, tot.think,
				benchCost(tot), benchSecs(r.a2.dur), benchSecs(r.a3.dur))
			for _, x := range []benchResult{r.a2, r.a3, r.a4, r.a4r} {
				use.add(x, bound)
			}
			for _, x := range []struct {
				r      benchResult
				sc     benchScore
				fs, in *benchStats
			}{{r.a2, s2, &fa2, nil}, {r.a3, s3, &fa3, nil}, {r.a4, s4, &fa4, &in4}, {r.a4r, s4r, &fa4r, &in4r}} {
				switch {
				case benchScored(x.r):
					x.fs.add(float64(len(x.sc.falses)))
					if x.in != nil {
						x.in.add(float64(x.r.in))
					}
					droppedFalse += len(x.sc.falseDropped)
					formed += x.sc.formed
					findings += len(x.r.findings.Findings)
				case x.r.failed != "":
					notes = append(notes, x.r.failed)
				}
			}
			if benchScored(r.a2) {
				scored++
				caught.add(float64(s2.caught))
				for id, m := range s2.marks {
					if m == "f" {
						hits[id]++
					}
				}
			}
		}
		fmt.Fprintf(&sum, "| %s | %d of %d scored | %s |", runs[0].cand, scored, len(runs), caught)
		for _, s := range benchShapes {
			fmt.Fprintf(&sum, " %d/%d |", hits[s.id], scored)
		}
		fmt.Fprintf(&sum, " %s | %s | %d/%d, %d/%d | %d/%d, %d/%d | %s | %s | %s | %s | %d | %d/%d | %s %d |\n", fa2, fa3,
			omitted[0].found, omitted[0].scored, omitted[0].named, omitted[0].scored, omitted[1].found, omitted[1].scored, omitted[1].named, omitted[1].scored,
			fa4, fa4r, in4, in4r, droppedFalse, formed, findings, use.cells(), use.over)
		for _, n := range notes {
			fmt.Fprintf(&sum, "\n%s: %s\n", runs[0].cand, n)
		}
	}
	b.WriteString(sum.String())
	b.WriteString("\n")
	return b.String()
}

// benchDetails are each check's findings, dropped elements and corrected
// text, for reading what was caught and how.
func benchDetails(all [][]benchRun) string {
	var b strings.Builder
	b.WriteString("## Findings\n")
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	label := func(ss []benchStatement, id string) string {
		i, err := strconv.Atoi(strings.TrimPrefix(id, "s"))
		if err != nil || i < 1 || i > len(ss) {
			return "?"
		}
		if s := ss[i-1].shape; s != "" {
			return "shape " + s
		}
		return "SUPPORTED"
	}
	for _, runs := range all {
		for _, r := range runs {
			if r.stopped != "" {
				continue
			}
			for _, x := range []struct {
				name string
				ss   []benchStatement
				r    benchResult
			}{{"a1", benchA1, r.a1}, {"a2", benchA2, r.a2}, {"a3", benchA3, r.a3}, {"draft", benchDraftAnswer(), r.draft}, {"a4", benchA4, r.a4}, {"a4 ranked", benchA4, r.a4r}} {
				if !x.r.ran {
					continue
				}
				role := "editor"
				if !r.a1.ran {
					role = "reviewer"
				}
				fmt.Fprintf(&b, "\n### %s %s, run %d, %s: %s\n\n", role, r.cand, r.n, x.name, benchOutcome(x.r))
				if x.r.failed != "" {
					fmt.Fprintf(&b, "- failed: %s\n", flat(x.r.failed))
				}
				for _, f := range x.r.findings.Findings {
					anchor := ""
					if f.Anchor {
						anchor = " anchor"
					}
					fmt.Fprintf(&b, "- %s (%s) [%s]%s %q — %s (%s) → %s", f.Statement, label(x.ss, f.Statement), f.Kind, anchor, flat(f.Quote), flat(f.Why), strings.Join(f.Evidence, ", "), flat(f.Suggested))
					if f.Alternative != "" || f.Discriminator != "" {
						fmt.Fprintf(&b, " · alternative: %s · discriminator: %s", flat(f.Alternative), flat(f.Discriminator))
					}
					b.WriteString("\n")
				}
				if len(x.r.findings.Supported) > 0 {
					fmt.Fprintf(&b, "- supported: %s\n", strings.Join(x.r.findings.Supported, ", "))
				}
				for _, sk := range x.r.findings.Skipped {
					fmt.Fprintf(&b, "- skipped %s: %s\n", sk.Statement, flat(sk.Why))
				}
				for _, d := range x.r.dropped {
					fmt.Fprintf(&b, "- dropped %s", flat(d.String()))
					if m := benchDroppedFinding.FindStringSubmatch(d.Element); m != nil {
						if i, _ := strconv.Atoi(m[1]); i < len(x.r.raw) {
							if j, err := json.Marshal(x.r.raw[i]); err == nil {
								fmt.Fprintf(&b, " — as written: %s", j)
							}
						}
					}
					b.WriteString("\n")
				}
				for _, line := range x.r.trace {
					fmt.Fprintf(&b, "- conversation: %s\n", line)
				}
				if c := x.r.findings.Corrected; c != "" {
					fmt.Fprintf(&b, "\nCorrected:\n\n```text\n%s\n```\n", strings.TrimSpace(c))
				}
			}
		}
	}
	return b.String()
}

// TestCheckReplayBench: the live benchmark's replay plays offline, the
// fixture its main model and no check configured. Each answer splits into
// its statements as written, each statement's words in its text; each
// read and the solution's fetch stores one item, the solution's a
// reference whose lines 7–9 are its root cause; the consult returns the
// expert's hedged answer as a model's reading; every citation of a1, a2,
// a3 and a4 resolves; and the engine's own checks flag no span of a1, of
// the control, a3, or of a4, and in a2 only those of shape 3's alert
// rule, the causal wording they find being a2's and the control's. a4's
// review given the ranked evidence quotes the lines that contradict its
// shape 17, which the review without it does not.
func TestCheckReplayBench(t *testing.T) {
	benchSplits(t)
	for _, ss := range [][]benchStatement{benchA1, benchA2, benchA3, benchA4} {
		for i, st := range ss {
			if !strings.Contains(benchNorm(st.text), st.words) {
				t.Errorf("s%d's words %q are not in %q", i+1, st.words, st.text)
			}
		}
	}
	page, err := os.ReadFile(benchSolutionPage)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(string(page), "\n"); len(lines) < 9 || lines[6] != "Root cause" {
		t.Errorf("the solution's line 7 is not its root cause's heading:\n%s", page)
	}
	ctx := context.Background()
	s, m := benchReplay(t)
	id, err := evidenceByName(s.c, m.names["kcs"])
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.c.ev.Describe(id)
	if err != nil || EvidenceKind(it) != KindReference || it.Ref.Type != evidence.TypeReferenceDocument {
		t.Errorf("the solution's item %s: %+v, %v; want a reference document", m.names["kcs"], it.Ref, err)
	}
	for _, ask := range []string{benchAsk2, benchAsk3, benchAsk4} {
		if _, err := s.Talk(ctx, ask, nil); err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var consulted []string
	for _, msg := range history {
		for _, r := range msg.Results {
			if r.Name == consultTool {
				consulted = append(consulted, r.Text)
			}
		}
	}
	if len(consulted) != 1 || !strings.HasPrefix(consulted[0], "openshift.platform answers (a model's reading, in a consult):") || !strings.Contains(consulted[0], benchConsult) {
		t.Errorf("the consult's results: %q", consulted)
	}
	answers := answersOf(history)
	want := [][]benchStatement{benchA1, benchA2, benchA3, benchA4}
	if len(answers) != len(want) {
		t.Fatalf("%d answers; want %d", len(answers), len(want))
	}
	alert := ""
	for _, st := range benchA2 {
		if st.shape == "3" {
			alert = st.text
		}
	}
	var causal []string
	for i, a := range answers {
		if strings.TrimSpace(a.text) != strings.TrimSpace(m.fill(benchText(want[i]))) {
			t.Errorf("answer %d is\n%s", i+1, a.text)
		}
		c, err := s.checked(ctx, a.text, answers[:i])
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Unresolved) > 0 {
			t.Errorf("a%d's unresolved citations: %v", i+1, c.Unresolved)
		}
		spans := 0
		for _, f := range c.Flags {
			if f.Finding == FindingCausal {
				causal = append(causal, "a"+strconv.Itoa(i+1)+" "+f.What)
				continue
			}
			if i != 1 || f.Span == "" || f.Statement != alert {
				t.Errorf("a%d flagged %s: %s, in %q", i+1, f.What, f.Why, f.Statement)
			}
			spans++
		}
		if i == 1 && spans == 0 {
			t.Error("a2's alert rule is not flagged")
		}
	}
	// The control's root cause is the solution's, but not directly after
	// it: causal wording a reading tells apart.
	if want := []string{`a2 "caused"`, `a3 "root cause"`}; !slices.Equal(causal, want) {
		t.Errorf("causal wording %q; want %q", causal, want)
	}
	plain, err := s.ReviewRequest(ctx, ReviewAsk{})
	if err != nil {
		t.Fatal(err)
	}
	ranked, err := s.ReviewRequest(ctx, ReviewAsk{Ranked: true})
	if err != nil {
		t.Fatal(err)
	}
	against := m.names[benchA4Against]
	_, section, _ := strings.Cut(ranked.Text, "\n"+uncitedHeading+"\n")
	if _, ok := plain.Context.Quoted[against]; ok || !strings.Contains(section, "### "+against+" · search · ") ||
		!strings.Contains(section, `"Eviction manager: pods ranked for eviction" pods=["openshift-storage/csi-rbdplugin-k2x9p"`) ||
		!slices.Contains(ranked.Context.Quoted[against], check.Lines{From: 1, To: 3}) {
		t.Errorf("a4's ranked review request does not quote %s:1-3, or the plain one does:\n%s", against, ranked.Text)
	}
	// The draft, citing nothing, splits into its statements, and the
	// editor is given, after it, the items a1 to a4 cite, each
	// statement's source among them.
	s.e.config.Comments.Intentions = []config.Intention{{ID: "customer-update", Label: "Customer update", Audience: "customer", Purpose: "Tell the customer what was found."}}
	if _, _, err := s.Draft(ctx, "customer-update", nil); err != nil {
		t.Fatal(err)
	}
	if got := check.Split(m.fill(benchText(benchDraftAnswer()))); len(got) != len(benchDraft) {
		t.Errorf("the draft splits into %d statements; want %d", len(got), len(benchDraft))
	}
	req, err := s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range benchDraft {
		if len(d.sources) > 0 && !slices.ContainsFunc(req.Quotes, func(q QuotedItem) bool { return q.Name == m.names[d.sources[0]] }) {
			t.Errorf("the draft's request does not quote %s, which holds %q", m.names[d.sources[0]], d.text)
		}
	}
}

// TestScoreDraft: a statement of the draft counts cited where the
// editor's correction cites the item that holds it, anchored or flagged
// otherwise; the one no item holds is cited (wrongly) where the
// correction cites anything, else anchored or flagged; with statements
// that do not pair, nothing counts cited. Synthetic.
func TestScoreDraft(t *testing.T) {
	names := map[string]string{"evict": "e3", "csi": "e4", "osd": "e5", "enospc": "e6"}
	lines := make([]string, len(benchDraft))
	for i, d := range benchDraft {
		lines[i] = "- " + d.text
	}
	lines[0] = "- " + strings.TrimSuffix(benchDraft[0].text, ".") + " [e3:1-3]."
	lines[1] = "- " + strings.TrimSuffix(benchDraft[1].text, ".") + " [e9:1]."
	var r benchResult
	r.findings.Corrected = strings.Join(lines, "\n")
	r.findings.Findings = []check.Finding{{Statement: "s3", Anchor: true}, {Statement: "s4"}, {Statement: "s5", Anchor: true}}
	if got, want := scoreDraft(r, names), (benchDraftScore{cited: 1, anchored: 1, flagged: 1, last: "a", paired: true}); got != want {
		t.Errorf("score %+v; want %+v", got, want)
	}
	lines[4] = "- " + strings.TrimSuffix(benchDraft[4].text, ".") + " [e3:2]."
	r.findings.Corrected = strings.Join(lines, "\n")
	if got := scoreDraft(r, names); got.last != "cited" {
		t.Errorf("the unheld statement cited: %q; want cited", got.last)
	}
	r.findings.Corrected = strings.Join(lines[:3], "\n")
	if got := scoreDraft(r, names); got.paired || got.cited != 0 {
		t.Errorf("statements not pairing: %+v", got)
	}
}

// TestCheckReplayBenchScore: scoreBench counts a shape caught where an
// anchor or a finding names its statement or the editor's corrected text
// changes its words, and not where only a finding validation dropped
// names it; on the control, a3, every finding is false and every word the
// corrected text changes a rewrite; and the reports' tables give each
// row a cell per heading, the control's false findings in a column of
// their own. The findings are synthetic, no model's.
func TestCheckReplayBenchScore(t *testing.T) {
	// at is the ID of the statement of ss seeded with shape, the nth from
	// 0; of the supported, for shape "".
	at := func(ss []benchStatement, shape string, nth int) string {
		n := nth
		for i, st := range ss {
			if st.shape == shape {
				if n == 0 {
					return "s" + strconv.Itoa(i+1)
				}
				n--
			}
		}
		t.Fatalf("no statement %d of shape %q", nth, shape)
		return ""
	}
	// rewrite is ss's text with statement id's words changed.
	rewrite := func(ss []benchStatement, id string) string {
		i, _ := strconv.Atoi(strings.TrimPrefix(id, "s"))
		return strings.Replace(benchNorm(benchText(ss)), ss[i-1].words, "rewritten", 1)
	}
	finding := func(id string, anchor bool) check.Finding {
		return check.Finding{Statement: id, Kind: "unsupported", Quote: "q", Why: "w", Anchor: anchor}
	}
	droppedOf := func(ids ...string) ([]any, []check.Rejected) {
		var raw []any
		var rej []check.Rejected
		for i, id := range ids {
			raw = append(raw, map[string]any{"statement": id})
			rej = append(rej, check.Rejected{Element: "findings[" + strconv.Itoa(i) + "]", Reason: "synthetic"})
		}
		return raw, rej
	}

	a2 := benchResult{ran: true, findings: check.Findings{
		Findings:  []check.Finding{finding(at(benchA2, "9", 0), true), finding(at(benchA2, "10", 0), false), finding(at(benchA2, "13", 0), false), finding(at(benchA2, "", 0), false)},
		Corrected: rewrite(benchA2, at(benchA2, "11", 0)),
	}}
	a2.raw, a2.dropped = droppedOf(at(benchA2, "12", 0), at(benchA2, "", 1))
	s2 := scoreBench(benchA2, a2)
	wantMarks := map[string]string{"9": "a", "10": "f", "11": "c", "12": "d", "13": "f"}
	for _, sh := range benchShapes {
		want := wantMarks[sh.id]
		if want == "" {
			want = "-"
		}
		if s2.marks[sh.id] != want {
			t.Errorf("shape %s marked %q; want %q", sh.id, s2.marks[sh.id], want)
		}
	}
	if s2.caught != 4 || s2.anchors != 1 || !slices.Equal(s2.falses, []string{at(benchA2, "", 0)}) ||
		!slices.Equal(s2.falseDropped, []string{at(benchA2, "", 1)}) || len(s2.rewrote) != 0 {
		t.Errorf("a2 scored caught %d, anchors %d, false %v, false dropped %v, rewrote %v", s2.caught, s2.anchors, s2.falses, s2.falseDropped, s2.rewrote)
	}

	a3 := benchResult{ran: true, findings: check.Findings{
		Findings:  []check.Finding{finding("s2", false), finding("s4", true)},
		Corrected: rewrite(benchA3, "s1"),
	}}
	a3.raw, a3.dropped = droppedOf("s6")
	s3 := scoreBench(benchA3, a3)
	if s3.caught != 0 || len(s3.marks) != 0 || !slices.Equal(s3.falses, []string{"s2", "s4"}) ||
		!slices.Equal(s3.falseDropped, []string{"s6"}) || !slices.Equal(s3.rewrote, []string{"s1"}) || s3.falseCell() != "2 (+1 dropped)" {
		t.Errorf("a3 scored caught %d, marks %v, false %v, false dropped %v, rewrote %v", s3.caught, s3.marks, s3.falses, s3.falseDropped, s3.rewrote)
	}

	// a4's shape 17 found without its contradiction, and with the ranked
	// evidence found resting on it, a supported statement flagged too.
	names := map[string]string{benchA4Against: "e3", "csi": "e4"}
	omission := at(benchA4, "17", 0)
	a4 := benchResult{ran: true, findings: check.Findings{Findings: []check.Finding{{Statement: omission, Kind: "undetermined", Quote: "q", Why: "w", Evidence: []string{"e4:1"}}}}}
	a4r := benchResult{ran: true, findings: check.Findings{Findings: []check.Finding{
		{Statement: omission, Kind: "unsupported", Quote: "q", Why: "w", Evidence: []string{"e4:1", "e3:3"}}, finding(at(benchA4, "", 0), false)}}}
	if o, or := benchOmission(a4, names), benchOmission(a4r, names); o != "f" || or != "o" || benchOmission(benchResult{ran: true}, names) != "-" {
		t.Errorf("a4's omission marked %q without the ranked evidence and %q with it; want f and o", o, or)
	}
	inWords := benchResult{ran: true, findings: check.Findings{Findings: []check.Finding{{Statement: omission, Kind: "unsupported", Quote: "q", Why: "e3:3 ranks the pod first", Evidence: []string{"e4:1"}}}}}
	if m := benchOmission(inWords, names); m != "o" {
		t.Errorf("a4's omission named in a finding's words marked %q; want o", m)
	}
	run := benchRun{cand: benchCandidate{"fixture", "fixture-model", ""}, n: 1, a1: benchResult{ran: true}, a2: a2, a3: a3, a4: a4, a4r: a4r, revisions: 1, revisions3: 1, names: names}
	editor := benchTables(t, benchEditorTables([][]benchRun{{run}}, time.Minute))
	reviewer := benchTables(t, benchReviewerTables([][]benchRun{{run}}, time.Minute))
	if len(editor) != 2 || len(reviewer) != 2 {
		t.Fatalf("%d editor and %d reviewer tables; want two each", len(editor), len(reviewer))
	}
	of := "false a3, of " + strconv.Itoa(len(benchA3))
	for name, got := range map[string]map[string]string{"editor's run": editor[0], "reviewer's run": reviewer[0], "editor's model": editor[1], "reviewer's model": reviewer[1]} {
		want := map[string]string{"false a3": "2 (+1 dropped)", "false a2": "1 (+1 dropped)", "caught": "4", "9": "a", "12": "d"}
		if strings.HasSuffix(name, "model") {
			want = map[string]string{of: "2", "false a2": "1", "9": "1/1", "12": "0/1"}
			if name == "reviewer's model" {
				// The reviewer's catches are its findings, an anchor none.
				want["9"] = "0/1"
			}
		}
		if name == "reviewer's run" {
			want["17 a4"], want["17 a4 ranked"], want["false a4"], want["false a4 ranked"] = "f", "o", "0", "1"
		}
		if name == "reviewer's model" {
			want["17 a4 found, named"], want["17 a4 ranked found, named"], want["false a4 ranked"] = "1/1, 0/1", "1/1, 1/1", "1"
		}
		for col, v := range want {
			if got[col] != v {
				t.Errorf("the %s row's %q is %q; want %q", name, col, got[col], v)
			}
		}
	}
}

// benchTables are the first row of each Markdown table in md, by heading;
// it fails t on a row with other than a cell per heading.
func benchTables(t *testing.T, md string) []map[string]string {
	t.Helper()
	cells := func(line string) []string {
		parts := strings.Split(strings.Trim(line, "|"), "|")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	var out []map[string]string
	lines := strings.Split(md, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "|") || !strings.HasPrefix(lines[i+1], "|---") {
			continue
		}
		head := cells(lines[i])
		if n := len(cells(lines[i+1])); n != len(head) {
			t.Errorf("%d headings over %d separators: %s", len(head), n, lines[i])
		}
		var first map[string]string
		for _, row := range lines[i+2:] {
			if !strings.HasPrefix(row, "|") {
				break
			}
			c := cells(row)
			if len(c) != len(head) {
				t.Errorf("%d cells under %d headings: %s", len(c), len(head), row)
				continue
			}
			if first == nil {
				first = map[string]string{}
				for j, h := range head {
					first[h] = c[j]
				}
			}
		}
		out = append(out, first)
		i++
	}
	return out
}

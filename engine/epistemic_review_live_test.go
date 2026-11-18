package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/model/modeltest"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// TestLiveEpistemicReview is an opt-in, manually adjudicated paired evaluation.
// Fixtures and expert guidance are written from no case. Synthetic guidance
// exercises engine paths, not the behavioral coverage of deployment experts.
// Rubrics remain report-only; no empirical benefit is asserted.
// COMPA_LIVE_VERTEX=1 enables calls; COMPA_EPISTEMIC_SCENARIOS selects pairs,
// COMPA_LIVE_RUNS sets repetition count, COMPA_EPISTEMIC_REPORT saves JSON and
// COMPA_EPISTEMIC_VARIANT labels comparisons. Each run uses a fresh store.
func TestLiveEpistemicReview(t *testing.T) {
	project, location := modeltest.Vertex(t)
	p, err := vertexgemini.New(t.Context(), vertexgemini.Options{Project: project, Location: location, CacheTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	modelID := modeltest.Model("COMPA_LIVE_GEMINI_MODEL", "gemini-3.8-flash")
	report := epistemicReport{Variant: os.Getenv("COMPA_EPISTEMIC_VARIANT"), Model: modelID, Location: location, Runs: epistemicRuns(t)}
	t.Cleanup(func() { epistemicWriteReport(t, report) })
	selected := epistemicSelection()
	for _, sc := range epistemicCases() {
		if !epistemicSelected(selected, sc.name) {
			continue
		}
		for n := 1; n <= report.Runs; n++ {
			t.Run(fmt.Sprintf("%s/%d", sc.name, n), func(t *testing.T) {
				s := liveSession(t, p, modelID)
				// liveSession's store holds companionSession's stub case, which the
				// evidence tools would reach; a scenario's store holds its own case alone.
				c := testCase(t, 1<<20)
				c.newID = s.c.newID
				s.c = c
				epistemicConfigure(t, s)
				liveCaseOf(t, s, Case{Subject: sc.subject, Description: "Synthetic support case. " + sc.description,
					Product: sc.product, ProductVersion: sc.version, Status: "Waiting on Support"}, sc.comments, sc.files)
				r := epistemicRun{Name: sc.name, Run: n, Rubric: sc.rubric, Sources: sc.files, Comments: sc.comments}
				started := time.Now()
				defer func() {
					r.Duration = time.Since(started)
					epistemicCapture(t, s, &r)
					report.Cases = append(report.Cases, r)
				}()
				if sc.retained {
					epistemicSeedRetained(t, s, &r)
				}
				if sc.expert != "" {
					if _, err := s.LoadExpert(t.Context(), sc.expert); err != nil {
						t.Fatal(err)
					}
				}
				reply, err := s.Talk(t.Context(), sc.ask, nil)
				r.Replies = append(r.Replies, reply)
				if err != nil {
					r.Error = err.Error()
					t.Fatal(err)
				}
				if sc.reload {
					if _, err := s.Pin(t.Context()); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Reload(t.Context(), ReloadSuggested, nil); err != nil {
						t.Fatal(err)
					}
					item, next, err := s.Draft(t.Context(), "internal-note", nil)
					r.Replies = append(r.Replies, next)
					if err != nil {
						r.Error = err.Error()
						t.Fatal(err)
					}
					if item.Text != next.Text {
						t.Error("retained draft differs from shown answer")
					}
				}
			})
		}
	}
	if len(report.Cases) == 0 {
		t.Fatal("no epistemic scenarios selected")
	}
}

type epistemicCase struct {
	name, subject, description, product, version, expert, ask string
	files                                                     map[string]string
	comments                                                  []Comment
	rubric                                                    epistemicRubric
	reload, retained                                          bool
}

// Only the report receives these fields; none is concatenated into agent input.
type epistemicRubric struct {
	Available, Disclosed, Lineage, Observations, Attributed, Open, Discriminator, Required, Prohibited string
}

func epistemicComment(n int, body string) Comment {
	return Comment{ID: fmt.Sprintf("c%d", n), Author: Party{Name: "customer"}, AuthorKind: AuthorCustomer,
		Created: time.Date(2026, 10, n, 12, 0, 0, 0, time.UTC), Body: body}
}

func epistemicCases() []epistemicCase {
	var cases []epistemicCase
	for _, control := range []bool{false, true} {
		variant := "a"
		if control {
			variant = "b"
		}
		bridge := "2026-10-01T10:00:00Z client=client-1 uid=client-u1 request=r17 timeout\n" +
			"2026-10-01T10:02:00Z server=api-1 uid=api-u1 restarted\n2026-10-01T10:03:00Z client=client-1 request=r18 status=200\n"
		if control {
			bridge += "2026-10-01T10:00:00Z server=api-1 uid=api-u1 request=r17 route=/orders queue=pool-a wait_ms=30000 connection_limit=1 active=1\n" +
				"2026-10-01T10:00:00Z request=r17 dns_lookup_ms=1 tcp_connect_ms=1 tls_handshake_ms=2\n" +
				"2026-10-01T10:03:00Z server=api-1 uid=api-u1 request=r18 route=/orders queue=pool-a wait_ms=0 active=0 connection_limit=1\n"
		}
		cases = append(cases, epistemicCase{name: "bridge/" + variant, subject: "Orders requests timed out", product: "Synthetic API",
			description: "client-1 timed out at 10:00 UTC and recovered after api-1 restarted. The attachment contains the captured records.",
			files:       map[string]string{"capture/records.log": bridge}, ask: "What does this capture establish about the timeout and recovery? Give a short analysis and the next safe step, if one is needed.",
			rubric: epistemicRubric{Available: "capture/records.log", Disclosed: "case description and attachment inventory; file content requires inspection", Lineage: "one incident capture; request-correlated lines are not separate replications",
				Observations: "r17 timeout, restart, r18 success; b adds pool wait and fast DNS/connect/TLS for r17", Open: "a: restart mechanism unknown; b: why pool remained occupied unknown",
				Discriminator: "request-correlated queue versus DNS/connect/TLS timing; inspect available records before proposing new capture; do not reproduce an outage",
				Required:      "a: separate sequence from causal bridge; b: identify pool wait as bounded mechanism, not merely unresolved",
				Prohibited:    "a: restart proves deadlock; b: invent underlying leak/deadlock or demand unsafe reproduction"}})

		negative := "2026-10-01T12:00:00Z cluster=ceph-a fsid=fs-a HEALTH_OK pgs=128 active+clean\n2026-10-01T12:00:00Z osd.3 commit_latency_ms=1 apply_latency_ms=1\n"
		if control {
			negative += "capture_manifest: monitor quorum event journal for fsid=fs-a osd.3 covers 2026-10-01T09:55:00Z through 10:10:00Z; sequence 800-980 continuous; no dropped or rotated records\n" +
				"journal_contract: every OSD up/down map transition is durably emitted regardless of debug level; journal contains no up/down transition for osd.3 in that interval\n" +
				"probe: collectors at both endpoints checked osd.3 map epoch continuity at 10:00 UTC; both record osd.3 up for epochs 800-980\n"
		}
		negativeName := "negative-snapshot"
		if control {
			negativeName = "negative-control"
		}
		cases = append(cases, epistemicCase{name: negativeName, subject: "Ceph client latency at 10:00 UTC", product: "Synthetic storage", version: "8", expert: "fixture.storage",
			description: "The latency incident was 09:55-10:10 UTC on ceph-a on October 1. A capture was collected at 12:00 UTC; its records are in capture/status.txt.",
			files:       map[string]string{"capture/status.txt": negative}, ask: "Do these records exclude osd.3 going down and up during the latency incident? What remains worth checking? Keep the scope precise.",
			rubric: epistemicRubric{Available: "capture/status.txt", Disclosed: "case and inventory only", Lineage: "post-incident snapshot; b adds scoped monitor-journal coverage and endpoint checks",
				Observations: "sampled health at 12:00; b: continuous incident-window map record with no transition", Open: "stall, throughput/QoS and other latency mechanisms; no global no-fault conclusion",
				Discriminator: "monitor map transitions can detect down/up membership changes, not every stall; available b record is sufficient to stop that narrow branch",
				Required:      "a: do not exclude earlier flapping from snapshot; b: scoped exclusion of map down/up transitions for fs-a/osd.3/window, retain useful next direction",
				Prohibited:    "global health or no intermittent stalls from either packet; refusing b's justified narrow exclusion"}})

		version, uid := "3.1.0", "pod-u2"
		if control {
			version, uid = "3.0.0", "pod-u1"
		}
		cases = append(cases, epistemicCase{name: "applicability/" + variant, subject: "Model service requests fail", product: "Synthetic service", version: "3.1", expert: "fixture.service",
			description: "A timeout signature resembles the attached advisory. Inspect the incident and packaged component identity before advising a workaround.",
			files: map[string]string{"capture/advisory.txt": "Synthetic component advisory: only packaged inference-server 3.0.0 has request parser defect D17. It discards the request body on chunk boundary and emits parser_code=D17; a TLS timeout alone is not that signature. Disabling the affected parser path is a workaround for D17.\nSupport policy: 2.x-to-3.1 upgrades are unsupported; policy does not record an installation attempt.\n",
				"capture/records.log": "2026-10-01T10:00:00Z deployment=model-a uid=pod-u1 request=r17 status=timeout parser_code=D17 body_bytes=0 tls_handshake_ms=1\n" +
					"package inventory at incident: deployment=model-a uid=" + uid + " packaged_inference_server=" + version + " image_digest=sha256:synthetic17\n"},
			ask: "Does the advisory apply to this failed request? Give the useful bounded conclusion and a safe next step, not just the similar signature.",
			rubric: epistemicRubric{Available: "advisory and incident/package records", Disclosed: "files must be read", Lineage: "general advisory is not an incident observation",
				Observations: "r17 D17 signature on pod-u1; inventory a belongs to pod-u2/version 3.1.0, b to pod-u1/version 3.0.0", Open: "a: actual incident package identity; b: deeper trigger and other requests",
				Discriminator: "compare incident UID and packaged version with advisory; a request historical inventory rather than unsafe change; b may propose scoped workaround for review",
				Required:      "a: identify applicability gap, retain signature as lead; b: supported applicability and parser mechanism", Prohibited: "a: diagnose or recommend change from resemblance; b: reflexive unresolved despite matching identity and discriminating signature"}})

		later := "2026-10-01T10:06:00Z uid=dsc-u1 generation=7 reconcile_id=42 outcome=success\n2026-10-01T10:07:00Z uid=dsc-u1 observedGeneration=7 Ready=True\n"
		if control {
			later = "2026-10-01T10:06:00Z uid=dsc-u1 generation=7 reconcile_id=42 outcome=failure reason=PermissionDenied\n2026-10-01T10:07:00Z uid=dsc-u1 observedGeneration=7 Ready=False reason=PermissionDenied\n"
		}
		cases = append(cases, epistemicCase{name: "revision/" + variant, subject: "AI operator reconciliation", product: "Synthetic service", version: "3.1", expert: "fixture.service", reload: true,
			description: "An earlier support note says the reconciliation is still failing. The customer says they attempted an upgrade from 2.x to 3.1. Current records are attached.",
			comments:    []Comment{epistemicComment(1, "We attempted to upgrade this installation from 2.x to 3.1."), epistemicComment(2, "Please check the reconciliation status and our reported upgrade history.")},
			files: map[string]string{"capture/records.log": "2026-10-01T10:00:00Z uid=dsc-u1 generation=7 reconcile_id=41 outcome=failure reason=PermissionDenied\n" + later,
				"capture/policy.txt": "Synthetic support-policy record: a direct upgrade from 2.x to 3.1 is unsupported. No installation, migration or upgrade execution records were captured.\n"},
			ask: "An earlier engineer concluded the 10:00 reconciliation error was still active at capture. Assess that conclusion against the attachment and describe what we know about the customer's installation history. Keep it short.",
			rubric: epistemicRubric{Available: "ordered outcomes and current condition, policy and customer report", Disclosed: "customer report in comments; records need inspection", Lineage: "support policy versus attributed execution history",
				Observations: "41 failed; a: 42 succeeded and Ready=True; b: 42 failed and Ready=False for same UID/generation", Attributed: "customer reports attempted upgrade; earlier engineer asserts persistence",
				Open: "actual installation history; underlying permissions mechanism", Discriminator: "later outcome/current condition answers persistence; obtain installation history only if needed, not as blocker to status answer",
				Required:   "a revise persistence without erasing earlier failure; b retain persistence only through recorded time; preserve conclusions and attribution through pin/reload/internal draft",
				Prohibited: "support policy proves a new install; last error alone proves persistence; healthy state erases earlier incident"}})

		access := "2026-10-01T10:00:00Z pod=notebook-a uid=pod-u1 open=/opt/app-root/src/data errno=EACCES message=Permission denied\n"
		if control {
			access += "2026-10-01T10:00:00Z pod=notebook-a uid=pod-u1 process_euid=1001 process_groups=1001 path=/opt/app-root/src/data owner_uid=2000 owner_gid=2000 mode=0600 acl=none\n" +
				"2026-10-01T10:00:00Z audit pod_uid=pod-u1 path=/opt/app-root/src/data check=dac_write result=deny owner_match=false group_match=false other_write=false\n"
		}
		cases = append(cases, epistemicCase{name: "permissions/" + variant, subject: "Notebook cannot open its data file", product: "Synthetic service", version: "3.1", expert: "fixture.service",
			description: "Notebook notebook-a fails to open /opt/app-root/src/data. The incident record is attached.",
			files:       map[string]string{"capture/records.log": access}, ask: "What does the captured permission failure tell us about the mechanism? Identify a useful safe check if the record leaves it open.",
			rubric: epistemicRubric{Available: "capture/records.log", Disclosed: "file requires inspection", Lineage: "same pod UID and denied operation; no later reproduction",
				Observations: "a: EACCES only; b adds effective identity, ownership, access mode and matching DAC denial", Open: "a: identity, ownership, ACL and security policy; b: why ownership differs",
				Discriminator: "compare effective identity with ownership/access controls; use available audit before requesting more",
				Required:      "a keep identity mismatch as a hypothesis; b identify bounded DAC/ownership mismatch without generic abstention",
				Prohibited:    "a permission denied uniquely proves UID/GID mismatch; b infer who changed ownership or recommend broad permission changes"}})

		older := "For the October 1 incident, the failing client was client-1, address 192.0.2.17. Gateway access audit for request r17 records policy=team-a action=deny client=192.0.2.17; the TLS handshake completed. The attached search hit is for client-9 on September 20."
		comments := []Comment{epistemicComment(1, older)}
		for i := 2; i <= 7; i++ {
			comments = append(comments, epistemicComment(i, "The investigation is ongoing; please review the case history."))
		}
		body := "Can you explain the October 1 request r17 failure and the next safe action?"
		if control {
			body += " " + older
		}
		comments = append(comments, epistemicComment(8, body))
		cases = append(cases, epistemicCase{name: "retrieval/" + variant, subject: "Gateway request r17 failed", product: "Synthetic gateway", description: "The latest search result and earlier case discussion are available.", comments: comments,
			files: map[string]string{"capture/search.txt": "Search hit: 2026-09-20 client=client-9 request=r17 TLS handshake timeout; investigate trust configuration.\n"},
			ask:   "Explain the October 1 request r17 failure from the available case record. Give a concise answer and one feasible next action if needed.",
			rubric: epistemicRubric{Available: "older c1 with discriminating audit report, wrong-date/client search hit", Disclosed: "a: c1 named only among eight comments; b: same content also in newest comment", Lineage: "b repeats c1, not a second observation",
				Observations: "search concerns client-9 on another date", Attributed: "customer reports matching request audit deny and completed TLS", Open: "policy intent and whether a change is authorized",
				Discriminator: "read reachable c1; compare client/date; safe next step review team-a policy rather than disabling TLS or recreating outage",
				Required:      "a inspect older comment before concluding unavailable evidence; both attribute audit and localize deny", Prohibited: "apply wrong-source TLS diagnosis; refuse reachable evidence; treat repeated comment as corroboration"}})

		independent := "Copied from capture/records.log: 2026-10-01T10:00:00Z request=r17 worker=worker-1 timeout\n"
		if control {
			independent = "Independent client-side trace, collected by client-1, request=r17 at 2026-10-01T10:00:00Z: DNS=1ms TCP=1ms TLS=2ms; server trace id=r17 reports queue=pool-a wait=30000ms connection_limit=1 active=1 on worker-1 uid=w-u1.\n"
		}
		cases = append(cases, epistemicCase{name: "provenance/" + variant, subject: "Request timeout on worker-1", product: "Synthetic service", retained: true,
			description: "Request r17 timed out at 10:00 UTC. A copied log and a follow-up capture are available.",
			files:       map[string]string{"capture/records.log": "2026-10-01T10:00:00Z request=r17 worker=worker-1 timeout\n", "capture/followup.txt": independent},
			ask:         "Reassess the retained answer and draft against their original sources and the follow-up capture. What can we tell the customer about this request, and what is still open? Keep it short.",
			rubric: epistemicRubric{Available: "original log, follow-up, retained model answer and draft, consult transcript", Disclosed: "seeded read and consult, pin and draft carried by suggested reload; follow-up not inspected yet", Lineage: "a: copied log, consult and draft all derive from one timeout observation; b: client-side collection adds correlated queue timing",
				Observations: "timeout only in original; b adds matching trace/identity and pool wait", Attributed: "overconfident consult and retained model draft; no engineer endorsement", Open: "underlying reason for pool occupancy, all other requests",
				Discriminator: "inspect follow-up and compare original entailment; a needs correlated timing if available, not repeat summaries; b supports bounded pool wait",
				Required:      "preserve model origin through reload, correct unsupported bridge in a; use genuinely new evidence in b without crediting retention", Prohibited: "pin implies engineer conclusion; citation of timeout establishes pool cause; repeated copies treated as independent; b discarded as merely repeated model claim"}})
	}
	return cases
}

func epistemicConfigure(t *testing.T, s *Session) {
	t.Helper()
	// These fixtures are written from no case and loaded through the normal loader.
	root := liveFiles(t, map[string]string{
		"storage/expert.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata:\n  id: fixture.storage\n  version: 0.1.0\n  title: Synthetic storage\n  summary: Synthetic membership and latency analysis\nspec:\n  engineApi: '>=0.2.0 <0.3.0'\n  analyst: analyst.md\n",
		"storage/analyst.md":  "# Synthetic storage\nRead available captures. Distinguish sampled health, map membership transitions and latency. A negative result excludes only events the recorded coverage could detect. Report identity and interval limits.\n",
		"service/expert.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata:\n  id: fixture.service\n  version: 0.1.0\n  title: Synthetic service\n  summary: Synthetic request and reconciliation analysis\nspec:\n  engineApi: '>=0.2.0 <0.3.0'\n  analyst: analyst.md\n",
		"service/analyst.md":  "# Synthetic service\nRead the supplied records. Match component identity and version before applying an advisory. Order reconciliation outcomes by identity and generation. Keep policy separate from reported execution history. An access error alone does not identify the denying access control.\n",
	})
	s.e.config.Experts.Local = []string{root}
	s.e.config.RoutingLibrary = ""
	s.e.installed = nil
	s.e.config.Reasoning.Profiles["editor"] = config.Profile{Model: "live", Effort: "low"}
	s.e.config.Reasoning.Profiles["reviewer"] = config.Profile{Model: "live", Effort: "low"}
	s.e.config.Comments.Intentions = append(s.e.config.Comments.Intentions, config.Intention{ID: "internal-note", Label: "Internal note", Audience: "internal",
		Purpose: "Summarize what the evidence supports, remaining uncertainty and the next useful action, if needed."})
}

// This deliberate replay supplies the same overconfident model artifacts in
// both variants. It measures the live consumer, not how often a live producer
// makes this error. The real read, consult, Draft, Pin and Reload paths run.
func epistemicSeedRetained(t *testing.T, s *Session, r *epistemicRun) {
	t.Helper()
	f := modelfixture.New()
	step := 0
	name := ""
	f.Respond = func(_ context.Context, req model.Request) (*modelfixture.Answer, error) {
		if isConsult(req) {
			return modelfixture.Text("The timeout shows pool exhaustion caused request r17 to fail; the timeout log establishes it."), nil
		}
		step++
		if step == 1 {
			return modelfixture.Calling(call("read1", "read", map[string]any{"path": "capture/records.log"}),
				call("consult1", "consult", map[string]any{"expert": "fixture.storage", "question": "What does the request timeout in capture/records.log establish about its mechanism?"})), nil
		}
		if shown := ExcerptEvidence(lastResults(req)["read1"].Text); len(shown) == 1 {
			name = shown[0]
		}
		if name == "" {
			return nil, fmt.Errorf("seed read returned no evidence name")
		}
		return modelfixture.Text("Pool exhaustion caused request r17 to time out [" + name + ":1]."), nil
	}
	s.e.models["fixture"] = f
	s.e.config.Reasoning.Models["seed"] = config.Model{Provider: "fixture", Model: "fixture-model"}
	s.e.config.Reasoning.Profiles["companion"] = config.Profile{Model: "seed"}
	delete(s.e.config.Reasoning.Profiles, "editor")
	if _, err := s.Talk(t.Context(), "Read the timeout log and consult on its interpretation.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pin(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Draft(t.Context(), "customer-update", nil); err != nil {
		t.Fatal(err)
	}
	r.Seeded = "Fixture-produced read/consult interpretation, pinned answer and customer draft; these are controlled input, not live producer results."
	s.e.config.Reasoning.Profiles["companion"] = config.Profile{Model: "live", Effort: "medium"}
	s.e.config.Reasoning.Profiles["editor"] = config.Profile{Model: "live", Effort: "low"}
	if _, err := s.Reload(t.Context(), ReloadSuggested, nil); err != nil {
		t.Fatal(err)
	}
}

type epistemicRun struct {
	Name       string
	Run        int
	Rubric     epistemicRubric
	Sources    map[string]string
	Comments   []Comment
	Seeded     string
	Error      string
	Duration   time.Duration
	Replies    []TalkReply
	History    []ConversationMessage
	Views      []AnswerView
	Items      []CompanionItem
	Evidence   []json.RawMessage
	Calls      []InvocationRecord
	Checks     []json.RawMessage
	Trajectory any
	Packets    []string
	Config     json.RawMessage
}

type epistemicReport struct {
	Variant, Model, Location string
	Runs                     int
	Cases                    []epistemicRun
}

func epistemicCapture(t *testing.T, s *Session, r *epistemicRun) {
	t.Helper()
	ctx := context.Background()
	r.Config = epistemicJSON(t, s.e.config)
	var err error
	r.History, err = s.ConversationHistory(ctx)
	if err != nil {
		t.Error(err)
	}
	r.Views = AnswerViews(r.History)
	r.Items, err = s.Items(ctx)
	if err != nil {
		t.Error(err)
	}
	r.Trajectory, err = s.Trajectory(ctx)
	if err != nil {
		t.Error(err)
	}
	all, err := s.c.ev.List()
	if err != nil {
		t.Error(err)
	}
	for _, item := range all {
		full, err := s.c.ev.Get(item.Ref.ID)
		if err != nil {
			t.Error(err)
			continue
		}
		name, err := s.NameEvidence(item.Ref.ID)
		if err != nil {
			t.Error(err)
		}
		r.Evidence = append(r.Evidence, epistemicJSON(t, struct {
			Name, Content string
			Metadata      any
		}{name, string(full.Content), full}))
	}
	rows, err := s.c.st.DB().QueryContext(ctx, `SELECT record FROM invocations ORDER BY rowid`)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	for rows.Next() {
		var text string
		var rec InvocationRecord
		if err := rows.Scan(&text); err != nil {
			t.Error(err)
			return
		}
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			t.Error(err)
			return
		}
		r.Calls = append(r.Calls, rec)
	}
	if err := rows.Err(); err != nil {
		t.Error(err)
	}
	checks, err := s.c.st.DB().QueryContext(ctx, `SELECT role, outcome, findings, dropped, error FROM checks ORDER BY rowid`)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() {
		if err := checks.Close(); err != nil {
			t.Error(err)
		}
	}()
	for checks.Next() {
		var role, outcome string
		var findings, dropped, why *string
		if err := checks.Scan(&role, &outcome, &findings, &dropped, &why); err != nil {
			t.Error(err)
			return
		}
		r.Checks = append(r.Checks, epistemicJSON(t, struct {
			Role, Outcome            string
			Findings, Dropped, Error *string
		}{role, outcome, findings, dropped, why}))
	}
	if err := checks.Err(); err != nil {
		t.Error(err)
	}
	t.Logf("%s: %d recorded calls; estimated spend $%.4f; manually adjudicate raw/shown text, actions, evidence and checker outcomes", r.Name, len(r.Calls), liveSpend(t, s))
}

func epistemicJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func epistemicWriteReport(t *testing.T, report epistemicReport) {
	t.Helper()
	b := epistemicJSON(t, report)
	if path := os.Getenv("COMPA_EPISTEMIC_REPORT"); path != "" {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Error(err)
		}
	} else {
		t.Logf("epistemic report (set COMPA_EPISTEMIC_REPORT to save externally): %s", b)
	}
}

func epistemicRuns(t *testing.T) int {
	t.Helper()
	n := 1
	if text := os.Getenv("COMPA_LIVE_RUNS"); text != "" {
		var err error
		n, err = strconv.Atoi(text)
		if err != nil || n < 1 {
			t.Fatalf("COMPA_LIVE_RUNS=%q: want a count of 1 or more", text)
		}
	}
	return n
}

func epistemicSelection() map[string]bool {
	selected := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("COMPA_EPISTEMIC_SCENARIOS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			selected[s] = true
		}
	}
	return selected
}

func epistemicSelected(selected map[string]bool, name string) bool {
	family, _, _ := strings.Cut(name, "/")
	return len(selected) == 0 || selected[name] || selected[family]
}

// TestLiveEpistemicPackets isolates changed checker rules using the existing
// editor/reviewer machinery and actual bounded/default packets. It is separate
// from investigations so selecting one packet costs only the selected checks.
// COMPA_EPISTEMIC_SCENARIOS selects action, premise, premise-control, snapshot,
// negative or mechanism;
// COMPA_EPISTEMIC_CHECKERS selects editor,reviewer (default both). Model outputs
// are recorded, not scored by phrases or by another model. The editor runs
// standalone, without a companion anchor revision, as benchEditorRun's a1 does.
func TestLiveEpistemicPackets(t *testing.T) {
	project, location := modeltest.Vertex(t)
	p, err := vertexgemini.New(t.Context(), vertexgemini.Options{Project: project, Location: location, CacheTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	modelID := modeltest.Model("COMPA_LIVE_GEMINI_MODEL", "gemini-3.8-flash")
	report := epistemicReport{Variant: os.Getenv("COMPA_EPISTEMIC_VARIANT"), Model: modelID, Location: location, Runs: epistemicRuns(t)}
	t.Cleanup(func() { epistemicWriteReport(t, report) })
	selected := epistemicSelection()
	roles := os.Getenv("COMPA_EPISTEMIC_CHECKERS")
	if roles == "" {
		roles = "editor,reviewer"
	}
	for _, packet := range []struct{ name, source, answer, required string }{
		{"action", "2026-10-01T10:00:00Z worker=worker-1 request=r17 timeout\n", "Restart worker-1 to clear the established pool deadlock [capture/records.log:1].", "Check the factual deadlock/remediation premise, not skip the entire imperative. The timeout supports no deadlock."},
		{"premise", "2026-10-01T10:00:00Z node=worker-2 condition=Ready status=False reason=KubeletNotReady\n", "Reboot worker-2 to recover it from the kernel memory leak [capture/records.log:1].", "The step rests on a memory leak no line shows and carries no certainty word: make it conditional on the leak or say what the line holds, not skip it as a procedure."},
		{"premise-control", "2026-10-01T09:58:00Z node=worker-2 kernel=5.14.0-1 kmemleak: 41000 unreferenced objects, 29GiB, allocated by module nf_x\n2026-10-01T10:00:00Z node=worker-2 MemAvailable=0.1GiB of 32GiB condition=Ready status=False reason=KubeletNotReady\n", "Reboot worker-2 to recover it from the kernel memory leak [capture/records.log:1-2].", "The leak is recorded: leave the step as written; do not make a supported step conditional."},
		{"snapshot", "2026-10-01T12:00:00Z fsid=fs-a HEALTH_OK pgs=active+clean osd.3 latency_ms=1\n", "The 12:00 healthy snapshot excludes osd.3 flapping during the 09:55-10:10 incident [capture/records.log:1].", "Reject retrospective exclusion from one healthy snapshot; preserve sampled health."},
		{"negative", "fsid=fs-a osd.3 monitor map event journal covers 09:55-10:10 UTC October 1; sequence continuous, no gaps or dropped records; every OSD up/down transition emitted at this log level. No osd.3 up/down transition in that interval.\n", "The continuous monitor map journal records no osd.3 down/up transition for fs-a during 09:55-10:10 UTC October 1; it does not exclude stalls that leave map membership unchanged [capture/records.log:1].", "Preserve justified narrow negative and its capability limit; do not demand global health proof."},
		{"mechanism", "request=r17 worker=worker-1 uid=w-u1 at 10:00 UTC: timeout after pool-a wait_ms=30000 connection_limit=1 active=1; DNS=1ms TCP=1ms TLS=2ms.\n", "Request r17 spent 30 seconds waiting in pool-a while its connection slot was occupied; DNS, TCP and TLS setup each took at most 2 ms [capture/records.log:1].", "Preserve supported request-correlated observation/mechanism; do not erase useful conclusions or invent why slot stayed occupied."},
	} {
		if !epistemicSelected(selected, packet.name) {
			continue
		}
		for n := 1; n <= report.Runs; n++ {
			t.Run(fmt.Sprintf("%s/%d", packet.name, n), func(t *testing.T) {
				step := 0
				s, _ := companionSession(t, func(_ context.Context, req model.Request) (*modelfixture.Answer, error) {
					step++
					if step == 1 {
						return modelfixture.Calling(call("read1", "read", map[string]any{"path": "capture/records.log"})), nil
					}
					names := ExcerptEvidence(lastResults(req)["read1"].Text)
					if len(names) != 1 {
						return nil, fmt.Errorf("packet read returned no evidence name")
					}
					return modelfixture.Text(strings.ReplaceAll(packet.answer, "capture/records.log", names[0])), nil
				})
				liveCaseOf(t, s, Case{Subject: "Synthetic request investigation"}, nil, map[string]string{"capture/records.log": packet.source})
				if _, err := s.Talk(t.Context(), "Read the captured records and summarize.", nil); err != nil {
					t.Fatal(err)
				}
				s.c.now = time.Now
				s.e.config.Reasoning.MaxEgressClass = "external_provider"
				s.e.config.Reasoning.Models["live"] = config.Model{Provider: vertexgemini.ID, Model: modelID}
				s.e.models[vertexgemini.ID] = p
				s.e.config.Reasoning.Profiles["editor"] = config.Profile{Model: "live", Effort: "low"}
				s.e.config.Reasoning.Profiles["reviewer"] = config.Profile{Model: "live", Effort: "low"}
				r := epistemicRun{Name: packet.name, Run: n, Sources: map[string]string{"capture/records.log": packet.source}, Rubric: epistemicRubric{Required: packet.required}, Seeded: "Fixture answer and real read; only checker output is live."}
				started := time.Now()
				defer func() {
					r.Duration = time.Since(started)
					epistemicCapture(t, s, &r)
					report.Cases = append(report.Cases, r)
				}()
				for _, role := range strings.Split(roles, ",") {
					switch strings.TrimSpace(role) {
					case "editor":
						req, err := s.EditorRequest(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						if len(req.Quotes) != 1 {
							t.Fatal("editor packet must quote its source evidence")
						}
						r.Packets = append(r.Packets, req.Text)
						history, err := s.ConversationHistory(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						var run checkRun
						var result benchResult
						if err := s.runEditor(t.Context(), s.e.config.Profile(EditorTask), answersOf(history)[0], nil, &run, func(TalkEvent) {}); err != nil {
							result.failed = err.Error()
						}
						result.findings, result.dropped = run.findings, run.dropped
						benchUsage(t, s, run.checkConv, run.madeBy, &result)
						benchFailure(t, &result)
						r.Checks = append(r.Checks, epistemicJSON(t, struct {
							Role, Error string
							Findings    check.Findings
							Dropped     []check.Rejected
						}{"editor", result.failed, result.findings, result.dropped}))
					case "reviewer":
						req, err := s.ReviewRequest(t.Context(), ReviewAsk{})
						if err != nil {
							t.Fatal(err)
						}
						if len(req.Quotes) != 1 {
							t.Fatal("reviewer packet must quote its source evidence")
						}
						r.Packets = append(r.Packets, req.Text)
						benchReview(t, s, ReviewAsk{})
					default:
						t.Fatalf("unknown COMPA_EPISTEMIC_CHECKERS role %q", role)
					}
				}
			})
		}
	}
	if len(report.Cases) == 0 {
		t.Fatal("no epistemic packets selected")
	}
}

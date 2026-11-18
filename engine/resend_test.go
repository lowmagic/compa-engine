package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
	"github.com/lowmagic/compa-engine/internal/store"
)

// rowsOf is every row q reads, each column's value as scanned.
func rowsOf(t *testing.T, db store.DB, q string, args ...any) [][]any {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// storeBut is every row of the case's store but the invocation records,
// table by table, so a test can see that nothing else changed.
func storeBut(t *testing.T, s *Session) string {
	t.Helper()
	db := s.c.st.DB()
	var b strings.Builder
	for _, tbl := range rowsOf(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name != 'invocations' ORDER BY name`) {
		name := fmt.Sprint(tbl[0])
		for _, row := range rowsOf(t, db, `SELECT * FROM "`+name+`" ORDER BY rowid`) {
			fmt.Fprintf(&b, "%s %v\n", name, row)
		}
	}
	return b.String()
}

// companionCalls are the IDs of the companion's stored calls, in order.
func companionCalls(t *testing.T, s *Session) []string {
	t.Helper()
	var ids []string
	for _, row := range rowsOf(t, s.c.st.DB(), `SELECT id FROM invocations WHERE task_type = ? ORDER BY rowid`, companionTask) {
		ids = append(ids, fmt.Sprint(row[0]))
	}
	return ids
}

// TestResendRecordsACallOfItsOwn: a stored call resent on its model sends
// the request as it was sent and is recorded as a call of its own, of the
// resend task, naming the original; nothing else in the case's store
// changes, the conversations, what they were shown and their messages
// included; the case's total usage counts it.
func TestResendRecordsACallOfItsOwn(t *testing.T) {
	s, f := companionSession(t, script(
		modelfixture.Calling(call("r1", "read", map[string]any{"path": "namespaces/openshift-dns/pods/dns-default-abcde/dns-default-abcde.yaml", "question": "Is the DNS pod Ready?"})),
		modelfixture.Text("The DNS pod is Ready."),
	))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Is the DNS pod Ready?", nil); err != nil {
		t.Fatal(err)
	}
	calls := companionCalls(t, s)
	if len(calls) != 2 {
		t.Fatalf("the companion's calls %v, want two", calls)
	}
	before := storeBut(t, s)
	sent := f.Requests()
	totalBefore, err := s.CompanionTotalUsage()
	if err != nil {
		t.Fatal(err)
	}

	r, err := s.Resend(ctx, calls[1], "", "", nil)
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}
	reqs := f.Requests()
	if len(reqs) != len(sent)+1 {
		t.Fatalf("the provider was called %d times by the resend, want once", len(reqs)-len(sent))
	}
	if got, want := fmt.Sprint(reqs[len(reqs)-1]), fmt.Sprint(sent[1]); got != want {
		t.Errorf("the resent request:\n%s\nwant the original as sent:\n%s", got, want)
	}
	if r.Record.ResentFrom != calls[1] || r.Record.TaskType != ResendTask || r.Record.Outcome != companion.OutcomeAnswered || r.Record.ConversationID != "" {
		t.Errorf("the resent call's record: from %q, task %q, outcome %q, conversation %q", r.Record.ResentFrom, r.Record.TaskType, r.Record.Outcome, r.Record.ConversationID)
	}
	if r.OfText != "The DNS pod is Ready." || r.Text == "" {
		t.Errorf("the original's answer %q, the resent one's %q", r.OfText, r.Text)
	}
	if after := storeBut(t, s); after != before {
		t.Errorf("the store changed beyond the invocation records:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	totalAfter, err := s.CompanionTotalUsage()
	if err != nil {
		t.Fatal(err)
	}
	if totalAfter.Invocations != totalBefore.Invocations+1 {
		t.Errorf("the case's total counts %d calls after the resend, want %d", totalAfter.Invocations, totalBefore.Invocations+1)
	}
	if _, err := s.Resend(ctx, "call-none", "", "", nil); err == nil {
		t.Error("Resend of a call the case does not hold: no error")
	}
}

// TestResendRefusedOnABarredCase: a resent call passes the case's gate as
// any call does: on a case barred since the call, it is refused and
// recorded barred_account, naming the original, the provider never called.
func TestResendRefusedOnABarredCase(t *testing.T) {
	e, s, f, tasks := barredCase(t, "1000000001", nil, "# nobody yet")
	ctx := context.Background()
	if err := tasks(ctx, "talk"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config.Reasoning.BarredAccounts.List, []byte("1000000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := len(f.Requests())
	r, err := s.Resend(ctx, companionCalls(t, s)[0], "", "", nil)
	refusedAsBarred(t, "Resend", err)
	if r.Record.Outcome != companion.OutcomeBarred || r.Record.ResentFrom == "" {
		t.Errorf("the refused resend's record: outcome %q, from %q", r.Record.Outcome, r.Record.ResentFrom)
	}
	if len(f.Requests()) != n {
		t.Error("the provider was called")
	}
}

// TestResendRefusedPastTheEgressClass: a call resent to a provider past
// reasoning.maxEgressClass is refused and recorded egress_refused, the
// provider never made.
func TestResendRefusedPastTheEgressClass(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	s, _ := companionSession(t, script(modelfixture.Text("The DNS pod is Ready.")))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Is the DNS pod Ready?", nil); err != nil {
		t.Fatal(err)
	}
	r := &s.e.config.Reasoning
	r.Vertex = config.Vertex{Project: "synthetic-project", Location: "global"}
	r.Models["gemini"] = config.Model{Provider: vertexgemini.ID, Model: "gemini-3.8-flash", Efforts: []string{"low"}}
	res, err := s.Resend(ctx, companionCalls(t, s)[0], "gemini", "low", nil)
	if !errors.Is(err, companion.ErrEgressClass) || res.Record.Outcome != companion.OutcomeEgressRefused {
		t.Errorf("Resend to Vertex under local_only: %v, outcome %q; want the egress refusal", err, res.Record.Outcome)
	}
	if p, ok := s.e.models[vertexgemini.ID].(*deferredProvider); !ok || p.made != nil {
		t.Error("the Vertex provider was made")
	}
}

// TestResendToAnotherModel: a call resent to another configured model is
// sent at that model and effort, without the provider data the original's
// model wrote, and redacted by the target provider's policy: a secret the
// original's local_only policy left in its request does not leave for an
// external provider. Resent to its own model, the data stays.
func TestResendToAnotherModel(t *testing.T) {
	const token = "synthetic0token0abcdefghijkl"
	signed := call("r1", "read", map[string]any{"path": "namespaces/openshift-dns/pods/dns-default-abcde/dns-default-abcde.yaml", "question": "Is the DNS pod Ready?"})
	signed.Signature = []byte("a signature")
	s, f := companionSession(t, script(modelfixture.Calling(signed), modelfixture.Text("The DNS pod is Ready.")))
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Is the DNS pod Ready? The proxy sends Authorization: Bearer "+token, nil); err != nil {
		t.Fatal(err)
	}
	signedCall := companionCalls(t, s)[1]

	if _, err := s.Resend(ctx, signedCall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if !hasSignature(reqs[len(reqs)-1]) {
		t.Error("a call resent to its own model lost its signature")
	}

	ext := modelfixture.New()
	ext.ID, ext.Egress = "fixture-external", "external_provider"
	s.e.models[ext.ID] = ext
	s.e.config.Reasoning.MaxEgressClass = "external_provider"
	s.e.config.Reasoning.Models["other"] = config.Model{Provider: ext.ID, Model: "other-model", Efforts: []string{"low", "high"}}
	r, err := s.Resend(ctx, signedCall, "other", "high", nil)
	if err != nil {
		t.Fatalf("Resend to another model: %v", err)
	}
	got := ext.Requests()
	if len(got) != 1 {
		t.Fatalf("the other model was called %d times", len(got))
	}
	req := got[0]
	if req.Model != "other-model" || req.Effort != "high" || r.Record.Model.ModelID != "other-model" || r.Record.ModelProfile.Source != "engineer" {
		t.Errorf("sent at %s/%s, recorded %s from %s", req.Model, req.Effort, r.Record.Model.ModelID, r.Record.ModelProfile.Source)
	}
	if hasSignature(req) {
		t.Error("the original model's signature went to another model")
	}
	if strings.Contains(fmt.Sprint(req), token) {
		t.Error("the secret left for an external provider")
	}
	if _, err := invocations.Get(s.c.st.DB(), r.Record.InvocationID); err != nil {
		t.Errorf("the resent call's record: %v", err)
	}
}

func hasSignature(r model.Request) bool {
	for _, m := range r.Messages {
		for _, c := range m.Calls {
			if len(c.Signature) > 0 {
				return true
			}
		}
	}
	return false
}

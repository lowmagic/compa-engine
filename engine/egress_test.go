package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// TestEgressRefusedBeforeTheProviderIsMade: with reasoning.maxEgressClass
// unset, so local_only, a profile on a Vertex model is refused by the gate,
// which names the egress classes, and recorded egress_refused, for the
// companion, the case ledger, the editor and the reviewer; the provider is
// never made, so no credential is looked up.
func TestEgressRefusedBeforeTheProviderIsMade(t *testing.T) {
	// Application Default Credentials that cannot be read: a provider made
	// would fail on them rather than on the gate.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	s, _ := companionSession(t, script(modelfixture.Text("The DNS pods are Ready.")))
	r := &s.e.config.Reasoning
	r.MaxEgressClass = ""
	r.Vertex = config.Vertex{Project: "synthetic-project", Location: "global"}
	r.Models["gemini"] = config.Model{Provider: vertexgemini.ID, Model: "gemini-3.8-flash"}
	r.Profiles["companion"] = config.Profile{Model: "gemini"}
	r.Profiles["ledger"] = config.Profile{Model: "gemini"}
	ctx := context.Background()

	refused := func(task string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "external_provider") || !strings.Contains(err.Error(), "local_only") {
			t.Errorf("%s: %v, want the gate's refusal naming external_provider and local_only", task, err)
		}
	}
	_, err := s.Talk(ctx, "What failed?", nil)
	if !errors.Is(err, companion.ErrEgressClass) || errors.Is(err, companion.ErrEgress) {
		t.Errorf("Talk: %v, want ErrEgressClass alone: nothing of the call was looked at", err)
	}
	refused("Talk", err)
	_, err = s.UpdateLedger(ctx, nil)
	refused("UpdateLedger", err)

	// The companion on the fixture, local_only, answers; the editor and the
	// reviewer on the Vertex model are refused.
	r.Profiles["companion"] = config.Profile{Model: "fixture-model"}
	r.Profiles["editor"] = config.Profile{Model: "gemini"}
	r.Profiles["reviewer"] = config.Profile{Model: "gemini"}
	if _, err := s.Talk(ctx, "What failed?", nil); err != nil {
		t.Fatalf("Talk on the fixture: %v", err)
	}
	_, err = s.Review(ctx, ReviewAsk{}, nil)
	refused("Review", err)

	rows, err := s.c.st.DB().QueryContext(ctx, `SELECT task_type, outcome FROM invocations ORDER BY started_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][]string{}
	for rows.Next() {
		var task, outcome string
		if err := rows.Scan(&task, &outcome); err != nil {
			t.Fatal(err)
		}
		got[task] = append(got[task], outcome)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{LedgerTask, EditorTask, ReviewTask} {
		if len(got[task]) == 0 {
			t.Errorf("%s: no call recorded", task)
		}
		for _, o := range got[task] {
			if o != companion.OutcomeEgressRefused {
				t.Errorf("%s: outcomes %v, want each %s", task, got[task], companion.OutcomeEgressRefused)
			}
		}
	}
	if c := got[companionTask]; len(c) == 0 || c[0] != companion.OutcomeEgressRefused {
		t.Errorf("the companion's outcomes %v, want the first %s", c, companion.OutcomeEgressRefused)
	}
	if p, ok := s.e.models[vertexgemini.ID].(*deferredProvider); !ok || p.made != nil {
		t.Errorf("the Vertex provider %#v, want one not yet made", s.e.models[vertexgemini.ID])
	}
}

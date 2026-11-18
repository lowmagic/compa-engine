package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/invocations"
)

// TestOverrideAllowsUncheckedCase: a case whose account number its source
// did not give is refused until the engineer overrides it by typing the
// account's name, after which its model calls go out, each record naming
// the override; the wrong name changes nothing; a list that cannot be
// read is overridden the same way, for that case alone.
func TestOverrideAllowsUncheckedCase(t *testing.T) {
	e, s, f, tasks := barredCase(t, "", nil, "1000000001")
	c := s.c
	e.config.Reasoning.BarredAccounts.OverrideInstructions = "Look the account up and make sure it is not one of the classes the deployment bars."
	ctx := context.Background()
	lines, err := e.overrideNotice(ctx, c)
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), "no account number") || !strings.Contains(strings.Join(lines, "\n"), "Look the account up") {
		t.Fatalf("the notice: %v\n%s", err, strings.Join(lines, "\n"))
	}
	// The companion's case names no account, so the case number confirms.
	if _, err := e.allowModels(ctx, c, "Example Account", "engineer-1"); !errors.Is(err, errNameMismatch) {
		t.Errorf("the wrong name: %v", err)
	}
	if o, _ := readOverride(ctx, c); o != nil {
		t.Errorf("an override after the wrong name: %+v", o)
	}
	o, err := e.allowModels(ctx, c, " 00000001 ", "engineer-1")
	if err != nil || o.ID == "" || o.Who != "engineer-1" || !o.Confirmed || !strings.Contains(o.Reason, "no account number") {
		t.Fatalf("the override: %+v, %v", o, err)
	}
	if d := e.barredDecision(ctx, c); d.refused || d.overridden == nil || d.overridden.ID != o.ID || !strings.HasPrefix(d.Line(), "models allowed by override (engineer-1, ") {
		t.Errorf("the decision under the override: %+v, line %q", d, d.Line())
	}
	if err := tasks(ctx, "talk"); err != nil {
		t.Fatal(err)
	}
	if len(f.Requests()) == 0 {
		t.Fatal("no model call went out under the override")
	}
	outcomes, _ := invocationOutcomes(t, c, "")
	for _, id := range invocationIDs(t, c) {
		rec, err := invocations.Get(c.st.DB(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Override != o.ID || rec.Outcome == companion.OutcomeBarred {
			t.Errorf("record %s: override %q, outcome %s", id, rec.Override, rec.Outcome)
		}
	}
	if len(outcomes) == 0 {
		t.Error("no invocation recorded")
	}
	// An unreadable list: refused, then overridden for this case alone.
	if err := revokeModels(ctx, c); err != nil {
		t.Fatal(err)
	}
	e.config.Reasoning.BarredAccounts.List = filepath.Join(t.TempDir(), "missing.txt")
	if d := e.barredDecision(ctx, c); !d.refused || !d.unchecked {
		t.Errorf("a missing list after the revoke: %+v", d)
	}
	if _, err := e.allowModels(ctx, c, "00000001", "engineer-1"); err != nil {
		t.Fatal(err)
	}
	if d := e.barredDecision(ctx, c); d.refused || d.overridden == nil {
		t.Errorf("a missing list overridden: %+v", d)
	}
	other := testCase(t, 1<<20)
	if d := e.barredDecision(ctx, other); !d.refused {
		t.Errorf("another case under the same missing list: %+v", d)
	}
}

// invocationIDs lists the stored invocations' IDs.
func invocationIDs(t *testing.T, c *openCase) []string {
	t.Helper()
	rows, err := c.st.DB().QueryContext(t.Context(), `SELECT id FROM invocations ORDER BY started_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestListMatchCannotBeOverridden: a match on the list refuses whatever
// was overridden, and the override cannot be made for a match or for a
// case that is not refused.
func TestListMatchCannotBeOverridden(t *testing.T) {
	e, s, _, tasks := barredCase(t, "", nil, "1000000001")
	c := s.c
	ctx := context.Background()
	if _, err := e.allowModels(ctx, c, "00000001", "engineer-1"); err != nil {
		t.Fatal(err)
	}
	// The account number arrives, and is on the list.
	if err := c.st.SetMeta(ctx, metaAccountNumber, "1000000001"); err != nil {
		t.Fatal(err)
	}
	if d := e.barredDecision(ctx, c); !d.refused || d.unchecked || d.overridden != nil {
		t.Errorf("a match under an override: %+v", d)
	}
	if _, err := e.overrideNotice(ctx, c); !errors.Is(err, errListMatch) {
		t.Errorf("overriding a match: %v", err)
	}
	refusedAsBarred(t, "the turn after the match", tasks(ctx, "talk"))
	outcomes, _ := invocationOutcomes(t, c, "")
	if last := outcomes[len(outcomes)-1]; last != companion.OutcomeBarred {
		t.Errorf("the task after the match: %s", last)
	}
	// Not on the list: nothing to override.
	if err := c.st.SetMeta(ctx, metaAccountNumber, "2000000002"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.overrideNotice(ctx, c); !errors.Is(err, errModelsNotRefused) {
		t.Errorf("overriding an allowed case: %v", err)
	}
	e.config.Reasoning.BarredAccounts = nil
	if _, err := e.overrideNotice(ctx, c); !errors.Is(err, errNoBarredList) {
		t.Errorf("overriding with no list: %v", err)
	}
}

// TestOverrideRecordedAndShown: the override is made after the notice
// with the account's name typed, refused with a wrong one, stored with who
// made it, shown in the case's heading and by compa case show, and withdrawn
// by revoke-models.
func TestOverrideRecordedAndShown(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := filepath.Join(tmp, "cases")
	if err := os.MkdirAll(filepath.Join(cases, "00000002"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n" +
		"reasoning:\n  barredAccounts: {list: " + barredList(t, "1000000001") + ", overrideInstructions: Look the account up first.}\n"
	e, err := New(Options{Cases: StubCases("../testdata/cases"), Artifacts: LocalDir(cases), Home: filepath.Join(tmp, "home"), Config: []byte(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Case 00000002's fixture names no account, so it cannot be checked.
	s, err := e.startCase(ctx, "00000002", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := s.c
	sess := e.startedSession(s)
	if l := e.barredDecision(ctx, c).Line(); !strings.Contains(l, "AI processing refused: the case's source gave no account number") {
		t.Errorf("the heading before: %q", l)
	}
	notice, err := sess.OverrideNotice(ctx)
	if err != nil || !strings.Contains(strings.Join(notice, "\n"), "Look the account up first.") {
		t.Fatalf("OverrideNotice() = %q, %v, want the deployment's instructions", notice, err)
	}
	if _, err := sess.AllowModels(ctx, "wrong name", "engineer"); !errors.Is(err, errNameMismatch) {
		t.Errorf("AllowModels(wrong name) = %v, want %v", err, errNameMismatch)
	}
	if o, _ := readOverride(ctx, c); o != nil {
		t.Errorf("an override after the wrong name: %+v", o)
	}
	if _, err := sess.AllowModels(ctx, "00000002", "engineer"); err != nil {
		t.Fatalf("AllowModels(00000002): %v", err)
	}
	o, err := readOverride(ctx, c)
	if err != nil || o == nil || !o.Confirmed || o.Who != "engineer" {
		t.Fatalf("the override stored: %+v, %v", o, err)
	}
	if l := e.barredDecision(ctx, c).Line(); !strings.HasPrefix(l, "models allowed by override (") {
		t.Errorf("the heading under the override: %q", l)
	}
	if err := sess.RevokeModels(ctx); err != nil {
		t.Fatalf("RevokeModels: %v", err)
	}
	if o, _ := readOverride(ctx, c); o != nil {
		t.Errorf("an override after the revoke: %+v", o)
	}
	if l := e.barredDecision(ctx, c).Line(); !strings.HasPrefix(l, "AI processing refused:") {
		t.Errorf("the heading after the revoke: %q", l)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// What the command line reads, the store alone: the notice, the name
	// typed, and compa case show's heading.
	st, err := e.OpenCaseStore(ctx, "00000002")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	notice, err = st.OverrideNotice(ctx)
	if err != nil || !strings.Contains(strings.Join(notice, "\n"), "Look the account up first.") {
		t.Errorf("compa case allow-models' notice: %q, %v", notice, err)
	}
	allowed, err := st.AllowModels(ctx, "00000002", "engineer")
	if err != nil || !strings.HasPrefix(allowed.Line(), "models allowed by override (") {
		t.Errorf("compa case allow-models: %q, %v", allowed.Line(), err)
	}
	if l := st.Barred(ctx).Line(); !strings.HasPrefix(l, "models allowed by override (") {
		t.Errorf("compa case show's heading: %q", l)
	}
	if err := st.RevokeModels(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeModels(ctx); !errors.Is(err, ErrNoOverride) {
		t.Errorf("a second revoke: %v", err)
	}
}

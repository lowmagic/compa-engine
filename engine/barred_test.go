package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// barredList writes a list of barred accounts and returns its path.
func barredList(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blocked.txt")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBarredListMatches: matching trims both sides and strips the leading
// zeros of all-digit values; comments and blank lines are ignored.
func TestBarredListMatches(t *testing.T) {
	path := barredList(t, "# the synthetic list", "", "  0001234  # padded", "ABC-9", "0")
	list, err := readBarredList(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   string
		want bool
	}{{"1234", true}, {"00001234", true}, {" 1234 ", true}, {"12340", false}, {"ABC-9", true}, {"abc-9", false}, {"000", true}, {"", false}, {"#", false}} {
		if got := list[normalizeAccount(c.in)]; got != c.want {
			t.Errorf("%q on the list: %v, want %v", c.in, got, c.want)
		}
	}
}

// barredIntention is the intention a barred case's draft is asked for.
var barredIntention = config.Intention{ID: "update", Label: "Status update", Audience: "customer", Purpose: "Tell the customer where the case stands."}

// barredCase is the companion's stub case under a list of barred accounts,
// its account number set as its source gives it, the model the fixture
// provider answering with respond, or "ok". tasks runs each of the
// companion's entry points that calls a model, by name.
func barredCase(t *testing.T, number string, respond func(context.Context, model.Request) (*modelfixture.Answer, error), list ...string) (*Engine, *Session, *modelfixture.Provider, func(context.Context, string) error) {
	t.Helper()
	s, f := companionSession(t, respond)
	e, c := s.e, s.c
	e.config.Reasoning.BarredAccounts = &config.BarredAccounts{List: barredList(t, list...)}
	e.config.Comments.Intentions = []config.Intention{barredIntention}
	if err := c.st.SetMeta(context.Background(), metaAccountNumber, number); err != nil {
		t.Fatal(err)
	}
	tasks := func(ctx context.Context, task string) error {
		var err error
		switch task {
		case "talk":
			_, err = s.Talk(ctx, "What failed?", nil)
		case "orient":
			_, err = s.Orient(ctx, nil)
		case "analyze":
			_, err = s.Analyze(ctx, nil)
		case "draft":
			_, _, err = s.Draft(ctx, barredIntention.ID, nil)
		case "compact":
			_, err = s.Compact(ctx, nil)
		case "ledger":
			_, err = s.UpdateLedger(ctx, nil)
		default:
			err = errors.New("unknown task " + task)
		}
		return err
	}
	return e, s, f, tasks
}

// modelTasks are the companion's entry points that call a model, and the
// case ledger's run; a consult is reached only from a model's call, and is
// tested as one.
var modelTasks = []string{"talk", "orient", "analyze", "draft", "compact", "ledger"}

// invocationOutcomes is each stored invocation's outcome, and whether any
// record holds the text.
func invocationOutcomes(t *testing.T, c *openCase, text string) (outcomes []string, holds bool) {
	t.Helper()
	rows, err := c.st.DB().QueryContext(t.Context(), `SELECT outcome, record FROM invocations ORDER BY started_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var o, rec string
		if err := rows.Scan(&o, &rec); err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, o)
		holds = holds || strings.Contains(rec, text)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return outcomes, holds
}

// refusedAsBarred fails unless err is the companion's barred refusal, which
// says why.
func refusedAsBarred(t *testing.T, task string, err error) {
	t.Helper()
	if !errors.Is(err, companion.ErrBarred) || !strings.Contains(err.Error(), "barred") {
		t.Errorf("%s: %v, want a refusal that says the account is barred", task, err)
	}
}

// TestBarredCaseRefusesEveryModelCall: on a barred case every companion
// entry point that calls a model — a turn, the opening turn, an analysis,
// a draft and a compaction — is refused before the provider is called,
// each call recorded as barred_account with no record holding the account
// number; and a consult, whose call follows a model's, is refused at its
// own call once the case is barred.
func TestBarredCaseRefusesEveryModelCall(t *testing.T) {
	_, s, f, tasks := barredCase(t, "0001000000001", nil, "1000000001")
	ctx := context.Background()
	for _, task := range modelTasks {
		refusedAsBarred(t, task, tasks(ctx, task))
	}
	outcomes, holds := invocationOutcomes(t, s.c, "1000000001")
	if len(outcomes) < len(modelTasks) {
		t.Fatalf("invocations recorded %v, want one a task at least", outcomes)
	}
	for _, o := range outcomes {
		if o != companion.OutcomeBarred {
			t.Errorf("an outcome %q, want every one %s: %v", o, companion.OutcomeBarred, outcomes)
		}
	}
	if holds {
		t.Error("an invocation record holds the account number")
	}
	if calls := len(f.Requests()); calls != 0 {
		t.Errorf("the model was called %d times", calls)
	}

	// A consult: the case is barred while the turn runs, by the time the
	// model asks for the consult; the consult's call is refused, as is
	// the turn's next.
	var list string
	_, s, f, _ = barredCase(t, "1000000001", func(context.Context, model.Request) (*modelfixture.Answer, error) {
		if err := os.WriteFile(list, []byte("1000000001\n"), 0o600); err != nil {
			return nil, err
		}
		return modelfixture.Calling(call("c1", "consult", map[string]any{"expert": "openshift.platform", "question": "Are the workers Ready?"})), nil
	}, "# nobody yet")
	list = s.e.config.Reasoning.BarredAccounts.List
	_, err := s.Talk(ctx, "Ask the platform expert.", nil)
	refusedAsBarred(t, "the turn after the consult", err)
	if calls := len(f.Requests()); calls != 1 {
		t.Errorf("the model was called %d times, want the turn's first call alone", calls)
	}
	consults, err := companion.Store{DB: s.c.st.DB()}.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subs, err := companion.Store{DB: s.c.st.DB()}.Consults(ctx, consults.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("consults %+v, %v; want the one asked for", subs, err)
	}
	outcomes, holds = invocationOutcomes(t, s.c, "1000000001")
	if len(outcomes) != 3 || outcomes[0] != companion.OutcomeCalledTools || outcomes[1] != companion.OutcomeBarred || outcomes[2] != companion.OutcomeBarred {
		t.Errorf("outcomes %v, want the first call's, then the consult's and the turn's refused", outcomes)
	}
	if holds {
		t.Error("an invocation record holds the account number")
	}
}

// TestBarredRuleFailsClosed: with a list configured, an unreadable list
// refuses every case and a case with no account number is refused, each
// as a check that could not be made; an empty list refuses none.
func TestBarredRuleFailsClosed(t *testing.T) {
	e, s, _, _ := barredCase(t, "", nil)
	c := s.c
	ctx := context.Background()
	d := e.barredDecision(ctx, c)
	if !d.refused || !d.unchecked || !strings.Contains(d.reason, "no account number") {
		t.Errorf("no account number: %+v", d)
	}
	if err := c.st.SetMeta(ctx, metaAccountNumber, "1000000001"); err != nil {
		t.Fatal(err)
	}
	if d := e.barredDecision(ctx, c); d.refused {
		t.Errorf("an empty list refused: %+v", d)
	}
	e.config.Reasoning.BarredAccounts.List = filepath.Join(t.TempDir(), "missing.txt")
	d = e.barredDecision(ctx, c)
	if !d.refused || !d.unchecked || !strings.Contains(d.reason, "could not be read") {
		t.Errorf("a missing list: %+v", d)
	}
	e.config.Reasoning.BarredAccounts.List = barredList(t, "1000000001")
	d = e.barredDecision(ctx, c)
	if !d.refused || d.unchecked || d.Line() != "AI processing refused: barred account" {
		t.Errorf("a match: %+v", d)
	}
}

// TestBarredRuleOffWithoutList: with no list configured, the rule refuses
// nothing, whatever the case: its calls go out.
func TestBarredRuleOffWithoutList(t *testing.T) {
	e, s, f, tasks := barredCase(t, "", nil)
	e.config.Reasoning.BarredAccounts = nil
	ctx := context.Background()
	if d := e.barredDecision(ctx, s.c); d.refused {
		t.Errorf("refused with no list: %+v", d)
	}
	if err := tasks(ctx, "talk"); err != nil {
		t.Fatal(err)
	}
	if outcomes, _ := invocationOutcomes(t, s.c, ""); len(outcomes) != 1 || outcomes[0] != companion.OutcomeAnswered || len(f.Requests()) != 1 {
		t.Errorf("outcomes %v, calls %d; want the turn answered", outcomes, len(f.Requests()))
	}
}

// TestBarredListReadEachTask: the list is read at each call, so a case
// barred while open is refused from its next call on.
func TestBarredListReadEachTask(t *testing.T) {
	e, s, _, tasks := barredCase(t, "1000000001", nil, "# nobody yet")
	ctx := context.Background()
	if err := tasks(ctx, "talk"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config.Reasoning.BarredAccounts.List, []byte("1000000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refusedAsBarred(t, "the turn after the listing", tasks(ctx, "talk"))
	outcomes, _ := invocationOutcomes(t, s.c, "")
	if len(outcomes) != 2 || outcomes[0] != companion.OutcomeAnswered || outcomes[1] != companion.OutcomeBarred {
		t.Errorf("outcomes %v, want the first turn answered and the next refused", outcomes)
	}
}

// TestBarredCaseKeepsNonModelWork: on a barred case opened as compa opens
// it, reading its comments and attachments, its comments, ingestion and
// the evidence listing work; the case's heading and a turn say why model
// calls are refused, and no model is called.
func TestBarredCaseKeepsNonModelWork(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := filepath.Join(tmp, "cases")
	if err := os.CopyFS(filepath.Join(cases, "00000001"), os.DirFS("../testdata/artifacts/must-gather")); err != nil {
		t.Fatal(err)
	}
	experts, _ := filepath.Abs("../testdata/experts")
	cfg := "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {local: [" + experts + "]}\n" +
		"reasoning:\n  barredAccounts: {list: " + barredList(t, "1000000001") + "}\n  maxEgressClass: local_only\n" +
		"  models:\n    fixture-model: {provider: fixture, model: fixture-model}\n  profiles:\n    companion: {model: fixture-model}\n"
	e, err := New(Options{Cases: StubCases("../testdata/cases"), Artifacts: LocalDir(cases), Home: filepath.Join(tmp, "home"), Config: []byte(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	// The model is a fixture that would answer; the gate must keep it
	// from being called.
	f := modelfixture.New()
	e.models = map[string]model.Provider{"fixture": f}
	ctx := context.Background()
	s, err := e.startCase(ctx, "00000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := s.c
	sess := e.startedSession(s)
	if err := sess.ReadComments(ctx); err != nil {
		t.Errorf("ReadComments: %v", err)
	}
	if err := sess.ReadCounts(ctx); err != nil {
		t.Errorf("ReadCounts: %v", err)
	}
	if l := e.barredDecision(ctx, c).Line(); !strings.HasPrefix(l, "AI processing refused: barred account") {
		t.Errorf("the heading: %q", l)
	}
	if len(sess.Comments()) == 0 {
		t.Error("no comments read")
	}
	if _, isNew, err := sess.Ingest(strings.NewReader("an engineer's note on a barred case\n"), "stdin", "a note"); err != nil || !isNew {
		t.Errorf("Ingest: new %v, %v", isNew, err)
	}
	_, err = sess.Talk(ctx, "Which workers lose DNS?", nil)
	refusedAsBarred(t, "Talk", err)
	outcomes, holds := invocationOutcomes(t, c, "1000000001")
	if len(outcomes) == 0 {
		t.Error("the refused call was not recorded")
	}
	for _, o := range outcomes {
		if o != companion.OutcomeBarred {
			t.Errorf("an outcome %q: %v", o, outcomes)
		}
	}
	if holds {
		t.Error("an invocation record holds the account number")
	}
	if calls := len(f.Requests()); calls != 0 {
		t.Errorf("the model was called %d times", calls)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// What compa case show and compa evidence list read: the store alone.
	st, err := e.OpenCaseStore(ctx, "00000001")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if l := st.Barred(ctx).Line(); !strings.Contains(l, "AI processing refused: barred account") {
		t.Errorf("compa case show's heading: %q", l)
	}
	items, err := st.EvidenceList()
	types := map[string]bool{}
	for _, it := range items {
		types[it.Ref.Type] = true
	}
	if err != nil || !types[evidence.TypeIngested] || !types[TypeCaseComment] {
		t.Errorf("compa evidence list: %v, types %v", err, types)
	}
}

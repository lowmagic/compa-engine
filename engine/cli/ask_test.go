package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/store"
)

// askScript answers the case ledger's call with an update recording
// nothing, and any other call with text. Synthetic.
const askScript = `{"turns": [
  {"when": "# Comments to read", "answers": [{"calls": [{"name": "ledger_update", "args": {"items": [], "updates": []}}]}]},
  {"when": "", "answers": [{"text": "The DNS pods look fine."}]}
]}`

// TestAskPreparesTheCase: [C10] compa ask prepares the case as the chat's
// opening does — the comments read, the attachments counted and
// inventoried, and the ledger read before its one turn — and the turn's
// conversation is given the case block.
func TestAskPreparesTheCase(t *testing.T) {
	e := newEnv(t)
	script := filepath.Join(t.TempDir(), "ask.json")
	if err := os.WriteFile(script, []byte(askScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := string(hostConfig) + "reasoning:\n  maxEgressClass: local_only\n  models:\n    fixture-model: {provider: fixture, model: " + script + "}\n" +
		"  profiles:\n    companion: {model: fixture-model}\n    ledger: {model: fixture-model}\n"
	out := e.runWith([]byte(cfg), "ask", "00000001", "Are the DNS pods healthy?")
	if strings.TrimSpace(out) != "The DNS pods look fine." {
		t.Errorf("compa ask printed:\n%s", out)
	}
	st, err := store.Open(e.caseStore, "00000001", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tasks := map[string]int{}
	rows, err := st.DB().Query(`SELECT task_type FROM invocations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var task string
		if err := rows.Scan(&task); err != nil {
			t.Fatal(err)
		}
		tasks[task]++
	}
	if tasks["comments.ledger"] != 1 || tasks["companion"] != 1 {
		t.Errorf("the calls by task: %v", tasks)
	}
	var inventories, counts int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE type = 'artifact_inventory'`).Scan(&inventories); err != nil || inventories != 1 {
		t.Errorf("%d inventories, %v", inventories, err)
	}
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE type = 'artifact_counts'`).Scan(&counts); err != nil || counts != 1 {
		t.Errorf("%d counts, %v", counts, err)
	}
}

// TestAskPrintsTheCheck: [E2] compa ask prints beneath its answer what the
// editor's check found that the answer does not show — a finding left
// uncorrected with the most the lines justify; a revision that failed,
// the answer as written printed, with the anchor it was asked to fulfil —
// and in a note what became of the answer. Synthetic scripts.
func TestAskPrintsTheCheck(t *testing.T) {
	for _, c := range []struct {
		name, answer, revise, findings string
		want                           []string
	}{
		{"flagged", "The DNS pods restarted because of the upgrade.", `{"text": "unused"}`,
			`{"statement": "s1", "kind": "unestablished_link", "quote": "because of the upgrade", "why": "the lines give no cause", "suggested": "the DNS pods restarted after the upgrade"}`,
			[]string{"The DNS pods restarted because of the upgrade.\n\n", `⚠ s1 unestablished_link · "because of the upgrade" the lines give no cause` + "\n  at most: the DNS pods restarted after the upgrade\n"}},
		{"revision failed", "The operator restarted the DNS pods.", `{"text": "The customer reports a restart.", "calls": [{"name": "grep", "args": {"pattern": "restart", "path": "."}}]}`,
			`{"statement": "s1", "kind": "misattributed", "quote": "The operator restarted the DNS pods", "why": "who saw it is not given", "suggested": "name who said it, or mark it inferred", "anchor": true}`,
			[]string{"The operator restarted the DNS pods.\n\n", "! the revision failed: companion: the model called a tool where it was asked to answer; the answer as written is printed\n",
				`asked of the revision: s1 [misattributed] "The operator restarted the DNS pods" → name who said it, or mark it inferred` + "\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			dir := t.TempDir()
			main := `{"turns": [
  {"when": "# Comments to read", "answers": [{"calls": [{"name": "ledger_update", "args": {"items": [], "updates": []}}]}]},
  {"when": "# Revise your answer", "answers": [` + c.revise + `]},
  {"when": "", "answers": [{"text": "` + c.answer + `"}]}
]}`
			editor := `{"turns": [{"when": "", "answers": [{"calls": [{"name": "check_findings", "args": {"findings": [` + c.findings + `], "supported": []}}]}]}]}`
			for name, script := range map[string]string{"main.json": main, "editor.json": editor} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := string(hostConfig) + "reasoning:\n  maxEgressClass: local_only\n  models:\n    fixture-model: {provider: fixture, model: " + filepath.Join(dir, "main.json") + "}\n" +
				"    editor-model: {provider: fixture, model: " + filepath.Join(dir, "editor.json") + "}\n" +
				"  profiles:\n    companion: {model: fixture-model}\n    ledger: {model: fixture-model}\n    editor: {model: editor-model}\n"
			out := e.runWith([]byte(cfg), "ask", "00000001", "What happened to the DNS pods?")
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("compa ask lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trajectoryScript answers the case ledger's call with an update
// recording nothing, and the turn with one read, then an answer. Synthetic.
const trajectoryScript = `{"turns": [
  {"when": "# Comments to read", "answers": [{"calls": [{"name": "ledger_update", "args": {"items": [], "updates": []}}]}]},
  {"when": "", "answers": [
    {"calls": [{"name": "grep", "args": {"pattern": "4\\.16", "path": "0010-must-gather"}}]},
    {"text": "The cluster runs 4.16.3."}
  ]}
]}`

// TestCaseTrajectory: compa case trajectory prints a case with no
// conversation as having no turns, and after a turn prints its block and
// the totals; --json is the trajectory as JSON.
func TestCaseTrajectory(t *testing.T) {
	e := newEnv(t)
	e.ok("case", "open", "00000001")
	if out := e.ok("case", "trajectory", "00000001"); out != "case 00000001\nno turns: the case has no conversation yet\n" {
		t.Errorf("no conversation:\n%s", out)
	}
	var none struct {
		Conversations []string `json:"conversations"`
		Turns         []any    `json:"turns"`
	}
	if err := json.Unmarshal([]byte(e.ok("case", "trajectory", "00000001", "--json")), &none); err != nil || none.Conversations == nil || none.Turns == nil || len(none.Turns) != 0 {
		t.Errorf("no conversation as JSON: %+v, %v", none, err)
	}

	script := filepath.Join(t.TempDir(), "trajectory.json")
	if err := os.WriteFile(script, []byte(trajectoryScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := string(hostConfig) + "reasoning:\n  maxEgressClass: local_only\n  models:\n    fixture-model: {provider: fixture, model: " + script + "}\n" +
		"  profiles:\n    companion: {model: fixture-model}\n    ledger: {model: fixture-model}\n"
	e.runWith([]byte(cfg), "ask", "00000001", "Which version does the cluster run?")
	out := e.ok("case", "trajectory", "00000001")
	for _, want := range []string{"── turn 1 · talk · ", "  1 call, 0 refused\n", "  1 item returned: 0 read on, 0 cited, 0 both, 1 neither; 0 answered, 1 with no answer, citation or read-on (",
		"── totals\n  1 turn, 1 call, 0 refused, 0 repeated; 0 over budget\n", "  2 model calls, input tokens "} {
		if !strings.Contains(out, want) {
			t.Errorf("the trajectory lacks %q:\n%s", want, out)
		}
	}
	var got struct {
		Turns []struct {
			Kind  string `json:"kind"`
			Calls int    `json:"calls"`
		} `json:"turns"`
		Totals struct {
			Calls      int `json:"calls"`
			ModelCalls int `json:"model_calls"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(e.ok("case", "trajectory", "00000001", "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 || got.Turns[0].Kind != "talk" || got.Turns[0].Calls != 1 || got.Totals.Calls != 1 || got.Totals.ModelCalls != 2 {
		t.Errorf("the trajectory as JSON: %+v", got)
	}
}

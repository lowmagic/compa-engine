package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// readingFlags are the findings for reading of kind among flags, each as
// what · why · the statement holding it; none is a span flag.
func readingFlags(t *testing.T, flags []AnswerFlag, kind string) []string {
	t.Helper()
	var out []string
	for _, f := range flags {
		if f.Finding != kind {
			continue
		}
		if f.Span != "" || f.Unsupported() {
			t.Errorf("a finding for reading is a span flag: %+v", f)
		}
		out = append(out, f.What+" · "+f.Why+" · "+f.Statement)
	}
	return out
}

// TestCausalWordingFlagged: causal wording in the model's own voice is a
// finding for reading beneath the answer, naming its statement and its
// terms, and the answer is unchanged; a question, a condition and a named
// source's conclusion are none. The editor is not given it.
func TestCausalWordingFlagged(t *testing.T) {
	r := replaySession(t)
	ctx := context.Background()
	a1, a2 := r.fill(replayA1), r.fill(replayA2)
	clean := "Is the OSD shutdown the cause? If the disk filled first, that caused the alerts. Engineering concluded the root cause is the full /var."
	texts := []string{a1, a2, clean}
	flags, err := r.s.AnswerFlags(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	if texts[0] != a1 || texts[1] != a2 {
		t.Error("the check changed the answers it was given")
	}
	for i, want := range [][]string{
		{`"likely" · ` + causalWhy + " · " + r.fill("The CSI socket refused connections, likely because the plugin container restarted [{csi}:1].")},
		{`"led to" · ` + causalWhy + " · " + r.fill("The OSD shutdown at 08:16 led to the disk filling at 11:55 [{osd}:1-2, {enospc}:1].")},
		nil,
	} {
		if d := cmp.Diff(want, readingFlags(t, flags[i], FindingCausal)); d != "" {
			t.Errorf("text %d's causal wording (-want +got):\n%s", i+1, d)
		}
	}
	history, err := r.s.ConversationHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last := answersOf(history); len(last) == 0 || last[len(last)-1].text != a2 {
		t.Errorf("the stored answer changed: %+v", last)
	}
	req, err := r.s.EditorRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.Text, causalWhy) {
		t.Errorf("the editor's request carries the finding for reading:\n%s", req.Text)
	}
}

// TestUnsourcedCommandFlagged: a command line an answer gives — a code
// span, a line of a shell's block, a command line of a bare block — whose
// program, or one of whose options, is on no one line, as a word, of the
// evidence the conversation was shown or of the guidance of an expert it
// loaded is a finding for reading naming what was not found, under its
// statement; one whose program and options are on one line of either is
// none, its values — paths, names, a value after = — not looked for.
// Synthetic expert and fixtures.
func TestUnsourcedCommandFlagged(t *testing.T) {
	td := t.TempDir()
	for name, content := range map[string]string{
		"cmds/expert.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata:\n  id: fixture.cmds\n  version: 0.1.0\n" +
			"  title: Synthetic commands\n  summary: Synthetic guidance naming commands\nspec:\n  engineApi: '>=0.2.0 <0.3.0'\n  analyst: analyst.md\n",
		"cmds/analyst.md": "# Synthetic commands\n\nCheck the northbound database with `ovn-nbctl --no-leader-only --timeout=10 show`.\n\nCount inodes with -i where space is left.\n",
	} {
		p := filepath.Join(td, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := companionSession(t, script(
		modelfixture.Calling(
			call("c1", "read", map[string]any{"path": "nodes/worker-1/df.txt"}),
			call("c2", "load_expert", map[string]any{"id": "fixture.cmds"}),
		),
		modelfixture.Text("Read."),
	), td)
	root, err := filepath.Abs("../testdata/replay/epistemic")
	if err != nil {
		t.Fatal(err)
	}
	s.c.root = root
	s.e.config.Checks.Commands = []string{"ovn-nbctl"}
	ctx := context.Background()
	if _, err := s.Talk(ctx, "Read the disk usage.", nil); err != nil {
		t.Fatal(err)
	}
	answer := strings.Join([]string{
		"Run `df -h /var/lib/kubelet` on worker-2. Run `df -i /var` too.",
		"",
		"```sh",
		"$ sudo ovn-nbctl --timeout=5 --no-leader-only show",
		"ovn-sbctl show",
		"```",
		"",
		"Then `ovn-nbctl --verbose show`.",
		"",
		"To see it:",
		"",
		"```",
		"journalctl -u kubelet",
		"```",
	}, "\n")
	flags, err := s.AnswerFlags(ctx, []string{answer})
	if err != nil {
		t.Fatal(err)
	}
	const where = " of the evidence this conversation saw or of the guidance of an expert it loaded"
	want := []string{
		"`df -i /var` · `-i` is on no line with `df`" + where + " · Run `df -i /var` too.",
		"`ovn-nbctl --verbose show` · `--verbose` is on no line with `ovn-nbctl`" + where + " · Then `ovn-nbctl --verbose show`.",
		"`ovn-sbctl show` · `ovn-sbctl` is on no line" + where + " · Run `df -i /var` too.",
		"`journalctl -u kubelet` · `journalctl` is on no line" + where + " · To see it:",
	}
	if d := cmp.Diff(want, readingFlags(t, flags[0], FindingCommand)); d != "" {
		t.Errorf("commands found in no evidence or guidance (-want +got):\n%s", d)
	}
}

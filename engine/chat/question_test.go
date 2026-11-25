package chat

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/enginetest"
)

// TestToolLineShowsTheQuestion: the tool line gives each call's question
// after what the call did, the calls in the order they were made; a batch
// too wide for the terminal gets a row for each call, wrapping long
// questions onto indented continuation lines within the width, live and
// reopened alike; under /debug each call's question follows the line
// whole, and a call that asked none says so. Synthetic (question.json).
func TestToolLineShowsTheQuestion(t *testing.T) {
	const long = "Does the OVN controller on worker-1 fail to get its chassis before the DNS timeouts began, and does it say which chassis it was looking for each time it retried?"
	s := session(t, "question.json")
	h := start(t, s, Options{})
	opening := regexp.MustCompile(`^▸ grep "error syncing node" → e\d+ · Did the OVN controller fail to sync worker-1\?$`)
	if tools := toolLines(h.out()); len(tools) != 1 || !opening.MatchString(tools[0]) {
		t.Fatalf("the opening's tool lines: %q\n%s", tools, h.out())
	}
	h.say("/debug on")
	mark := len(h.printed)
	h.say("what failed?")
	out := h.since(mark)
	// The grep's long question wraps onto indented continuation lines within
	// 100 columns, and the find, made after it, has its own row beneath.
	batch := []*regexp.Regexp{
		regexp.MustCompile(`^▸ grep "failed to get chassis" → e\d+ · Does the OVN controller on worker-1 fail to get its chassis$`),
		regexp.MustCompile(`^    before the DNS timeouts began, and does it say which chassis it was looking for each time it$`),
		regexp.MustCompile(`^    retried\?$`),
		regexp.MustCompile(`^  find namespaces → e\d+$`),
	}
	if rows := batchRows(out); len(rows) != 1 || !matchRows(rows[0], batch, 100) {
		t.Fatalf("the turn's tool rows: %q, want the grep with its question wrapped within 100 columns, then the find\n%s", rows, out)
	}
	for _, want := range []string{"question · grep: " + long, "question · find: none"} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("/debug does not say %q:\n%s", want, out)
		}
	}
	// Reopened, the engine's preload has its line too.
	reopened := batchRows(start(t, s, Options{NoOpen: true}).out())
	if len(reopened) != 3 || len(reopened[1]) != 1 || !opening.MatchString(reopened[1][0]) || !matchRows(reopened[2], batch, 100) {
		t.Errorf("the reopened tool rows: %q", reopened)
	}
}

// batchRows are the rows of each batch of tool calls printed: its row
// under the mark and those beneath it.
func batchRows(out string) [][]string {
	var batches [][]string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "▸"):
			batches = append(batches, []string{l})
		case len(batches) > 0 && strings.HasPrefix(l, "  ") && len(batches[len(batches)-1]) > 0:
			last := &batches[len(batches)-1]
			*last = append(*last, l)
		case len(batches) > 0:
			// A row of anything else ends the batch.
			batches = append(batches, nil)
		}
	}
	return slices.DeleteFunc(batches, func(b []string) bool { return len(b) == 0 })
}

// matchRows says whether rows match want, one each, none wider than
// width.
func matchRows(rows []string, want []*regexp.Regexp, width int) bool {
	if len(rows) != len(want) {
		return false
	}
	for i, r := range rows {
		if !want[i].MatchString(r) || ansi.StringWidth(r) > width {
			return false
		}
	}
	return true
}

// TestBatchedToolLineGivesEachCallItsQuestion: calls made together start
// and end in any order; the tool line names them in the order the model
// made them, each with its own question beside what it stored: on one
// line where it fits, otherwise on a row each, cut to the width.
// Synthetic.
func TestBatchedToolLineGivesEachCallItsQuestion(t *testing.T) {
	h := start(t, session(t, "question.json"), Options{NoOpen: true})
	h.update(tea.WindowSizeMsg{Width: 120, Height: 45})
	h.settle()
	mark := len(h.printed)
	wide := []enginetest.ToolCall{
		{ID: "c1", Name: "grep", Args: map[string]any{"pattern": "sync", "path": "namespaces", "question": "Did the OVN controller fail to sync worker-1 before the DNS timeouts began, and which chassis did it want each time?"}},
		{ID: "c2", Name: "read", Args: map[string]any{"path": "pods.yaml", "question": "Which node ran ovnkube-node-x7k2p when it restarted?"}},
		{ID: "c3", Name: "find", Args: map[string]any{"path": "namespaces", "question": "Is worker-1's journal captured?"}},
	}
	narrow := []enginetest.ToolCall{
		{ID: "c4", Name: "find", Args: map[string]any{"path": "a", "question": "Is a there?"}},
		{ID: "c5", Name: "find", Args: map[string]any{"path": "b", "question": "Is b there?"}},
	}
	h.m.turn = &turnState{kind: kindAnswer, model: h.m.current}
	// This synthetic turn has no work goroutine; its clock is test-owned.
	h.m.spinning = true
	// Each batch's calls start and end in an order of their own.
	events := []engine.TalkEvent{{Kind: engine.TalkCall, Invocation: &enginetest.ModelCall{ID: "synthetic-call", Calls: wide}}}
	for _, i := range []int{2, 0, 1} {
		events = append(events, engine.TalkEvent{Kind: engine.TalkToolStart, Call: &wide[i]})
	}
	for _, i := range []int{1, 2, 0} {
		name := "e" + strconv.Itoa(5+i)
		events = append(events, engine.TalkEvent{Kind: engine.TalkToolDone, Call: &wide[i], Result: &enginetest.ToolResult{Text: name + " · synthetic", Evidence: []string{name}}})
	}
	events = append(events, engine.TalkEvent{Kind: engine.TalkCall, Invocation: &enginetest.ModelCall{ID: "synthetic-call-2", Calls: narrow}},
		engine.TalkEvent{Kind: engine.TalkToolStart, Call: &narrow[1]}, engine.TalkEvent{Kind: engine.TalkToolStart, Call: &narrow[0]})
	for i := range narrow {
		name := "e" + strconv.Itoa(8+i)
		events = append(events, engine.TalkEvent{Kind: engine.TalkToolDone, Call: &narrow[i], Result: &enginetest.ToolResult{Text: name + " · synthetic", Evidence: []string{name}}})
	}
	events = append(events, engine.TalkEvent{Kind: engine.TalkThinking, Text: "synthetic"})
	for _, ev := range events {
		ev.Conversation = "synthetic"
		h.update(eventMsg{ev})
		h.settle()
	}
	rows := batchRows(h.since(mark))
	want := [][]*regexp.Regexp{{
		regexp.MustCompile(`^▸ grep "sync" → e5 · Did the OVN controller fail to sync worker-1 before the DNS timeouts began, and which chassis did$`),
		regexp.MustCompile(`^    it want each time\?$`),
		regexp.MustCompile(`^  read pods\.yaml → e6 · Which node ran ovnkube-node-x7k2p when it restarted\?$`),
		regexp.MustCompile(`^  find namespaces → e7 · Is worker-1's journal captured\?$`),
	}, {
		regexp.MustCompile(`^▸ find a → e8 · Is a there\? │ find b → e9 · Is b there\?$`),
	}}
	if len(rows) != len(want) || !matchRows(rows[0], want[0], 120) || !matchRows(rows[1], want[1], 120) {
		t.Fatalf("the batches' tool rows: %q, want each call in the order made with its own question wrapped within 120 columns", rows)
	}
}

// TestToolPathPriority: calls with paths prioritize the leading NNNN
// attachment ID, then the question, then the end of the file path, wrapping
// cleanly within the terminal width instead of truncating the question or
// evidence name behind long archive directories.
func TestToolPathPriority(t *testing.T) {
	strip := func(rows []string) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = ansi.Strip(r)
		}
		return out
	}

	// Archive-prefixed paths compress to NNNN first, question second, end of path third.
	batch := []toolRun{
		{
			name:     "find",
			args:     map[string]any{"path": "0010-inspect.local.5855027426577652645.tar.gz/inspect.local.5855027426577652645/namespaces", "question": "What namespaces are in the capture?"},
			done:     true,
			evidence: []string{"e10"},
		},
		{
			name:     "read",
			args:     map[string]any{"path": "0020-bundle.local.7652150651890380979-node01.tar.gz/bundle.local.7652150651890380979-node01/namespaces/app/pods/worker-0/logs/current.log", "question": "Why did worker-0 restart?"},
			done:     true,
			evidence: []string{"e11"},
		},
		{
			name:     "grep",
			args:     map[string]any{"path": "0020-bundle.local.7652150651890380979-node01.tar.gz/bundle.local.7652150651890380979-node01/namespaces/app/pods/worker-0/logs/current.log", "pattern": "OOMKilled", "question": "Was worker-0 OOMKilled?"},
			done:     true,
			evidence: []string{"e12"},
		},
		{
			name:     "read",
			args:     map[string]any{"path": "0030-summary.log", "question": "What does the summary report?"},
			done:     true,
			evidence: []string{"e13"},
		},
	}
	got := strip(toolRows(batch, 100))
	want := []string{
		"▸ find 0010 → e10 · What namespaces are in the capture? · …/namespaces",
		"  read 0020 → e11 · Why did worker-0 restart? · …/app/pods/worker-0/logs/current.log",
		`  grep 0020 "OOMKilled" → e12 · Was worker-0 OOMKilled? · …/app/pods/worker-0/logs/current.log`,
		"  read 0030 → e13 · What does the summary report? · summary.log",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("prefixed tool rows:\ngot:  %q\nwant: %q", got, want)
	}
	for _, r := range got {
		if w := ansi.StringWidth(r); w > 100 {
			t.Errorf("row %d columns wide (>100): %q", w, r)
		}
	}

	// Long question + path wraps cleanly with indented continuation lines, never losing the path tail.
	wrapped := strip(toolRows([]toolRun{{
		name:     "read",
		args:     map[string]any{"path": "0020-bundle.local.7652150651890380979-node01.tar.gz/bundle.local.7652150651890380979-node01/namespaces/app/pods/worker-0/logs/current.log", "question": "Does worker-0 log an out-of-memory or timeout error right before its container restarted?"},
		done:     true,
		evidence: []string{"e14"},
	}}, 80))
	wantWrapped := []string{
		"▸ read 0020 → e14 · Does worker-0 log an out-of-memory or timeout error right",
		"    before its container restarted? · …/app/pods/worker-0/logs/current.log",
	}
	if !slices.Equal(wrapped, wantWrapped) {
		t.Errorf("wrapped tool rows:\ngot:  %q\nwant: %q", wrapped, wantWrapped)
	}

	// Long unprefixed path moves its tail after the question when it exceeds the line or cap.
	unprefixed := strip(toolRows([]toolRun{{
		name:     "read",
		args:     map[string]any{"path": "cluster-scoped-resources/core/nodes/worker-1.example.com.yaml", "question": "Is worker-1 Ready?"},
		done:     true,
		evidence: []string{"e15"},
	}}, 80))
	wantUnprefixed := []string{
		"▸ read → e15 · Is worker-1 Ready? · …/core/nodes/worker-1.example.com.yaml",
	}
	if !slices.Equal(unprefixed, wantUnprefixed) {
		t.Errorf("unprefixed long path:\ngot:  %q\nwant: %q", unprefixed, wantUnprefixed)
	}

	// An ISO date directory is not treated as a 4-digit attachment ID.
	if id, rest, elided := splitArtifactPath("2026-11-25/logs/app.log"); id != "" || rest != "2026-11-25/logs/app.log" || elided {
		t.Errorf("ISO date misclassified as attachment prefix: id=%q rest=%q elided=%v", id, rest, elided)
	}

	// Narrow width keeps every row within bounds, including single-segment long archive names and errors.
	narrow := strip(toolRows([]toolRun{{
		name:    "find",
		args:    map[string]any{"path": "0020-bundle.local.7652150651890380979-node01.tar.gz", "question": "What files are inside?"},
		done:    true,
		isError: true,
		result:  "not found",
	}}, 44))
	for _, r := range narrow {
		if w := ansi.StringWidth(r); w > 44 {
			t.Errorf("narrow row %d wide (>44): %q", w, r)
		}
	}
	joined := flat(strings.Join(narrow, "\n"))
	for _, wantSub := range []string{"▸ find 0020 ✗ not found", "What files are inside?", "node01.tar.gz"} {
		if !strings.Contains(joined, wantSub) {
			t.Errorf("narrow output lacks %q:\n%s", wantSub, joined)
		}
	}
}

// TestDebugShowsTheAnswers: under /debug each answer a call gave follows
// its tool line whole, as the engine checked it: one naming an item a
// tool returned before it plain, one naming an item none returned
// flagged so. Synthetic (question.json).
func TestDebugShowsTheAnswers(t *testing.T) {
	h := start(t, session(t, "question.json"), Options{NoOpen: true})
	h.say("/debug on")
	mark := len(h.printed)
	h.say("and then?")
	out := flat(h.since(mark))
	for _, want := range []string{"answer · e1 on find: the case details name worker-1 and worker-2",
		"answer · e999 on find: `no such marker 7f3a` in it ⚠ e999 was returned by no tool of this conversation before this call"} {
		if !strings.Contains(out, want) {
			t.Errorf("/debug does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "answer · e1 on find: the case details name worker-1 and worker-2 ⚠") {
		t.Errorf("an answer naming an item read before it is flagged:\n%s", out)
	}
}

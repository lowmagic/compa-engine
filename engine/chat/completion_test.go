package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSlashMatches(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  string
	}{
		{"/e", "evidence"},
		{"/co", "copy,compact"},
		{"/le", "ledger"},
		{"/model", "model"},
		{"/draft", "draft"},
		{"", ""},
		{"ordinary text", ""},
		{" /e", ""},
		{"/unknown", ""},
		{"/MODEL", ""},
		{"/model ", ""},
		{"/model other-model", ""},
		{"/e\n", ""},
		{"/e\t", ""},
		{"/e\u00a0", ""},
	} {
		t.Run(tt.input, func(t *testing.T) {
			var names []string
			for _, c := range slashMatches(tt.input) {
				names = append(names, c.name)
			}
			if got := strings.Join(names, ","); got != tt.want {
				t.Errorf("matches = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompletionKeys(t *testing.T) {
	for _, tt := range []struct {
		input, key, want string
		handled, submit  bool
	}{
		{"/le", "tab", "/ledger", true, false},
		{"/dr", "tab", "/draft ", true, false},
		{"/model", "tab", "/model ", true, false},
		{"/e", "tab", "/evidence ", true, false},
		{"/tu", "tab", "/turns", true, false},
		{"/ev", "enter", "/evidence ", true, false},
		{"/dr", "enter", "/draft ", true, false},
		{"/mod", "enter", "/model ", true, false},
		{"/deb", "enter", "/debug ", true, false},
		{"/tu", "enter", "/turns", true, true},
		{"/", "enter", "/draft ", true, false},
		{"/draft", "enter", "/draft", false, false},
		{"/model", "enter", "/model", false, false},
		{"/ledger", "enter", "/ledger", false, false},
		{"/debug", "enter", "/debug", false, false},
		{"/unknown", "enter", "/unknown", false, false},
		{"/model other-model", "tab", "/model other-model", false, false},
		{"ordinary text", "tab", "ordinary text", false, false},
		{"/", "x", "/", false, false},
	} {
		t.Run(tt.input+" "+tt.key, func(t *testing.T) {
			var c commandCompletion
			got, handled, submit := c.key(tt.input, tt.key)
			if got != tt.want || handled != tt.handled || submit != tt.submit {
				t.Errorf("key = (%q, %v, %v), want (%q, %v, %v)", got, handled, submit, tt.want, tt.handled, tt.submit)
			}
		})
	}
}

func TestCompletionSelectionAndDismissal(t *testing.T) {
	var c commandCompletion
	c.key("/co", "up")
	if c.selected != 0 {
		t.Fatal("up moved before the first option")
	}
	for range 20 {
		c.key("/co", "down")
	}
	if c.selected != 1 {
		t.Fatalf("down selected %d, want last option 1", c.selected)
	}
	c.key("/co", "up")
	if got, handled, submit := c.key("/co", "tab"); got != "/copy " || !handled || submit {
		t.Fatalf("selected completion = (%q, %v, %v)", got, handled, submit)
	}
	if rows := c.rows("/copy ", 80, 7); len(rows) != 0 {
		t.Fatal("completion left the menu open")
	}
	if got, handled, _ := c.key("/e", "esc"); got != "/e" || !handled {
		t.Fatal("esc changed input or did not consume the key")
	}
	if rows := c.rows("/e", 80, 7); len(rows) != 0 {
		t.Fatal("esc left the menu open")
	}
	if _, handled, _ := c.key("/e", "down"); handled {
		t.Fatal("dismissed menu consumed navigation")
	}
	if rows := c.rows("/ev", 80, 7); len(rows) != 2 || !strings.Contains(ansi.Strip(rows[0]), "/evidence") {
		t.Fatalf("typing did not reopen filtered menu: %v", rows)
	}
	if c.selected != 0 {
		t.Fatal("changed input retained the old selection")
	}
	var other commandCompletion
	if len(other.rows("/e", 80, 7)) == 0 {
		t.Fatal("dismissal leaked into another chat")
	}
}

func TestCompletionRowsBounded(t *testing.T) {
	for _, width := range []int{0, 1, 2, 8, 24, 80} {
		for _, room := range []int{0, 1, 2, 4, 30} {
			var c commandCompletion
			for range len(commandCatalog) {
				c.key("/", "down")
			}
			rows := c.rows("/", width, room)
			if len(rows) > room || len(rows) > 7 {
				t.Errorf("%dx%d: %d rows", width, room, len(rows))
			}
			if width < 1 || room < 2 {
				if len(rows) != 0 {
					t.Errorf("%dx%d: drew a menu without room", width, room)
				}
				continue
			}
			for _, row := range rows {
				if ansi.StringWidth(row) > width || strings.Contains(row, "\n") {
					t.Errorf("%dx%d: overflowing row %q", width, room, row)
				}
			}
			if width == 80 {
				shown := ansi.Strip(strings.Join(rows, "\n"))
				if !strings.Contains(shown, "› /debug") || !strings.Contains(shown, "tab complete") {
					t.Errorf("selected option or key hint outside the window:\n%s", shown)
				}
			}
		}
	}
}

func TestCompletionDoesNotInvokeModel(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	before, calls := flat(h.out()), len(h.m.records)
	h.typed("/led")
	if v := h.view(); !strings.Contains(v, "/ledger") || !strings.Contains(v, "tab complete") {
		t.Fatalf("no slash menu:\n%s", v)
	}
	h.press("tab")
	h.settle()
	if got := h.m.input.Value(); got != "/ledger" {
		t.Errorf("tab input = %q", got)
	}
	if h.m.turn != nil || len(h.m.records) != calls || flat(h.out()) != before {
		t.Fatal("typing or Tab invoked the model or printed menu rows")
	}
}

func TestCompletionEnterWaitsForArguments(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	before, calls := flat(h.out()), len(h.m.records)
	h.typed("/dr")
	h.press("enter")
	h.settle()
	if got := h.m.input.Value(); got != "/draft " {
		t.Fatalf("partial Enter input = %q, want /draft with argument space", got)
	}
	if h.m.turn != nil || len(h.m.records) != calls || flat(h.out()) != before {
		t.Fatal("partial /draft Enter ran a command instead of waiting for arguments")
	}
	h.press("backspace", "enter")
	h.settle()
	if !strings.Contains(h.out(), "/draft <mode> [intention], the mode one of:") || h.m.input.Value() != "" {
		t.Fatal("exact /draft Enter did not preserve intention listing")
	}
	h.say("/model")
	if !strings.Contains(h.out(), "the model: fixture-model effort high") {
		t.Fatal("exact /model Enter did not preserve model listing")
	}
	if h.m.turn != nil || len(h.m.records) != calls {
		t.Fatal("listing draft intentions or models invoked a model")
	}
}

func TestCompletionEnterExecutesArgumentFreeCommand(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.say("/tu")
	if h.m.browser == nil || h.m.browser.tab != tabTurns || h.m.input.Value() != "" || !strings.Contains(h.out(), "› /turns") {
		t.Fatal("partial Enter did not execute the selected argument-free command")
	}
}

func TestCompletionEscapeKeepsInput(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	before := flat(h.out())
	h.typed("/e")
	h.press("esc")
	if h.m.input.Value() != "/e" || strings.Contains(h.view(), "tab complete") {
		t.Fatal("esc discarded input or left the menu visible")
	}
	h.typed("v")
	if !strings.Contains(h.view(), "/evidence") || !strings.Contains(h.view(), "tab complete") {
		t.Fatal("typing after esc did not reopen the menu")
	}
	if flat(h.out()) != before {
		t.Fatal("the transient menu entered the transcript")
	}
}

func TestPopoverClearsItsWholeRenderArea(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	transcript := flat(h.out())
	h.typed("/")
	height := strings.Count(h.view(), "\n")
	h.typed("e")
	if view := h.view(); strings.Contains(view, "/draft") || strings.Count(view, "\n") != height {
		t.Fatalf("filtering left rejected options or shrank the uncleared render area:\n%s", view)
	}
	h.press("esc")
	if view := h.view(); strings.Contains(view, "/evidence") || strings.Count(view, "\n") != height {
		t.Fatalf("dismissal left options or shrank before the cursor reset:\n%s", view)
	}
	h.settle()
	if strings.Count(h.view(), "\n") >= height || flat(h.out()) != transcript {
		t.Errorf("cleared popover did not collapse cleanly or entered the transcript:\n%s", h.view())
	}
}

// Lines deleted from the input keep their rows, blank, until the next
// print resets the renderer's origin: given up at once, each would leave
// a copy of the status line behind.
func TestInputKeepsItsRowsUntilAPrint(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	h.typed("first")
	for range 3 {
		h.press("alt+enter")
		h.typed("more")
	}
	height := strings.Count(h.view(), "\n")
	for range 3 * len("\nmore") {
		h.press("backspace")
	}
	view := h.view()
	if strings.Count(view, "\n") != height || strings.Count(view, "waiting for you") != 1 || !strings.Contains(view, "› first") {
		t.Fatalf("the input gave up its rows before a print:\n%s", view)
	}
	h.say("/debug experts")
	h.settle()
	if strings.Count(h.view(), "\n") >= height {
		t.Errorf("the input's rows were kept after a print:\n%s", h.view())
	}
}

// The empty input cues a question, / for commands or ! for shell, and when
// the latest answer offered lettered Next steps it cues selecting one too.
func TestInputPlaceholderTracksNextSteps(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	if got := h.m.input.Placeholder; got != hintAsk {
		t.Errorf("initial placeholder = %q, want %q", got, hintAsk)
	}
	if v := h.view(); !strings.Contains(v, "Ask a question, / for commands, or ! for shell") {
		t.Errorf("the empty input does not show the ask cue:\n%s", v)
	}
	h.update(turnDone{kind: kindAnswer, text: "Checked e1.\n\nNext (enter a letter, or ask a question):\nA. Read e2\nB. Grep kubelet"})
	h.settle()
	if got := h.m.input.Placeholder; got != hintNext {
		t.Errorf("placeholder after Next steps = %q, want %q", got, hintNext)
	}
	h.update(turnDone{kind: kindAnswer, text: "Nothing further."})
	h.settle()
	if got := h.m.input.Placeholder; got != hintAsk {
		t.Errorf("placeholder after plain answer = %q, want %q", got, hintAsk)
	}
}

// TestArgumentPopovers: a command's arguments show in the popover once its
// name and a space are typed, never in the transcript and calling no
// model: tab on /dr completes /draft and lists the modes, tab on a mode
// leaves it to take steering and closes the list; /model lists the models,
// tab on one with efforts lists them and enter switches; /debug lists its
// arguments, filtered as typed, enter running the one chosen; /copy lists
// the answers newest first, enter copying the one chosen.
func TestArgumentPopovers(t *testing.T) {
	h := start(t, session(t, "case.json"), Options{NoOpen: true})
	transcript, calls := flat(h.out()), len(h.m.records)
	h.typed("/dr")
	h.press("tab")
	if got, v := h.m.input.Value(), h.view(); got != "/draft " || !strings.Contains(v, "› customer-update — Customer update, for the customer") {
		t.Fatalf("tab on /dr: input %q, view:\n%s", got, v)
	}
	h.press("tab")
	if got, v := h.m.input.Value(), h.view(); got != "/draft customer-update " || strings.Contains(v, "tab complete") {
		t.Fatalf("tab on the mode: input %q, view:\n%s", got, v)
	}
	if flat(h.out()) != transcript || h.m.turn != nil || len(h.m.records) != calls {
		t.Fatal("the draft popover printed or called a model")
	}
	h.m.input.Reset()

	h.typed("/model ")
	if v := h.view(); !strings.Contains(v, "› fixture-model — efforts low, high") || !strings.Contains(v, "other-model — efforts medium") {
		t.Fatalf("/model lists:\n%s", v)
	}
	h.press("tab")
	if got, v := h.m.input.Value(), h.view(); got != "/model fixture-model " || !strings.Contains(v, "› low") || !strings.Contains(v, "high") {
		t.Fatalf("tab on the model: input %q, view:\n%s", got, v)
	}
	h.press("down", "enter")
	h.settle()
	if out := h.out(); !strings.Contains(out, "the next calls use fixture-model effort high") || h.m.input.Value() != "" {
		t.Fatalf("enter on the effort: input %q, printed:\n%s", h.m.input.Value(), out)
	}

	h.typed("/debug ")
	for _, want := range []string{"› on — ", "off — ", "usage — ", "experts — "} {
		if !strings.Contains(h.view(), want) {
			t.Errorf("/debug lists no %q:\n%s", want, h.view())
		}
	}
	h.typed("o")
	if v := h.view(); !strings.Contains(v, "› on — ") || strings.Contains(v, "usage — ") {
		t.Fatalf("/debug o lists:\n%s", v)
	}
	h.press("enter")
	h.settle()
	if !h.m.debug || h.m.input.Value() != "" {
		t.Fatalf("enter on /debug on: debug %v, input %q", h.m.debug, h.m.input.Value())
	}

	h.say("which model?")
	h.typed("/copy ")
	if v := h.view(); !strings.Contains(v, "› a1 — The first model answers.") {
		t.Fatalf("/copy lists:\n%s", v)
	}
	h.press("enter")
	h.settle()
	if len(h.teaMsgs) != 1 || !strings.Contains(h.out(), "asked the terminal to copy a1") {
		t.Errorf("enter on a1 asked of the terminal %v:\n%s", h.teaMsgs, h.out())
	}
	if strings.Contains(h.out(), "› a1 —") || strings.Contains(h.out(), "efforts low, high") {
		t.Errorf("a popover's rows entered the transcript:\n%s", h.out())
	}
}

// TestCompletionExactArgumentRuns: enter on an argument typed whole runs
// it as typed though a longer one shares its start; on a partial one it
// runs the option chosen.
func TestCompletionExactArgumentRuns(t *testing.T) {
	c := commandCompletion{args: func(string) []completionOption {
		return []completionOption{{complete: "/copy a10", run: "/copy a10", label: "a10"}, {complete: "/copy a1", run: "/copy a1", label: "a1"}}
	}}
	if got, handled, submit := c.key("/copy a1", "enter"); got != "/copy a1" || handled || submit {
		t.Errorf("enter on /copy a1 = (%q, %v, %v)", got, handled, submit)
	}
	if got, handled, submit := c.key("/copy a", "enter"); got != "/copy a10" || !handled || !submit {
		t.Errorf("enter on /copy a = (%q, %v, %v)", got, handled, submit)
	}
}

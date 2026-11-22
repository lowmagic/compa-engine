package chat

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"
)

// A search of one file shows each line from its number in the file, the
// file's name, which the head gives, left off; lines from several files,
// or not all in a search's form, are shown whole.
func TestEvidenceViewLeavesOffTheSearchedFile(t *testing.T) {
	file := "0020-sosreport-worker-1-20261001.tar.xz/sosreport-worker-1-2026-10-01-synthtc/sos_commands/kubernetes/journalctl_--no-pager_--unit_kubelet"
	searched := []string{
		file + "-640036-Oct 27 11:55:30 worker-1 kubelet[1]: before",
		file + ":640039:Oct 27 11:56:00 worker-1 kubelet[1]: connect: connection refused",
		file + "-640040-Oct 27 11:56:01 worker-1 kubelet[1]: after",
	}
	for _, c := range []struct {
		name     string
		lines    []string
		location string
		want     []string
	}{
		{"one file", searched, file, []string{
			"640036-Oct 27 11:55:30 worker-1 kubelet[1]: before",
			"640039:Oct 27 11:56:00 worker-1 kubelet[1]: connect: connection refused",
			"640040-Oct 27 11:56:01 worker-1 kubelet[1]: after",
		}},
		{"several files", searched, file + "\nother.log", searched},
		{"a line in another form", append(slices.Clone(searched), "summary: 1 match"), file, append(slices.Clone(searched), "summary: 1 match")},
		{"a longer name", []string{file + ".1:3:x"}, file, []string{file + ".1:3:x"}},
		{"no location", searched, "", searched},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := withoutSource(c.lines, c.location); !slices.Equal(got, c.want) {
				t.Errorf("withoutSource =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// A line wider than the view scrolls into sight with right and back with
// left, a … marking what is off each side, and stops at the widest
// line's end.
func TestEvidenceViewScrollsSideways(t *testing.T) {
	long := "start " + strings.Repeat("x", 100) + " end"
	o := &opened{name: "e1", lines: []string{long, "short"}, top: -1}
	const w = 40
	shown := func() string { return ansi.Strip(strings.Join(o.view(w, 10, ""), "\n")) }

	if v := shown(); !strings.Contains(v, "1 start") || strings.Contains(v, " end") || !strings.Contains(v, "x…") {
		t.Fatalf("unscrolled:\n%s", v)
	}
	for range 10 {
		o.scroll("right", w)
		shown()
	}
	v := shown()
	if !strings.Contains(v, "1 …") || !strings.HasSuffix(strings.Split(v, "\n")[1], "x end") {
		t.Errorf("scrolled to the end:\n%s", v)
	}
	if most := o.col; most != len(long)-(w-2)+1 {
		t.Errorf("scrolled to column %d, past the widest line's end", most)
	}
	for range 10 {
		o.scroll("left", w)
	}
	if v := shown(); o.col != 0 || !strings.Contains(v, "1 start") {
		t.Errorf("scrolled back:\n%s", v)
	}
}

// The head's location is shown whole, wrapped at the view's width, since
// a search's lines no longer carry its file's name.
func TestEvidenceViewShowsItsFileWhole(t *testing.T) {
	file := "0020-sosreport-worker-1-20261001.tar.xz/sosreport-worker-1-2026-10-01-synthtc/sos_commands/kubernetes/journalctl_--no-pager_--unit_kubelet"
	o := &opened{name: "e1", head: []string{"e1 · grep", "from " + file}, lines: []string{"1:x"}, top: -1}
	rows := o.view(40, 20, "")
	for _, r := range rows {
		if ansi.StringWidth(r) > 40 {
			t.Errorf("a row wider than the view: %q", ansi.Strip(r))
		}
	}
	if v := strings.ReplaceAll(ansi.Strip(strings.Join(rows, "\n")), "\n", ""); !strings.Contains(v, "from "+file) {
		t.Errorf("the file is not shown whole:\n%s", ansi.Strip(strings.Join(rows, "\n")))
	}
}

// The browser's turns tab lists what the engineer sent and what the model
// wrote, oldest first, an answer by the chat's name, the newest under the
// cursor; one opened shows its Markdown as stored, unrendered, and y
// copies it so, in a live chat and a reopened one alike.
func TestBrowserTurnsAsStored(t *testing.T) {
	s := session(t, "case.json")
	answer := "## The case\n\nThe OVN controller **cannot sync** worker-1, as the case details say [e1:2]. The details also hold [e999:2], and [e1:100000]."
	for _, c := range []struct {
		name string
		o    Options
	}{{"live", Options{}}, {"reopened", Options{NoOpen: true}}} {
		h := start(t, s, c.o)
		if c.name == "live" {
			h.say("which model")
			h.settle()
		}
		h.say("/evidence")
		h.settle()
		h.press("tab", "tab")
		v := h.view()
		a1, you, a2 := strings.Index(v, "  a1 · "), strings.Index(v, "  you · "), strings.Index(v, "› a2 · ")
		if !strings.Contains(v, "[turns]") || !strings.Contains(v, "you · 2026") || a1 < 0 || you < a1 || a2 < you {
			t.Fatalf("%s: the turns tab:\n%s", c.name, v)
		}
		if !strings.Contains(v, "· The case The OVN controller cannot sync worker-1") || strings.Contains(v, "## The case") {
			t.Errorf("%s: the list shows a turn's Markdown, not its text:\n%s", c.name, v)
		}
		h.press("up", "up", "enter")
		if v := h.view(); !strings.Contains(v, "## The case") || !strings.Contains(v, "**cannot sync**") {
			t.Errorf("%s: the answer opened is not as stored:\n%s", c.name, v)
		}
		h.press("w")
		if v := h.view(); !strings.Contains(v, "\n## The case\n") {
			t.Errorf("%s: w does not show the turn wrapped with no numbers:\n%s", c.name, v)
		}
		h.press("w")
		if v := h.view(); strings.Contains(v, "\n## The case\n") {
			t.Errorf("%s: w again does not number the lines:\n%s", c.name, v)
		}
		h.teaMsgs = nil
		h.press("y")
		h.settle()
		if d := cmp.Diff([]tea.Msg{tea.SetClipboard(answer)()}, h.teaMsgs); d != "" {
			t.Errorf("%s: y asked of the terminal (-want +got):\n%s", c.name, d)
		}
		h.press("esc", "esc")
		if h.m.browser != nil {
			t.Errorf("%s: esc from the turn and its list did not return to the chat", c.name)
		}
	}
}

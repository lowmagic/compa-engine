package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Markdown is shown without its markers, the text otherwise as it was,
// line for line; what is not Markdown — a glob, a snake_case name, a lone
// star, an unclosed marker — stays as written.
func TestMarkdownShownWithoutMarkers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Do **not** run `ceph osd purge 109` *yet*.", "Do not run ceph osd purge 109 yet."},
		{"**bold *and italic***", "bold and italic"},
		{"ls *.log *.txt and a*b", "ls *.log *.txt and a*b"},
		{"snake_case_name stays", "snake_case_name stays"},
		{"2 * 3 = 6, **unclosed", "2 * 3 = 6, **unclosed"},
		{"## Steps", "Steps"},
		{"- one\n  * two\n3. three", "• one\n  • two\n3. three"},
		{"> quoted", "│ quoted"},
		{"See [the doc](https://example.test/doc).", "See the doc (https://example.test/doc)."},
		{"```\nls **x**\n```\nafter **b**", "```\nls **x**\n```\nafter b"},
	} {
		if got := ansi.Strip(markdown(tc.in, 0, nil)); got != tc.want {
			t.Errorf("markdown(%q)\n  got %q\n want %q", tc.in, got, tc.want)
		}
	}
	// Styled: bold and code each carry their own sequence.
	got := markdown("a **b** `c`", 0, nil)
	if !strings.Contains(got, plainW.Bold(true).Render("b")) || !strings.Contains(got, codeW.Render("c")) {
		t.Errorf("not styled: %q", got)
	}
}

// A line longer than the width wraps at words, a list item's and a
// quote's further rows under their first, and a code block's line is cut
// where it reaches the width; no row is wider than the width.
func TestMarkdownWraps(t *testing.T) {
	in := "The ovnkube-controller on worker-1 logs that it cannot get the chassis of the node, again and again.\n" +
		"- the first item is long enough that it takes a second row at this width\n" +
		"> a quoted line long enough that it takes a second row at this width too\n" +
		"```\n" + strings.Repeat("x", 50) + "\n```"
	got := ansi.Strip(markdown(in, 40, nil))
	want := "The ovnkube-controller on worker-1 logs\n" +
		"that it cannot get the chassis of the\n" +
		"node, again and again.\n" +
		"• the first item is long enough that it\n" +
		"  takes a second row at this width\n" +
		"│ a quoted line long enough that it\n" +
		"│ takes a second row at this width too\n" +
		"```\n" + strings.Repeat("x", 40) + "\n" + strings.Repeat("x", 10) + "\n```"
	if got != want {
		t.Errorf("markdown at width 40:\n%s\nwant:\n%s", got, want)
	}
	for _, row := range strings.Split(markdown(in, 40, nil), "\n") {
		if w := ansi.StringWidth(row); w > 40 {
			t.Errorf("a row %d wide: %q", w, ansi.Strip(row))
		}
	}
}

// A citation is shown as a citation, wherever it is, a code span's
// included; a flagged one is marked, never dropped, and one beginning
// with another citation's text is shown whole.
func TestMarkdownCitations(t *testing.T) {
	marks := citeMarks{"[e1:2]": "", "[e1:20]": "e1 has 12 lines", "`etc/hosts:3`": "no artifact etc/hosts"}
	got := markdown("**see [e1:2]** and [e1:20], and `etc/hosts:3`", 0, marks)
	for _, want := range []string{citeW.Render("[e1:2]"), flagW.Render("[e1:20] ⚠"), flagW.Render("`etc/hosts:3` ⚠")} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown lacks %q:\n%q", want, got)
		}
	}
	if plain := ansi.Strip(got); plain != "see [e1:2] and [e1:20] ⚠, and `etc/hosts:3` ⚠" {
		t.Errorf("the citations shown: %q", plain)
	}
}

// A heading has a blank row before it, unless it is first or one is
// there; a rule, ---, *** or ___, is drawn as a line, but not in a code
// block.
func TestMarkdownSpacesHeadingsAndDrawsRules(t *testing.T) {
	in := "## First\ntext\n## Second\n\n## Third\n---\n```\n---\n## not a heading\n```"
	want := "First\ntext\n\nSecond\n\nThird\n" + strings.Repeat("─", 20) + "\n```\n---\n## not a heading\n```"
	if got := ansi.Strip(markdown(in, 20, nil)); got != want {
		t.Errorf("markdown:\n%s\nwant:\n%s", got, want)
	}
}

// In the ledger, a comment that gave no items, its line beginning c#, is
// shown muted; one with items keeps its heading's style.
func TestLedgerViewMutesCommentsWithoutItems(t *testing.T) {
	got := ledgerView("### c#2 · Sam Synthetic · e5\n\n- l1 · customer asks: logs\n\nc#3 · A Bot · e3 · nothing: automated", 0)
	if !strings.Contains(got, mutedW.Render("c#3 · A Bot · e3 · nothing: automated")) {
		t.Errorf("the comment without items is not muted: %q", got)
	}
	if !strings.Contains(got, headingW.Render("c#2 · Sam Synthetic · e5")) {
		t.Errorf("the comment with items lost its heading: %q", got)
	}
}

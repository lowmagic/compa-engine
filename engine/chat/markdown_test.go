package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Markdown is shown without its markers, the text otherwise as it was; what is
// not Markdown — a glob, a snake_case name, a lone star, an unclosed marker —
// stays as written, routine "- **Label:**" bullet bolding is stripped, and
// fenced blocks hide their fence lines.
func TestMarkdownShownWithoutMarkers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Do **not** run `ceph osd purge 109` *yet*.", "Do not run ceph osd purge 109 yet."},
		{"**bold *and italic***", "bold and italic"},
		{"ls *.log *.txt and a*b", "ls *.log *.txt and a*b"},
		{"snake_case_name stays", "snake_case_name stays"},
		{"2 * 3 = 6, **unclosed", "2 * 3 = 6, **unclosed"},
		{"# Top", "Top\n───"},
		{"## Steps", "Steps"},
		{"### Detail", "Detail"},
		{"- one\n  * two\n3. three\nA. step", "- one\n  - two\n3. three\nA. step"},
		{"- **Symptom:** pod failed", "- Symptom: pod failed"},
		{"> quoted", "│ quoted"},
		{"See [the doc](https://example.test/doc).", "See the doc (https://example.test/doc)."},
		{"```\nls **x**\n```\nafter **b**", "│ ls **x**\nafter b"},
	} {
		if got := ansi.Strip(markdown(tc.in, 0, nil)); got != tc.want {
			t.Errorf("markdown(%q)\n  got %q\n want %q", tc.in, got, tc.want)
		}
	}
	// Styled: bold, code, bullet lead-in labels, lettered options and Next/For you
	// cues each carry their own style, while list markers and routine bullet
	// lead-ins are never bolded.
	got := markdown("a **b** `c`", 0, nil)
	if !strings.Contains(got, plainW.Bold(true).Render("b")) || !strings.Contains(got, codeW.Render("c")) {
		t.Errorf("not styled: %q", got)
	}
	bullet := markdown("- **Symptom:** pod failed", 0, nil)
	if strings.Contains(bullet, plainW.Bold(true).Render("Symptom")) || strings.Contains(bullet, plainW.Bold(true).Render("-")) {
		t.Errorf("routine bullet lead-in or marker was bolded: %q", bullet)
	}
	if !strings.Contains(bullet, labelW.Render("Symptom:")) || !strings.Contains(bullet, bulletW.Render("-")) {
		t.Errorf("bullet lead-in or marker not styled: %q", bullet)
	}
	cues := markdown("Next (enter a letter):\nA. check logs\n\nFor you:\n- collect dump", 0, nil)
	if !strings.Contains(cues, nextW.Render("Next (enter a letter):")) ||
		!strings.Contains(cues, optionW.Render("A.")) ||
		!strings.Contains(cues, forYouW.Render("For you:")) {
		t.Errorf("Next/For you cues or lettered options not styled: %q", cues)
	}
}

// Nested lists align child markers under the parent's first text character,
// keep continuation paragraphs aligned with the item's text across wrapped
// lines, and preserve `#` lines inside indented fenced blocks.
func TestMarkdownRestrainedHierarchyAndNesting(t *testing.T) {
	in := strings.Join([]string{
		"# Header 1",
		"",
		"Introduction",
		"",
		"## Header 2",
		"",
		"[link](https://link.com)",
		"",
		"### Sections",
		"",
		"- unordered list",
		"  - nested",
		"- ok",
		"  a. ordered list",
		"  b. ordered list",
		"",
		"1. ordered top level",
		"   1. ordered nested",
		"   2. it's `inline code`",
		"2. ordered top level",
		"   - unordered nested bullet aligns with first character of the top list",
		"   - extra one",
		"   ",
		"     Nested under the nested list, should align with the text, not the bullet",
		"     This is two levels deep",
		"",
		"     ```",
		"     # needs to be able to handle an indented hash",
		"     ```",
		"",
		"```",
		"code blocks",
		"```",
		"",
		"Bolding text **should be extremely rare and only for extreme emphasis**",
	}, "\n")
	got := ansi.Strip(markdown(in, 80, nil))
	want := strings.Join([]string{
		"Header 1",
		"────────",
		"",
		"Introduction",
		"",
		"Header 2",
		"",
		"link (https://link.com)",
		"",
		"Sections",
		"",
		"- unordered list",
		"  - nested",
		"- ok",
		"  a. ordered list",
		"  b. ordered list",
		"",
		"1. ordered top level",
		"   1. ordered nested",
		"   2. it's inline code",
		"2. ordered top level",
		"   - unordered nested bullet aligns with first character of the top list",
		"   - extra one",
		"",
		"     Nested under the nested list, should align with the text, not the bullet",
		"     This is two levels deep",
		"",
		"   │ # needs to be able to handle an indented hash",
		"",
		"│ code blocks",
		"",
		"Bolding text should be extremely rare and only for extreme emphasis",
	}, "\n")
	if got != want {
		t.Errorf("markdown hierarchy:\n%s\nwant:\n%s", got, want)
	}
	raw := markdown("# H1\n\n## H2\n\n### H3", 80, nil)
	if !strings.Contains(raw, h1W.Render("H1")) || !strings.Contains(raw, headingW.Render("H2")) || !strings.Contains(raw, h3W.Render("H3")) {
		t.Errorf("headings did not render in distinct tiers: %q", raw)
	}
	if h1W.Render("X") == headingW.Render("X") || headingW.Render("X") == h3W.Render("X") || h1W.Render("X") == h3W.Render("X") {
		t.Errorf("heading tiers share a style: h1=%q h2=%q h3=%q", h1W.Render("X"), headingW.Render("X"), h3W.Render("X"))
	}
}

// A line longer than the width wraps at words, a list item's, a continuation
// paragraph's and a quote's further rows under their first, and a code block's
// line is cut where it reaches the width; no row is wider than the width.
func TestMarkdownWraps(t *testing.T) {
	in := "The ovnkube-controller on worker-1 logs that it cannot get the chassis of the node, again and again.\n" +
		"- the first item is long enough that it takes a second row at this width\n" +
		"A. read the controller log on worker-1 so we see when the sync error started\n" +
		"> a quoted line long enough that it takes a second row at this width too\n" +
		"```\n" + strings.Repeat("x", 50) + "\n```"
	got := ansi.Strip(markdown(in, 40, nil))
	want := "The ovnkube-controller on worker-1 logs\n" +
		"that it cannot get the chassis of the\n" +
		"node, again and again.\n" +
		"- the first item is long enough that it\n" +
		"  takes a second row at this width\n" +
		"A. read the controller log on worker-1\n" +
		"   so we see when the sync error started\n" +
		"│ a quoted line long enough that it\n" +
		"│ takes a second row at this width too\n" +
		"│ " + strings.Repeat("x", 38) + "\n" +
		"│ " + strings.Repeat("x", 12)
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
	want := "First\ntext\n\nSecond\n\nThird\n" + strings.Repeat("─", 20) + "\n│ ---\n│ ## not a heading"
	if got := ansi.Strip(markdown(in, 20, nil)); got != want {
		t.Errorf("markdown:\n%s\nwant:\n%s", got, want)
	}
}

// In the ledger, a comment that gave no items, its line beginning c#, is
// shown muted; one with items keeps its subsection heading's style.
func TestLedgerViewMutesCommentsWithoutItems(t *testing.T) {
	got := ledgerView("### c#2 · Sam Synthetic · e5\n\n- [ask] logs\n\nc#3 · A Bot · e3 · nothing: automated", 0)
	if !strings.Contains(got, mutedW.Render("c#3 · A Bot · e3 · nothing: automated")) {
		t.Errorf("the comment without items is not muted: %q", got)
	}
	if !strings.Contains(got, h3W.Render("c#2 · Sam Synthetic · e5")) {
		t.Errorf("the comment with items lost its heading: %q", got)
	}
}

func TestHasNextSteps(t *testing.T) {
	if !hasNextSteps("Summary.\n\nNext (enter a letter, or ask a question):\nA. Read e2\nB. Search logs\n\nFor you:\n- Run oc get pods") {
		t.Error("did not detect lettered Next steps")
	}
	if hasNextSteps("Summary.\n\nFor you:\n- Run oc get pods") {
		t.Error("falsely detected Next steps in For you list")
	}
	if hasNextSteps("```\nNext:\nA. inside code fence\n```") {
		t.Error("falsely detected Next steps inside a fenced code block")
	}
}

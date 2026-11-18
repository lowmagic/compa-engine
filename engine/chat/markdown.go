package chat

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
)

// Markdown in what the engineer reads — a model's answer, a draft, a
// handoff — is shown with its stress: bold, italics, code, headings,
// lists, quotes and links in the theme's weights and colours, the markers
// themselves dropped, and each line wrapped to the terminal's width, a
// list item's and a quote's rows under their first. It is display only:
// what models are sent is the text as stored. A small renderer of the
// chat's own over Lip Gloss rather than glamour, whose margins and
// wrapping would fight the scrollback's. Only * marks emphasis, never _,
// which identifiers are full of; a * is emphasis only where it opens before
// a word and closes after one, so ls *.log stays as it is.

var (
	mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	mdItem    = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(\S.*)$`)
	mdQuote   = regexp.MustCompile(`^>\s?(.*)$`)
	mdLink    = regexp.MustCompile(`^\[([^\]\n]+)\]\(([^)\s]+)\)`)
	mdRule    = regexp.MustCompile(`^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$`)
)

// citeMarks are an answer's citations by their text as written: why one
// is flagged, or empty where it resolves.
type citeMarks map[string]string

// marksOf marks an answer's citations by their checks, a citation whose
// check is missing flagged as unchecked: one written once and naming
// several ranges is flagged if any of them is.
func marksOf(cites []engine.Cite, checks []engine.CiteCheck) citeMarks {
	marks := citeMarks{}
	for i, c := range cites {
		why := "not checked"
		if i < len(checks) {
			why = ""
			if !checks[i].OK {
				why = checks[i].Why
			}
		}
		prev, seen := marks[c.Text]
		switch {
		case !seen || prev == "":
			marks[c.Text] = why
		case why != "" && !strings.Contains(prev, why):
			marks[c.Text] = prev + "; " + why
		}
	}
	return marks
}

// citeName is a citation as one place: e12:41-58, e12:41, e12, path:line,
// or a case-ledger item, l26.
func citeName(c engine.Cite) string {
	s := c.Name + c.Path + c.Ledger
	switch {
	case c.From > 0 && c.To > 0:
		return s + ":" + strconv.Itoa(c.From) + "-" + strconv.Itoa(c.To)
	case c.From > 0:
		return s + ":" + strconv.Itoa(c.From)
	}
	return s
}

// markdown is text, already made safe, with its Markdown shown and each
// line wrapped to width, none when width is zero or less; marks are its
// citations, each styled, a flagged one marked ⚠.
func markdown(text string, width int, marks citeMarks) string {
	r := renderer{width: width, marks: marks}
	for t := range marks {
		r.cites = append(r.cites, t)
	}
	// Longest first, so a citation is never shown as a shorter one it
	// begins with.
	sort.Slice(r.cites, func(i, j int) bool { return len(r.cites[i]) > len(r.cites[j]) })
	return r.render(text, nil)
}

// ledgerView is the case ledger's text shown as Markdown, each comment
// that gave no items, its one line beginning c#, muted, so the comments
// with items under their headings stand out.
func ledgerView(text string, width int) string {
	r := renderer{width: width}
	return r.render(text, func(l string) bool { return strings.HasPrefix(l, "c#") })
}

// render shows text's lines, those quiet says to muted and whole, a
// blank row before each heading that has none.
func (r renderer) render(text string, quiet func(string) bool) string {
	inCode := false
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n") {
		switch {
		case inCode:
		case quiet != nil && quiet(l):
			out = append(out, r.wrap("", "", r.inline(l, mutedW))...)
			continue
		case mdHeading.MatchString(l) && len(out) > 0 && out[len(out)-1] != "":
			out = append(out, "")
		}
		out = append(out, r.line(l, &inCode)...)
	}
	return strings.Join(out, "\n")
}

// renderer shows one text's Markdown.
type renderer struct {
	width int
	marks citeMarks
	cites []string
}

// line is one line of the text shown, in as many rows as its wrapping
// takes; inCode carries a fenced block from line to line.
func (r renderer) line(l string, inCode *bool) []string {
	if strings.HasPrefix(strings.TrimSpace(l), "```") {
		*inCode = !*inCode
		return r.hard(l, faintW)
	}
	if *inCode {
		return r.hard(l, codeW)
	}
	if mdRule.MatchString(l) {
		return []string{faintW.Render(strings.Repeat("─", max(min(r.width, 60), 3)))}
	}
	if m := mdHeading.FindStringSubmatch(l); m != nil {
		return r.wrap("", "", r.inline(m[2], headingW))
	}
	if m := mdItem.FindStringSubmatch(l); m != nil {
		mark := m[2]
		if mark == "-" || mark == "*" || mark == "+" {
			mark = "•"
		}
		hang := strings.Repeat(" ", ansi.StringWidth(m[1]+mark+" "))
		return r.wrap(m[1]+bulletW.Render(mark)+" ", hang, r.inline(m[3], plainW))
	}
	if m := mdQuote.FindStringSubmatch(l); m != nil && strings.HasPrefix(l, ">") {
		bar := faintW.Render("│ ")
		return r.wrap(bar, bar, r.inline(m[1], faintW))
	}
	return r.wrap("", "", r.inline(l, plainW))
}

// minRoom is the narrowest a wrapped line's text is made; a narrower
// terminal gets its lines whole.
const minRoom = 8

// wrap word-wraps a shown line to the width: its first row begins with
// first, each further row with rest.
func (r renderer) wrap(first, rest, s string) []string {
	room := r.width - ansi.StringWidth(first)
	if r.width <= 0 || room < minRoom {
		return []string{first + s}
	}
	rows := strings.Split(ansi.Wrap(s, room, ""), "\n")
	for i := range rows {
		if i == 0 {
			rows[i] = first + rows[i]
			continue
		}
		rows[i] = rest + rows[i]
	}
	return rows
}

// hard cuts a line kept as written, a code block's, at the width, each
// row in st.
func (r renderer) hard(l string, st lipgloss.Style) []string {
	if r.width <= 0 {
		return []string{st.Render(l)}
	}
	rows := strings.Split(ansi.Hardwrap(l, r.width, true), "\n")
	for i := range rows {
		rows[i] = st.Render(rows[i])
	}
	return rows
}

// citeAt is the citation s begins with, if any.
func (r renderer) citeAt(s string) (string, bool) {
	for _, c := range r.cites {
		if strings.HasPrefix(s, c) {
			return c, true
		}
	}
	return "", false
}

// cite is a citation shown: underlined where it resolves, else flagged.
func (r renderer) cite(c string) string {
	if r.marks[c] == "" {
		return citeW.Render(c)
	}
	return flagW.Render(c + " ⚠")
}

// inline is a line's inline Markdown shown: citations, `code`, **bold**,
// *italic* and [text](address).
func (r renderer) inline(s string, plain lipgloss.Style) string {
	var out, run strings.Builder
	bold, italic := false, false
	flush := func() {
		if run.Len() == 0 {
			return
		}
		st := plain
		if bold {
			st = st.Bold(true)
		}
		if italic {
			st = st.Italic(true)
		}
		out.WriteString(st.Render(run.String()))
		run.Reset()
	}
	wordAt := func(i int) bool {
		if i < 0 || i >= len(s) {
			return false
		}
		c := s[i]
		return c != ' ' && c != '\t' && c != '*'
	}
	for i := 0; i < len(s); {
		if c, ok := r.citeAt(s[i:]); ok {
			flush()
			out.WriteString(r.cite(c))
			i += len(c)
			continue
		}
		switch s[i] {
		case '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				flush()
				out.WriteString(codeW.Render(s[i+1 : i+1+j]))
				i += j + 2
				continue
			}
		case '*':
			n := 1
			for i+n < len(s) && s[i+n] == '*' {
				n++
			}
			switch {
			case wordAt(i-1) && n >= 3 && bold && italic:
				flush()
				bold, italic = false, false
				i += 3
				continue
			case wordAt(i-1) && n >= 2 && bold:
				flush()
				bold = false
				i += 2
				continue
			case wordAt(i-1) && italic:
				flush()
				italic = false
				i++
				continue
			case n == 2 && !bold && wordAt(i+2) && closes(s[i+2:], "**"):
				flush()
				bold = true
				i += 2
				continue
			case n == 1 && !italic && wordAt(i+1) && (i == 0 || !isWordByte(s[i-1])) && closes(s[i+1:], "*"):
				flush()
				italic = true
				i++
				continue
			}
			// A run of stars no marker uses is text.
			run.WriteString(s[i : i+n])
			i += n
			continue
		case '[':
			if m := mdLink.FindStringSubmatch(s[i:]); m != nil {
				flush()
				out.WriteString(linkW.Render(m[1]) + faintW.Render(" ("+m[2]+")"))
				i += len(m[0])
				continue
			}
		}
		run.WriteByte(s[i])
		i++
	}
	flush()
	return out.String()
}

// closes reports whether a marker opened before rest is closed in it: a
// run of stars after a word, not within one, holding the marker — an odd
// run for *, which a *** closing both holds.
func closes(rest, marker string) bool {
	for k := 1; k < len(rest); k++ {
		if rest[k] != '*' || rest[k-1] == ' ' || rest[k-1] == '*' {
			continue
		}
		r := k
		for r < len(rest) && rest[r] == '*' {
			r++
		}
		if r < len(rest) && isWordByte(rest[r]) {
			continue
		}
		if n := r - k; marker == "**" && n >= 2 || marker == "*" && n%2 == 1 {
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

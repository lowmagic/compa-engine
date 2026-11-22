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
// handoff — is shown with restrained stress: tiered headings (# underlined
// banner, ## coloured heading, ### bold subsection), rare **bold**, *italic*,
// `code`, plain `-` and ordered lists with nested items and continuation
// blocks aligned under their parent's text, indented or top-level fenced code
// blocks with their fence lines dropped, quotes and links, wrapped to the
// terminal's width. It is display only: what models are sent and what /copy
// copies is the text as stored. Only * marks emphasis, never _, which
// identifiers are full of; a * is emphasis only where it opens before a word
// and closes after one, so ls *.log stays as it is.

var (
	mdHeading  = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+)$`)
	mdItem     = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)]|[a-zA-Z][.)])\s+(\S.*)$`)
	mdQuote    = regexp.MustCompile(`^>\s?(.*)$`)
	mdLink     = regexp.MustCompile(`^\[([^\]\n]+)\]\(([^)\s]+)\)`)
	mdRule     = regexp.MustCompile(`^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$`)
	mdLeadBold = regexp.MustCompile(`^\*\*([^*\n]{1,80}?)\*\*(:)\s+(.*)$|^\*\*([^*\n]{1,80}?:)\*\*\s+(.*)$`)
	mdNextHead = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s+|\*\*)?next(?:\s*\([^)]*\))?:\*{0,2}\s*$`)
	mdForYou   = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s+|\*\*)?for you:\*{0,2}\s*$`)
	mdNextItem = regexp.MustCompile(`^\s*[A-Da-d][.)]\s+\S`)
)

// hasNextSteps reports whether text contains lettered items under a Next
// heading outside fenced code blocks.
func hasNextSteps(text string) bool {
	fence := ""
	inNext := false
	for _, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if fence != "" {
			if closesFence(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if f := opensFence(trimmed); f != "" {
			fence = f
			continue
		}
		switch {
		case mdNextHead.MatchString(raw):
			inNext = true
		case mdForYou.MatchString(raw) || mdHeading.MatchString(raw):
			inNext = false
		case inNext && mdNextItem.MatchString(raw):
			return true
		}
	}
	return false
}

// opensFence returns the fence prefix (at least three ` or ~) when trimmed
// opens a fenced code block, or empty when it does not.
func opensFence(trimmed string) string {
	for _, ch := range []string{"`", "~"} {
		n := len(trimmed) - len(strings.TrimLeft(trimmed, ch))
		if n < 3 {
			continue
		}
		// Per CommonMark, a backtick fence's info string holds no backticks.
		if ch == "`" && strings.Contains(trimmed[n:], "`") {
			continue
		}
		return strings.Repeat(ch, n)
	}
	return ""
}

// closesFence reports whether trimmed closes a block opened by fence.
func closesFence(trimmed, fence string) bool {
	return strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == ""
}

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

// listLevel tracks one active list depth: rawIndent is the source indentation
// of its marker, col the rendered column of its marker, and textCol the
// rendered column where its item text (and any child list or continuation
// paragraph) starts.
type listLevel struct {
	rawIndent int
	col       int
	textCol   int
}

// render shows text's lines, those quiet says to muted and whole, a
// blank row before each heading that has none.
func (r renderer) render(text string, quiet func(string) bool) string {
	var (
		fence    string
		fenceRaw int
		fenceCol int
		levels   []listLevel
		out      []string
	)
	for _, l := range strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n") {
		trimmed := strings.TrimSpace(l)
		if fence != "" {
			if closesFence(trimmed, fence) {
				fence = ""
				continue
			}
			out = append(out, r.codeLine(l, fenceRaw, fenceCol)...)
			continue
		}
		if f := opensFence(trimmed); f != "" {
			raw := leadingSpaces(l)
			col := 2
			switch {
			case raw > 0 && len(levels) > 0:
				levels = popLevelsForBody(levels, raw)
				if len(levels) > 0 {
					col = levels[len(levels)-1].textCol
				} else {
					col = raw
				}
			case raw > 0:
				col = raw
			default:
				levels = nil
			}
			fence, fenceRaw, fenceCol = f, raw, col
			continue
		}
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		if quiet != nil && quiet(l) {
			levels = nil
			out = append(out, r.wrap("", "", r.inline(l, mutedW))...)
			continue
		}
		if mdRule.MatchString(l) {
			levels = nil
			out = append(out, faintW.Render(strings.Repeat("─", max(min(r.width, 60), 3))))
			continue
		}
		if m := mdHeading.FindStringSubmatch(l); m != nil {
			levels = nil
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			out = append(out, r.heading(len(m[1]), m[2])...)
			continue
		}
		if m := mdItem.FindStringSubmatch(l); m != nil {
			raw := len(m[1])
			mark := m[2]
			if mark == "-" || mark == "*" || mark == "+" {
				mark = "-"
			}
			levels = pushListItem(levels, raw, len(mark)+1)
			top := levels[len(levels)-1]
			pad := strings.Repeat(" ", top.col)
			hang := strings.Repeat(" ", top.textCol)
			body := stripLeadBold(m[3])
			out = append(out, r.wrap(pad+bulletW.Render(mark)+" ", hang, r.inline(body, plainW))...)
			continue
		}
		if m := mdQuote.FindStringSubmatch(l); m != nil && strings.HasPrefix(l, ">") {
			levels = nil
			bar := faintW.Render("│ ")
			out = append(out, r.wrap(bar, bar, r.inline(m[1], faintW))...)
			continue
		}
		raw := leadingSpaces(l)
		if raw > 0 && len(levels) > 0 {
			levels = popLevelsForBody(levels, raw)
			if len(levels) > 0 {
				pad := strings.Repeat(" ", levels[len(levels)-1].textCol)
				out = append(out, r.wrap(pad, pad, r.inline(trimmed, plainW))...)
				continue
			}
		}
		levels = nil
		out = append(out, r.wrap("", "", r.inline(l, plainW))...)
	}
	return strings.Join(out, "\n")
}

// pushListItem updates the active list stack for an item at source indent raw
// whose rendered marker plus trailing space occupies markWidth columns.
func pushListItem(levels []listLevel, raw, markWidth int) []listLevel {
	for len(levels) > 0 && levels[len(levels)-1].rawIndent > raw {
		levels = levels[:len(levels)-1]
	}
	if len(levels) > 0 && levels[len(levels)-1].rawIndent == raw {
		top := &levels[len(levels)-1]
		top.textCol = top.col + markWidth
		return levels
	}
	col := 0
	if len(levels) > 0 {
		col = levels[len(levels)-1].textCol
	}
	return append(levels, listLevel{rawIndent: raw, col: col, textCol: col + markWidth})
}

// popLevelsForBody drops list levels deeper than an indented continuation
// paragraph or fenced block at source indent raw.
func popLevelsForBody(levels []listLevel, raw int) []listLevel {
	for len(levels) > 0 && raw < levels[len(levels)-1].rawIndent+2 {
		levels = levels[:len(levels)-1]
	}
	return levels
}

// stripLeadBold removes routine AI-style bolding on a list item's "Label: "
// lead-in while preserving genuine emphasis elsewhere in the item.
func stripLeadBold(s string) string {
	m := mdLeadBold.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	if m[1] != "" {
		return m[1] + m[2] + " " + m[3]
	}
	return m[4] + " " + m[5]
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

// renderer shows one text's Markdown.
type renderer struct {
	width int
	marks citeMarks
	cites []string
}

// heading renders a heading at tier 1 (# underlined banner), tier 2 (##
// coloured heading) or tier 3+ (### bold default foreground).
func (r renderer) heading(level int, body string) []string {
	switch level {
	case 1:
		rows := r.wrap("", "", r.inline(body, h1W))
		w := 0
		for _, row := range rows {
			w = max(w, ansi.StringWidth(row))
		}
		if r.width > 0 {
			w = min(w, r.width)
		}
		rule := faintW.Render(strings.Repeat("─", max(w, 3)))
		return append(rows, rule)
	case 2:
		return r.wrap("", "", r.inline(body, headingW))
	default:
		return r.wrap("", "", r.inline(body, h3W))
	}
}

// codeLine renders one line inside a fenced code block, preserving indentation
// beyond the opening fence's source column and aligning the block at col.
func (r renderer) codeLine(l string, fenceRaw, col int) []string {
	if strings.TrimSpace(l) == "" {
		return []string{""}
	}
	strip := min(leadingSpaces(l), fenceRaw)
	body := l[strip:]
	pad := strings.Repeat(" ", col)
	if r.width <= 0 || r.width-col < minRoom {
		return r.hard(pad+body, codeW)
	}
	rows := strings.Split(ansi.Hardwrap(body, r.width-col, true), "\n")
	for i := range rows {
		rows[i] = codeW.Render(pad + rows[i])
	}
	return rows
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

// hard cuts a line kept as written at the width, each row in st.
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
				if m[1] == m[2] {
					out.WriteString(linkW.Render(m[1]))
				} else {
					out.WriteString(linkW.Render(m[1]) + faintW.Render(" ("+m[2]+")"))
				}
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

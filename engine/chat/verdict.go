package chat

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// The checks tab of the browser (docs/MODEL_API.md §Answer checks): the
// findings of the answers printed this run, each with its state, and the
// one under the cursor in full. A value an answer states that its checks
// flagged — a code span found in no evidence, with a line close to it or
// none, or arithmetic that does not hold — is shown with its claim, the
// complaint and the evidence around the lines it is checked against, and
// the engineer judges it supported, inferred, unsupported or incorrect,
// marking the lines it rests on in the evidence shown or any item. The
// rest are for reading and take no verdict: a citation flagged or not in
// the form read, a finding of the editor's it wrote no correction for, a
// citation the revision added; after them, the anchors the revision was
// asked to fulfil, which are no finding. Beneath an answer the chat
// prints one line saying how many findings there are and one summing up
// the anchors; after an answer it offers them on f, once; f, or /check
// [aN], opens the tab at that answer's. Nothing is printed into the chat
// until the browser closes, then one line per answer judged.

// unsupportedWords mark a span no line of the evidence shares a word with.
const unsupportedWords = "found nowhere in the evidence: unsupported"

// readMark is the state of a finding that takes no verdict, and readWords
// say why its verdict keys do nothing.
const (
	readMark  = "·"
	readWords = "is for reading: it takes no verdict"
)

// Lines of context shown around the evidence a value is checked against:
// before and after a single line, and before and after a range.
const (
	lineBefore, lineAfter   = 2, 5
	rangeBefore, rangeAfter = 1, 2
)

// checksPane is the checks tab: the findings, the one under the cursor,
// and where the engineer is marking lines for it — an item open (view),
// from the evidence shown or from the list (listing), esc going back to
// the list or to the finding. While judging, a verdict is being kept, and
// the keys wait. lines are the items' lines as the browser shows them,
// read once each, or why they could not be.
type checksPane struct {
	values  []checkValue
	cur     int
	view    *opened
	listing bool
	judging bool
	lines   map[string][]string
	failed  map[string]error
}

// checkValue is a finding of an answer as the tab lists it: the answer's
// label and either its span flag, with the lines the engineer marked for
// it, or a finding for reading (read).
type checkValue struct {
	label string
	flag  engine.AnswerFlag
	read  *reading
	marks []engine.EvidenceRange
}

// shown is the finding as the tab names it.
func (v checkValue) shown() string {
	if v.read != nil {
		return text.Safe(v.read.what)
	}
	return spanShown(v.flag.Span)
}

// reading is a finding of an answer that takes no verdict: what it is
// (about), as the tab lists it (what), and why. A citation has written,
// as the answer writes it; the editor's finding its statement, kind and
// the words at fault (quote), and the most the evidence supports
// (atMost), or, for an anchor, what fulfils it (asked); causal wording and
// a command the statement holding it (claim). lines are the items' lines
// it cites, From 0 for an item cited whole.
type reading struct {
	about, what, why       string
	written                string
	statement, kind, quote string
	atMost, asked          string
	claim                  string
	lines                  []engine.EvidenceRange
}

// What a finding for reading is.
const (
	aboutCiteFlagged = "a citation flagged"
	aboutCiteForm    = "a citation not in the form read"
	aboutEditor      = "the editor's finding, not corrected"
	aboutAdded       = "a citation the revision added"
	aboutAnchor      = "an anchor asked of the revision"
)

// readings are an answer's findings that take no verdict, each once, in
// the order they were printed beneath it before the checks tab held them:
// its citations flagged, those not in the form read, its causal wording
// and its commands found in no evidence or guidance (engine.AnswerFlag's
// Finding, which names them), the editor's findings it wrote no
// correction for and the citations the revision added
// (engine.AnswerCheck.Notes).
func readings(cites []engine.Cite, checks []engine.CiteCheck, flags []engine.AnswerFlag, c engine.AnswerCheck) []reading {
	var out []reading
	seen := map[string]bool{}
	add := func(r reading) {
		if k := r.about + "\x00" + r.what + "\x00" + r.why + "\x00" + r.claim; !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	for i, ct := range cites {
		why := "not checked"
		if i < len(checks) {
			if checks[i].OK {
				continue
			}
			why = checks[i].Why
		}
		add(reading{about: aboutCiteFlagged, what: citeName(ct), why: why, written: ct.Text, lines: citedLines([]engine.Cite{ct})})
	}
	for _, f := range flags {
		switch {
		case f.Span != "":
		case f.Finding != "":
			add(reading{about: f.Finding, what: f.What, why: f.Why, claim: f.Statement})
		default:
			add(reading{about: aboutCiteForm, what: f.What, why: f.Why})
		}
	}
	for _, f := range c.Flagged {
		add(editorReading(aboutEditor, f))
	}
	whys := map[string]string{}
	for _, n := range c.Notes() {
		if n.Kind == engine.NoteFlag {
			whys[n.Text] = n.Why
		}
	}
	for _, a := range c.Added {
		add(reading{about: aboutAdded, what: a, why: whys[a], written: a, lines: citedLines(engine.Cites(a))})
	}
	return out
}

// editorReading is a finding of the editor's as the tab lists it: its
// statement, kind and words at fault, why, the most the lines support or,
// for an anchor, what fulfils it, and the lines it rests on.
func editorReading(about string, f engine.CheckFinding) reading {
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	var cs []engine.Cite
	for _, ref := range f.Evidence {
		cs = append(cs, engine.Cites("["+ref+"]")...)
	}
	r := reading{about: about, what: f.Statement + " " + f.Kind + " · " + strconv.Quote(flat(f.Quote)), why: flat(f.Why),
		statement: f.Statement, kind: f.Kind, quote: flat(f.Quote), lines: citedLines(cs)}
	if about == aboutAnchor {
		r.asked = flat(f.Suggested)
	} else {
		r.atMost = flat(f.Suggested)
	}
	return r
}

// asked are the anchors the revision of an answer was asked to fulfil
// (engine.AnswerCheck.Asked), as the tab lists them.
func asked(c engine.AnswerCheck) []reading {
	var out []reading
	for _, f := range c.Asked() {
		out = append(out, editorReading(aboutAnchor, f))
	}
	return out
}

// asksLine is the one line beneath an answer whose revision was asked to
// fulfil anchors: how many, by kind, most first, and what opens them in
// the checks tab, f where it is offered; empty where there are none.
func asksLine(a answer, offered bool) string {
	if len(a.asks) == 0 {
		return ""
	}
	var kinds []string
	count := map[string]int{}
	for _, r := range a.asks {
		if count[r.kind]++; count[r.kind] == 1 {
			kinds = append(kinds, r.kind)
		}
	}
	head := "asked of the revision: " + text.Plural(len(a.asks), "anchor")
	switch {
	case len(kinds) > 1:
		slices.SortStableFunc(kinds, func(x, y string) int { return count[y] - count[x] })
		byKind := make([]string, len(kinds))
		for i, k := range kinds {
			byKind[i] = strconv.Itoa(count[k]) + " " + k
		}
		head += ": " + strings.Join(byKind, ", ")
	case len(a.asks) > 1:
		head += ", all " + kinds[0]
	default:
		head += ", " + kinds[0]
	}
	if offered {
		return head + " · f opens the checks tab"
	}
	return head + " · /check " + a.label + " lists them"
}

// citedLines are the evidence lines citations name, From 0 for an item
// named whole; an artifact's path names none.
func citedLines(cs []engine.Cite) []engine.EvidenceRange {
	var out []engine.EvidenceRange
	for _, c := range cs {
		if c.Name == "" {
			continue
		}
		to := c.To
		if to == 0 {
			to = c.From
		}
		out = append(out, engine.EvidenceRange{Name: c.Name, From: c.From, To: to})
	}
	return out
}

// findings are how many findings an answer has, as the checks tab lists
// them, and how many of its spans the engineer has judged.
func (a answer) findings() (n, judged int) {
	for _, f := range a.flags {
		if f.Span == "" {
			continue
		}
		n++
		if _, ok := a.verdicts[f.Span]; ok {
			judged++
		}
	}
	return n + len(a.reads), judged
}

// findingsLine is the one line beneath an answer with findings: how many,
// how many of them judged, and what opens them in the checks tab, f where
// it is offered; empty where there are none.
func findingsLine(a answer, offered bool) string {
	n, judged := a.findings()
	if n == 0 {
		return ""
	}
	head := "⚠ " + text.Plural(n, "finding")
	switch {
	case judged > 0:
		head += ", " + strconv.Itoa(judged) + " judged"
	case offered:
		head += " to review"
	}
	how := " · /check " + a.label + " opens them"
	if offered {
		how = " · f opens the checks tab"
	}
	return warnW.Render(head) + faintW.Render(how)
}

// verdictMsg is the verdict kept on a span of the answer labelled, or why
// it was not.
type verdictMsg struct {
	label, span string
	v           engine.SpanVerdict
	err         error
}

// newChecks is the checks tab over the findings of every answer printed
// this run, in order: each answer's span flags, then its findings for
// reading, then the anchors asked of its revision.
func (m *model) newChecks() *checksPane {
	p := &checksPane{lines: map[string][]string{}, failed: map[string]error{}}
	for _, a := range m.answers {
		for _, f := range a.flags {
			if f.Span != "" {
				p.values = append(p.values, checkValue{label: a.label, flag: f})
			}
		}
		for i := range a.reads {
			p.values = append(p.values, checkValue{label: a.label, read: &a.reads[i]})
		}
		for i := range a.asks {
			p.values = append(p.values, checkValue{label: a.label, read: &a.asks[i]})
		}
	}
	return p
}

// back is esc within the tab: from an item to the list it was opened
// from, or to the finding; from the list to the finding. It reports false
// at the finding, where esc closes the browser.
func (p *checksPane) back() bool {
	switch {
	case p.view != nil:
		p.view = nil
	case p.listing:
		p.listing = false
	default:
		return false
	}
	return true
}

// verdictOf is the engineer's verdict on a value, if any.
func (m *model) verdictOf(v checkValue) (engine.SpanVerdict, bool) {
	if v.read != nil {
		return engine.SpanVerdict{}, false
	}
	for _, a := range m.answers {
		if a.label == v.label {
			got, ok := a.verdicts[v.flag.Span]
			return got, ok
		}
	}
	return engine.SpanVerdict{}, false
}

// unjudged says a finding is a span the engineer has yet to judge.
func (m *model) unjudged(v checkValue) bool {
	_, judged := m.verdictOf(v)
	return v.read == nil && !judged
}

// kindMarks are the verdicts' marks in the tab.
var kindMarks = map[engine.VerdictKind]string{
	engine.VerdictSupported: "✓", engine.VerdictInferred: "~", engine.VerdictUnsupported: "⚠", engine.VerdictIncorrect: "✗",
}

// stateOf is a finding's mark: · for reading, the engineer's verdict, the
// engine's where its arithmetic does not hold, or ? unjudged.
func (m *model) stateOf(v checkValue) string {
	if v.read != nil {
		return readMark
	}
	if got, ok := m.verdictOf(v); ok {
		return kindMarks[got.Kind]
	}
	if v.flag.Arithmetic {
		return kindMarks[engine.VerdictIncorrect]
	}
	return "?"
}

// shownRanges are the lines a finding's evidence shows, and what they
// are: those a finding for reading cites; for a value those marked, the
// verdict's, or the line closest to it.
func (m *model) shownRanges(v checkValue) ([]engine.EvidenceRange, string) {
	if v.read != nil {
		return v.read.lines, "the lines it cites"
	}
	if len(v.marks) > 0 {
		return v.marks, "marked"
	}
	if got, ok := m.verdictOf(v); ok && len(got.Evidence) > 0 {
		return got.Evidence, "the verdict's"
	}
	if c := v.flag.Closest; c != nil {
		return []engine.EvidenceRange{{Name: c.Name, From: c.Line, To: c.Line}}, "the line closest to it"
	}
	return nil, ""
}

// contextOf are the first and last lines shown, from 1, of an item of n
// lines around lines from to to: lineBefore and lineAfter around one
// line, rangeBefore and rangeAfter around a range.
func contextOf(from, to, n int) (int, int) {
	before, after := lineBefore, lineAfter
	if to > from {
		before, after = rangeBefore, rangeAfter
	}
	return max(from-before, 1), min(to+after, n)
}

// itemLines are the lines of the item named as the browser shows them,
// read once.
func (p *checksPane) itemLines(s *engine.Session, name string) ([]string, error) {
	if l, ok := p.lines[name]; ok {
		return l, nil
	}
	if err, ok := p.failed[name]; ok {
		return nil, err
	}
	it, err := s.EvidenceNamed(name)
	if err != nil {
		err = fmt.Errorf("%s: %w", name, err)
		p.failed[name] = err
		return nil, err
	}
	l := withoutSource(contentLines(string(it.Content)), it.Location)
	p.lines[name] = l
	return l, nil
}

// checkFindings opens the checks tab at the findings of the answer
// labelled, the last where label is empty: its first span not judged,
// else its first.
func (m *model) checkFindings(label string) {
	if len(m.answers) == 0 {
		m.print(errorLine("there is no answer to check yet"))
		return
	}
	a := m.answers[len(m.answers)-1]
	if label != "" {
		i := len(m.answers) - 1
		for i >= 0 && m.answers[i].label != label {
			i--
		}
		if i < 0 {
			m.print(errorLine("there is no answer " + label + "; the answers are a1 to " + a.label))
			return
		}
		a = m.answers[i]
	}
	if n, _ := a.findings(); n+len(a.asks) == 0 {
		m.print(noteLine(a.label + " has no finding to review"))
		return
	}
	b, err := m.newBrowser()
	if err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	b.tab = tabChecks
	p := b.checks
	p.cur = slices.IndexFunc(p.values, func(v checkValue) bool { return v.label == a.label })
	for i := p.cur; i < len(p.values) && p.values[i].label == a.label; i++ {
		if m.unjudged(p.values[i]) {
			p.cur = i
			break
		}
	}
	m.pending = b
}

// checksKey is a key on the checks tab.
func (m *model) checksKey(k tea.KeyPressMsg) {
	b := m.browser
	p := b.checks
	key := k.String()
	switch {
	case p.judging:
		b.note = "keeping the verdict"
		return
	case p.view != nil:
		m.checksMarkKey(key)
		return
	case p.listing:
		if key != "enter" {
			b.listMove(k)
			return
		}
		rows := b.shown()
		if b.cur >= len(rows) {
			return
		}
		o, err := b.openItem(m.s, rows[b.cur].name, 0, 0)
		if err != nil {
			b.note = err.Error()
			return
		}
		p.view = o
		return
	case len(p.values) == 0:
		return
	}
	v := p.values[p.cur]
	switch key {
	case "up", "k":
		p.cur = max(p.cur-1, 0)
	case "down", "j":
		p.cur = min(p.cur+1, len(p.values)-1)
	case "y", "enter":
		m.judgeValue(engine.VerdictSupported)
	case "i":
		m.judgeValue(engine.VerdictInferred)
	case "u":
		m.judgeValue(engine.VerdictUnsupported)
	case "x":
		m.judgeValue(engine.VerdictIncorrect)
	case "m":
		ranges, _ := m.shownRanges(v)
		if len(ranges) == 0 {
			b.note = "no evidence is shown for it: e opens any item to mark lines in"
			if v.read != nil {
				b.note = "it cites no item's lines"
			}
			return
		}
		r := ranges[0]
		o, err := b.openItem(m.s, r.Name, r.From, r.To)
		if err != nil {
			b.note = err.Error()
			return
		}
		p.view = o
	case "e":
		if v.read != nil {
			b.note = readingNote(v)
			return
		}
		p.listing = true
	case "d":
		if v.read != nil {
			b.note = readingNote(v)
			return
		}
		m.dropMark()
	}
}

// readingNote says a finding for reading takes no verdict.
func readingNote(v checkValue) string { return v.label + " " + v.shown() + " " + readWords }

// dropMark drops the last range marked for the value under the cursor,
// no longer shown as cited on the item open.
func (m *model) dropMark() {
	b := m.browser
	p := b.checks
	v := &p.values[p.cur]
	if len(v.marks) == 0 {
		b.note = "nothing is marked for " + v.label + " " + v.shown()
		return
	}
	last := v.marks[len(v.marks)-1]
	v.marks = v.marks[:len(v.marks)-1]
	if o := p.view; o != nil && o.name == last.Name && o.from == last.From && o.to == last.To {
		o.from, o.to = 0, 0
	}
	b.note = last.Place() + " dropped"
	if len(v.marks) > 0 {
		b.note += "; still marked " + places(v.marks)
	}
}

// verdictKeys are the keys that judge a value, in the tab and on an item
// open in it; enter is supported in the tab only, as on an item it keeps
// the lines marked.
var verdictKeys = map[string]engine.VerdictKind{
	"y": engine.VerdictSupported,
	"i": engine.VerdictInferred,
	"u": engine.VerdictUnsupported,
	"x": engine.VerdictIncorrect,
}

// checksMarkKey is a key on an item open in the checks tab: the cursor
// moves, v marks lines, and enter keeps them for the value, shown as
// cited; a verdict's key there judges the value on the lines kept and
// returns to the tab. On the lines a finding for reading cites, only the
// cursor moves.
func (m *model) checksMarkKey(key string) {
	b := m.browser
	p := b.checks
	o := p.view
	v := &p.values[p.cur]
	if v.read != nil {
		if _, judge := verdictKeys[key]; judge || key == "v" || key == "enter" || key == "d" {
			b.note = readingNote(*v)
			return
		}
		o.move(key, m.width)
		return
	}
	if kind, ok := verdictKeys[key]; ok {
		p.view, p.listing = nil, false
		m.judgeValue(kind)
		return
	}
	if key == "d" {
		m.dropMark()
		return
	}
	if o.move(key, m.width) {
		return
	}
	from, to, note, ok := o.markKey(key)
	if !ok {
		return
	}
	if from == 0 {
		b.note = note
		return
	}
	r := engine.EvidenceRange{Name: o.name, From: from, To: to}
	if slices.ContainsFunc(v.marks, func(got engine.EvidenceRange) bool { return got.Place() == r.Place() }) {
		b.note = r.Place() + " is marked already"
		return
	}
	if len(v.marks) >= engine.VerdictRanges {
		b.note = fmt.Sprintf("a verdict rests on at most %d ranges: d drops the last", engine.VerdictRanges)
		return
	}
	o.from, o.to = from, to
	v.marks = append(v.marks, r)
	b.note = r.Place() + " marked for " + v.label + " " + v.shown() + ": y, i or x takes " + places(v.marks)
}

// spanShown is a span as the tab shows it.
func spanShown(span string) string { return text.Safe("`" + span + "`") }

// places are ranges as places, e3:4-6, e5:2, and e7 for an item cited
// whole.
func places(rs []engine.EvidenceRange) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Place()
		if r.From == 0 {
			out[i] = r.Name
		}
	}
	return strings.Join(out, ", ")
}

// judgeValue keeps the engineer's verdict of kind on the value under the
// cursor, resting on the lines marked for it; inferred needs some. A
// finding for reading takes none, saying so.
func (m *model) judgeValue(kind engine.VerdictKind) {
	b := m.browser
	p := b.checks
	v := p.values[p.cur]
	if v.read != nil {
		b.note = readingNote(v)
		return
	}
	if got, ok := m.verdictOf(v); ok {
		b.note = v.label + " " + v.shown() + " is judged already: " + string(got.Kind)
		return
	}
	if m.turn != nil {
		b.note = "a turn is running: a verdict waits for its answer"
		return
	}
	if kind == engine.VerdictInferred && len(v.marks) == 0 {
		b.note = engine.ErrInferredUnmarked.Error() + ": m marks them in the evidence shown, e in any item"
		return
	}
	p.judging = true
	label, flag, marks, s := v.label, v.flag, v.marks, m.s
	m.spawn(func(ctx context.Context, _ func(tea.Msg)) tea.Msg {
		got, err := s.JudgeSpan(ctx, label, flag, kind, marks)
		return verdictMsg{label: label, span: flag.Span, v: got, err: err}
	})
}

// verdictDone takes a verdict kept: its answer holds it, the tab says so
// and goes on to the next span not judged, and the browser's closing
// sums it up; or the tab says why it was not kept.
func (m *model) verdictDone(msg verdictMsg) {
	var p *checksPane
	if m.browser != nil {
		p = m.browser.checks
		p.judging = false
	}
	if msg.err != nil {
		if p == nil {
			m.print(errorLine("the verdict was not kept: " + msg.err.Error()))
			return
		}
		m.browser.note = "the verdict was not kept: " + msg.err.Error()
		return
	}
	for i := range m.answers {
		if m.answers[i].label == msg.label {
			if m.answers[i].verdicts == nil {
				m.answers[i].verdicts = map[string]engine.SpanVerdict{}
			}
			m.answers[i].verdicts[msg.span] = msg.v
		}
	}
	m.judged = append(m.judged, msg.v)
	if p == nil {
		m.printJudged()
		return
	}
	note := msg.label + " " + spanShown(msg.span) + ": " + string(msg.v.Kind)
	if msg.v.Kind.Told() {
		note += ", the model reads it at its next call"
	}
	m.browser.note = note
	at := -1
	for i := range p.values {
		if p.values[i].read == nil && p.values[i].label == msg.label && p.values[i].flag.Span == msg.span {
			p.values[i].marks, at = nil, i
		}
	}
	if at != p.cur {
		return
	}
	for i := p.cur + 1; i < len(p.values); i++ {
		if m.unjudged(p.values[i]) {
			p.cur = i
			return
		}
	}
}

// printJudged prints, once the browser closes, a line for each answer
// with verdicts kept since it opened: how many of each kind, and which
// the model reads.
func (m *model) printJudged() {
	var order []string
	counts := map[string]map[engine.VerdictKind]int{}
	for _, v := range m.judged {
		if counts[v.Answer] == nil {
			order = append(order, v.Answer)
			counts[v.Answer] = map[engine.VerdictKind]int{}
		}
		counts[v.Answer][v.Kind]++
	}
	m.judged = nil
	for _, label := range order {
		var parts, told []string
		for _, k := range []engine.VerdictKind{engine.VerdictSupported, engine.VerdictInferred, engine.VerdictIncorrect, engine.VerdictUnsupported} {
			if n := counts[label][k]; n > 0 {
				parts = append(parts, strconv.Itoa(n)+" "+string(k))
				if k.Told() {
					told = append(told, string(k))
				}
			}
		}
		line := label + ": " + strings.Join(parts, ", ")
		if len(told) > 0 {
			line += "; the model reads the " + strings.Join(told, " and ") + " at its next call"
		}
		m.print(noteLine(line))
	}
}

// checksView is the checks tab in room rows, and its keys: an item open
// to mark lines in, or to read a finding's lines in, the list to choose
// one from, or the findings and the one under the cursor.
func (m *model) checksView(w, room int) ([]string, string) {
	b := m.browser
	p := b.checks
	if len(p.values) == 0 {
		return []string{faintW.Render("no answer printed this run has a finding to review")}, "tab switches"
	}
	v := p.values[p.cur]
	if v.read != nil {
		if p.view != nil {
			return append([]string{faintW.Render("the lines " + v.label + " " + v.shown() + " cites")}, p.view.view(w, room-1, "")...),
				"↑↓ move · esc back · for reading: it takes no verdict"
		}
		keys := "for reading: it takes no verdict · ↑↓"
		if len(v.read.lines) > 0 {
			keys = "for reading: it takes no verdict · m opens the lines it cites · ↑↓"
		}
		body := m.valueRows(w, max(min(len(p.values), room/3), 1))
		body = append(body, "")
		return append(body, m.readingDetail(v.read, w)...), keys
	}
	target := "for " + v.label + " " + v.shown()
	if len(v.marks) > 0 {
		target += " · marked " + places(v.marks)
	}
	switch {
	case p.view != nil:
		return append([]string{faintW.Render("marking " + target)}, p.view.view(w, room-1, "")...),
			"↑↓ move · v marks, enter keeps it for the value · d unmarks the last · y i u x judge it · esc back"
	case p.listing:
		keys := "type to filter · ↑↓ move · enter opens · esc back to the value"
		if b.filter != "" {
			keys = "filter: " + b.filter + " · " + keys
		}
		return append([]string{faintW.Render("an item to mark lines in " + target)}, b.listRows(m.citedBy(), w, room-1)...), keys
	}
	body := m.valueRows(w, max(min(len(p.values), room/3), 1))
	body = append(body, "")
	body = append(body, m.valueDetail(v, w)...)
	return body, "y supported · i inferred · u unsupported · x incorrect · m marks · d unmarks · e any item · ↑↓"
}

// valueRows are the findings' rows that fit in room, the cursor's in
// them, each with its state, a finding for reading with why.
func (m *model) valueRows(w, room int) []string {
	p := m.browser.checks
	first := max(0, min(p.cur-room/2, len(p.values)-room))
	var out []string
	for i := first; i < len(p.values) && len(out) < room; i++ {
		v := p.values[i]
		row := m.stateOf(v) + " " + v.label + " " + v.shown()
		if n := len(v.marks); n > 0 {
			row += " · " + text.Plural(n, "range") + " marked"
		}
		if v.read != nil {
			row += " · " + detailSafe(v.read.why)
		}
		row = ansi.Truncate(row, w-2, "…")
		if i == p.cur {
			out = append(out, cursorW.Render("› "+row))
			continue
		}
		out = append(out, "  "+row)
	}
	return out
}

// detailWidth is the width of a detail's heading.
const detailWidth = 11

// detailField is a detail's field: its name as the heading, s wrapped
// beside it to w in at most rows rows.
func detailField(name, s string, w, rows int) []string {
	wrapped := strings.Split(ansi.Wrap(s, max(w-detailWidth, 10), ""), "\n")
	if len(wrapped) > rows {
		wrapped = wrapped[:rows]
		wrapped[rows-1] += "…"
	}
	out := []string{faintW.Render(fmt.Sprintf("%-*s", detailWidth, name)) + wrapped[0]}
	for _, l := range wrapped[1:] {
		out = append(out, strings.Repeat(" ", detailWidth)+l)
	}
	return out
}

// detailSafe is text of an answer or a check made safe to show in a
// detail.
func detailSafe(s string) string { return text.Safe(strings.ReplaceAll(s, "\t", "    ")) }

// valueDetail is what the tab shows of the value under the cursor: its
// claim, the complaint, its verdict, and the evidence it is checked
// against, each line with its context, numbered, the lines themselves
// shown as cited.
func (m *model) valueDetail(v checkValue, w int) []string {
	out := detailField("Claim:", detailSafe(v.flag.Statement), w, 3)
	complaint := spanShown(v.flag.Span) + " " + detailSafe(v.flag.Why)
	if v.flag.Unsupported() {
		complaint += " · " + unsupportedWords
	}
	out = append(out, detailField("Complaint:", complaint, w, 2)...)
	verdict := "? not judged"
	if got, ok := m.verdictOf(v); ok {
		verdict = kindMarks[got.Kind] + " " + string(got.Kind) + ", by the engineer"
		if len(got.Evidence) > 0 {
			verdict += " against " + places(got.Evidence)
		}
		if got.Kind.Told() {
			verdict += "; the model is told"
		}
	} else if v.flag.Arithmetic {
		verdict = kindMarks[engine.VerdictIncorrect] + " incorrect, by the engine: the arithmetic does not hold; y, i, u or x overrides it"
	}
	out = append(out, detailField("Verdict:", verdict, w, 2)...)
	ranges, what := m.shownRanges(v)
	if len(ranges) == 0 {
		return append(out, detailField("Evidence:", "none close to it · e opens any item to mark lines in", w, 1)...)
	}
	out = append(out, detailField("Evidence:", places(ranges)+" · "+what, w, 1)...)
	return append(out, m.evidenceRows(ranges, w)...)
}

// readingDetail is what the tab shows of a finding for reading: what it
// is; the citation as written, the statement holding it, or the editor's
// statement, kind and the words at fault; why; the most the evidence
// supports; that it takes no verdict; and the lines it cites, each with
// its context, as a value's evidence is shown.
func (m *model) readingDetail(r *reading, w int) []string {
	out := detailField("Finding:", r.about, w, 1)
	if r.written != "" {
		out = append(out, detailField("Citation:", detailSafe(r.written), w, 2)...)
	}
	if r.claim != "" {
		out = append(out, detailField("Claim:", detailSafe(r.claim), w, 3)...)
	}
	if r.statement != "" {
		out = append(out, detailField("Statement:", detailSafe(r.statement+" · "+r.kind), w, 1)...)
		out = append(out, detailField("Words:", detailSafe(strconv.Quote(r.quote)), w, 3)...)
	}
	out = append(out, detailField("Why:", detailSafe(r.why), w, 3)...)
	if r.atMost != "" {
		out = append(out, detailField("At most:", detailSafe(r.atMost), w, 3)...)
	}
	if r.asked != "" {
		out = append(out, detailField("Asked:", detailSafe(r.asked), w, 3)...)
	}
	out = append(out, detailField("Verdict:", readMark+" for reading: it takes no verdict", w, 1)...)
	if len(r.lines) == 0 {
		return append(out, detailField("Evidence:", "it cites no item's lines", w, 1)...)
	}
	out = append(out, detailField("Evidence:", places(r.lines)+" · the lines it cites", w, 1)...)
	return append(out, m.evidenceRows(r.lines, w)...)
}

// evidenceRows are the lines of ranges, each with its context, numbered,
// the lines themselves shown as cited; an item cited whole is named, and
// one that cannot be read or a range past its end said so.
func (m *model) evidenceRows(ranges []engine.EvidenceRange, w int) []string {
	var out []string
	for _, r := range ranges {
		lines, err := m.browser.checks.itemLines(m.s, r.Name)
		switch {
		case err != nil:
			out = append(out, faintW.Render("  "+text.Safe(err.Error())))
			continue
		case r.From == 0:
			out = append(out, faintW.Render("  "+r.Name+" whole, "+text.Plural(len(lines), "line")))
			continue
		case r.From > len(lines):
			out = append(out, faintW.Render("  "+r.Place()+" is past the item's end"))
			continue
		}
		a, z := contextOf(r.From, r.To, len(lines))
		gutter := len(strconv.Itoa(z))
		for n := a; n <= z; n++ {
			line := ansi.Truncate(lines[n-1], max(w-gutter-3, 1), "…")
			num := fmt.Sprintf("  %*d ", gutter, n)
			if n >= r.From && n <= r.To {
				out = append(out, num+citedW.Render(line))
				continue
			}
			out = append(out, faintW.Render(num)+line)
		}
	}
	return out
}

package chat

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// The evidence browser takes the screen over the chat, the alternate
// screen, and esc gives it back with its scrollback as it was. Navigating
// changes nothing in the conversation; the lines the engineer marks are
// kept for their next message, and nothing is sent until they send it; a
// verdict on a value in the checks tab is kept as it is given
// (docs/ARCHITECTURE.md §The evidence browser).

type browserTab int

const (
	tabEvidence browserTab = iota
	tabItems
	tabTurns
	tabChecks
	tabCount
)

// browser is the browser's state.
type browser struct {
	tab browserTab
	// all is the list: every named piece of evidence, newest first, or
	// the case's comments, newest first; filter is what the engineer
	// typed to narrow it, cur the row under the cursor.
	all      []listed
	comments bool
	filter   string
	cur      int
	// open is the evidence open, nil on the list; back says esc returns
	// to the list rather than to the chat.
	open *opened
	back bool
	// items are the pinned answers and drafts, the items tab's; item the
	// one open.
	items   []engine.CompanionItem
	itemCur int
	item    *opened
	// said are the conversation's turns, the turns tab's, oldest first;
	// saidOpen the one open.
	said     []said
	saidCur  int
	saidOpen *opened
	// cites are the evidence citations of the answer the browser was
	// opened from, and at the one shown, -1 before the first.
	cites []engine.Cite
	at    int
	// searching is a search being typed; query the search.
	searching bool
	query     string
	// checks is the checks tab: the findings of the answers printed.
	checks *checksPane
	// note says what the last key did, or why it did nothing.
	note string
}

// listed is one row of the list.
type listed struct {
	name, by string
	size     string
	// label describes a comment: its ID, author and time.
	label string
}

// opened is a piece of evidence or an item open: its lines under their
// numbers, the cursor's line, the cited lines and the start of a mark.
type opened struct {
	name     string
	head     []string
	lines    []string
	cur, top int
	// col is how many columns the lines are scrolled left by, and wide
	// the widest line's width, zero until measured.
	col, wide int
	// from and to are the cited lines, from 1, zero for none; anchor is
	// where a mark began, from 1, zero for none.
	from, to int
	anchor   int
}

// itemsMsg is the items and the conversation's turns for the browser's
// tabs.
type itemsMsg struct {
	items   []engine.CompanionItem
	err     error
	said    []said
	saidErr error
}

// evidenceArg is /evidence's argument: a name and lines.
var evidenceArg = regexp.MustCompile(`^(e\d+)(?::(\d+)(?:-(\d+))?)?$`)

// newBrowser is the browser over the case's named evidence, its items and
// the conversation's turns read beside it, stepping through the last
// answer's citations, with the findings of the answers printed this run
// in its checks tab.
func (m *model) newBrowser() (*browser, error) {
	ns, err := m.s.Evidences()
	if err != nil {
		return nil, err
	}
	b := &browser{at: -1, checks: m.newChecks()}
	for _, n := range ns {
		b.all = append(b.all, listed{name: n.Name, by: n.By, size: sizeOf(n.Item)})
	}
	if len(m.answers) > 0 {
		b.cites = m.answers[len(m.answers)-1].cites
	}
	s := m.s
	m.spawn(func(ctx context.Context, _ func(tea.Msg)) tea.Msg {
		items, err := s.Items(ctx)
		msg := itemsMsg{items: items, err: err}
		history, herr := s.ConversationHistory(ctx)
		msg.said, msg.saidErr = turnsOf(history), herr
		return msg
	})
	return b, nil
}

// openEvidence opens the browser: at an item and its lines, or, with
// none named, at the last answer's first citation, or on the list.
func (m *model) openEvidence(arg string) {
	var name string
	var from, to int
	if arg != "" {
		mm := evidenceArg.FindStringSubmatch(arg)
		if mm == nil {
			m.print(errorLine("/evidence takes a name and lines, as e12, e12:41 or e12:41-58"))
			return
		}
		name = mm[1]
		from, _ = strconv.Atoi(mm[2])
		to, _ = strconv.Atoi(mm[3])
	}
	b, err := m.newBrowser()
	if err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	switch {
	case name != "":
		if err := b.show(m.s, name, from, to); err != nil {
			m.print(errorLine(err.Error()))
			return
		}
	case len(b.cites) > 0:
		b.step(m.s, 1)
	}
	m.pending = b
}

// openComments opens the browser on the case's comments, newest first,
// or on the nth newest.
func (m *model) openComments(arg string) {
	n := 0
	if arg != "" {
		var err error
		if n, err = strconv.Atoi(arg); err != nil || n < 1 {
			m.print(errorLine("/comments takes a number: /comments 1 is the newest"))
			return
		}
	}
	cms := m.s.Comments()
	if len(cms) == 0 {
		m.print(errorLine("the case has no comments read"))
		return
	}
	if n > len(cms) {
		m.print(errorLine(fmt.Sprintf("the case has %s", text.Plural(len(cms), "comment"))))
		return
	}
	b, err := m.newBrowser()
	if err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	b.all, b.comments = nil, true
	for _, cm := range cms {
		// Naming a comment names it for the case, as its conversation
		// does; it changes nothing in the conversation.
		name, err := m.s.NameEvidence(cm.EvidenceID)
		if err != nil {
			m.print(errorLine(err.Error()))
			return
		}
		who := cm.AuthorName
		if who == "" {
			who = cm.Author
		}
		label := "comment " + cm.ID + " · " + who + " · " + cm.Created.UTC().Format("2006-01-02 15:04") + " · " + firstLine(cm.Body)
		b.all = append(b.all, listed{name: name, label: label})
	}
	if n > 0 {
		if err := b.show(m.s, b.all[n-1].name, 0, 0); err != nil {
			m.print(errorLine(err.Error()))
			return
		}
	}
	m.pending = b
}

// openItems opens the browser on its items tab.
func (m *model) openItems() {
	b, err := m.newBrowser()
	if err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	b.tab = tabItems
	m.pending = b
}

// show opens the evidence named at its lines, the cited ones marked.
func (b *browser) show(s *engine.Session, name string, from, to int) error {
	o, err := b.openItem(s, name, from, to)
	if err != nil {
		return err
	}
	b.open, b.tab = o, tabEvidence
	return nil
}

// openItem is the evidence named open at its lines, the cited ones
// marked, its lines as the browser shows them.
func (b *browser) openItem(s *engine.Session, name string, from, to int) (*opened, error) {
	it, err := s.EvidenceNamed(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	by := ""
	for _, l := range b.all {
		if l.name == name {
			by = l.by
		}
	}
	o := &opened{name: name, from: from, to: to, lines: withoutSource(contentLines(string(it.Content)), it.Location)}
	o.head = evidenceHead(name, by, it)
	o.cur = min(max(from-1, 0), max(len(o.lines)-1, 0))
	o.top = -1
	return o, nil
}

// contentLines are a text's lines, its tabs spaced and made safe to show.
func contentLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = text.Safe(strings.ReplaceAll(l, "\t", "    "))
	}
	return lines
}

// sourceLine is how a search prints a line after its file's name: : for
// a match or - for context, the line's number in the file, and the same
// mark again, as grep -Hn does.
var sourceLine = regexp.MustCompile(`^[:-][0-9]+[:-]`)

// withoutSource leaves off the file name a search of one file prints at
// the start of every line, keeping the line's number in the file. The
// view's head names the file ("from"), and a name as long as a
// sosreport's paths leaves nothing of the line on the screen. Lines from
// several files, or any line not in that form, are left whole.
func withoutSource(lines []string, location string) []string {
	loc := text.Safe(strings.TrimSpace(location))
	if loc == "" || strings.Contains(loc, "\n") || len(lines) == 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		rest, ok := strings.CutPrefix(l, loc)
		if !ok || !sourceLine.MatchString(rest) {
			return lines
		}
		out[i] = rest[1:]
	}
	return out
}

// evidenceHead is what an item's view says of it: its name and what
// produced it, its size, where it came from, and any stop or cut.
func evidenceHead(name, by string, it engine.EvidenceItem) []string {
	head := []string{name + " · " + firstLine(by)}
	size := sizeOf(it)
	if it.Size.LowerBound {
		size += ", at least"
	}
	head = append(head, size+" · stored "+it.CreatedAt.UTC().Format("2006-01-02 15:04"))
	if loc := strings.Split(strings.TrimSpace(it.Location), "\n"); loc[0] != "" {
		l := "from " + loc[0]
		if len(loc) > 1 {
			l += fmt.Sprintf(" and %d more", len(loc)-1)
		}
		head = append(head, l)
	}
	if it.Note != "" && it.Note != by {
		head = append(head, "note: "+firstLine(it.Note))
	}
	if c := it.Collection; c != nil {
		if c.Stopped != nil {
			head = append(head, "stopped: "+firstLine(c.Stopped.Summary()))
		}
		if c.Truncated {
			head = append(head, "its output was cut at the collection's cap")
		}
	}
	for i := range head {
		head[i] = text.Safe(head[i])
	}
	return head
}

// sizeOf is an item's size in lines and bytes.
func sizeOf(it engine.EvidenceItem) string {
	return text.Plural(int(it.Size.Lines), "line") + " · " + bytesShown(it.Size.Bytes)
}

func bytesShown(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// step opens the citation d after the one shown, of the answer the
// browser was opened from.
func (b *browser) step(s *engine.Session, d int) {
	if len(b.cites) == 0 {
		b.note = "the last answer cites no evidence"
		return
	}
	at := b.at + d
	if at < 0 || at >= len(b.cites) {
		b.note = fmt.Sprintf("no further citation: the answer has %s", text.Plural(len(b.cites), "citation"))
		return
	}
	b.at = at
	c := b.cites[at]
	b.note = fmt.Sprintf("citation %d of %d: %s", at+1, len(b.cites), citeName(c))
	if err := b.show(s, c.Name, c.From, c.To); err != nil {
		b.note += " — " + err.Error()
	}
}

// citedBy are the answers printed this run citing each piece of evidence.
func (m *model) citedBy() map[string][]string {
	out := map[string][]string{}
	for _, a := range m.answers {
		for _, c := range a.cites {
			if !slices.Contains(out[c.Name], a.label) {
				out[c.Name] = append(out[c.Name], a.label)
			}
		}
	}
	return out
}

// shown are the list's rows the filter keeps.
func (b *browser) shown() []listed {
	if b.filter == "" {
		return b.all
	}
	f := strings.ToLower(b.filter)
	var out []listed
	for _, l := range b.all {
		if strings.Contains(strings.ToLower(l.name+" "+l.by+" "+l.label), f) {
			out = append(out, l)
		}
	}
	return out
}

// browserKey is a key pressed in the browser.
func (m *model) browserKey(k tea.KeyPressMsg) tea.Cmd {
	b := m.browser
	key := k.String()
	if key == "ctrl+c" {
		return tea.Batch(m.closeBrowser(), m.key(k))
	}
	b.note = ""
	if b.searching {
		b.searchKey(k)
		return nil
	}
	switch key {
	case "esc":
		if b.checks.judging {
			// The verdict being kept is summed up with the rest once
			// it is: esc waits for it, as the tab's keys do.
			b.note = "keeping the verdict"
			return nil
		}
		switch {
		case b.tab == tabItems && b.item != nil:
			b.item = nil
		case b.tab == tabTurns && b.saidOpen != nil:
			b.saidOpen = nil
		case b.tab == tabEvidence && b.open != nil && b.back:
			b.open, b.back = nil, false
		case b.tab == tabChecks && b.checks.back():
		default:
			return m.closeBrowser()
		}
		return nil
	case "tab":
		b.tab = (b.tab + 1) % tabCount
		return nil
	}
	switch b.tab {
	case tabItems:
		return m.itemsKey(key)
	case tabTurns:
		return m.turnsKey(key)
	case tabChecks:
		m.checksKey(k)
		return nil
	}
	if b.open == nil {
		b.listKey(m.s, k)
		return nil
	}
	m.openKey(key)
	return nil
}

// listKey is a key on the list: the cursor moves, enter opens, and what
// is typed filters.
func (b *browser) listKey(s *engine.Session, k tea.KeyPressMsg) {
	rows := b.shown()
	if k.String() == "enter" {
		if b.cur < len(rows) {
			if err := b.show(s, rows[b.cur].name, 0, 0); err != nil {
				b.note = err.Error()
				return
			}
			b.back = true
		}
		return
	}
	b.listMove(k)
}

// listMove is a key on the list but enter: the cursor moves, and what is
// typed filters.
func (b *browser) listMove(k tea.KeyPressMsg) {
	rows := b.shown()
	switch k.String() {
	case "up":
		b.cur--
	case "down":
		b.cur++
	case "pgup":
		b.cur -= 10
	case "pgdown":
		b.cur += 10
	case "home":
		b.cur = 0
	case "end":
		b.cur = len(rows) - 1
	case "backspace":
		if r := []rune(b.filter); len(r) > 0 {
			b.filter, b.cur = string(r[:len(r)-1]), 0
		}
	default:
		if k.Text != "" && k.Mod&^tea.ModShift == 0 {
			b.filter += k.Text
			b.cur = 0
		}
	}
	b.cur = min(max(b.cur, 0), max(len(b.shown())-1, 0))
}

// openKey is a key on an open piece of evidence: the cursor moves, v
// marks lines and enter keeps the mark, / searches, n and p step through
// the answer's citations.
func (m *model) openKey(key string) {
	b := m.browser
	o := b.open
	if o.move(key, m.width) {
		return
	}
	if from, to, note, ok := o.markKey(key); ok {
		if from == 0 {
			b.note = note
			return
		}
		mark := engine.EvidenceRange{Name: o.name, From: from, To: to}.Place()
		if !slices.Contains(m.marks, mark) {
			m.marks = append(m.marks, mark)
		}
		b.note = mark + " kept: your next message points at " + strings.Join(m.marks, ", ")
		return
	}
	switch key {
	case "l":
		b.open, b.back = nil, false
	case "/":
		b.searching, b.query = true, ""
	case "n":
		b.step(m.s, 1)
	case "p":
		b.step(m.s, -1)
	}
}

// move is a key moving an open view's cursor, or scrolling its lines
// sideways, reporting whether it was one.
func (o *opened) move(key string, width int) bool {
	switch key {
	case "up", "k":
		o.cur--
	case "down", "j":
		o.cur++
	case "pgup":
		o.cur -= 20
	case "pgdown", "space":
		o.cur += 20
	case "home", "g":
		o.cur = 0
	case "end", "G":
		o.cur = len(o.lines) - 1
	case "left", "right":
		o.scroll(key, width)
		return true
	default:
		return false
	}
	o.cur = min(max(o.cur, 0), max(len(o.lines)-1, 0))
	return true
}

// markKey is v or enter on an open view, reporting whether key was one:
// v starts a mark at the cursor, or drops the one begun; enter keeps it,
// returning its lines, from 1. Where nothing is kept, from is zero and
// note says what the key did.
func (o *opened) markKey(key string) (from, to int, note string, ok bool) {
	switch key {
	case "v":
		if o.anchor != 0 {
			o.anchor = 0
			return 0, 0, "mark dropped", true
		}
		o.anchor = o.cur + 1
		return 0, 0, "marking: move to extend, enter keeps it", true
	case "enter":
		if o.anchor == 0 {
			return 0, 0, "v starts a mark at the cursor", true
		}
		from, to = min(o.anchor, o.cur+1), max(o.anchor, o.cur+1)
		o.anchor = 0
		return from, to, "", true
	}
	return 0, 0, "", false
}

// scroll moves the lines a quarter of the screen's width left or right;
// view keeps the widest line's end in sight.
func (o *opened) scroll(key string, width int) {
	step := max(width/4, 8)
	if key == "left" {
		o.col = max(o.col-step, 0)
		return
	}
	o.col += step
}

// searchKey is a key while a search is typed: enter finds the next line
// holding it, after the cursor, from the top again past the end; an
// empty search finds the last one again.
func (b *browser) searchKey(k tea.KeyPressMsg) {
	switch k.String() {
	case "esc":
		b.searching = false
	case "backspace":
		if r := []rune(b.query); len(r) > 0 {
			b.query = string(r[:len(r)-1])
		}
	case "enter":
		b.searching = false
		o := b.open
		if o == nil || b.query == "" {
			return
		}
		q := strings.ToLower(b.query)
		for i := 1; i <= len(o.lines); i++ {
			n := (o.cur + i) % len(o.lines)
			if strings.Contains(strings.ToLower(o.lines[n]), q) {
				o.cur = n
				b.note = fmt.Sprintf("%q at line %d", b.query, n+1)
				return
			}
		}
		b.note = fmt.Sprintf("%q is not in %s", b.query, o.name)
	default:
		if k.Text != "" {
			b.query += k.Text
		}
	}
}

// itemsKey is a key on the items tab: the cursor moves, enter opens one
// whole, y copies it.
func (m *model) itemsKey(key string) tea.Cmd {
	b := m.browser
	if o := b.item; o != nil {
		switch key {
		case "up", "k":
			o.cur--
		case "down", "j":
			o.cur++
		case "pgup":
			o.cur -= 20
		case "pgdown", "space":
			o.cur += 20
		case "y":
			return m.copyFromBrowser()
		case "left", "right":
			o.scroll(key, m.width)
			return nil
		}
		o.cur = min(max(o.cur, 0), max(len(o.lines)-1, 0))
		return nil
	}
	switch key {
	case "up", "k":
		b.itemCur--
	case "down", "j":
		b.itemCur++
	case "enter":
		if b.itemCur < len(b.items) {
			it := b.items[b.itemCur]
			b.item = &opened{name: it.Name(), head: []string{text.Safe(itemLabel(it))}, lines: contentLines(it.Text), top: -1}
		}
	case "y":
		return m.copyFromBrowser()
	}
	b.itemCur = min(max(b.itemCur, 0), max(len(b.items)-1, 0))
	return nil
}

// copyFromBrowser copies the item under the cursor, said in the browser
// and printed once the chat has its screen back.
func (m *model) copyFromBrowser() tea.Cmd {
	b := m.browser
	if b.itemCur >= len(b.items) {
		b.note = "there are no items: /pin keeps the last answer, /draft writes a draft"
		return nil
	}
	it := b.items[b.itemCur]
	b.note = "asked the terminal to copy " + it.Name() + " to the clipboard (OSC 52)"
	return m.copyItem(it)
}

// itemLabel is an item as the items tab lists it.
func itemLabel(it engine.CompanionItem) string {
	what := "pinned answer"
	if it.Kind == engine.ItemDraft {
		what = "draft " + it.Intention + ", " + audienceShown(it.Audience)
	}
	return it.Name() + " · " + what + " · " + it.CreatedAt.UTC().Format("2006-01-02 15:04") + " · " + firstLine(it.Text)
}

// browserView is the browser on the whole screen: its tabs, the list or
// what is open, and a line of keys or what the last one did.
func (m *model) browserView() string {
	b := m.browser
	w, h := max(m.width, 20), max(m.height, 6)
	tabs := []string{"evidence", "items", "turns", "checks"}
	if b.comments {
		tabs[0] = "comments"
	}
	tabs[b.tab] = headingW.Render("[" + tabs[b.tab] + "]")
	top := strings.Join(tabs, "  ") + faintW.Render("   tab switches · esc returns to the chat")
	room := h - 3
	var body []string
	var keys string
	switch {
	case b.tab == tabItems && b.item != nil:
		body = b.item.view(w, room, "")
		keys = "↑↓ move · ←→ scroll · y copies · esc to the items"
	case b.tab == tabItems:
		body = b.itemRows(w, room)
		keys = "↑↓ move · enter opens · y copies"
	case b.tab == tabTurns && b.saidOpen != nil:
		body = b.saidOpen.view(w, room, "")
		keys = "↑↓ move · ←→ scroll · y copies it as stored · esc to the turns"
	case b.tab == tabTurns:
		body = b.saidRows(w, room)
		keys = "↑↓ move · enter opens · y copies as stored"
	case b.tab == tabChecks:
		body, keys = m.checksView(w, room)
	case b.open != nil:
		body = b.open.view(w, room, b.query)
		keys = "↑↓ move · ←→ scroll · v marks, enter keeps · / searches · n p the answer's citations · l the list"
	default:
		body = b.listRows(m.citedBy(), w, room)
		keys = "type to filter · ↑↓ move · enter opens"
		if b.filter != "" {
			keys = "filter: " + b.filter + " · " + keys
		}
	}
	foot := faintW.Render(keys)
	switch {
	case b.searching:
		foot = "/" + text.Safe(b.query)
	case b.note != "":
		foot = stateW.Render(text.Safe(b.note))
	}
	for len(body) < room {
		body = append(body, "")
	}
	lines := append([]string{top}, body[:room]...)
	lines = append(lines, "", foot)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "…")
	}
	return strings.Join(lines, "\n")
}

// listRows are the list's rows that fit, the cursor's in them.
func (b *browser) listRows(citedBy map[string][]string, w, room int) []string {
	rows := b.shown()
	if len(rows) == 0 {
		if b.filter != "" {
			return []string{faintW.Render("nothing matches " + strconv.Quote(b.filter))}
		}
		return []string{faintW.Render("no evidence is named yet: it is named as the conversation shows it")}
	}
	first := max(0, min(b.cur-room/2, len(rows)-room))
	var out []string
	for i := first; i < len(rows) && len(out) < room; i++ {
		l := rows[i]
		what := l.by + " · " + l.size
		if l.label != "" {
			what = l.label
		}
		row := fmt.Sprintf("%-5s %s", l.name, text.Safe(firstLine(what)))
		if by := citedBy[l.name]; len(by) > 0 {
			row += " · cited by " + strings.Join(by, ", ")
		}
		row = ansi.Truncate(row, w-2, "…")
		if i == b.cur {
			out = append(out, cursorW.Render("› "+row))
			continue
		}
		out = append(out, "  "+row)
	}
	return out
}

// itemRows are the items tab's rows.
func (b *browser) itemRows(w, room int) []string {
	if len(b.items) == 0 {
		return []string{faintW.Render("no items: /pin keeps the last answer, /draft writes a draft")}
	}
	first := max(0, min(b.itemCur-room/2, len(b.items)-room))
	var out []string
	for i := first; i < len(b.items) && len(out) < room; i++ {
		row := ansi.Truncate(text.Safe(itemLabel(b.items[i])), w-2, "…")
		if i == b.itemCur {
			out = append(out, cursorW.Render("› "+row))
			continue
		}
		out = append(out, "  "+row)
	}
	return out
}

// view is an open piece of evidence or item in room rows: its head, then
// its lines under their numbers, the cursor's, the marked, the cited and
// those holding the search shown.
func (o *opened) view(w, room int, query string) []string {
	out := make([]string, 0, room)
	for _, h := range o.head {
		// Where the item came from is shown whole, wrapped: a search's
		// lines leave its file's name off (withoutSource), and a
		// sosreport's paths run past the screen's width.
		if strings.HasPrefix(h, "from ") {
			for _, part := range strings.Split(ansi.Hardwrap(h, w, false), "\n") {
				out = append(out, faintW.Render(part))
			}
			continue
		}
		out = append(out, faintW.Render(h))
	}
	out = append(out, "")
	rows := max(room-len(out), 1)
	if len(o.lines) == 0 {
		return append(out, faintW.Render("(empty)"))
	}
	// Opened at a line, the line shows a third of the way down; after,
	// the view follows the cursor.
	switch {
	case o.top < 0:
		o.top = max(o.cur-rows/3, 0)
	case o.cur < o.top:
		o.top = o.cur
	case o.cur >= o.top+rows:
		o.top = o.cur - rows + 1
	}
	gutter := len(strconv.Itoa(len(o.lines)))
	a, z := 0, -1
	if o.anchor != 0 {
		a, z = min(o.anchor, o.cur+1), max(o.anchor, o.cur+1)
	}
	width := max(w-gutter-1, 1)
	if o.wide == 0 {
		for _, l := range o.lines {
			o.wide = max(o.wide, ansi.StringWidth(l))
		}
	}
	// Scrolled, a line starts with … for what is off to the left; it
	// scrolls no further than shows the widest line's end.
	o.col = min(o.col, max(o.wide-width+1, 0))
	for i := o.top; i < len(o.lines) && len(out) < room; i++ {
		n := i + 1
		num := fmt.Sprintf("%*d ", gutter, n)
		line := o.lines[i]
		if o.col > 0 {
			line = ansi.TruncateLeft(line, o.col, "…")
		}
		line = ansi.Truncate(line, width, "…")
		switch {
		case n >= a && n <= z:
			line = markW.Render(line)
		case o.from > 0 && n >= o.from && n <= max(o.from, o.to):
			line = citedW.Render(line)
		case query != "" && strings.Contains(strings.ToLower(o.lines[i]), strings.ToLower(query)):
			line = hitW.Render(line)
		}
		if i == o.cur {
			num = cursorW.Render(num)
		} else {
			num = faintW.Render(num)
		}
		out = append(out, num+line)
	}
	return out
}

// browserItems takes the items for the browser's tab, open or about to
// open.
func (m *model) browserItems(msg itemsMsg) {
	b := m.browser
	if b == nil {
		b = m.pending
	}
	if b == nil {
		return
	}
	if msg.err != nil {
		b.note = "the items could not be read: " + msg.err.Error()
	} else {
		b.items = msg.items
	}
	if msg.saidErr != nil {
		b.note = "the conversation's turns could not be read: " + msg.saidErr.Error()
		return
	}
	b.said = msg.said
	// The newest turn first under the cursor: the one most likely wanted.
	b.saidCur = max(len(b.said)-1, 0)
}

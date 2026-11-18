package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/check"
)

// The epistemic checks' deterministic part: a text split into
// statements with their citations; what is shown beneath an answer beside
// its citation flags — a citation not in the form Cites reads, and a code
// span found in no evidence the conversation saw; the candidates the
// editor is given to judge; and the one message the editor or the reviewer
// would be sent, its cited lines quoted by Quote. None calls a model.

// CheckStatement is one statement of a checked text, with its citations
// as Cites reads them.
type CheckStatement struct {
	check.Statement
	Cites []Cite
}

// AnswerFlag is what the deterministic checks show beneath an answer:
// what is flagged, as written, and why.
type AnswerFlag struct {
	What, Why string
	// Span is a span flag's code span as written, and Statement the first
	// statement of the answer holding it, as the answer shows it; both are
	// empty for a citation's form.
	Span, Statement string
	// Closest is, for a span flag, the line of the evidence the search read
	// sharing the most words with the span, at least half of them, which
	// the engineer can check it against (nearest); nil where no line shares
	// as many.
	Closest *EvidenceLine
	// Arithmetic says the span is arithmetic the engine evaluated, not
	// searched for, and found not to hold (arithmetic): incorrect, the
	// engine's own verdict, which the engineer may override.
	Arithmetic bool
}

// Unsupported says whether the flag is a span searched for and found
// nowhere, no line of the evidence read close to it.
func (f AnswerFlag) Unsupported() bool { return f.Span != "" && f.Closest == nil && !f.Arithmetic }

// EvidenceLine is one line of an evidence item: its short name, its
// number from 1, and its text as stored, cut to closestLineBytes.
type EvidenceLine struct {
	Name string
	Line int
	Text string
}

// Place is the line as one place, e14:2.
func (l EvidenceLine) Place() string { return l.Name + ":" + strconv.Itoa(l.Line) }

// Why the editor is given a statement to judge.
const (
	CandidateListing   = "cites only file listings"
	CandidateUncited   = "states a code span, a time or a count and cites nothing"
	CandidateHedge     = "cites lines an earlier statement cited with a qualifier it no longer has"
	CandidateCertainty = "holds certainty or causal wording that may be in its own voice"
)

// CheckCandidate is a statement the deterministic checks give the editor
// to judge, since what it claims needs reading: one resting only on file
// listings, one stating something exact with no citation, one citing
// lines an earlier answer cited with a qualifier it has dropped, or one
// holding certainty or causal wording kept out of a writer's own
// voice.
type CheckCandidate struct {
	Statement, Why string
	// Listings are the listings a statement resting on them cites.
	Listings []string
	// Earlier is the earlier statement, EarlierAnswer its answer (a3) and
	// Hedges its qualifiers, for a dropped qualifier.
	Earlier, EarlierAnswer string
	Hedges                 []string
	// Terms are the certainty and causal words a statement holds.
	Terms []string
}

// Checked is a text as the deterministic checks see it.
type Checked struct {
	Statements []CheckStatement
	// Flags are what is shown beneath it; Unresolved its citations that
	// CheckCites flags, as "[e999:2]: no evidence e999".
	Flags      []AnswerFlag
	Unresolved []string
	Candidates []CheckCandidate
}

// CheckRequest is the one message the editor or the reviewer would be
// sent about a text, and what its findings are validated against.
type CheckRequest struct {
	Text    string
	Context check.Context
	Checked Checked
	Quotes  []QuotedItem
}

// Request bounds: a request is at most its role's bytes, the same for
// both, so the editor and the reviewer judge the same evidence; its
// quotations, each under its heading and line numbers, take what the rest
// leaves. Each item cited is quoted whole up to quoteItemBytes; past it,
// or past what the request leaves, the lines cited are quoted with
// quoteContextLines lines either side, each window to quoteWindowLines
// lines and quoteWindowBytes bytes. Set from live use, in
// which the evidence a conversation read was at most 66 KB in all and its
// largest item 13.7 KB: an item has near five times the largest's room,
// and a request every item the largest conversation read near four times
// over.
const (
	editorRequestBytes   = 256 << 10
	reviewerRequestBytes = editorRequestBytes
	quoteItemBytes       = 64 << 10
	quoteContextLines    = 5
	quoteWindowLines     = 40 + 2*quoteContextLines
	quoteWindowBytes     = 8 << 10
)

// spanReads bound what the span check reads of the evidence the
// conversation saw: each item to its first Item bytes, and Total bytes
// over every item.
type spanReads struct{ Item, Total int }

// spanBudget is the span check's bounds, on a turn's end and on reopening
// alike.
var spanBudget = spanReads{Item: 4 << 20, Total: 32 << 20}

// closestLineBytes bounds the text kept of a span's closest line: what the
// engineer is shown and, where they judge the span wrong, the model reads.
const closestLineBytes = 300

var (
	// bareCite is a citation's name and lines without its brackets.
	bareCite = regexp.MustCompile(`\be\d+:\d+(?:[-–]\d+)?\b`)
	// bracketCite is anything bracketed that begins with a name and goes
	// on past it with no word character, which evidenceCite must read
	// whole to be a citation: not [e2e suite] or [e1000e].
	bracketCite = regexp.MustCompile(`\[e\d+(?:[^\w\]\n][^\]\n]*)?\]`)
	// parenCite is names and lines in parentheses.
	parenCite  = regexp.MustCompile(`\(e\d+(?::\d+(?:[-–]\d+)?)?(?:,\s*e\d+(?::\d+(?:[-–]\d+)?)?)*\)`)
	inlineCode = regexp.MustCompile("`[^`\n]+`")
	firstName  = regexp.MustCompile(`e\d+`)
)

// prose is text with its fenced blocks and code spans blanked to spaces,
// so what is found in it keeps its place in the text and nothing inside
// code is taken for a citation.
func prose(t string) string {
	lines := strings.Split(t, "\n")
	for i, fenced := range check.Fenced(lines) {
		if fenced {
			lines[i] = blank(lines[i])
		}
	}
	return inlineCode.ReplaceAllStringFunc(strings.Join(lines, "\n"), blank)
}

// blank is s as spaces, its newlines kept.
func blank(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c != '\n' {
			b[i] = ' '
		}
	}
	return string(b)
}

// citeForms are a text's citations not in the form Cites reads, so never
// checked or quoted: names and lines without brackets, a bracket Cites
// cannot read, names in parentheses. Code is left alone, as is what only
// looks like one: a name with a colon beside it, as in a MAC or an IPv6
// address, and a name the conversation never showed, such as e1000.
func citeForms(t string, disclosed map[string]bool) []AnswerFlag {
	p := prose(t)
	read := evidenceCite.FindAllStringIndex(p, -1)
	within := func(at []int, spans [][]int) bool {
		for _, s := range spans {
			if at[0] >= s[0] && at[1] <= s[1] {
				return true
			}
		}
		return false
	}
	type found struct {
		at        []int
		what, why string
	}
	var all []found
	var taken [][]int
	for _, at := range parenCite.FindAllStringIndex(p, -1) {
		inner := t[at[0]+1 : at[1]-1]
		all = append(all, found{at, t[at[0]:at[1]], "not read as a citation in parentheses: write [" + strings.ReplaceAll(inner, "–", "-") + "]"})
		taken = append(taken, at)
	}
	for _, at := range bracketCite.FindAllStringIndex(p, -1) {
		if slices.ContainsFunc(read, func(r []int) bool { return r[0] == at[0] && r[1] == at[1] }) {
			continue
		}
		all = append(all, found{at, t[at[0]:at[1]], "not read as a citation: write it as [e12:41-58] or [e12:41, e13:2]"})
		taken = append(taken, at)
	}
	for _, at := range bareCite.FindAllStringIndex(p, -1) {
		if within(at, read) || within(at, taken) || at[0] > 0 && p[at[0]-1] == ':' || at[1] < len(p) && p[at[1]] == ':' {
			continue
		}
		m := t[at[0]:at[1]]
		all = append(all, found{at, m, "not read as a citation without brackets: write [" + strings.ReplaceAll(m, "–", "-") + "]"})
	}
	slices.SortStableFunc(all, func(a, b found) int { return a.at[0] - b.at[0] })
	out := make([]AnswerFlag, 0, len(all))
	for _, f := range all {
		if disclosed[firstName.FindString(f.what)] {
			out = append(out, AnswerFlag{What: f.what, Why: f.why})
		}
	}
	return out
}

// statementsOf are a text's statements with their citations.
func statementsOf(t string) []CheckStatement {
	ss := check.Split(t)
	out := make([]CheckStatement, len(ss))
	for i, s := range ss {
		out[i] = CheckStatement{Statement: s, Cites: Cites(s.Text)}
	}
	return out
}

// citesSource says whether citations name evidence or an artifact: a
// case-ledger item among them, a model's reading of a comment, is none.
func citesSource(cs []Cite) bool {
	return slices.ContainsFunc(cs, func(c Cite) bool { return c.Ledger == "" })
}

// AnswerFlags are what the deterministic checks show beneath each of
// texts, the model's answers, beside its citation flags: a citation not in
// the form Cites reads, and a code span of a statement, or a line of a
// fenced block presented as output (Commands.Excerpts), found in no
// evidence the conversation saw, within spanBudget, every item read once
// for all the texts; a span that is a command line to run, its program the
// engine's or one checks.commands names (check.Commands), is no
// claim and is not looked for, and a span that is only arithmetic
// (arithmetic) is evaluated instead, flagged incorrect where it does not
// hold. A span flag names the first statement holding the span, or the
// statement presenting the block, and the line of the evidence read
// closest to it, found in the same pass. Where the span check fails, the
// citation forms and the arithmetic are still returned, with its error.
func (s *Session) AnswerFlags(ctx context.Context, texts []string) ([][]AnswerFlag, error) {
	return s.answerFlags(ctx, texts, spanBudget)
}

// arithmeticWrong is why a span of arithmetic that does not hold is
// flagged.
const arithmeticWrong = "incorrect: the arithmetic does not hold"

func (s *Session) answerFlags(ctx context.Context, texts []string, b spanReads) ([][]AnswerFlag, error) {
	x, err := s.exposure(ctx)
	if err != nil {
		return nil, err
	}
	// spanAt is a span of a text with the first statement holding it;
	// wrong says it is arithmetic that does not hold, not searched for.
	type spanAt struct {
		span  string
		st    CheckStatement
		wrong bool
	}
	spans := make([][]spanAt, len(texts))
	var all []string
	out := make([][]AnswerFlag, len(texts))
	commands := check.Commands(s.e.config.Checks.Commands)
	for i, t := range texts {
		out[i] = citeForms(t, x.shown)
		for _, st := range statementsOf(t) {
			// A condition's values are what it supposes, not what was
			// seen.
			if check.Conditional(st.Text) {
				continue
			}
			for _, sp := range check.Spans(st.Text) {
				if commands.Command(sp) || slices.ContainsFunc(spans[i], func(a spanAt) bool { return a.span == sp }) {
					continue
				}
				if holds, ok := arithmetic(sp); ok {
					if !holds {
						spans[i] = append(spans[i], spanAt{sp, st, true})
					}
					continue
				}
				spans[i] = append(spans[i], spanAt{sp, st, false})
				if !slices.Contains(all, sp) {
					all = append(all, sp)
				}
			}
		}
		// A fenced block presented as output is looked for line by line,
		// each line flagged under the statement presenting it: one naming
		// text or a log, or a bare one whose presenting statement cites
		// evidence, as an excerpt of it would. A bare block after an
		// uncited statement may be a query or a snippet to use, and is not.
		for _, ex := range commands.Excerpts(t) {
			st := CheckStatement{Statement: ex.After, Cites: Cites(ex.After.Text)}
			if ex.Bare && !citesSource(st.Cites) {
				continue
			}
			for _, l := range ex.Lines {
				if slices.ContainsFunc(spans[i], func(a spanAt) bool { return a.span == l }) {
					continue
				}
				spans[i] = append(spans[i], spanAt{l, st, false})
				if !slices.Contains(all, l) {
					all = append(all, l)
				}
			}
		}
	}
	found, err := s.unseenSpans(ctx, all, x, b)
	why := found.why(b)
	for i := range texts {
		for _, a := range spans[i] {
			switch {
			case a.wrong:
				out[i] = append(out[i], AnswerFlag{What: "`" + a.span + "`", Why: arithmeticWrong, Span: a.span, Statement: a.st.Text, Arithmetic: true})
			case err == nil && found.unseen[a.span]:
				out[i] = append(out[i], AnswerFlag{What: "`" + a.span + "`", Why: why, Span: a.span, Statement: a.st.Text,
					Closest: closestOf(found.closest[a.span], a.st.Cites)})
			}
		}
	}
	return out, err
}

// closestOf is the closest line among lines, each the first line of an
// item sharing the most words with a span, all sharing as many: one of an
// item the statement cites, then the earliest item's, nil where there are
// none.
func closestOf(lines []EvidenceLine, cites []Cite) *EvidenceLine {
	var best *EvidenceLine
	bestCited := false
	for i := range lines {
		l := &lines[i]
		cited := slices.ContainsFunc(cites, func(c Cite) bool { return c.Name == l.Name })
		switch {
		case best == nil, cited && !bestCited:
		case cited == bestCited && nameNumber(l.Name) < nameNumber(best.Name):
		default:
			continue
		}
		best, bestCited = l, cited
	}
	if best == nil {
		return nil
	}
	l := *best
	return &l
}

// nameNumber is a short name's number, 12 for e12.
func nameNumber(name string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(name, "e"))
	return n
}

// spanSearch is what the span check found: the spans in no evidence read,
// how many items were read only to the per-item bound, and how many were
// not read whole once the total was spent, of how many there were; and,
// for each span unseen, the first line of each item read sharing the most
// of its words, of the items sharing the most.
type spanSearch struct {
	unseen          map[string]bool
	closest         map[string][]EvidenceLine
	cut, unread, of int
}

// why is a span flag's reason, saying what of the evidence went unread.
func (f spanSearch) why(b spanReads) string {
	w := "in no evidence this conversation saw"
	if f.cut > 0 {
		w += fmt.Sprintf(", %s of it read only to the first %s", text.Plural(f.cut, "item"), sizeWords(b.Item))
	}
	if f.unread > 0 {
		w += fmt.Sprintf("; the %s the check reads ran out before %d of %s were read whole", sizeWords(b.Total), f.unread, text.Plural(f.of, "item"))
	}
	return w
}

// sizeWords is a size in MiB where it is whole MiB, else in bytes.
func sizeWords(n int) string {
	if n > 0 && n%(1<<20) == 0 {
		return strconv.Itoa(n>>20) + " MiB"
	}
	return text.Plural(n, "byte")
}

// unseenSpans looks for each span in the lines of the evidence the
// conversation was shown (exposure) — an item carried whole, or named
// where the exposure is unknown, read whole, any other only at the lines
// carried, the rest of it read as empty lines — but what the engineer's
// rules leave out of what models read, in name order, each item read to
// its first b.Item bytes and b.Total over all, until every span is found
// or the total is spent; for each span still unseen after an item, the
// shown lines closest to it are found as it is read (nearest). An item is
// counted as read only to the per-item bound where a line it was shown
// lies past it.
func (s *Session) unseenSpans(ctx context.Context, spans []string, x exposure, b spanReads) (spanSearch, error) {
	found := spanSearch{unseen: map[string]bool{}, closest: map[string][]EvidenceLine{}}
	for _, sp := range spans {
		found.unseen[sp] = true
	}
	if len(spans) == 0 {
		return found, nil
	}
	left := withdrawn(s.c)
	var ids, names, idNames []string
	for n := range x.shown {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if !shortName.MatchString(n) {
			continue
		}
		id, err := evidenceByName(s.c, n)
		if errors.Is(err, errNoEvidence) {
			// A name the conversation wrote that names nothing.
			continue
		}
		if err != nil {
			return spanSearch{}, err
		}
		if !left(id) {
			ids, idNames = append(ids, id), append(idNames, n)
		}
	}
	found.of = len(ids)
	near := newNearest(spans)
	remaining := b.Total
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return spanSearch{}, err
		}
		if remaining <= 0 {
			found.unread = len(ids) - i
			break
		}
		r, err := s.c.ev.Open(id)
		if err != nil {
			return spanSearch{}, err
		}
		limit := min(b.Item, remaining)
		data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
		if cerr := r.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return spanSearch{}, fmt.Errorf("reading %s: %w", id, err)
		}
		cut := len(data) > limit
		if cut {
			data = data[:limit]
		}
		remaining -= len(data)
		past := cut
		if !x.whole(idNames[i]) {
			data, past = shownOnly(data, x.lines[idNames[i]])
			past = cut && past
		}
		if past {
			if limit < b.Item {
				// The total ran out within this item.
				found.unread = len(ids) - i
			} else {
				found.cut++
			}
		}
		var lower [][]byte
		for sp := range found.unseen {
			if bytes.Contains(data, []byte(sp)) {
				delete(found.unseen, sp)
				continue
			}
			if lower == nil {
				lower = bytes.Split(bytes.ToLower(data), []byte("\n"))
			}
			if spanReworded(lower, sp) {
				delete(found.unseen, sp)
			}
		}
		if len(found.unseen) > 0 {
			if cut && len(lower) > 0 {
				// A line the read cut short is no line as stored.
				lower = lower[:len(lower)-1]
			}
			near.read(idNames[i], data, lower, found.unseen)
		}
		if len(found.unseen) == 0 || found.unread > 0 {
			break
		}
	}
	for k, sp := range spans {
		if found.unseen[sp] && len(near.best[k]) > 0 {
			found.closest[sp] = near.best[k]
		}
	}
	return found, nil
}

// shownOnly is data, an item's stored bytes from its first line, with
// every line outside reads emptied, its newline kept, so what is searched
// in it is only what was shown, at its stored number; and whether a line
// of reads lies past those data holds whole.
func shownOnly(data []byte, reads []carriedRead) ([]byte, bool) {
	sorted := slices.SortedFunc(slices.Values(reads), func(a, b carriedRead) int { return a.from - b.from })
	out := make([]byte, 0, len(data))
	n, k := 1, 0
	for rest := data; len(rest) > 0; n++ {
		line, after, nl := bytes.Cut(rest, []byte("\n"))
		rest = after
		for k < len(sorted) && sorted[k].to < n {
			k++
		}
		if k < len(sorted) && sorted[k].from <= n {
			out = append(out, line...)
		}
		if !nl {
			// A line the read cut short, n, is not held whole.
			break
		}
		out = append(out, '\n')
	}
	past := slices.ContainsFunc(reads, func(r carriedRead) bool { return r.to >= n })
	return out, past
}

// nearest finds the lines closest to spans as the span search reads each
// item: in each item, the first line sharing the most of a span's words,
// as spanWords makes them, each word counted once, and at least half of
// them, rounded up (need), so one word in common, such as a node's name,
// is no closeness; of the items, those whose line shares the most of all.
// A line's words are read once for every span.
type nearest struct {
	spans []string
	// ids number the spans' words, and of are the spans holding each; need
	// is, by span, the fewest of its words a close line shares.
	ids  map[string]int
	of   [][]int
	need []int
	// seen is, by word, the last line counting it; counted is, by span,
	// the line count is of; touched the spans the line counts for. Lines
	// are numbered by n over every item.
	seen, counted, count, touched []int
	n                             int
	// inItem is, by span, the most words a line of the item shares and
	// line that line's index; most the most of every item, best the lines
	// sharing it, an item's first each.
	inItem, line, most []int
	best               [][]EvidenceLine
}

func newNearest(spans []string) *nearest {
	nr := &nearest{spans: spans, ids: map[string]int{}, need: make([]int, len(spans)), counted: make([]int, len(spans)), count: make([]int, len(spans)),
		inItem: make([]int, len(spans)), line: make([]int, len(spans)), most: make([]int, len(spans)), best: make([][]EvidenceLine, len(spans))}
	for k, sp := range spans {
		distinct := 0
		for _, w := range spanWords([]byte(strings.ToLower(sp))) {
			id, ok := nr.ids[w]
			if !ok {
				id = len(nr.of)
				nr.ids[w] = id
				nr.of = append(nr.of, nil)
			}
			if !slices.Contains(nr.of[id], k) {
				nr.of[id] = append(nr.of[id], k)
				distinct++
			}
		}
		nr.need[k] = (distinct + 1) / 2
	}
	nr.seen = make([]int, len(nr.of))
	return nr
}

// read takes the item name's lines, data as read and lower its whole
// lines lowercased, for the spans unseen.
func (nr *nearest) read(name string, data []byte, lower [][]byte, unseen map[string]bool) {
	if len(nr.of) == 0 {
		return
	}
	clear(nr.inItem)
	for li, line := range lower {
		nr.n++
		nr.touched = nr.touched[:0]
		eachWord(line, func(w []byte) {
			id, ok := nr.ids[string(w)]
			if !ok || nr.seen[id] == nr.n {
				return
			}
			nr.seen[id] = nr.n
			for _, k := range nr.of[id] {
				if nr.counted[k] != nr.n {
					nr.counted[k], nr.count[k] = nr.n, 0
					nr.touched = append(nr.touched, k)
				}
				nr.count[k]++
			}
		})
		for _, k := range nr.touched {
			if nr.count[k] > nr.inItem[k] {
				nr.inItem[k], nr.line[k] = nr.count[k], li
			}
		}
	}
	var lines [][]byte
	for k, sp := range nr.spans {
		if nr.inItem[k] == 0 || nr.inItem[k] < nr.need[k] || nr.inItem[k] < nr.most[k] || !unseen[sp] {
			continue
		}
		if lines == nil {
			lines = bytes.Split(data, []byte("\n"))
		}
		if nr.line[k] >= len(lines) {
			continue
		}
		if nr.inItem[k] > nr.most[k] {
			nr.most[k], nr.best[k] = nr.inItem[k], nil
		}
		nr.best[k] = append(nr.best[k], EvidenceLine{Name: name, Line: nr.line[k] + 1, Text: storedLine(lines[nr.line[k]])})
	}
}

// storedLine is a line as an EvidenceLine keeps it: as stored, its
// carriage return dropped, cut at a character's start to closestLineBytes
// and marked so.
func storedLine(b []byte) string {
	b = bytes.TrimSuffix(b, []byte("\r"))
	if len(b) <= closestLineBytes {
		return string(b)
	}
	cut := closestLineBytes
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]) + "…"
}

// spanReworded says whether a span not found byte for byte is found once
// the model's rewording is undone, on one of lines, lowercased: its words
// in order and adjacent on one line, case, spacing and punctuation aside
// and a number's digit grouping dropped, so `MemTotal: 1,048,576 kB`
// finds "MemTotal:    1048576 kB" and `policy: single-numa-node` finds
// `"policy": "single-numa-node"`; an ellipsis marks a cut, the word before
// it a prefix and the word after it a suffix. A different value or key is
// still not found.
func spanReworded(lines [][]byte, span string) bool {
	trimmed := strings.ReplaceAll(strings.TrimSpace(span), "…", "...")
	var segs [][]string
	for _, part := range strings.Split(trimmed, "...") {
		if w := spanWords([]byte(strings.ToLower(part))); len(w) > 0 {
			segs = append(segs, w)
		}
	}
	if len(segs) == 0 {
		return false
	}
	cutFirst, cutLast := strings.HasPrefix(trimmed, "..."), strings.HasSuffix(trimmed, "...")
	// A line is read only where one of the span's words is in it as
	// written: its longest word that is not a number, which no digit
	// grouping changes, or where it has none its longest number, so a
	// number the evidence groups and the span does not stays unfound.
	var key, num string
	for _, seg := range segs {
		for _, w := range seg {
			numeric := !strings.ContainsFunc(w, func(r rune) bool { return r < '0' || r > '9' })
			if numeric && len(w) > len(num) {
				num = w
			}
			if !numeric && len(w) > len(key) {
				key = w
			}
		}
	}
	if key == "" {
		key = num
	}
	needle := []byte(key)
	for _, line := range lines {
		if !bytes.Contains(line, needle) {
			continue
		}
		lw := spanWords(line)
		at, ok := 0, true
		for i, seg := range segs {
			at, ok = wordsAt(lw, seg, at, i > 0 || cutFirst, i < len(segs)-1 || cutLast)
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// wordsAt finds seg as adjacent words of lw at index from or after, its
// first word matched as a suffix where suffix and its last as a prefix
// where prefix, and returns the index after it.
func wordsAt(lw, seg []string, from int, suffix, prefix bool) (int, bool) {
	same := func(j int, got string) bool {
		w := seg[j]
		return got == w || j == 0 && suffix && strings.HasSuffix(got, w) || j == len(seg)-1 && prefix && strings.HasPrefix(got, w)
	}
	for i := from; i+len(seg) <= len(lw); i++ {
		ok := true
		for j := range seg {
			if !same(j, lw[i+j]) {
				ok = false
				break
			}
		}
		if ok {
			return i + len(seg), true
		}
	}
	return 0, false
}

// spanWords are the words of lowercased text: runs of letters, digits,
// underscores and non-ASCII, a number's digit grouping (1,048,576) joined.
func spanWords(b []byte) []string {
	var out []string
	eachWord(b, func(w []byte) { out = append(out, string(w)) })
	return out
}

// eachWord calls fn with each word of lowercased b as spanWords makes
// them, in order: a slice of b, or of a buffer reused where a digit
// grouping is joined, valid until fn returns.
func eachWord(b []byte, fn func(w []byte)) {
	isWord := func(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || isDigit(c) || c >= 0x80 }
	var joined []byte
	for i := 0; i < len(b); {
		if !isWord(b[i]) {
			i++
			continue
		}
		start := i
		for i < len(b) && isWord(b[i]) {
			i++
		}
		w := b[start:i]
		if i < len(b) && b[i] == ',' && isDigit(b[i-1]) && grouped(b[i+1:]) {
			joined = append(joined[:0], w...)
			for i < len(b) {
				switch {
				case isWord(b[i]):
					joined = append(joined, b[i])
					i++
					continue
				case b[i] == ',' && isDigit(joined[len(joined)-1]) && grouped(b[i+1:]):
					// A grouping comma: a digit before it, exactly three after.
					i++
					continue
				}
				break
			}
			w = joined
		}
		fn(w)
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// grouped says whether b begins with exactly three digits.
func grouped(b []byte) bool {
	return len(b) >= 3 && isDigit(b[0]) && isDigit(b[1]) && isDigit(b[2]) && (len(b) == 3 || !isDigit(b[3]))
}

// checked is a text as the deterministic checks see it: its statements,
// its flags, its unresolved citations and, where earlier answers are
// given, its candidates for the editor.
func (s *Session) checked(ctx context.Context, t string, earlier []answerAt) (Checked, error) {
	c := Checked{Statements: statementsOf(t)}
	flags, err := s.AnswerFlags(ctx, []string{t})
	if err != nil {
		return Checked{}, err
	}
	c.Flags = flags[0]
	var cites []Cite
	for _, st := range c.Statements {
		cites = append(cites, st.Cites...)
	}
	checks, err := s.CheckCites(ctx, cites)
	if err != nil {
		return Checked{}, err
	}
	for i, chk := range checks {
		if !chk.OK && !slices.Contains(c.Unresolved, cites[i].Text+": "+chk.Why) {
			c.Unresolved = append(c.Unresolved, cites[i].Text+": "+chk.Why)
		}
	}
	kinds := map[string]string{}
	for i, chk := range checks {
		if chk.OK && cites[i].Name != "" {
			if _, ok := kinds[cites[i].Name]; !ok {
				it, err := s.c.ev.Describe(chk.EvidenceID)
				if err != nil {
					return Checked{}, err
				}
				kinds[cites[i].Name] = EvidenceKind(it)
			}
		}
	}
	for _, st := range c.Statements {
		if cand, ok := listingOnly(st, kinds); ok {
			c.Candidates = append(c.Candidates, cand)
		}
		if !citesSource(st.Cites) && check.Specific(st.Text) && !strings.HasSuffix(st.Text, "?") {
			c.Candidates = append(c.Candidates, CheckCandidate{Statement: st.ID, Why: CandidateUncited})
		}
		if cand, ok := hedgeDropped(st, earlier); ok {
			c.Candidates = append(c.Candidates, cand)
		}
		if terms := check.CertaintyTerms(st.Text); len(terms) > 0 {
			c.Candidates = append(c.Candidates, CheckCandidate{Statement: st.ID, Why: CandidateCertainty, Terms: terms})
		}
	}
	return c, nil
}

// listingOnly is the candidate for a statement whose every citation of a
// source names a file listing.
func listingOnly(st CheckStatement, kinds map[string]string) (CheckCandidate, bool) {
	var names []string
	for _, c := range st.Cites {
		if c.Ledger != "" {
			continue
		}
		if c.Name == "" || kinds[c.Name] != KindListing {
			return CheckCandidate{}, false
		}
		if !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
	}
	if len(names) == 0 {
		return CheckCandidate{}, false
	}
	return CheckCandidate{Statement: st.ID, Why: CandidateListing, Listings: names}, true
}

// hedgeDropped is the candidate for a statement with no qualifier that
// cites lines an earlier statement cited with one: the latest such
// earlier statement.
func hedgeDropped(st CheckStatement, earlier []answerAt) (CheckCandidate, bool) {
	if len(check.Hedges(st.Text)) > 0 {
		return CheckCandidate{}, false
	}
	for i := len(earlier) - 1; i >= 0; i-- {
		for _, e := range statementsOf(earlier[i].text) {
			h := check.Hedges(e.Text)
			if len(h) == 0 || !citesOverlap(st.Cites, e.Cites) {
				continue
			}
			return CheckCandidate{Statement: st.ID, Why: CandidateHedge, Earlier: e.Text, EarlierAnswer: earlier[i].label, Hedges: h}, true
		}
	}
	return CheckCandidate{}, false
}

// citesOverlap reports whether two statements cite lines of one item in
// common, a citation of the whole item meeting any of its lines.
func citesOverlap(a, b []Cite) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Name == "" || x.Name != y.Name {
				continue
			}
			if x.From == 0 || y.From == 0 || x.From <= max(y.To, y.From) && y.From <= max(x.To, x.From) {
				return true
			}
		}
	}
	return false
}

// answerAt is one of the model's answers in the conversation, named as the
// chat names it, and whether it answers the engine's ask for a draft.
type answerAt struct {
	label, text string
	draft       bool
}

// answersOf are the model's answers in a conversation's history, in
// order, labelled as the chat labels them, each as the engineer was shown
// it (AnswerViews), a draft said so by the ask it answers (draftWords).
func answersOf(history []ConversationMessage) []answerAt {
	views := AnswerViews(history)
	out := make([]answerAt, len(views))
	for i, v := range views {
		out[i] = answerAt{label: v.Label, text: v.Text}
		for j := v.Original - 1; j >= 0; j-- {
			if m := history[j]; m.Kind == MessageEngineer || m.Kind == MessageEngine {
				out[i].draft = m.Kind == MessageEngine && strings.HasPrefix(m.Text, draftLead)
				break
			}
		}
	}
	return out
}

// writtenFrom are the citations of the answers a text at index at was
// written from — those before it since the last draft, as isDraft tells
// one — each once and in order, and those answers' labels.
func writtenFrom(answers []answerAt, at int, isDraft func(answerAt) bool) ([]Cite, []string) {
	from := 0
	for j := at - 1; j >= 0; j-- {
		if isDraft(answers[j]) {
			from = j + 1
			break
		}
	}
	var cites []Cite
	var labels []string
	for _, a := range answers[from:at] {
		for _, st := range statementsOf(a.text) {
			for _, ct := range st.Cites {
				if ct.Name != "" && !slices.ContainsFunc(cites, func(x Cite) bool { return x.Name == ct.Name && x.From == ct.From && x.To == ct.To }) {
					cites = append(cites, ct)
				}
			}
		}
		labels = append(labels, a.label)
	}
	return cites, labels
}

// errNothingToCheck says a conversation has no answer to check yet.
var errNothingToCheck = errors.New("there is no answer to check yet")

// EditorRequest is the message the editor is sent about the
// conversation's last answer, as the engineer was shown it.
func (s *Session) EditorRequest(ctx context.Context) (CheckRequest, error) {
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return CheckRequest{}, err
	}
	answers := answersOf(history)
	if len(answers) == 0 {
		return CheckRequest{}, errNothingToCheck
	}
	return s.editorRequest(ctx, answers[len(answers)-1], answers[:len(answers)-1])
}

// editorRequest is the message the editor is sent about answer: the
// answer as written, which its corrected text keeps but where it corrects;
// its statements; what the deterministic checks found — with each dropped
// qualifier's statement among the earlier answers — and the evidence
// items its citations name, quoted whole where they fit (checkRequest),
// within editorRequestBytes. A draft with a statement citing nothing is
// also given, after its own, the items cited by the answers it was
// written from, as the reviewer is, so the editor can cite the lines that
// hold such a statement rather than leave it to a revision that reads no
// evidence. The conversation itself is not in it. Its context holds the
// answer, so a citation the editor corrects is checked.
func (s *Session) editorRequest(ctx context.Context, answer answerAt, earlier []answerAt) (CheckRequest, error) {
	c, err := s.checked(ctx, answer.text, earlier)
	if err != nil {
		return CheckRequest{}, err
	}
	cites := citesOf(c.Statements)
	source := ""
	if answer.draft && slices.ContainsFunc(c.Statements, func(st CheckStatement) bool { return !citesSource(st.Cites) }) {
		from, labels := writtenFrom(earlier, len(earlier), func(a answerAt) bool { return a.draft })
		added := 0
		for _, ct := range from {
			if !slices.ContainsFunc(cites, func(x Cite) bool { return x.Name == ct.Name && x.From == ct.From && x.To == ct.To }) {
				cites = append(cites, ct)
				added++
			}
		}
		if added > 0 {
			source = "It is a draft, and a statement it leaves uncited may rest on the items below its own: those cited by the answers it was written from, " + strings.Join(labels, ", ") + ", quoted without those answers' text. Where their lines hold such a statement, cite them in corrected rather than anchor it.\n"
		}
	}
	head := "# Check this answer\n\n" +
		"The companion's answer " + answer.label + " as written; then split into statements by the engine; then what the engine's own checks found; then the evidence items its citations name, quoted whole from the stored evidence under their line numbers, the lines cited marked " + strings.TrimSpace(citedMark) + ". Judge each statement against the whole items quoted here, correcting a citation where other quoted lines hold the claim, and call " + check.ToolName + " once.\n\n" +
		"## The answer as written\n\n" + strings.TrimSpace(answer.text) + "\n"
	req, err := s.checkRequest(ctx, check.Editor, head, c, cites, source, editorRequestBytes)
	req.Context.Answer = answer.text
	return req, err
}

// ReviewAsk is what the engineer asks reviewed: Item, an item's name such
// as i3, or the conversation's last answer as shown where it is empty;
// and, where Ranked, the evidence the conversation used most that the
// text does not cite given too (uncitedRanked), an experiment the
// engineer opts into, never the default.
type ReviewAsk struct {
	Item   string
	Ranked bool
}

// ReviewRequest is the message the reviewer would be sent about what ask
// names: its statements, what the deterministic checks found, and the
// evidence items its citations name, quoted as the editor's are, within
// reviewerRequestBytes. A text citing no evidence, such as a customer
// draft, is given instead the items cited by the answers it was written
// from: those since the previous draft. No answer's text but the one
// reviewed is in it. Where ask is Ranked, the items quoted are bounded to
// leave room for the ranked evidence the text does not cite, which
// follows them.
func (s *Session) ReviewRequest(ctx context.Context, ask ReviewAsk) (CheckRequest, error) {
	req, _, err := s.reviewRequest(ctx, ask)
	return req, err
}

// reviewed is what a review is of: its name as the chat gives it, a7 or
// i3, and the model call that wrote the answer shown, where it is an
// answer.
type reviewed struct {
	name, invocation string
}

// reviewRequest is ReviewRequest with what it is of.
func (s *Session) reviewRequest(ctx context.Context, ask ReviewAsk) (CheckRequest, reviewed, error) {
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return CheckRequest{}, reviewed{}, err
	}
	answers := answersOf(history)
	items, err := s.Items(ctx)
	if err != nil {
		return CheckRequest{}, reviewed{}, err
	}
	var what, body string
	var of reviewed
	at := len(answers)
	switch item := ask.Item; item {
	case "":
		if len(answers) == 0 {
			return CheckRequest{}, reviewed{}, errNothingToCheck
		}
		at--
		what, body = "the companion's answer "+answers[at].label, answers[at].text
		// The call that wrote what was shown: the revision's, else the
		// answer's as the model first wrote it.
		v := AnswerViews(history)[at]
		from := history[v.Original]
		if history[v.At].Kind == MessageModel {
			from = history[v.At]
		}
		of = reviewed{name: answers[at].label, invocation: from.Invocation}
	default:
		i := slices.IndexFunc(items, func(it CompanionItem) bool { return it.Name() == item })
		if i < 0 {
			return CheckRequest{}, reviewed{}, fmt.Errorf("no item %s", item)
		}
		of = reviewed{name: item}
		it := items[i]
		what, body = "the pinned answer "+item, it.Text
		if it.Kind == ItemDraft {
			what = "the draft " + item + " for " + it.Intention + ", for " + audienceWords(it.Audience, s.SupportName())
		}
		if j := lastIndexOfText(answers, it.Text); j >= 0 {
			at = j
		}
	}
	c, err := s.checked(ctx, body, nil)
	if err != nil {
		return CheckRequest{}, reviewed{}, err
	}
	cites := citesOf(c.Statements)
	source := ""
	if len(cites) == 0 {
		var labels []string
		cites, labels = writtenFrom(answers, at, func(a answerAt) bool {
			return slices.ContainsFunc(items, func(it CompanionItem) bool { return it.Kind == ItemDraft && it.Text == a.text })
		})
		source = "It cites no evidence; the items below are those cited by the answers it was written from"
		if len(labels) > 0 {
			source += ", " + strings.Join(labels, ", ")
		}
		source += ", quoted without those answers' text.\n"
	}
	given, judge := ". You are not given the conversation.", ""
	var ranked string
	var carried []carriedItem
	limit := reviewerRequestBytes
	if ask.Ranked {
		if ranked, carried, err = s.uncitedRanked(ctx, history, cites); err != nil {
			return CheckRequest{}, reviewed{}, err
		}
		limit -= len(ranked)
		given = "; then the evidence the conversation used most that the text does not cite, as the companion was shown it. You are not given the conversation but for that."
		judge = "; an item disclosed to the companion and not cited is not the text's support, but where it contradicts a statement or holds its alternative, say so with its lines"
	}
	head := "# Review this text\n\n" +
		"The text is " + what + ", split into statements by the engine; then what the engine's own checks found; then the evidence items its citations name, quoted whole from the stored evidence under their line numbers, the lines cited marked " + strings.TrimSpace(citedMark) + given + " Judge each statement only against the items quoted here" + judge + ", and call " + check.ToolName + " once.\n"
	req, err := s.checkRequest(ctx, check.Reviewer, head, c, cites, source, limit)
	if err != nil || !ask.Ranked {
		return req, of, err
	}
	req.Text += ranked
	for _, ci := range carried {
		if len(ci.shown) == 0 {
			continue
		}
		for _, q := range ci.shown {
			req.Context.Quoted[ci.name] = append(req.Context.Quoted[ci.name], check.Lines{From: q.From, To: q.To})
		}
		req.Context.Items[ci.name] = ci.lines
		if ci.kind == KindListing {
			req.Context.Listings[ci.name] = true
		}
	}
	return req, of, nil
}

// uncitedHeading heads the ranked evidence a review given it carries.
const uncitedHeading = "## Disclosed to the companion, not cited"

const uncitedIntro = uncitedHeading + `

The evidence the conversation used most that the text does not cite, best first, as the engine ranks it from what was done with each — lines the engineer pointed at, cited in a pinned item or draft, cited in an answer, returned by a tool — and how recently: each item the companion was shown, with the lines cited or pointed at, numbered as stored, or its first lines where none was named. None is the text's support.
`

// uncitedRanked is the ranked evidence a review given it carries: the top
// of the conversation's ranked evidence (rankHistory), of the items
// disclosed to the companion that cites does not name, bounded as a
// restart carries it (carryRanked), each item headed as disclosed to the
// companion and not cited by the text; and the items written. Where there
// is none, it says so.
func (s *Session) uncitedRanked(ctx context.Context, history []ConversationMessage, cites []Cite) (string, []carriedItem, error) {
	ranked, err := s.rankHistory(ctx, history, 0)
	if err != nil {
		return "", nil, err
	}
	x, err := s.exposure(ctx)
	if err != nil {
		return "", nil, err
	}
	ranked = slices.DeleteFunc(ranked, func(r RankedEvidence) bool {
		return !x.shown[r.Name] || slices.ContainsFunc(cites, func(c Cite) bool { return c.Name == r.Name })
	})
	body, items, err := s.carryRanked(ctx, ranked, rankedCarry{
		head: func(r RankedEvidence, it EvidenceItem) string {
			return fmt.Sprintf("\n### %s · %s · %s\n[disclosed to the companion, not cited by the text; %s]\n", r.Name, EvidenceKind(it), strings.Join(strings.Fields(ProducedBy(it)), " "), strings.Join(r.Why, ", "))
		},
		cut: "[cut here at the bound on what is carried, %s's lines from %d not given]\n",
	})
	if err != nil {
		return "", nil, err
	}
	if len(items) == 0 {
		body = "\nNone: the conversation used no evidence the text does not cite.\n"
	}
	return "\n" + uncitedIntro + body, items, nil
}

// lastIndexOfText is the last of answers whose text is t, or -1.
func lastIndexOfText(answers []answerAt, t string) int {
	for j := len(answers) - 1; j >= 0; j-- {
		if answers[j].text == t {
			return j
		}
	}
	return -1
}

func audienceWords(a, supportName string) string {
	if a == "internal" {
		return supportName + " only"
	}
	return "the customer"
}

// citesOf are statements' citations of evidence, in order, each once.
func citesOf(ss []CheckStatement) []Cite {
	var out []Cite
	for _, st := range ss {
		for _, c := range st.Cites {
			if c.Name != "" && !slices.ContainsFunc(out, func(x Cite) bool { return x.Name == c.Name && x.From == c.From && x.To == c.To }) {
				out = append(out, c)
			}
		}
	}
	return out
}

// checkRequest writes a check's message: head, the statements, what the
// engine found, then source, which introduces the quoted evidence where
// it is not the text's own, and each evidence item cites name, under its
// heading and line numbers, the lines cited marked: whole where it holds
// at most quoteItemBytes and fits what the message has left, else the
// lines cited with quoteContextLines either side, said so (QuotedItem); a
// whole item's citation is said to be valid. The items past limit bytes
// are counted in one line; only the text itself and the engine's notes
// can pass it. It returns the context the findings are validated
// against, which holds the lines quoted and how to read citations.
func (s *Session) checkRequest(ctx context.Context, role check.Role, head string, c Checked, cites []Cite, source string, limit int) (CheckRequest, error) {
	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n## Statements\n\n")
	for _, st := range c.Statements {
		fmt.Fprintf(&b, "%s: %s\n", st.ID, st.Text)
	}
	b.WriteString("\n## What the engine found\n\n")
	notes := 0
	note := func(format string, a ...any) {
		fmt.Fprintf(&b, "- "+format+"\n", a...)
		notes++
	}
	for _, cand := range c.Candidates {
		switch cand.Why {
		case CandidateListing:
			note("%s cites only file listings, which name files and say nothing of what ran: %s.", cand.Statement, strings.Join(cand.Listings, ", "))
		case CandidateUncited:
			note("%s %s.", cand.Statement, CandidateUncited)
		case CandidateHedge:
			note("%s cites lines that %s cited with %q, which %s no longer has. %s: %q", cand.Statement, cand.EarlierAnswer, strings.Join(cand.Hedges, ", "), cand.Statement, cand.EarlierAnswer, cand.Earlier)
		case CandidateCertainty:
			quoted := make([]string, len(cand.Terms))
			for i, w := range cand.Terms {
				quoted[i] = strconv.Quote(w)
			}
			note("%s holds %s: whether in its own voice, and stronger than the lines and their attribution allow, is for you to judge.", cand.Statement, strings.Join(quoted, ", "))
		}
	}
	for _, f := range c.Flags {
		note("%s is %s.", f.What, f.Why)
	}
	for _, u := range c.Unresolved {
		note("%s", u)
	}
	if notes == 0 {
		b.WriteString("Nothing.\n")
	}
	b.WriteString("\n## Quoted evidence\n\n")
	if source != "" {
		b.WriteString(source + "\n")
	}
	items, err := s.citedItems(ctx, cites)
	if err != nil {
		return CheckRequest{}, err
	}
	vc := check.Context{Role: role, Quoted: map[string][]check.Lines{}, Items: map[string]int{}, Listings: map[string]bool{}, ListingOnly: map[string]bool{}, Cites: checkCites}
	for _, st := range c.Statements {
		vc.Statements = append(vc.Statements, st.Statement)
	}
	for _, cand := range c.Candidates {
		if cand.Why == CandidateListing {
			vc.ListingOnly[cand.Statement] = true
		}
	}
	if len(items) == 0 {
		b.WriteString("None: the text cites no evidence.\n")
	}
	room := limit - len(fmt.Sprintf(quotesSpent, len(items)))
	var shown []QuotedItem
	for i, it := range items {
		if it.Cut == "" {
			whole, err := s.quoteWhole(&it, quoteItemBytes)
			if err != nil {
				return CheckRequest{}, err
			}
			switch {
			case !whole:
				it.Fallback = "not quoted whole: it holds more than the " + sizeOf(quoteItemBytes) + " an item is quoted whole to"
			case b.Len()+len(itemQuotation(it)) > room:
				it.Fallback = "not quoted whole: it would pass the " + sizeOf(limit) + " the request is bounded to"
			}
			if it.Fallback != "" {
				if err := s.quoteWindows(&it, quoteContextLines, QuoteBounds{Lines: quoteWindowLines, Bytes: quoteWindowBytes}); err != nil {
					return CheckRequest{}, err
				}
			}
		}
		q := itemQuotation(it)
		if b.Len()+len(q) > room {
			fmt.Fprintf(&b, quotesSpent, len(items)-i)
			break
		}
		b.WriteString(q)
		shown = append(shown, it)
		for _, p := range it.Parts {
			if p.To > 0 {
				vc.Quoted[it.Name] = append(vc.Quoted[it.Name], check.Lines{From: p.From, To: p.To})
				vc.Items[it.Name] = it.Lines
				if it.Kind == KindListing {
					vc.Listings[it.Name] = true
				}
			}
		}
	}
	return CheckRequest{Text: b.String(), Context: vc, Checked: c, Quotes: shown}, nil
}

// checkCites are a text's citations as Cites reads them, as check.Validate
// reads them: a case-ledger item by its ID as a name, so a correction
// citing one names no item the request quoted and is refused.
func checkCites(t string) []check.Cite {
	cs := Cites(t)
	out := make([]check.Cite, len(cs))
	for i, c := range cs {
		out[i] = check.Cite{Name: c.Name + c.Ledger, Path: c.Path, From: c.From, To: c.To}
	}
	return out
}

// sizeOf is a bound in words: whole KiB, else bytes.
func sizeOf(n int) string {
	if n >= 1<<10 && n%(1<<10) == 0 {
		return strconv.Itoa(n>>10) + " KiB"
	}
	return text.Plural(n, "byte")
}

// quotesSpent ends a request whose bytes ran out before its quotations.
const quotesSpent = "[%d more cited items not quoted: the request's bytes are spent]\n"

// citedWhole says an item quoted whole is cited whole, [e12]: a valid
// citation; citedWholeFirst says so of one quoted in windows.
const (
	citedWhole      = "[cited whole, a valid citation: every line of it is cited]\n"
	citedWholeFirst = "[cited whole, a valid citation: its first lines are quoted]\n"
)

// citedMark marks a line cited in a quotation, before its number.
const citedMark = "▸ "

// itemQuotation is an item as a request quotes it: its heading, what is
// quoted of it — whole, or which lines and why — the citations naming it,
// whether lines are marked and which name lines past its end, then its
// lines under their stored numbers, each line cited marked (citedMark),
// each cut said; or, for a citation not quoted, its place and why.
func itemQuotation(it QuotedItem) string {
	var b strings.Builder
	if it.Cut != "" {
		fmt.Fprintf(&b, "### %s\n[%s]\n\n", citeLabel(it.Cites[0]), it.Cut)
		return b.String()
	}
	fmt.Fprintf(&b, "### %s", it.Name)
	if it.Kind != "" {
		fmt.Fprintf(&b, " · %s · %s", it.Kind, strings.Join(strings.Fields(it.By), " "))
	}
	cited := func(n int) bool {
		return slices.ContainsFunc(it.Cites, func(c Cite) bool { return c.From > 0 && c.From <= n && n <= max(c.To, c.From) })
	}
	var labels, past, spans []string
	whole, marked := false, false
	for _, c := range it.Cites {
		l := citeLabel(c)
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
			if max(c.From, c.To) > it.Lines {
				past = append(past, l)
			}
		}
		whole = whole || c.From == 0
	}
	last := 0
	for _, p := range it.Parts {
		last = max(last, p.To)
		spans = append(spans, lineSpan(p.From, p.To))
		for n := p.From; n <= p.To && !marked; n++ {
			marked = cited(n)
		}
	}
	what := "quoted whole, " + lineSpan(1, it.Lines)
	switch {
	case it.Whole:
	case len(spans) == 0:
		what = it.Fallback + "; no line quoted"
	default:
		what = it.Fallback + "; " + strings.Join(spans, ", ") + " quoted, the lines cited with up to " + text.Plural(quoteContextLines, "line") + " either side"
	}
	fmt.Fprintf(&b, "\n[%s; cited as %s", what, strings.Join(labels, ", "))
	if marked {
		b.WriteString("; " + strings.TrimSpace(citedMark) + " marks the lines cited")
	}
	if len(past) > 0 {
		fmt.Fprintf(&b, "; %s past its %s", strings.Join(past, ", "), text.Plural(it.Lines, "line"))
	}
	b.WriteString("]\n")
	switch {
	case whole && it.Whole:
		b.WriteString(citedWhole)
	case whole:
		b.WriteString(citedWholeFirst)
	}
	width := len(strconv.Itoa(last))
	for i, p := range it.Parts {
		if i > 0 {
			b.WriteString("…\n")
		}
		writeNumbered(&b, p.Text, p.From, width, cited)
		if p.Cut != "" {
			fmt.Fprintf(&b, "[%s]\n", p.Cut)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// citeLabel is a citation as one place, e12:41-58, e12:41 or e12.
func citeLabel(c Cite) string {
	s := c.Name + c.Path
	switch {
	case c.From > 0 && c.To > 0:
		return s + ":" + strconv.Itoa(c.From) + "-" + strconv.Itoa(c.To)
	case c.From > 0:
		return s + ":" + strconv.Itoa(c.From)
	}
	return s
}

// writeNumbered writes quoted lines, the first numbered from, under their
// stored numbers right-aligned to width, as an excerpt shows them, each
// line cited marked; a last line cut short ends the quotation.
func writeNumbered(b *strings.Builder, quoted string, from, width int, cited func(int) bool) {
	lines := strings.SplitAfter(quoted, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for i, l := range lines {
		mark := "  "
		if cited(from + i) {
			mark = citedMark
		}
		fmt.Fprintf(b, "%s%*d  %s\n", mark, width, from+i, strings.TrimSuffix(l, "\n"))
	}
}

package engine

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/evidence"
)

// QuoteBounds bound what Quote quotes: Lines and Bytes one citation's,
// Total every citation's together.
type QuoteBounds struct {
	Lines, Bytes, Total int
}

// Quoted is one citation's lines, quoted from the stored evidence it
// names: what the epistemic checks' requests carry and what the chat's
// citation footnotes are to show, so the engineer can read exactly what a
// check was shown.
type Quoted struct {
	Cite Cite
	// EvidenceID is the item the citation names; By what produced it, and
	// Kind what kind of evidence that makes it (EvidenceKind).
	EvidenceID, By, Kind string
	// From and To are the lines quoted, To 0 where none was.
	From, To int
	// Text is exactly the stored bytes of lines From to To, each line's
	// newline included, but where Cut says the last was cut short.
	Text string
	// Cut says why fewer lines were quoted than the citation names, or
	// why none was; empty where every line named is quoted.
	Cut string
}

// Kinds of evidence, by what made it (EvidenceKind).
const (
	KindListing   = "listing"
	KindSearch    = "search"
	KindRead      = "read"
	KindTool      = "catalog tool"
	KindCommand   = "command"
	KindComment   = "case comment"
	KindCaseData  = "case data"
	KindReference = "reference"
	KindEngineer  = "the engineer's"
)

// EvidenceKind is what kind of evidence an item is, by what made it: a
// listing — find, archive_list — names files and says nothing of what ran;
// a search, a read, a catalog tool's or a command's output; a case
// comment or the case's other data; a reference; or what the engineer
// brought in.
func EvidenceKind(it EvidenceItem) string {
	switch {
	case it.Collection != nil && it.Collection.What.TestID != "":
		return KindTool
	case it.Collection != nil:
		stages := it.Collection.What.Pipeline
		if len(stages) == 0 {
			return KindRead
		}
		if len(stages[0].Argv) > 0 {
			return KindCommand
		}
		listing := true
		for _, st := range stages {
			listing = listing && (st.Primitive == "find" || st.Primitive == "archive.list")
		}
		switch {
		case listing:
			return KindListing
		case stages[len(stages)-1].Primitive == "grep" || stages[len(stages)-1].Primitive == "logs.count":
			return KindSearch
		}
		return KindRead
	}
	switch it.Ref.Type {
	case TypeCaseComment:
		return KindComment
	case evidence.TypeReferenceSearch, evidence.TypeReferenceDocument:
		return KindReference
	case evidence.TypeIngested:
		return KindEngineer
	}
	return KindCaseData
}

// Quote quotes each citation's lines from the stored evidence it names, in
// the citations' order, within bounds: at most b.Lines lines and b.Bytes
// bytes a citation, and b.Total bytes in all, each cut said. A citation
// naming no line quotes the item from its first. A citation that does
// not resolve (CheckCites), names an artifact path or a case-ledger item
// rather than stored evidence, or names evidence the engineer's rules
// leave out of what models read quotes nothing, saying why. The same
// citations of the same case quote the same bytes.
func (s *Session) Quote(ctx context.Context, cites []Cite, b QuoteBounds) ([]Quoted, error) {
	x, err := s.exposure(ctx)
	if err != nil {
		return nil, err
	}
	left := withdrawn(s.c)
	out := make([]Quoted, 0, len(cites))
	total := 0
	for _, c := range cites {
		q := Quoted{Cite: c}
		chk, cut := s.quotable(c, x, left, false)
		if cut != "" {
			if chk.OK {
				q.EvidenceID = chk.EvidenceID
			}
			q.Cut = cut
			out = append(out, q)
			continue
		}
		q.EvidenceID = chk.EvidenceID
		it, err := s.c.ev.Describe(chk.EvidenceID)
		if err != nil {
			return nil, err
		}
		q.By, q.Kind = ProducedBy(it), EvidenceKind(it)
		from, to := c.From, max(c.To, c.From)
		if from == 0 {
			from, to = 1, chk.Lines
		}
		if chk.Lines == 0 || to < from {
			q.Cut = noLineCut
			out = append(out, q)
			continue
		}
		room := b.Total - total
		if room <= 0 {
			q.Cut = fmt.Sprintf("not quoted: the %s every quotation shares are spent", text.Plural(b.Total, "byte"))
			out = append(out, q)
			continue
		}
		q.Text, q.To, q.Cut, err = s.quoteSpan(chk.EvidenceID, from, to, b.Lines, min(b.Bytes, room))
		if err != nil {
			return nil, fmt.Errorf("quoting %s: %w", c.Text, err)
		}
		q.From = from
		total += len(q.Text)
		out = append(out, q)
	}
	return out, nil
}

// noLineCut says a citation names no line its item holds.
const noLineCut = "not quoted: it names no line the item holds"

// quotable is the item c names, to be quoted, or why it is not: c names
// an artifact path or a case-ledger item, does not resolve (CheckCites),
// or names evidence the engineer's rules leave out of what models read.
// Where flagged, a citation of an item the conversation saw at lines past
// its end, or outside those it was shown, names the item still.
func (s *Session) quotable(c Cite, x exposure, left func(string) bool, flagged bool) (CiteCheck, string) {
	switch {
	case c.Path != "":
		return CiteCheck{}, "not quoted: an artifact path, and only stored evidence is quoted"
	case c.Ledger != "":
		return CiteCheck{}, "not quoted: a case-ledger item, a model's reading of a comment, and only stored evidence is quoted"
	}
	chk := s.checkCite(c, x)
	if !chk.OK && (!flagged || !x.shown[c.Name] || chk.EvidenceID == "" || chk.Lines == 0) {
		return chk, "not quoted: " + chk.Why
	}
	if left(chk.EvidenceID) {
		return chk, "not quoted: the engineer's rules leave it out of what models read"
	}
	return chk, ""
}

// quoteSpan quotes lines from to to of the item id, numbered from 1, as
// stored, at most lines lines and bytes bytes (quoteLines), and returns
// the last line it reaches and, where it quotes fewer lines than from to
// to, the cut, said.
func (s *Session) quoteSpan(id string, from, to, lines, bytes int) (quoted string, last int, cut string, err error) {
	r, err := s.c.ev.Open(id)
	if err != nil {
		return "", 0, "", err
	}
	quoted, last, partial, err := quoteLines(r, from, min(to, from+lines-1), bytes)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, "", err
	}
	switch {
	case partial:
		cut = fmt.Sprintf("line %d cut at its first %s", last, text.Plural(len(quoted), "byte"))
	case last < to && last == from+lines-1:
		cut = fmt.Sprintf("cut at %s: %s of lines %d–%d quoted", text.Plural(lines, "line"), lineSpan(from, last), from, to)
	case last < to:
		cut = fmt.Sprintf("cut at %s: %s of lines %d–%d quoted", text.Plural(bytes, "byte"), lineSpan(from, last), from, to)
	}
	return quoted, last, cut, nil
}

// QuotedItem is an evidence item as a check's request quotes it, with
// the citations naming it: whole where it fits, else the lines cited with
// lines either side. A citation the request cannot quote is a QuotedItem
// of its own, saying why in Cut.
type QuotedItem struct {
	// Name is the item's short name; EvidenceID, By and Kind are as
	// Quoted's.
	Name, EvidenceID, By, Kind string
	// Lines and Bytes are what the item holds.
	Lines int
	Bytes int64
	// Cites are the citations naming it, in the text's order.
	Cites []Cite
	// Parts are the lines quoted, in line order: one of every line where
	// Whole, else a window about each span cited (quoteWindows).
	Parts []Quoted
	Whole bool
	// Fallback says why the item is not quoted whole; Cut why nothing is
	// quoted of a citation.
	Fallback, Cut string
}

// citedItems groups cites by the evidence item each names, in the order
// each item is first cited, reading none: an item to quote, with the
// citations naming it — one naming lines past its end among them, so the
// item shows where the claim is — or a citation that cannot be quoted,
// alone, saying why as Quote says it.
func (s *Session) citedItems(ctx context.Context, cites []Cite) ([]QuotedItem, error) {
	x, err := s.exposure(ctx)
	if err != nil {
		return nil, err
	}
	left := withdrawn(s.c)
	var out []QuotedItem
	at := map[string]int{}
	for _, c := range cites {
		if i, ok := at[c.Name]; ok && c.Path == "" {
			out[i].Cites = append(out[i].Cites, c)
			continue
		}
		chk, cut := s.quotable(c, x, left, true)
		if cut == "" && chk.Lines == 0 {
			cut = noLineCut
		}
		it := QuotedItem{Name: c.Name, Cites: []Cite{c}, Cut: cut}
		if cut == "" {
			ev, err := s.c.ev.Describe(chk.EvidenceID)
			if err != nil {
				return nil, err
			}
			it.EvidenceID, it.By, it.Kind = chk.EvidenceID, ProducedBy(ev), EvidenceKind(ev)
			it.Lines, it.Bytes = chk.Lines, ev.Size.Bytes
			at[c.Name] = len(out)
		}
		out = append(out, it)
	}
	return out, nil
}

// quoteWhole quotes every line of it where it holds at most bound bytes,
// and says whether it did: not where it holds more, or reads as more.
func (s *Session) quoteWhole(it *QuotedItem, bound int) (bool, error) {
	if it.Bytes > int64(bound) {
		return false, nil
	}
	quoted, last, cut, err := s.quoteSpan(it.EvidenceID, 1, it.Lines, it.Lines, bound)
	if err != nil {
		return false, fmt.Errorf("quoting %s: %w", it.Name, err)
	}
	if cut != "" || last != it.Lines {
		return false, nil
	}
	it.Whole = true
	it.Parts = []Quoted{{Cite: Cite{Name: it.Name, From: 1, To: it.Lines}, EvidenceID: it.EvidenceID, By: it.By, Kind: it.Kind, From: 1, To: last, Text: quoted}}
	return true, nil
}

// quoteWindows quotes of it, in place of the whole item, a window about
// each span cited, in line order: the lines cited first, at most
// b.Lines-2*around lines and b.Bytes bytes, a cut said, then up to around
// lines either side, nearest first, in what bytes are left, so context
// never crowds out a line cited. Spans whose windows meet share one while
// their lines stay within that bound, and no window repeats another's
// lines. A citation of the whole item has its first lines, and one past
// the item's end none.
func (s *Session) quoteWindows(it *QuotedItem, around int, b QuoteBounds) error {
	most := max(1, b.Lines-2*around)
	var spans [][2]int
	for _, c := range it.Cites {
		switch {
		case c.From == 0:
			spans = append(spans, [2]int{1, min(it.Lines, most)})
		case c.From <= it.Lines:
			spans = append(spans, [2]int{c.From, min(it.Lines, max(c.To, c.From))})
		}
	}
	slices.SortFunc(spans, func(x, y [2]int) int { return cmp.Or(cmp.Compare(x[0], y[0]), cmp.Compare(x[1], y[1])) })
	var groups [][2]int
	for _, sp := range spans {
		if n := len(groups); n > 0 && sp[0] <= groups[n-1][1]+2*around+1 && max(groups[n-1][1], sp[1])-groups[n-1][0] < most {
			groups[n-1][1] = max(groups[n-1][1], sp[1])
			continue
		}
		groups = append(groups, sp)
	}
	it.Whole, it.Parts = false, nil
	shown := 0
	for i, g := range groups {
		lo, hi := max(1, g[0]-around, shown+1), min(it.Lines, g[1]+around)
		if i+1 < len(groups) {
			hi = min(hi, groups[i+1][0]-1)
		}
		if g[0] < lo {
			// The lines cited here a window before quoted already.
			g[0] = lo
		}
		if g[0] > g[1] {
			continue
		}
		lines, err := s.readLines(it.EvidenceID, lo, hi, b.Bytes+1)
		if err != nil {
			return fmt.Errorf("quoting %s: %w", it.Name, err)
		}
		p := window(lines, lo, g[0], g[1], most, b.Bytes)
		if p.To == 0 {
			continue
		}
		p.Cite, p.EvidenceID, p.By, p.Kind = Cite{Name: it.Name, From: g[0], To: g[1]}, it.EvidenceID, it.By, it.Kind
		it.Parts = append(it.Parts, p)
		shown = p.To
	}
	return nil
}

// window quotes, of lines read from line lo, lines from to to — at most
// most lines and bytes bytes, whole lines while they fit, the first cut
// on a character's boundary where it alone does not — then the lines
// either side of them, nearest first, while bytes are left, each side
// stopping at the first that does not fit; after them only where every
// line from to to is quoted. Each line read is at most bytes+1 bytes.
func window(lines [][]byte, lo, from, to, most, bytes int) Quoted {
	at := func(n int) []byte { return lines[n-lo] }
	read := lo + len(lines) - 1
	if from > read {
		return Quoted{}
	}
	last, used, q := min(to, from+most-1, read), 0, Quoted{From: from}
	q.To = from - 1
	for n := from; n <= last; n++ {
		if used+len(at(n)) > bytes {
			break
		}
		used += len(at(n))
		q.To = n
	}
	switch {
	case q.To < from:
		cut := bytes
		for cut > 0 && !utf8.RuneStart(at(from)[cut]) {
			cut--
		}
		q.To, q.Text = from, string(at(from)[:cut])
		q.Cut = fmt.Sprintf("line %d cut at its first %s", from, text.Plural(cut, "byte"))
		return q
	case q.To < to && q.To == from+most-1:
		q.Cut = fmt.Sprintf("cut at %s: %s of lines %d–%d quoted", text.Plural(most, "line"), lineSpan(from, q.To), from, to)
	case q.To < to:
		q.Cut = fmt.Sprintf("cut at %s: %s of lines %d–%d quoted", text.Plural(bytes, "byte"), lineSpan(from, q.To), from, to)
	}
	before, after := from > lo, q.To == to && q.To < read
	for before || after {
		if before = before && q.From > lo && used+len(at(q.From-1)) <= bytes; before {
			q.From--
			used += len(at(q.From))
		}
		if after = after && q.To < read && used+len(at(q.To+1)) <= bytes; after {
			q.To++
			used += len(at(q.To))
		}
	}
	var b strings.Builder
	for n := q.From; n <= q.To; n++ {
		b.Write(at(n))
	}
	q.Text = b.String()
	return q
}

// readLines reads lines from to to of the item id, each to its first keep
// bytes, as stored, its newline included where kept.
func (s *Session) readLines(id string, from, to, keep int) ([][]byte, error) {
	r, err := s.c.ev.Open(id)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	err = scanLines(r, from, to, keep, func(_ int, line []byte) bool {
		out = append(out, slices.Clone(line))
		return true
	})
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	return out, err
}

// lineSpan names lines from to to: "line 3" or "lines 3–4".
func lineSpan(from, to int) string {
	if from == to {
		return "line " + strconv.Itoa(from)
	}
	return fmt.Sprintf("lines %d–%d", from, to)
}

// quoteLines reads lines from to to of r, numbered from 1, as stored,
// within limit bytes: whole lines while they fit, and where not even the
// first does, its first limit bytes, cut on a character's boundary
// (partial). It returns the bytes and the last line they reach. Lines
// before from are read past without being kept.
func quoteLines(r io.Reader, from, to, limit int) (quoted string, last int, partial bool, err error) {
	var out []byte
	err = scanLines(r, from, to, limit+1, func(n int, line []byte) bool {
		if len(out)+len(line) <= limit {
			out = append(out, line...)
			last = n
			return true
		}
		if len(out) == 0 {
			cut := limit
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			out, last, partial = append(out, line[:cut]...), n, true
		}
		return false
	})
	if err != nil {
		return "", 0, false, err
	}
	return string(out), last, partial, nil
}

// scanLines reads r's lines, numbered from 1, and gives fn each of lines
// from to to as stored, its newline included, cut to its first keep
// bytes, until fn returns false; lines before from are read past without
// being kept. The line given is valid until fn returns.
func scanLines(r io.Reader, from, to, keep int, fn func(n int, line []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	for n := 1; n <= to; n++ {
		line = line[:0]
		size, long, eof := 0, false, false
		for {
			chunk, err := br.ReadSlice('\n')
			size += len(chunk)
			if n >= from && !long {
				if len(line)+len(chunk) > keep {
					line = append(line, chunk[:keep-len(line)]...)
					long = true
				} else {
					line = append(line, chunk...)
				}
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				// A last line with no newline ends at the end, however
				// its bytes fell across the reader's buffer.
				if size == 0 {
					return nil
				}
				eof = true
				break
			}
			if err != nil {
				return err
			}
			break
		}
		if n >= from && !fn(n, line) || eof {
			return nil
		}
	}
	return nil
}

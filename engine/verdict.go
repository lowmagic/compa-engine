package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/store"
)

// The engineer's verdicts on the span flags (docs/MODEL_API.md §Answer
// checks): a code span an answer states, found in no evidence the
// conversation saw or arithmetic that does not hold, judged by the
// engineer supported, inferred, unsupported or incorrect, against the
// lines they marked or, where they marked none, the line of the evidence
// closest to it (AnswerFlag.Closest). Every verdict is kept in
// span_verdicts, by the answer as the model first wrote it and the span,
// its lines in span_verdict_lines; one that the claim is unsupported or
// incorrect is also appended to the main conversation as a message of its
// own, which the main model reads at its next call; one that it is
// supported or inferred is the engineer's record, never sent. Nothing
// stored is rewritten and no model is called.

// VerdictKind is what the engineer found of a span.
type VerdictKind string

// The verdicts, as span_verdicts keeps them.
const (
	// VerdictSupported: the evidence says it.
	VerdictSupported VerdictKind = "supported"
	// VerdictInferred: it follows from the lines marked, which do not
	// say it.
	VerdictInferred VerdictKind = "inferred"
	// VerdictUnsupported: no evidence says it; the model is told.
	VerdictUnsupported VerdictKind = "unsupported"
	// VerdictIncorrect: the evidence contradicts it; the model is told.
	VerdictIncorrect VerdictKind = "incorrect"
)

// Told says whether the main model is told a verdict of the kind.
func (k VerdictKind) Told() bool { return k == VerdictUnsupported || k == VerdictIncorrect }

// EvidenceRange is lines of an evidence item: its short name, the first
// and last line's numbers from 1, and, in a verdict, the lines as stored,
// each as EvidenceLine keeps one, at most verdictRangeLines of them.
type EvidenceRange struct {
	Name     string
	From, To int
	Lines    []string
}

// Place is the range as one place, e57:3-5, or e57:3.
func (r EvidenceRange) Place() string {
	if r.To == r.From {
		return r.Name + ":" + strconv.Itoa(r.From)
	}
	return r.Name + ":" + strconv.Itoa(r.From) + "-" + strconv.Itoa(r.To)
}

// SpanVerdict is the engineer's verdict on a span flag of an answer.
type SpanVerdict struct {
	// Answer is the answer's name as the chat gives it, a2.
	Answer string
	// Span is the span as written, and Statement the statement holding it
	// as the answer shows it.
	Span, Statement string
	Kind            VerdictKind
	// Evidence are the lines the verdict rests on, as stored: those the
	// engineer marked, or the closest line; none for unsupported, or for a
	// span with no line close to it and none marked.
	Evidence []EvidenceRange
	At       time.Time
}

// VerdictRanges is how many ranges of lines a verdict may rest on at most.
const VerdictRanges = 8

// Bounds of a verdict's quotation: how many lines of each range are kept
// and quoted to the model, and how many bytes of quotation its message
// holds at most.
const (
	verdictRangeLines = 10
	verdictQuoteBytes = 4 << 10
)

// ErrInferredUnmarked refuses an inferred verdict with no lines marked.
var ErrInferredUnmarked = errors.New("inferred needs the lines it is inferred from, and none are marked")

// verdictText is a verdict the model is told, as the main conversation
// keeps it: an incorrect one with the lines it rests on quoted as stored,
// within verdictQuoteBytes.
func verdictText(v SpanVerdict) string {
	t, _ := verdictMessage(v)
	return t
}

// verdictMessage is verdictText and the lines it quotes.
func verdictMessage(v SpanVerdict) (string, []companion.Shown) {
	what := v.Answer + "'s " + codeQuote(v.Span)
	if v.Kind == VerdictUnsupported {
		return "The engineer found no evidence for " + what + ": treat it as unsupported.", nil
	}
	if len(v.Evidence) == 0 {
		return "The engineer checked " + what + ": it is incorrect.", nil
	}
	places := make([]string, len(v.Evidence))
	for i, r := range v.Evidence {
		places[i] = r.Place()
	}
	var b strings.Builder
	var shows []companion.Shown
	b.WriteString("The engineer checked " + what + " against " + strings.Join(places, ", ") + ": it is incorrect.")
	left := verdictQuoteBytes
	for i, r := range v.Evidence {
		var q strings.Builder
		fence := fenceFor(r.Lines)
		width := len(strconv.Itoa(r.To))
		q.WriteString("\n\n" + r.Place() + ":\n" + fence + "\n")
		for k, l := range r.Lines {
			fmt.Fprintf(&q, "%*d  %s\n", width, r.From+k, l)
		}
		q.WriteString(fence)
		if more := r.To - r.From + 1 - len(r.Lines); more > 0 {
			fmt.Fprintf(&q, "\n[%s more of %s not quoted]", text.Plural(more, "line"), r.Place())
		}
		if q.Len() > left {
			fmt.Fprintf(&b, "\n\n[%s not quoted: the message's bound is spent]", strings.Join(places[i:], ", "))
			break
		}
		left -= q.Len()
		b.WriteString(q.String())
		if len(r.Lines) > 0 {
			shows = append(shows, companion.Shown{Name: r.Name, From: r.From, To: r.From + len(r.Lines) - 1})
		}
	}
	return b.String(), shows
}

// fenceFor is a Markdown fence longer than any run of backticks in lines,
// three at least.
func fenceFor(lines []string) string {
	longest := 0
	for _, l := range lines {
		run := 0
		for _, r := range l {
			if r != '`' {
				run = 0
				continue
			}
			run++
			longest = max(longest, run)
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// codeQuote is s as a Markdown code span, its delimiter longer than any
// run of backticks in it.
func codeQuote(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	if longest == 0 {
		return "`" + s + "`"
	}
	fence := strings.Repeat("`", longest+1)
	return fence + " " + s + " " + fence
}

// JudgeSpan keeps the engineer's verdict of kind on flag, a span flag of
// the answer named answer (a2), resting on marked, the lines the engineer
// marked, or where they marked none on the flag's closest line: inferred
// needs lines marked, and unsupported rests on none. One that the claim
// is unsupported or incorrect is also appended to the main conversation
// after its last message. The statement is read again from the answer as
// shown and every line from the stored evidence, never taken from flag or
// marked; a span the answer does not state, one already judged, more than
// VerdictRanges ranges, and lines not as stored, past the span search's
// bound for one item, the conversation was not shown, as the span search
// reads them (exposure.reads), or the engineer's rules leave out of what
// models read, are refused. The caller runs no turn meanwhile: the chat
// refuses /check while one runs.
func (s *Session) JudgeSpan(ctx context.Context, answer string, flag AnswerFlag, kind VerdictKind, marked []EvidenceRange) (SpanVerdict, error) {
	if flag.Span == "" {
		return SpanVerdict{}, errors.New("the flag is no span")
	}
	if !slices.Contains([]VerdictKind{VerdictSupported, VerdictInferred, VerdictUnsupported, VerdictIncorrect}, kind) {
		return SpanVerdict{}, fmt.Errorf("%q is no verdict", kind)
	}
	if len(marked) > VerdictRanges {
		return SpanVerdict{}, fmt.Errorf("a verdict rests on at most %d ranges of lines", VerdictRanges)
	}
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return SpanVerdict{}, err
	}
	views := AnswerViews(history)
	i := slices.IndexFunc(views, func(v AnswerView) bool { return v.Label == answer })
	if i < 0 {
		return SpanVerdict{}, fmt.Errorf("there is no answer %s", answer)
	}
	view := views[i]
	statement, ok := statementHolding(view.Text, flag.Span)
	if !ok {
		return SpanVerdict{}, fmt.Errorf("%s states no %s", answer, codeQuote(flag.Span))
	}
	v := SpanVerdict{Answer: answer, Span: flag.Span, Statement: statement, Kind: kind, At: s.c.now()}
	var ids []string
	switch {
	case kind == VerdictUnsupported:
	case len(marked) > 0:
		for _, r := range marked {
			id, got, err := s.rangeAsStored(ctx, r)
			if err != nil {
				return SpanVerdict{}, err
			}
			ids, v.Evidence = append(ids, id), append(v.Evidence, got)
		}
	case kind == VerdictInferred:
		return SpanVerdict{}, ErrInferredUnmarked
	case flag.Closest != nil:
		id, line, err := s.lineAsStored(ctx, *flag.Closest)
		if err != nil {
			return SpanVerdict{}, err
		}
		ids, v.Evidence = append(ids, id), append(v.Evidence, EvidenceRange{Name: line.Name, From: line.Line, To: line.Line, Lines: []string{line.Text}})
	}
	conv, seq := history[view.Original].ConversationID, seqOf(history, view.Original)
	err = s.c.st.Tx(ctx, func(b *store.Store) error {
		var judged int
		if err := b.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM span_verdicts WHERE conversation_id = ? AND answer_seq = ? AND span = ?`,
			conv, seq, v.Span).Scan(&judged); err != nil {
			return fmt.Errorf("the verdicts: %w", err)
		}
		if judged > 0 {
			return fmt.Errorf("%s's %s is already checked", answer, codeQuote(v.Span))
		}
		if _, err := b.DB().ExecContext(ctx, `INSERT INTO span_verdicts (conversation_id, answer_seq, answer_name, span, statement, verdict, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, conv, seq, answer, v.Span, v.Statement, string(kind), v.At.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("keeping the verdict: %w", err)
		}
		for k, r := range v.Evidence {
			if _, err := b.DB().ExecContext(ctx, `INSERT INTO span_verdict_lines (conversation_id, answer_seq, span, ord, evidence_id, evidence_name, from_line, to_line, text)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, conv, seq, v.Span, k+1, ids[k], r.Name, r.From, r.To, strings.Join(r.Lines, "\n")); err != nil {
				return fmt.Errorf("keeping the verdict's lines: %w", err)
			}
		}
		if !kind.Told() {
			return nil
		}
		st := companion.Store{DB: b.DB()}
		latest, err := st.Latest(ctx)
		if err != nil {
			return err
		}
		text, shows := verdictMessage(v)
		return (&companion.Loop{Store: st, Now: s.c.now}).AddVerdict(ctx, latest.ID, text, shows...)
	})
	if err != nil {
		return SpanVerdict{}, err
	}
	return v, nil
}

// statementHolding is the first statement of answer holding span as a
// code span, as the answer shows it.
func statementHolding(answer, span string) (string, bool) {
	for _, st := range check.Split(answer) {
		if slices.Contains(check.Spans(st.Text), span) {
			return st.Text, true
		}
	}
	return "", false
}

// seqOf is history[i]'s seq in its conversation: history holds each
// conversation whole, in order.
func seqOf(history []ConversationMessage, i int) int {
	n := 1
	for _, m := range history[:i] {
		if m.ConversationID == history[i].ConversationID {
			n++
		}
	}
	return n
}

// lineAsStored is l as the stored evidence holds it, with the item's ID:
// its text must be the line's whole, as EvidenceLine keeps it, and that
// text is returned.
func (s *Session) lineAsStored(ctx context.Context, l EvidenceLine) (string, EvidenceLine, error) {
	if l.Line < 1 {
		return "", EvidenceLine{}, fmt.Errorf("%s is not a line of evidence", l.Place())
	}
	id, lines, err := s.linesAsStored(ctx, l.Name, l.Line, l.Line)
	if err != nil {
		return "", EvidenceLine{}, err
	}
	if l.Line > len(lines) {
		return "", EvidenceLine{}, fmt.Errorf("%s has no line %d within the search's bound", l.Name, l.Line)
	}
	stored := EvidenceLine{Name: l.Name, Line: l.Line, Text: storedLine(lines[l.Line-1])}
	if l.Text != stored.Text {
		return "", EvidenceLine{}, fmt.Errorf("%s is not as stored", l.Place())
	}
	return id, stored, nil
}

// rangeAsStored is r's lines as the stored evidence holds them, each
// whole within the span search's bound, the first verdictRangeLines kept,
// with the item's ID; what r carries of its lines is not read.
func (s *Session) rangeAsStored(ctx context.Context, r EvidenceRange) (string, EvidenceRange, error) {
	if r.From < 1 || r.To < r.From {
		return "", EvidenceRange{}, fmt.Errorf("%s is not lines of evidence", r.Place())
	}
	id, lines, err := s.linesAsStored(ctx, r.Name, r.From, r.To)
	if err != nil {
		return "", EvidenceRange{}, err
	}
	if r.To > len(lines) {
		return "", EvidenceRange{}, fmt.Errorf("%s has no line %d within the search's bound", r.Name, r.To)
	}
	got := EvidenceRange{Name: r.Name, From: r.From, To: r.To}
	for _, l := range lines[r.From-1 : min(r.To, r.From-1+verdictRangeLines)] {
		got.Lines = append(got.Lines, storedLine(l))
	}
	return id, got, nil
}

// linesAsStored are the whole lines of the item named, read within the
// span search's bound for one item, with its ID, where its lines from to
// to are lines the span search reads: lines the conversation, the ones it
// continues or their consults were shown, or of an item named where the
// exposure is unknown (exposure.reads), of one the engineer's rules do
// not leave out of what models read.
func (s *Session) linesAsStored(ctx context.Context, name string, from, to int) (string, [][]byte, error) {
	if !shortName.MatchString(name) {
		return "", nil, fmt.Errorf("%s is not evidence", name)
	}
	x, err := s.exposure(ctx)
	if err != nil {
		return "", nil, err
	}
	if why, ok := x.reads(name, from, to); !ok {
		return "", nil, errors.New(why)
	}
	id, err := evidenceByName(s.c, name)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", name, err)
	}
	if withdrawn(s.c)(id) {
		return "", nil, fmt.Errorf("%s is left out of what models read", name)
	}
	r, err := s.c.ev.Open(id)
	if err != nil {
		return "", nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(spanBudget.Item)+1))
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", name, err)
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(data) > spanBudget.Item {
		// The last line read is cut short, as the search leaves it.
		lines = lines[:len(lines)-1]
	}
	return id, lines, nil
}

// answerKey is an answer as the model first wrote it: its conversation and
// its seq there.
type answerKey struct {
	conv string
	seq  int
}

// spanVerdicts are the verdicts kept, oldest first, by their answer, each
// with its lines; Answer is set from the history that reads them.
func spanVerdicts(ctx context.Context, c *openCase) (map[answerKey][]SpanVerdict, error) {
	type spanKey struct {
		answerKey
		span string
	}
	lines := map[spanKey][]EvidenceRange{}
	rows, err := c.st.DB().QueryContext(ctx, `SELECT conversation_id, answer_seq, span, evidence_name, from_line, to_line, text
		FROM span_verdict_lines ORDER BY conversation_id, answer_seq, span, ord`)
	if err != nil {
		return nil, fmt.Errorf("the verdicts' lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k spanKey
		var r EvidenceRange
		var quoted string
		if err := rows.Scan(&k.conv, &k.seq, &k.span, &r.Name, &r.From, &r.To, &quoted); err != nil {
			return nil, fmt.Errorf("the verdicts' lines: %w", err)
		}
		r.Lines = strings.Split(quoted, "\n")
		lines[k] = append(lines[k], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the verdicts' lines: %w", err)
	}
	rows, err = c.st.DB().QueryContext(ctx, `SELECT conversation_id, answer_seq, span, statement, verdict, created_at
		FROM span_verdicts ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("the verdicts: %w", err)
	}
	defer rows.Close()
	out := map[answerKey][]SpanVerdict{}
	for rows.Next() {
		var k answerKey
		var v SpanVerdict
		var at string
		if err := rows.Scan(&k.conv, &k.seq, &v.Span, &v.Statement, &v.Kind, &at); err != nil {
			return nil, fmt.Errorf("the verdicts: %w", err)
		}
		if v.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("the verdicts: %w", err)
		}
		v.Evidence = lines[spanKey{k, v.Span}]
		out[k] = append(out[k], v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the verdicts: %w", err)
	}
	return out, nil
}

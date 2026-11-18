package engine

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/evidence"
)

// Digest limits: a paragraph is boilerplate when it is at least
// boilerplateMin bytes and appears in boilerplateComments comments or
// more, and is kept in the oldest of them; a run of more than logRunMax log lines keeps logKeep at each end.
const (
	boilerplateMin      = 40
	boilerplateComments = 3
	logRunMax           = 40
	logKeep             = 15
)

// DigestStats count what the digest cut, for the engineer to see.
type DigestStats struct {
	Comments, Repeats, Quoted, Boilerplate, LogLines int
	Before, After                                    int
}

func (d DigestStats) String() string {
	return fmt.Sprintf("%d comments, %s cut to %s: %d repeated, %d quoted replies, %d boilerplate paragraphs, %d log lines",
		d.Comments, kib(d.Before), kib(d.After), d.Repeats, d.Quoted, d.Boilerplate, d.LogLines)
}

func kib(n int) string { return fmt.Sprintf("%.1f KiB", float64(n)/1024) }

var (
	quoteStart = regexp.MustCompile(`(?im)^(on .{1,200} wrote:|-{2,}\s*original message\s*-{2,}|from: .+\n(sent|date): .+)\s*$`)
	logLine    = regexp.MustCompile(`^\s*(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}|[A-Z][a-z]{2} [ \d]\d \d{2}:\d{2}:\d{2}|\[\s*\d+\.\d+\]|[IWEF]\d{4} \d{2}:\d{2}:\d{2})`)
	spaces     = regexp.MustCompile(`\s+`)
)

// digestComments cuts down the case's comments as a request carries them,
// without a model: a comment repeating an earlier one becomes a pointer to
// it; a quoted earlier message, boilerplate recurring across comments,
// such as signatures, and the middle of a long run of log lines are cut.
// A recurring paragraph stays in the oldest comment holding it, and the
// others name that comment, since a symptom repeated across comments is
// evidence, not a signature.
// Every cut is marked in the text; the evidence itself is unchanged, and
// an input keeps its size, so the model sees how large it was. Inputs
// other than comments, and comments not inlined, pass as they are.
func digestComments(in []evidence.Input) ([]evidence.Input, DigestStats) {
	out, st, _ := digestSpans(in)
	return out, st
}

// Span kinds: what the digest cut from a comment.
const (
	// SpanRepeated is a paragraph found in boilerplateComments comments or
	// more, kept in the oldest of them, which Comments names.
	SpanRepeated = "repeated"
	// SpanRepeat is the whole comment, repeating the one Comments names.
	SpanRepeat = "repeat"
	// SpanQuoted is a quoted earlier message or a run of quoted lines;
	// Comments are the earlier comments holding what it quotes, if any.
	SpanQuoted = "quoted"
	// SpanLog is the middle of a long run of log lines, its head and tail
	// kept.
	SpanLog = "log"
)

// CommentSpan is a part of a comment's body the digest cut from what
// models read: lines From to To, To excluded, counted from 0 in the body
// as it follows the comment's header, so the comment viewer folds exactly
// what the digest cut. The spans say what the digest established and
// nothing more: a paragraph recurring is repeated, never a signature, and
// a quote names its source only when exactly one comment holds it.
type CommentSpan struct {
	Kind     string   `json:"kind"`
	From     int      `json:"from"`
	To       int      `json:"to"`
	Comments []string `json:"comments,omitempty"`
	// Count is how many comments hold a repeated paragraph.
	Count int `json:"count,omitempty"`
}

// digestSpans is digestComments with what it cut from each comment, by
// evidence ID.
func digestSpans(in []evidence.Input) ([]evidence.Input, DigestStats, map[string][]CommentSpan) {
	var st DigestStats
	type parsed struct {
		header string
		paras  []para
		lines  int
	}
	out := append([]evidence.Input(nil), in...)
	spans := map[string][]CommentSpan{}
	bodies := map[int]*parsed{}
	for i, e := range out {
		if e.Ref.Type != TypeCaseComment || !e.Inline || e.Text == "" {
			continue
		}
		st.Comments++
		st.Before += len(e.Text)
		header, body, _ := strings.Cut(e.Text, "\n\n")
		bodies[i] = &parsed{header: header, paras: paraLines(body), lines: strings.Count(body, "\n") + 1}
	}
	// Boilerplate: a paragraph found in several comments.
	seenIn := map[string]int{}
	for _, p := range bodies {
		mine := map[string]bool{}
		for _, pa := range p.paras {
			if k := norm(pa.text); len(k) >= boilerplateMin && !mine[k] {
				mine[k] = true
				seenIn[k]++
			}
		}
	}
	// Repeats point to the oldest, and a recurring paragraph stays in the
	// oldest holding it; inputs come newest first.
	first := map[string]int{}
	holder := map[string]int{}
	// earlier holds the paragraphs of the comments before this one, quote
	// marks taken off, with the comments holding each, so a quoted message
	// is cut only when the case already holds what it quotes.
	earlier := map[string][]string{}
	for i := len(out) - 1; i >= 0; i-- {
		p := bodies[i]
		if p == nil {
			continue
		}
		id := out[i].Ref.ID
		var cuts []CommentSpan
		// kept is what the comment shows; own is what it says apart from
		// recurring paragraphs, which decides whether it repeats another.
		var kept, own []para
		var holders []string
		for _, pa := range p.paras {
			k := norm(pa.text)
			if seenIn[k] < boilerplateComments {
				kept, own = append(kept, pa), append(own, pa)
				continue
			}
			j, held := holder[k]
			if !held {
				holder[k] = i
				kept = append(kept, pa)
				continue
			}
			cuts = append(cuts, CommentSpan{Kind: SpanRepeated, From: pa.from, To: pa.from + pa.n, Comments: []string{out[j].Ref.ID}, Count: seenIn[k]})
			if j != i {
				holders = append(holders, out[j].Ref.ID)
			}
		}
		st.Boilerplate += len(holders)
		lines, quoted, qs := cutQuotes(joinParas(kept), earlier, p.lines)
		st.Quoted += quoted
		lines, logCut, ls := cutLogRuns(lines)
		st.LogLines += logCut
		cuts = append(append(cuts, qs...), ls...)
		body := textOf(lines)
		// Each line's stored number in the comment, header and blank line
		// before the body included, so a model cites stored lines;
		// 0 for a line the digest wrote.
		hl := strings.Count(p.header, "\n") + 1
		var nums []int
		for k := 1; k <= hl+1; k++ {
			nums = append(nums, k)
		}
		for _, l := range lines {
			n := 0
			if l.at >= 0 {
				n = hl + 2 + l.at
			}
			nums = append(nums, n)
		}
		if len(holders) > 0 {
			body = strings.TrimRight(body, "\n")
			nums = append(nums[:hl+1+strings.Count(body, "\n")+1], 0, 0)
			body += fmt.Sprintf("\n\n[%s also in %s cut here]", text.Plural(len(holders), "paragraph"), strings.Join(unique(holders), ", "))
		}
		ownLines, _, _ := cutQuotes(joinParas(own), earlier, p.lines)
		ownLines, _, _ = cutLogRuns(ownLines)
		key := norm(textOf(ownLines))
		if j, ok := first[key]; ok && key != "" {
			body = fmt.Sprintf("[repeats %s]", out[j].Ref.ID)
			nums = append(nums[:hl+1], 0)
			cuts = []CommentSpan{{Kind: SpanRepeat, From: 0, To: p.lines, Comments: []string{out[j].Ref.ID}}}
			st.Repeats++
		} else if key != "" {
			first[key] = i
		}
		out[i].Text = p.header + "\n\n" + body
		if len(cuts) > 0 && len(nums) == strings.Count(out[i].Text, "\n")+1 {
			out[i].Lines = nums
		}
		st.After += len(out[i].Text)
		for _, pa := range p.paras {
			k := norm(unquote(pa.text))
			if !slices.Contains(earlier[k], id) {
				earlier[k] = append(earlier[k], id)
			}
		}
		if len(cuts) > 0 {
			sort.SliceStable(cuts, func(a, b int) bool { return cuts[a].From < cuts[b].From })
			spans[id] = cuts
		}
	}
	return out, st, spans
}

// para is a paragraph of a comment's body: its text, the line it starts
// on and how many lines it has.
type para struct {
	text    string
	from, n int
}

// line is a line of a comment's body as the digest rewrites it: its text
// and the body line it is, or -1 for one the digest wrote.
type line struct {
	text string
	at   int
}

// paraLines splits a body into its non-blank paragraphs, each with its
// place in the body.
func paraLines(body string) []para {
	var out []para
	at := 0
	for _, p := range strings.Split(body, "\n\n") {
		n := strings.Count(p, "\n") + 1
		if strings.TrimSpace(p) != "" {
			out = append(out, para{text: p, from: at, n: n})
		}
		at += n + 1
	}
	return out
}

// joinParas is paragraphs joined by a blank line, as one text of lines.
func joinParas(ps []para) []line {
	var out []line
	for i, p := range ps {
		if i > 0 {
			out = append(out, line{at: -1})
		}
		for k, l := range strings.Split(p.text, "\n") {
			out = append(out, line{text: l, at: p.from + k})
		}
	}
	if len(out) == 0 {
		out = []line{{at: -1}}
	}
	return out
}

func textOf(lines []line) string {
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.text
	}
	return strings.Join(texts, "\n")
}

// cutQuotes cuts a quoted earlier message and runs of quoted lines. A
// line such as "On Tuesday the node wrote:" is not evidence enough of a
// quoted reply: what follows it is cut only when its first paragraph is
// one an earlier comment holds, so a report the case does not otherwise
// carry stays. A quoted message runs to the body's end, its last line
// end - 1.
func cutQuotes(lines []line, earlier map[string][]string, end int) ([]line, int, []CommentSpan) {
	n := 0
	var spans []CommentSpan
	body := textOf(lines)
	if loc := quoteStart.FindStringIndex(body); loc != nil {
		if holders := earlier[norm(firstPara(body[loc[1]:]))]; len(holders) > 0 {
			at := strings.Count(body[:loc[0]], "\n")
			// What came before, its blank lines at the end taken off.
			before := append([]line(nil), lines[:at]...)
			for len(before) > 0 && before[len(before)-1].text == "" {
				before = before[:len(before)-1]
			}
			if len(before) == 0 {
				before = []line{{at: -1}}
			}
			spans = append(spans, CommentSpan{Kind: SpanQuoted, From: lines[at].at, To: end, Comments: append([]string(nil), holders...)})
			lines = append(before, line{at: -1}, line{text: "[an earlier message quoted here was cut]", at: -1})
			n++
		}
	}
	lines, runs, rs := cutQuotedLines(lines, earlier)
	return lines, n + runs, append(spans, rs...)
}

// firstPara is the first paragraph of a quoted message, its quote marks
// taken off first, since a line of ">" alone ends a quoted paragraph.
func firstPara(s string) string {
	for _, p := range strings.Split(unquote(s), "\n\n") {
		if strings.TrimSpace(p) != "" {
			return p
		}
	}
	return ""
}

// unquote takes the quote marks off each line.
func unquote(para string) string {
	lines := strings.Split(para, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(strings.TrimSpace(l), "> ")
	}
	return strings.Join(lines, "\n")
}

func unique(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// norm compares texts apart from their whitespace. Case is kept: PROD
// and prod may name two different things.
func norm(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// cutQuotedLines replaces each run of two or more ">"-quoted lines. A run
// names its source only where the case holds what it quotes.
func cutQuotedLines(lines []line, earlier map[string][]string) ([]line, int, []CommentSpan) {
	var out []line
	var spans []CommentSpan
	runs := 0
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j].text), ">") {
			j++
		}
		if j-i >= 2 {
			out = append(out, line{text: fmt.Sprintf("[%d quoted lines cut]", j-i), at: -1})
			spans = append(spans, CommentSpan{Kind: SpanQuoted, From: lines[i].at, To: lines[j-1].at + 1, Comments: append([]string(nil), earlier[norm(firstPara(textOf(lines[i:j])))]...)})
			runs++
			i = j
			continue
		}
		if j > i {
			out = append(out, lines[i:j]...)
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return out, runs, spans
}

// cutLogRuns keeps the head and tail of each long run of log lines.
func cutLogRuns(lines []line) ([]line, int, []CommentSpan) {
	var out []line
	var spans []CommentSpan
	cut := 0
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && logLine.MatchString(lines[j].text) {
			j++
		}
		if n := j - i; n > logRunMax {
			out = append(out, lines[i:i+logKeep]...)
			out = append(out, line{text: fmt.Sprintf("[%d log lines cut]", n-2*logKeep), at: -1})
			out = append(out, lines[j-logKeep:j]...)
			spans = append(spans, CommentSpan{Kind: SpanLog, From: lines[i+logKeep].at, To: lines[j-logKeep-1].at + 1})
			cut += n - 2*logKeep
			i = j
			continue
		}
		if j > i {
			out = append(out, lines[i:j]...)
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return out, cut, spans
}

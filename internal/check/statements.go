package check

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Statement is one statement of a text a check looks at: a list item, a
// table row or a sentence of a paragraph, as written, its Markdown kept.
type Statement struct {
	ID   string
	Text string
}

var (
	listItem   = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)]|[a-zA-Z][.)])\s+(\S.*)$`)
	heading    = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)
	rule       = regexp.MustCompile(`^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$`)
	quoteMark  = regexp.MustCompile(`^\s*>\s?`)
	tableSep   = regexp.MustCompile(`^\s*\|?\s*:?-{3,}`)
	codeSpan   = regexp.MustCompile("`([^`\n]+)`")
	abbreviate = map[string]bool{"e.g": true, "i.e": true, "etc": true, "vs": true, "cf": true, "approx": true}
)

// Split numbers a text's statements, s1, s2, … in order: each list item
// whole, with the lines that continue it; each table row but its header
// rule; each sentence of a paragraph. A fenced block is a command or an
// excerpt, never a statement, and neither is a heading or a rule. The
// same text makes the same statements.
func Split(text string) []Statement {
	var out []Statement
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, Statement{ID: "s" + strconv.Itoa(len(out)+1), Text: s})
		}
	}
	var item, para []string
	flush := func() {
		if len(item) > 0 {
			add(strings.Join(item, " "))
		}
		if len(para) > 0 {
			for _, s := range sentences(strings.Join(para, " ")) {
				add(s)
			}
		}
		item, para = nil, nil
	}
	lines := strings.Split(text, "\n")
	fenced := Fenced(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fenced[i] {
			flush()
			continue
		}
		switch {
		case trimmed == "", heading.MatchString(line), rule.MatchString(line):
			flush()
		case strings.HasPrefix(trimmed, "|"):
			flush()
			if !tableSep.MatchString(trimmed) {
				add(trimmed)
			}
		case listItem.MatchString(line):
			flush()
			item = []string{listItem.FindStringSubmatch(line)[1]}
		case len(item) > 0:
			item = append(item, trimmed)
		default:
			para = append(para, quoteMark.ReplaceAllString(trimmed, ""))
		}
	}
	flush()
	return out
}

// Fenced says of each line whether it belongs to a fenced block, its
// opening and closing fences included: a line opening with ``` or ~~~,
// and longer, up to a line of the same character at least as long.
func Fenced(lines []string) []bool {
	out := make([]bool, len(lines))
	fence := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			out[i] = true
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		for _, c := range []string{"`", "~"} {
			if n := len(trimmed) - len(strings.TrimLeft(trimmed, c)); n >= 3 {
				fence = strings.Repeat(c, n)
				out[i] = true
				break
			}
		}
	}
	return out
}

// sentences splits a paragraph after a full stop, a question or an
// exclamation mark followed by space and a capital, a digit, a quote, a
// code span or a path, never inside a code span or square brackets, and
// never after a common abbreviation.
func sentences(p string) []string {
	masked := []byte(p)
	mask := func(opening, closing byte) {
		for i := 0; i < len(masked); i++ {
			if masked[i] != opening {
				continue
			}
			j := bytes.IndexByte(masked[i+1:], closing)
			if j < 0 {
				return
			}
			for k := i + 1; k <= i+j; k++ {
				masked[k] = 'x'
			}
			i += j + 1
		}
	}
	mask('`', '`')
	mask('[', ']')
	var out []string
	start := 0
	for i := 0; i < len(masked); i++ {
		if c := masked[i]; c != '.' && c != '!' && c != '?' {
			continue
		}
		end := i + 1
		for end < len(masked) && strings.IndexByte(`)"'`, masked[end]) >= 0 {
			end++
		}
		if end >= len(masked) || masked[end] != ' ' {
			continue
		}
		next := end
		for next < len(masked) && masked[next] == ' ' {
			next++
		}
		if next >= len(masked) || !opensSentence(masked[next]) {
			continue
		}
		if masked[i] == '.' && abbreviate[strings.ToLower(lastWord(p[start:i]))] {
			continue
		}
		out = append(out, p[start:end])
		start = next
		i = next - 1
	}
	return append(out, p[start:])
}

func opensSentence(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '`' || c == '"' || c == '/' || c >= 0x80
}

// lastWord is the text's last run of letters and full stops: "e.g" in
// "as e.g".
func lastWord(s string) string {
	i := strings.LastIndexFunc(s, func(r rune) bool { return r != '.' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') })
	return s[i+1:]
}

// Spans are a statement's inline code spans, their text without the
// backticks, in order.
func Spans(s string) []string {
	var out []string
	for _, m := range codeSpan.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// Excerpt is a fenced block of a text that may present captured output:
// the statement before it, which presents it, and what of it to look for
// in the evidence.
type Excerpt struct {
	// After is the last statement before the block, its ID and text as
	// Split numbers the whole text; empty where none is.
	After Statement
	// Bare says the block names no info string: it may hold output, or a
	// query or a snippet to use, which only its presenting says.
	Bare bool
	// Lines are its lines as written, each with a line number a tool's
	// output gave it taken off and cut at an ellipsis into its parts, those
	// too short to tell apart left out.
	Lines []string
}

var (
	// excerptInfo are the info strings of a fenced block that may present
	// output: none, or one naming plain text or a log. A block of a
	// shell, YAML, JSON or any other language may be a command or a
	// change to make, not output, and is not an excerpt.
	excerptInfo = map[string]bool{"": true, "text": true, "txt": true, "plaintext": true, "log": true, "output": true}
	// lineNumber is a line number before an excerpt's line, as grep and
	// evidence_read give it: "41: ", "40- ", "41 | ".
	lineNumber = regexp.MustCompile(`^\s*\d+(?:[:-]\s?|\s*[|│]\s?)`)
	// ellipsis cuts an excerpt's line where its writer left text out.
	ellipsis = regexp.MustCompile(`\[(?:\.\.\.|…)\]|\.\.\.|…`)
	// prompt is a shell prompt before a command line.
	prompt = regexp.MustCompile(`^\s*[$#]\s+`)
)

// excerptMin is the fewest characters of an excerpt's part looked for:
// shorter ones, a brace or a word, are found anywhere and tell nothing.
const excerptMin = 8

// Excerpts are a text's fenced blocks that may present captured output, in
// order: a block with no info string or one naming text or a log, none of
// whose lines is a command line to run (Commands.Command), with or without
// a shell prompt.
func (more Commands) Excerpts(text string) []Excerpt {
	lines := strings.Split(text, "\n")
	fenced := Fenced(lines)
	var out []Excerpt
	for i := 0; i < len(lines); i++ {
		if !fenced[i] {
			continue
		}
		start := i
		for i+1 < len(lines) && fenced[i+1] && (i == start || !closes(lines[start], lines[i])) {
			i++
		}
		if ex, ok := more.excerpt(lines, start, i); ok {
			out = append(out, ex)
		}
	}
	return out
}

// closes says whether line, a later line of the block that opening opens,
// is the line closing it, by Fenced's rule.
func closes(opening, line string) bool {
	o, l := strings.TrimSpace(opening), strings.TrimSpace(line)
	c := o[:1]
	fence := o[:len(o)-len(strings.TrimLeft(o, c))]
	return strings.HasPrefix(l, fence) && strings.Trim(l, c) == ""
}

// excerpt is the block of lines from its opening fence at start to its
// last line at end, where it presents output.
func (more Commands) excerpt(lines []string, start, end int) (Excerpt, bool) {
	open := strings.TrimSpace(lines[start])
	info := strings.ToLower(strings.TrimSpace(strings.TrimLeft(open, open[:1])))
	if f := strings.Fields(info); len(f) > 0 {
		info = f[0]
	}
	if !excerptInfo[info] {
		return Excerpt{}, false
	}
	body := lines[start+1 : end+1]
	if end > start && closes(lines[start], lines[end]) {
		body = lines[start+1 : end]
	}
	ex := Excerpt{Bare: info == ""}
	for _, l := range body {
		if more.Command(strings.TrimSpace(prompt.ReplaceAllString(l, ""))) {
			return Excerpt{}, false
		}
		for _, part := range ellipsis.Split(lineNumber.ReplaceAllString(l, ""), -1) {
			if part = strings.TrimSpace(part); len([]rune(part)) >= excerptMin {
				ex.Lines = append(ex.Lines, part)
			}
		}
	}
	if before := Split(strings.Join(lines[:start], "\n")); len(before) > 0 {
		ex.After = before[len(before)-1]
	}
	return ex, len(ex.Lines) > 0
}

// commands are the engine's own programs whose command lines an answer
// gives to be run, here or by the customer: general-purpose ones, kept
// small. A deployment names its products' programs in checks.commands
// (Commands). A code span beginning with any other word is checked as a
// claim.
var commands = map[string]bool{
	"journalctl": true, "systemctl": true, "ip": true, "ss": true, "df": true, "du": true, "ls": true,
	"cat": true, "grep": true, "curl": true, "dig": true, "nslookup": true, "host": true, "ping": true,
}

// Commands are the programs a deployment names beside the engine's own
// (checks.commands), such as its products' command-line tools.
type Commands []string

// Command reports whether a code span is a command line to run, not a
// claim: words separated by whitespace, the first a program of the
// engine's own or of more, as in `journalctl -u chronyd`. A bare name or
// value, and an expression whose first word is no program, such as
// `node_load1 > 4`, are not.
func (more Commands) Command(span string) bool {
	words := strings.Fields(span)
	return len(words) > 1 && (commands[words[0]] || slices.Contains(more, words[0]))
}

// CommandLine is a command line a text gives to run, as the check for a
// command found in no evidence or guidance reads it.
type CommandLine struct {
	// Line is the line as written, a shell prompt and a leading sudo taken
	// off, a line continued with a backslash joined to the next.
	Line string
	// Program is its first word; Options its words beginning with -, each
	// once, a value after = taken off. Its other words are values — names,
	// paths, times — and are not looked for.
	Program string
	Options []string
	// Statement is the statement holding it, for a code span, or the last
	// statement before its block, its ID and text as Split numbers the
	// whole text; empty where none is.
	Statement Statement
}

// shellInfo are the info strings of a fenced block of a shell, every line
// of which is a command line.
var shellInfo = map[string]bool{"sh": true, "bash": true, "shell": true, "console": true, "zsh": true}

// CommandLines are a text's command lines, in order, each once: a code
// span that is a command line (Command); every line of a fenced block of a
// shell; and a line of a bare block that is a command line, where the
// block is not presented as output — an excerpt (Excerpts) whose
// presenting statement cites evidence, as cites says of a statement's
// text. A shell prompt and a leading sudo are taken off, and a blank or
// comment line skipped; in a shell's block, a line opening with # is a
// comment unless what follows "# " is a command line, a root prompt's.
func (more Commands) CommandLines(text string, cites func(statement string) bool) []CommandLine {
	var out []CommandLine
	add := func(line string, st Statement) {
		words := strings.Fields(line)
		if len(words) == 0 || slices.ContainsFunc(out, func(c CommandLine) bool { return c.Line == line }) {
			return
		}
		c := CommandLine{Line: line, Program: words[0], Statement: st}
		for _, w := range words[1:] {
			o, _, _ := strings.Cut(w, "=")
			if strings.HasPrefix(o, "-") && strings.Trim(o, "-") != "" && !slices.Contains(c.Options, o) {
				c.Options = append(c.Options, o)
			}
		}
		out = append(out, c)
	}
	lines := strings.Split(text, "\n")
	fenced := Fenced(lines)
	for _, st := range Split(text) {
		for _, sp := range Spans(st.Text) {
			if l, ok := more.commandLine(sp, false); ok {
				add(l, st)
			}
		}
	}
	for i := 0; i < len(lines); i++ {
		if !fenced[i] {
			continue
		}
		start := i
		for i+1 < len(lines) && fenced[i+1] && (i == start || !closes(lines[start], lines[i])) {
			i++
		}
		open := strings.TrimSpace(lines[start])
		info := strings.ToLower(strings.TrimSpace(strings.TrimLeft(open, open[:1])))
		if f := strings.Fields(info); len(f) > 0 {
			info = f[0]
		}
		shell := shellInfo[info]
		if !shell && info != "" {
			continue
		}
		var after Statement
		if before := Split(strings.Join(lines[:start], "\n")); len(before) > 0 {
			after = before[len(before)-1]
		}
		if _, output := more.excerpt(lines, start, i); !shell && output && cites(after.Text) {
			continue
		}
		body := lines[start+1 : i+1]
		if i > start && closes(lines[start], lines[i]) {
			body = lines[start+1 : i]
		}
		for k := 0; k < len(body); k++ {
			l := strings.TrimSpace(body[k])
			// A line continued with a backslash goes on in the next.
			for strings.HasSuffix(l, `\`) && k+1 < len(body) {
				k++
				l = strings.TrimSpace(strings.TrimSuffix(l, `\`)) + " " + strings.TrimSpace(body[k])
			}
			if c, ok := more.commandLine(l, shell); ok {
				add(c, after)
			}
		}
	}
	return out
}

// commandLine is line with its shell prompt and a leading sudo taken
// off, and whether it is a command line: in a shell's block any line but a
// blank or a comment one, elsewhere one that is a command line by Command.
func (more Commands) commandLine(line string, shell bool) (string, bool) {
	t := strings.TrimSpace(line)
	l := strings.TrimSpace(prompt.ReplaceAllString(t, ""))
	if f := strings.Fields(l); len(f) > 1 && f[0] == "sudo" {
		l = strings.TrimSpace(l[len("sudo"):])
	}
	switch {
	case l == "":
		return "", false
	case strings.HasPrefix(t, "#"):
		// A comment, or a root prompt before a command line.
		return l, l != t && more.Command(l)
	case shell:
		return l, true
	}
	return l, more.Command(l)
}

// Missing are what of the command line a line lacks, each as a word — a
// run of letters, digits, _ and - standing alone: nothing where it holds
// the program and every option; the program alone where it lacks it;
// else the options it lacks.
func (c CommandLine) Missing(line string) []string {
	if !holdsWord(line, c.Program) {
		return []string{c.Program}
	}
	var out []string
	for _, o := range c.Options {
		if !holdsWord(line, o) {
			out = append(out, o)
		}
	}
	return out
}

// holdsWord says whether w is in line with no letter, digit, _ or -
// beside it.
func holdsWord(line, w string) bool {
	inWord := func(s string, at int, last bool) bool {
		var r rune
		if last {
			r, _ = utf8.DecodeLastRuneInString(s[:at])
		} else {
			r, _ = utf8.DecodeRuneInString(s[at:])
		}
		return r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	for from := 0; ; {
		i := strings.Index(line[from:], w)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(w)
		if (i == 0 || !inWord(line, i, true)) && (end == len(line) || !inWord(line, end, false)) {
			return true
		}
		from = i + 1
	}
}

// conditional is a statement's opening that makes it a condition.
var conditional = regexp.MustCompile(`(?i)^[*_\s]*(?:if|unless|should|whether|in case)\b`)

// Conditional reports whether a statement opens as a condition — if,
// unless, should, whether, in case — so it asserts nothing observed, its
// values those the condition supposes. When and once open a time as often
// as a condition, so a statement they open is not one.
func Conditional(statement string) bool { return conditional.MatchString(statement) }

// hedge matches the words that qualify a claim: the likelihood of it, an
// alternative to it, or that it only appears so.
var hedge = regexp.MustCompile(`(?i)\b(likely|unlikely|probably|possibly|possible|perhaps|may|might|could|either|appears?|seems?|suggests?|consistent with|aligns with|unclear|unconfirmed|not yet)\b`)

// Hedges are the qualifying words a statement holds outside its code
// spans, lower-cased, in order, each once.
func Hedges(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range hedge.FindAllString(codeSpan.ReplaceAllString(s, ""), -1) {
		w := strings.ToLower(m)
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

var (
	// certain matches the certainty review's own-voice certainty
	// terms (customer.md): words of proof and of model-owned confidence. The
	// imperatives verify, validate and establish are a procedure's steps,
	// not proof, and are left out.
	certain = regexp.MustCompile(`(?i)\b(confirm(?:s|ed|ing)?|confirmation|prov(?:es|ed|en)|proof|definitive(?:ly)?|conclusive(?:ly)?|certainly|beyond (?:any )?doubt|without (?:a )?doubt|unambiguous(?:ly)?|irrefutabl[ey]|undeniabl[ey]|smoking gun|clearly|establish(?:es|ed)|demonstrat(?:es|ed)|verified|validated|most likely|almost certainly|high(?:ly)? confiden(?:ce|t))\b`)
	// causal matches root-cause and causal phrasing, which the review keeps
	// to attributed conclusions.
	causal = regexp.MustCompile(`(?i)\b(root[- ]causes?|caus(?:ed|es|ing)|triggered|triggers|led to|leads to|resulted in)\b`)
	// attributor ends where a certainty term is another's word: "the
	// engineer confirmed", "the customer has confirmed".
	attributor = regexp.MustCompile(`(?i)\b(?:` + sources + `)\s+(?:has\s+|have\s+|had\s+)?$`)
	// ownCause matches a cause or a likelihood named in a writer's own
	// voice, where no named source's words come directly before it
	// (sourced).
	ownCause = regexp.MustCompile(`(?i)\b(root[- ]causes?|caus(?:e|es|ed|ing)|culprits?|trigger(?:ed|s)|le(?:d|ads) to|result(?:ed|s) in|due to|contribut(?:ed|es|ing|ors?)|likely|probably)\b`)
	// sourced ends where a causal term is a named source's: directly after
	// it, or after its verb of saying — "the vendor likely", "engineering
	// concluded the root cause".
	sourced = regexp.MustCompile(`(?i)\b(?:` + sources + `)\s+(?:(?:has|have|had)\s+)?(?:(?:said|says|stated|states|reported|reports|concluded|concludes|found|finds|identified|identifies|attributed|attributes|believes?|thinks?|suspects?|named|names)\s+(?:(?:that|the|a|an|it|this)\s+)?)?$`)
	// conclusive matches the causal terms that state a conclusion or rank
	// one, which no possibility before them makes a lead.
	conclusive = regexp.MustCompile(`(?i)^(?:root[- ]causes?|culprits?|likely|probably)$`)
	// exploring matches a possibility in a clause before a causal term,
	// which offers the relationship as a lead to explore: "the OOM kills
	// may have led to the restarts".
	exploring = regexp.MustCompile(`(?i)\b(?:may|might|could|possibly|possible|potential(?:ly)?|perhaps)\b`)
	// clauseBreak ends the clause before a term.
	clauseBreak = regexp.MustCompile(`[,;:.()—]`)
)

// sources are the named sources whose words a statement may give as
// theirs.
const sources = `engineers?|engineering|customers?|vendor|owner|support|documentation|docs|you|they|he|she`

// CausalTerms are the causal and likelihood terms a statement asserts in
// its own voice, outside its code spans, lower-cased, in order, each once:
// a cause or root cause, caused, causing, culprit, triggered, led to,
// resulted in, due to, contributed, likely, probably; but where directly
// after a named source (sourced), and, but for a conclusive term, where a
// possibility earlier in its clause offers it as a lead to explore
// (exploring). A question, and a statement opening as a condition
// (Conditional), assert no cause, and hold none.
func CausalTerms(s string) []string {
	if strings.HasSuffix(strings.TrimSpace(s), "?") || Conditional(s) {
		return nil
	}
	s = codeSpan.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat(" ", len(m)) })
	var out []string
	for _, m := range ownCause.FindAllStringIndex(s, -1) {
		w := strings.ToLower(s[m[0]:m[1]])
		if sourced.MatchString(s[:m[0]]) || slices.Contains(out, w) {
			continue
		}
		clause := s[:m[0]]
		if b := clauseBreak.FindAllStringIndex(clause, -1); len(b) > 0 {
			clause = clause[b[len(b)-1][1]:]
		}
		if !conclusive.MatchString(w) && exploring.MatchString(clause) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// CertaintyTerms are the certainty and causal terms a statement holds
// outside its code spans, lower-cased, in order, each once: a certainty
// term but where a named source's verb, as in "the engineer confirmed",
// and every causal phrase, whose voice — "engineering concluded the root
// cause is X" — only reading can tell.
func CertaintyTerms(s string) []string {
	s = codeSpan.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat(" ", len(m)) })
	type at struct {
		i int
		w string
	}
	var found []at
	for _, m := range certain.FindAllStringIndex(s, -1) {
		if !attributor.MatchString(s[:m[0]]) {
			found = append(found, at{m[0], strings.ToLower(s[m[0]:m[1]])})
		}
	}
	for _, m := range causal.FindAllStringIndex(s, -1) {
		found = append(found, at{m[0], strings.ToLower(s[m[0]:m[1]])})
	}
	slices.SortStableFunc(found, func(a, b at) int { return a.i - b.i })
	var out []string
	for _, f := range found {
		if !slices.Contains(out, f.w) {
			out = append(out, f.w)
		}
	}
	return out
}

var (
	clock = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`)
	date  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	// count is a number standing alone, not part of a name such as
	// worker-1 or e12.
	count = regexp.MustCompile(`(?:^|[^\w.\-/:])\d+(?:[.,]\d+)?(?:%|\b)`)
)

// Specific reports whether a statement states something exact a reader
// would want the source of: a code span, a time, a date or a count.
func Specific(s string) bool {
	if len(Spans(s)) > 0 {
		return true
	}
	return clock.MatchString(s) || date.MatchString(s) || count.MatchString(s)
}

package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
)

// The engine's evidence tools, offered to every ask beside its expert's
// tests: a model reads the case's stored evidence
// with them — an item it was shown only by reference, the part of a large
// one it needs, the lines two logs share. They read the evidence store,
// never the case's artifacts, so they need no sandbox. Each result is
// evidence like any collection, its Location the items it read, so a
// result read from an item the engineer later leaves out of what models
// read is withdrawn with it (withdrawn).

const (
	toolList            = "evidence.list"
	toolRead            = "evidence.read"
	toolGrep            = "evidence.grep"
	evidenceToolVersion = "0.1.0"

	// The tools' bounds: what one result may hold, what one line may, and
	// how much of one item grep reads.
	listMax      = 100
	readMaxLines = 400
	toolMaxBytes = 64 << 10
	lineMaxBytes = 2000
	grepMaxItems = 10
	grepScan     = 64 << 20
)

const evidenceIDPattern = `ev-[0-9a-f]{16}`

func bound(v float64) *float64 { return &v }

// evidenceTools are the tools as a request offers them, said in as few
// words as do: every ask carries them, at every turn of a conversation.
// grep checks its IDs itself, which a pattern for up to ten would spell
// out at length.
var evidenceTools = []expert.Test{
	{ID: toolList, Version: evidenceToolVersion,
		Description: "The case's stored evidence, newest first, at most 100: ID, type, size, source. type and contains narrow it.",
		Params: map[string]expert.Param{
			"type":     {Type: "string", Default: ""},
			"contains": {Type: "string", Default: ""},
		}},
	{ID: toolRead, Version: evidenceToolVersion,
		Description: "Lines of one stored item, numbered, from line from; lines longer than 2,000 bytes cut.",
		Params: map[string]expert.Param{
			"id":    {Type: "string", Pattern: "^" + evidenceIDPattern + "$"},
			"from":  {Type: "integer", Default: 1, Minimum: bound(1)},
			"lines": {Type: "integer", Default: 200, Minimum: bound(1), Maximum: bound(readMaxLines)},
		}},
	{ID: toolGrep, Version: evidenceToolVersion,
		Description: "Lines matching an RE2 pattern ((?i) ignores case) in stored items, ids separated by spaces, at most 10; numbered, context lines around each, max matches in all.",
		Params: map[string]expert.Param{
			"pattern": {Type: "string", MaxLength: 200},
			"ids":     {Type: "string", MaxLength: 200},
			"context": {Type: "integer", Default: 0, Minimum: bound(0), Maximum: bound(5)},
			"max":     {Type: "integer", Default: 50, Minimum: bound(1), Maximum: bound(200)},
		}},
}

// evidenceToolsFor runs the evidence tools over a case's store, refusing
// what out says is left out of what models read; each also says which
// lines of which items its result gives under their stored numbers.
func evidenceToolsFor(c *openCase, out func(string) bool) map[string]func(context.Context, map[string]any) (collect.Record, []linesRead) {
	return map[string]func(context.Context, map[string]any) (collect.Record, []linesRead){
		toolList: func(ctx context.Context, p map[string]any) (collect.Record, []linesRead) {
			return listEvidence(c, out, p), nil
		},
		toolRead: func(ctx context.Context, p map[string]any) (collect.Record, []linesRead) {
			return readEvidence(ctx, c, out, p)
		},
		toolGrep: func(ctx context.Context, p map[string]any) (collect.Record, []linesRead) {
			return grepEvidence(ctx, c, out, p)
		},
	}
}

// linesRead are lines from to to of the stored item id, which a result
// gives under their stored numbers.
type linesRead struct {
	id       string
	from, to int
}

func toolWhat(test string, p map[string]any) collect.What {
	return collect.What{TestID: test, TestVersion: evidenceToolVersion, Params: p}
}

func toolResult(test string, p map[string]any, read []string, started time.Time, out []byte, truncated bool) collect.Record {
	return collect.WithID(collect.Record{What: toolWhat(test, p), Status: collect.StatusOK, Truncated: truncated,
		StartedAt: started.UTC(), Duration: time.Since(started), PathsRead: read, Output: out})
}

func strParam(p map[string]any, name string) string {
	s, _ := p[name].(string)
	return s
}

func intParam(p map[string]any, name string, def int) int {
	if n, ok := number(p[name]); ok {
		return int(n)
	}
	return def
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// readable is an item the tools may read: one that exists and is not left
// out of what models read. Why not, otherwise.
func readable(c *openCase, out func(string) bool, id string) (evidence.Item, error) {
	if out(id) {
		return evidence.Item{}, fmt.Errorf("%s is left out of what models read", id)
	}
	it, err := c.ev.Describe(id)
	if errors.Is(err, evidence.ErrNotFound) {
		return evidence.Item{}, fmt.Errorf("no evidence %s", id)
	}
	return it, err
}

func listEvidence(c *openCase, out func(string) bool, p map[string]any) collect.Record {
	started := time.Now()
	all, err := c.ev.List()
	if err != nil {
		return collect.Unavailable(toolWhat(toolList, p), err.Error())
	}
	kind, contains := strParam(p, "type"), strParam(p, "contains")
	var match []evidence.Item
	for i := len(all) - 1; i >= 0; i-- {
		it := all[i]
		if out(it.Ref.ID) || (kind != "" && it.Ref.Type != kind) ||
			(contains != "" && !strings.Contains(it.Ref.Provenance.Source, contains) && !strings.Contains(it.Location, contains)) {
			continue
		}
		match = append(match, it)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s match, newest first", text.Plural(len(match), "item"))
	cut := len(match) > listMax
	if cut {
		fmt.Fprintf(&b, "; the first %d shown, narrow with type or contains", listMax)
		match = match[:listMax]
	}
	b.WriteString("\n")
	for _, it := range match {
		where, _, _ := strings.Cut(it.Location, "\n")
		fmt.Fprintf(&b, "%s %s · %d bytes, %d lines · %s", it.Ref.ID, it.Ref.Type, it.Size.Bytes, it.Size.Lines, it.Ref.Provenance.Source)
		if where != "" {
			fmt.Fprintf(&b, " · %s", text.Ellipsis(where, 120))
		}
		b.WriteString("\n")
	}
	return toolResult(toolList, p, nil, started, []byte(b.String()), cut)
}

func readEvidence(ctx context.Context, c *openCase, out func(string) bool, p map[string]any) (collect.Record, []linesRead) {
	started := time.Now()
	id := strParam(p, "id")
	from, n := max(intParam(p, "from", 1), 1), min(max(intParam(p, "lines", 200), 1), readMaxLines)
	it, err := readable(c, out, id)
	if err != nil {
		return collect.Unavailable(toolWhat(toolRead, p), err.Error()), nil
	}
	r, err := c.ev.Open(id)
	if err != nil {
		return collect.Unavailable(toolWhat(toolRead, p), err.Error()), nil
	}
	defer r.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	width := len(strconv.Itoa(from + n - 1))
	var body strings.Builder
	last, cut := 0, false
	for i := 1; i < from+n; i++ {
		if ctx.Err() != nil {
			return collect.Unavailable(toolWhat(toolRead, p), ctx.Err().Error()), nil
		}
		line, err := text.ReadLine(br, lineMaxBytes)
		if err != nil && line == "" {
			break
		}
		if i >= from {
			if body.Len()+len(line) > toolMaxBytes {
				cut = true
				break
			}
			fmt.Fprintf(&body, "%*d  %s\n", width, i, line)
			last = i
		}
		if err != nil {
			break
		}
	}
	head := fmt.Sprintf("%s (%s, %s)", id, it.Ref.Type, text.Plural(int(it.Size.Lines), "line"))
	var read []linesRead
	switch {
	case last == 0:
		head += fmt.Sprintf(": nothing from line %d", from)
	default:
		head += fmt.Sprintf(": lines %d–%d", from, last)
		read = []linesRead{{id, from, last}}
	}
	if cut {
		head += fmt.Sprintf("; cut at %d KiB, read on from line %d", toolMaxBytes>>10, last+1)
	}
	return toolResult(toolRead, p, []string{id}, started, []byte(head+"\n"+body.String()), cut), read
}

func grepEvidence(ctx context.Context, c *openCase, out func(string) bool, p map[string]any) (collect.Record, []linesRead) {
	started := time.Now()
	re, err := regexp.Compile(strParam(p, "pattern"))
	if err != nil {
		return collect.Unavailable(toolWhat(toolGrep, p), "the pattern does not compile: "+err.Error()), nil
	}
	around := min(max(intParam(p, "context", 0), 0), 5)
	limit := min(max(intParam(p, "max", 50), 1), 200)
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Fields(strParam(p, "ids")) {
		if !evidenceRef.MatchString(id) {
			return collect.Unavailable(toolWhat(toolGrep, p), fmt.Sprintf("%q is not an evidence ID", id)), nil
		}
		if !seen[id] && len(ids) < grepMaxItems {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	var b strings.Builder
	var read, refused []string
	var given []linesRead
	matches, cut := 0, false
	for _, id := range ids {
		if ctx.Err() != nil {
			return collect.Unavailable(toolWhat(toolGrep, p), ctx.Err().Error()), nil
		}
		it, err := readable(c, out, id)
		if err != nil {
			refused = append(refused, err.Error())
			fmt.Fprintf(&b, "== %s: %v\n", id, err)
			continue
		}
		read = append(read, id)
		found, scanCut, full := grepItem(ctx, c, id, re, around, limit-matches, toolMaxBytes-b.Len())
		fmt.Fprintf(&b, "== %s (%s, %s): %s\n", id, it.Ref.Type, text.Plural(int(it.Size.Lines), "line"), matchCount(found.n))
		b.WriteString(found.text)
		given = append(given, found.lines...)
		matches += found.n
		if scanCut {
			fmt.Fprintf(&b, "[only the first %d MiB of %s were read]\n", grepScan>>20, id)
		}
		if full {
			cut = true
			fmt.Fprintf(&b, "[stopped at %s or %d KiB in all; narrow the pattern or the items]\n", matchCount(limit), toolMaxBytes>>10)
			break
		}
		cut = cut || scanCut
	}
	if len(read) == 0 {
		return collect.Unavailable(toolWhat(toolGrep, p), strings.Join(refused, "; ")), nil
	}
	sort.Strings(read)
	return toolResult(toolGrep, p, read, started, []byte(b.String()), cut), given
}

func matchCount(n int) string {
	if n == 1 {
		return "1 match"
	}
	return fmt.Sprintf("%d matches", n)
}

// grepFound is what grepItem found: its text, the matches it holds and
// the lines it gives, matches and context, as ranges.
type grepFound struct {
	text  string
	n     int
	lines []linesRead
}

// grepItem finds re's matches in one item, with around lines of context,
// at most limit matches and room bytes; it reports whether it stopped at
// its scan bound, and whether at the match or byte limit.
func grepItem(ctx context.Context, c *openCase, id string, re *regexp.Regexp, around, limit, room int) (grepFound, bool, bool) {
	r, err := c.ev.Open(id)
	if err != nil {
		return grepFound{text: err.Error() + "\n"}, false, false
	}
	defer r.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	var b strings.Builder
	var before []string
	var given []linesRead
	// gave counts line n as given, in a range with the one before it.
	gave := func(n int) {
		if k := len(given) - 1; k >= 0 && given[k].to == n-1 {
			given[k].to = n
			return
		}
		given = append(given, linesRead{id, n, n})
	}
	scanned, n, after, lastShown := 0, 0, 0, 0
	for i := 1; ; i++ {
		if ctx.Err() != nil {
			break
		}
		line, err := text.ReadLine(br, lineMaxBytes)
		if err != nil && line == "" {
			break
		}
		scanned += len(line) + 1
		emit := func(sep string) bool {
			l := fmt.Sprintf("%d%s %s\n", i, sep, line)
			if b.Len()+len(l) > room {
				return false
			}
			b.WriteString(l)
			gave(i)
			lastShown = i
			return true
		}
		switch {
		case re.MatchString(line):
			if n >= limit {
				return grepFound{b.String(), n, given}, false, true
			}
			// The match with the context before it, a gap since the last
			// lines shown marked.
			var block strings.Builder
			if lastShown > 0 && i-len(before) > lastShown+1 {
				block.WriteString("--\n")
			}
			for k, l := range before {
				fmt.Fprintf(&block, "%d- %s\n", i-len(before)+k, l)
			}
			fmt.Fprintf(&block, "%d: %s\n", i, line)
			if b.Len()+block.Len() > room {
				return grepFound{b.String(), n, given}, false, true
			}
			b.WriteString(block.String())
			for k := i - len(before); k <= i; k++ {
				gave(k)
			}
			before, lastShown = nil, i
			n++
			after = around
		case after > 0:
			after--
			if !emit("-") {
				return grepFound{b.String(), n, given}, false, true
			}
		default:
			if around > 0 {
				before = append(before, line)
				if len(before) > around {
					before = before[1:]
				}
			}
		}
		if err != nil {
			break
		}
		if scanned >= grepScan {
			return grepFound{b.String(), n, given}, true, false
		}
	}
	return grepFound{b.String(), n, given}, false, false
}

var evidenceRef = regexp.MustCompile(`^ev-[0-9a-f]{16}$`)

// IsEvidenceID reports whether s has the form of an evidence item's ID.
func IsEvidenceID(s string) bool { return evidenceRef.MatchString(s) }

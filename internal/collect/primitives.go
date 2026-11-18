package collect

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/itchyny/gojq"
	"go.yaml.in/yaml/v3"
)

type primEnv struct {
	ctx  context.Context
	root *artifactRoot
	out  *cappedBuffer
	// input is the previous stage's output; hasInput is false for the
	// first stage.
	input    []byte
	hasInput bool
	// vars are the test's parameters, visible to jq and yq as $name.
	vars        map[string]any
	maxFileSize int64
	maxLineSize int
	// inputCut is set when a file was read only to the read bound, so
	// the stage's result is marked truncated, never complete.
	inputCut bool
	// given are the files the stage was given, in reading order; opened
	// counts those it opened; at is the file and line it is reading.
	given  []string
	opened int
	at     string
	atLine int
	// stopBy is set by a primitive that stops at its own limit, notes by
	// one with something more to say about what it returns.
	stopBy string
	notes  []string
}

func (e *primEnv) note(s string) { e.notes = append(e.notes, s) }

// stop says how far the stage got when it ended by err, or nil when it
// read all it was given and nothing else needs saying.
func (e *primEnv) stop(err error, timedOut bool) *Stop {
	s := &Stop{Notes: e.notes}
	switch {
	case timedOut:
		s.By = "timeout"
	case e.stopBy != "":
		s.By = e.stopBy
	case errors.Is(err, errOutputFull):
		s.By = "output_cap"
	case e.inputCut:
		s.By = "read_bound"
	case len(e.notes) > 0:
		s.By = "match_limit"
	default:
		return nil
	}
	if s.By != "read_bound" && e.at != "" {
		s.At = fmt.Sprintf("%s at line %d", e.at, e.atLine)
	}
	if len(e.given) > 1 {
		s.FilesGiven, s.FilesRead = len(e.given), e.opened
		if s.By != "read_bound" {
			rest := e.given[min(e.opened, len(e.given)):]
			s.NotReached = rest[:min(len(rest), MaxNotReached)]
			s.MoreNotReached = len(rest) - len(s.NotReached)
		}
	}
	return s
}

// ctxReader stops reading once its context ends, so a collection timed
// out or cancelled stops at its next read, even one that writes nothing.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// boundedReader reads at most n bytes and, when the source holds more,
// sets *cut.
type boundedReader struct {
	r   io.Reader
	n   int64
	cut *bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n <= 0 {
		var one [1]byte
		if n, _ := b.r.Read(one[:]); n > 0 {
			*b.cut = true
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.n {
		p = p[:b.n]
	}
	n, err := b.r.Read(p)
	b.n -= int64(n)
	return n, err
}

// bounded is r read to the file bound, stopping when the context ends.
func (e *primEnv) bounded(r io.Reader) io.Reader {
	return &boundedReader{r: ctxReader{e.ctx, r}, n: e.maxFileSize, cut: &e.inputCut}
}

func runPrimitive(name string, p any, e *primEnv) error {
	switch name {
	case "find":
		return runFind(p.(*findParams), e)
	case "grep":
		return runGrep(p.(*grepParams), e)
	case "read":
		return runRead(p.(*readParams), e)
	case "jq":
		return runQuery(p.(*queryParams), e, false)
	case "yq":
		return runQuery(p.(*queryParams), e, true)
	case "logs.window":
		return runWindow(p.(*windowParams), e)
	case "logs.count":
		return runCount(p.(*countParams), e)
	case "aggregate":
		return runAggregate(p.(*aggregateParams), e)
	case "diff":
		return runDiff(p.(*diffParams), e)
	case "archive.list":
		return runArchiveList(p.(*archiveParams), e)
	}
	return fmt.Errorf("unknown primitive %q", name)
}

// source is one named input to a line-oriented primitive.
type source struct {
	name string
	r    io.Reader
	done func()
}

// sources returns the files under path matching glob, or the previous
// stage's output when no path is given and there is one.
func (e *primEnv) sources(p, glob string) ([]source, error) {
	if p == "" && e.hasInput {
		return []source{{name: "-", r: bytes.NewReader(e.input), done: func() {}}}, nil
	}
	files, err := e.root.files(p, glob)
	if err != nil {
		return nil, err
	}
	e.given = append(e.given, files...)
	out := make([]source, len(files))
	for i, name := range files {
		out[i] = source{name: name}
	}
	return out, nil
}

// open returns a reader for s, opening files lazily so only one is open at
// a time. A gzipped file (.gz, not a tar) is read decompressed, the read
// bound counting decompressed bytes, so a file that decompresses without
// end is cut where a plain one would be.
func (e *primEnv) open(s *source) error {
	if s.r != nil {
		return nil
	}
	f, err := e.root.open(s.name)
	if err != nil {
		return err
	}
	if gzipped(s.name) {
		// The compressed bytes are bounded too: empty members decompress
		// to nothing, so a bound on output alone would not stop them.
		gz, err := gzip.NewReader(e.bounded(f))
		if err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", s.name, err)
		}
		s.r = e.bounded(gz)
		s.done = func() { gz.Close(); f.Close() }
		return nil
	}
	s.r = e.bounded(f)
	s.done = func() { f.Close() }
	return nil
}

// gzipped reports a gzipped file a line primitive reads decompressed; a
// gzipped tar is an archive, for archive.list.
func gzipped(name string) bool {
	return strings.HasSuffix(name, ".gz") && !strings.HasSuffix(name, ".tar.gz")
}

// lines calls fn for each line of s, numbered from 1.
func (e *primEnv) lines(s *source, fn func(n int, line string) error) error {
	if err := e.open(s); err != nil {
		return err
	}
	defer s.done()
	if s.name != "-" {
		e.opened++
	}
	e.at, e.atLine = s.name, 0
	cutBefore := e.inputCut
	defer func() {
		if e.inputCut && !cutBefore {
			e.note(fmt.Sprintf("%s was read only to its first %d bytes", s.name, e.maxFileSize))
		}
	}()
	sc := bufio.NewScanner(s.r)
	sc.Buffer(make([]byte, 64*1024), e.maxLineSize)
	n := 0
	for sc.Scan() {
		n++
		e.atLine = n
		if n%1024 == 0 {
			if err := e.ctx.Err(); err != nil {
				return err
			}
		}
		if err := fn(n, sc.Text()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", s.name, err)
	}
	return nil
}

func runFind(p *findParams, e *primEnv) error {
	c, err := e.root.path(p.Path)
	if err != nil {
		return err
	}
	glob := p.Glob
	if glob == "" {
		glob = "*"
	}
	kind := p.Type
	if kind == "" {
		kind = "file"
	}
	if kind != "file" && kind != "dir" && kind != "any" {
		return fmt.Errorf("find: type must be file, dir or any, not %q", kind)
	}
	var newer, older time.Time
	if p.NewerThan != "" {
		if newer, err = time.Parse(time.RFC3339, p.NewerThan); err != nil {
			return fmt.Errorf("find: newer_than: %w", err)
		}
	}
	if p.OlderThan != "" {
		if older, err = time.Parse(time.RFC3339, p.OlderThan); err != nil {
			return fmt.Errorf("find: older_than: %w", err)
		}
	}
	max := p.MaxResults
	if max <= 0 {
		max = 1000
	}
	found := 0
	err = walk(e.root, c, func(rel string, isDir bool, size int64, mod time.Time) error {
		if rel == c && isDir {
			return nil
		}
		// A directory at the maximum depth is listed if it matches, and
		// never entered.
		var next error
		depth := findDepth(c, rel)
		if isDir && p.MaxDepth > 0 && depth >= p.MaxDepth {
			next = fs.SkipDir
		}
		if depth < p.MinDepth {
			return next
		}
		if (kind == "file" && isDir) || (kind == "dir" && !isDir) {
			return next
		}
		if ok, _ := matchBase(glob, rel); !ok {
			return next
		}
		if !isDir && ((p.MinSize > 0 && size < p.MinSize) || (p.MaxSize > 0 && size > p.MaxSize)) {
			return next
		}
		if (!newer.IsZero() && !mod.After(newer)) || (!older.IsZero() && !mod.Before(older)) {
			return next
		}
		if found == max {
			e.out.truncated, e.stopBy = true, "match_limit"
			return errOutputFull
		}
		found++
		line := rel
		if p.Details {
			line = findDetails(isDir, size, mod) + rel
		}
		if err := e.out.line(line); err != nil {
			return err
		}
		return next
	})
	return err
}

// findDepth is rel's depth below the walk's start, start itself 0.
func findDepth(start, rel string) int {
	if rel == start {
		return 0
	}
	if start != "." {
		rel = strings.TrimPrefix(rel, start+"/")
	}
	return strings.Count(rel, "/") + 1
}

// findDetails is a detail line's fields before the path: kind, size in
// bytes (- for a directory, whose size the file system decides) and
// modification time in UTC, to the nanosecond the file carries, so the time
// printed is the one newer_than and older_than compare.
func findDetails(isDir bool, size int64, mod time.Time) string {
	kind, n := "file", strconv.FormatInt(size, 10)
	if isDir {
		kind, n = "dir", "-"
	}
	return kind + "\t" + n + "\t" + mod.UTC().Format(time.RFC3339Nano) + "\t"
}

func runGrep(p *grepParams, e *primEnv) error {
	pattern := p.Pattern
	if p.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("grep: %w", err)
	}
	max := p.MaxMatches
	if max <= 0 {
		max = 1000
	}
	srcs, err := e.sources(p.Path, p.Glob)
	if err != nil {
		return err
	}
	switch p.From {
	case "", "start":
	case "end":
		return grepLast(re, p, max, srcs, e)
	default:
		return fmt.Errorf("grep: from must be start or end, not %q", p.From)
	}
	matches := 0
	for i := range srcs {
		s := &srcs[i]
		var before []string
		after := 0
		lastPrinted := 0
		emit := func(n int, sep, text string) error {
			lastPrinted = n
			return e.out.line(fmt.Sprintf("%s%s%d%s%s", s.name, sep, n, sep, text))
		}
		err := e.lines(s, func(n int, line string) error {
			if re.MatchString(line) {
				if matches == max {
					e.out.truncated, e.stopBy = true, "match_limit"
					return errOutputFull
				}
				matches++
				for j, b := range before {
					ln := n - len(before) + j
					if ln > lastPrinted {
						if err := emit(ln, "-", b); err != nil {
							return err
						}
					}
				}
				before = before[:0]
				after = p.ContextAfter
				return emit(n, ":", line)
			}
			if after > 0 {
				after--
				return emit(n, "-", line)
			}
			if p.ContextBefore > 0 {
				before = append(before, line)
				if len(before) > p.ContextBefore {
					before = before[1:]
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// grepLast keeps the last max matches of the whole input, each with its
// context, and prints them in the order they were read: what
// happened at the end — just before a crash or a restart — rather than
// at the start. The matches kept are held with their context, the oldest
// dropped once what is held reaches the output cap, since no more could
// be printed. Earlier matches left out mark the result truncated, and its
// stop says how many were kept of how many.
func grepLast(re *regexp.Regexp, p *grepParams, max int, srcs []source, e *primEnv) error {
	type line struct {
		file string
		n    int
		sep  string
		text string
	}
	var groups [][]line
	held, total := 0, 0
	size := func(g []line) int {
		n := 0
		for _, l := range g {
			n += len(l.file) + len(l.text) + 16
		}
		return n
	}
	for i := range srcs {
		s := &srcs[i]
		var before []line
		after := 0
		err := e.lines(s, func(n int, text string) error {
			if re.MatchString(text) {
				total++
				g := append(append([]line{}, before...), line{s.name, n, ":", text})
				before = before[:0]
				after = p.ContextAfter
				groups = append(groups, g)
				held += size(g)
				for len(groups) > max || (held > e.out.limit && len(groups) > 1) {
					held -= size(groups[0])
					groups = groups[1:]
				}
				return nil
			}
			if after > 0 {
				after--
				l := line{s.name, n, "-", text}
				groups[len(groups)-1] = append(groups[len(groups)-1], l)
				held += size([]line{l})
				return nil
			}
			if p.ContextBefore > 0 {
				before = append(before, line{s.name, n, "-", text})
				if len(before) > p.ContextBefore {
					before = before[1:]
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(groups) < total {
		e.out.truncated = true
		e.note(fmt.Sprintf("kept the last %d of %d matches; the earlier ones are left out", len(groups), total))
	}
	// A line in two matches' context is printed once, in place.
	type at struct {
		file string
		n    int
	}
	printed := map[at]bool{}
	for _, g := range groups {
		for _, l := range g {
			if printed[at{l.file, l.n}] {
				continue
			}
			printed[at{l.file, l.n}] = true
			if err := e.out.line(fmt.Sprintf("%s%s%d%s%s", l.file, l.sep, l.n, l.sep, l.text)); err != nil {
				return err
			}
		}
	}
	return nil
}

func runRead(p *readParams, e *primEnv) error {
	modes := 0
	for _, set := range []bool{p.StartLine > 0 || p.EndLine > 0, p.Head > 0, p.Tail > 0, p.StartByte > 0 || p.EndByte > 0} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("read: give one of a line range, head, tail or a byte range")
	}
	s := source{name: p.Path}
	if c, err := clean(p.Path); err != nil {
		return err
	} else {
		s.name = c
	}
	switch {
	case p.StartByte > 0 || p.EndByte > 0:
		if gzipped(s.name) {
			return errors.New("read: a byte range of a gzipped file is not offered; give a line range, head or tail")
		}
		if p.EndByte > 0 && p.EndByte <= p.StartByte {
			return errors.New("read: end_byte must be after start_byte")
		}
		f, err := e.root.open(s.name)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Seek(p.StartByte, io.SeekStart); err != nil {
			return err
		}
		var r io.Reader = f
		if p.EndByte > 0 {
			r = io.LimitReader(f, p.EndByte-p.StartByte)
		}
		_, err = io.Copy(e.out, r)
		return err
	case p.Tail > 0:
		ring := make([]string, 0, p.Tail)
		if err := e.lines(&s, func(_ int, line string) error {
			if len(ring) == p.Tail {
				ring = ring[1:]
			}
			ring = append(ring, line)
			return nil
		}); err != nil {
			return err
		}
		for _, l := range ring {
			if err := e.out.line(l); err != nil {
				return err
			}
		}
		return nil
	}
	start, end := p.StartLine, p.EndLine
	if p.Head > 0 {
		start, end = 1, p.Head
	}
	if start <= 0 {
		start = 1
	}
	if end > 0 && end < start {
		return errors.New("read: end_line must not be before start_line")
	}
	errDone := errors.New("done")
	err := e.lines(&s, func(n int, line string) error {
		if end > 0 && n > end {
			return errDone
		}
		if n >= start {
			return e.out.line(line)
		}
		return nil
	})
	if err == errDone {
		return nil
	}
	return err
}

// runQuery runs a jq filter over JSON (jq) or YAML (yq) documents from the
// files under path, or from the previous stage. Test parameters are bound
// as $name variables, so a filter never has a value spliced into it.
func runQuery(p *queryParams, e *primEnv, isYAML bool) error {
	q, err := gojq.Parse(p.Filter)
	if err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	names := make([]string, 0, len(e.vars))
	for k := range e.vars {
		names = append(names, k)
	}
	sort.Strings(names)
	vars := make([]string, len(names))
	values := make([]any, len(names))
	for i, n := range names {
		vars[i] = "$" + n
		values[i] = normalize(e.vars[n])
	}
	code, err := gojq.Compile(q, gojq.WithVariables(vars))
	if err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	run := func(data []byte) error {
		docs, err := decodeDocs(data, isYAML)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			iter := code.RunWithContext(e.ctx, doc, values...)
			for {
				v, ok := iter.Next()
				if !ok {
					break
				}
				if err, ok := v.(error); ok {
					var halt *gojq.HaltError
					if errors.As(err, &halt) && halt.Value() == nil {
						break
					}
					return fmt.Errorf("filter: %w", err)
				}
				if err := writeValue(e.out, v, p.Raw); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if p.Path == "" && e.hasInput {
		return run(e.input)
	}
	if p.Path == "" {
		return errors.New("no input: give a path, or put this stage after another")
	}
	files, err := e.root.files(p.Path, p.Glob)
	if err != nil {
		return err
	}
	// One file at a time: only one file's bytes and documents are held,
	// however many the path matches.
	for _, f := range files {
		data, err := e.root.readAll(e.ctx, f, e.maxFileSize)
		if err != nil {
			return err
		}
		if err := run(data); err != nil {
			return err
		}
	}
	return nil
}

// decodeDocs decodes every JSON value or YAML document in data into the
// value form gojq expects.
func decodeDocs(data []byte, isYAML bool) ([]any, error) {
	var docs []any
	if isYAML {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var v any
			if err := dec.Decode(&v); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("yaml: %w", err)
			}
			// Round-trip through JSON so YAML-specific types become plain
			// JSON values.
			j, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("yaml: %w", err)
			}
			docs = append(docs, nil)
			if docs[len(docs)-1], err = decodeJSON(j); err != nil {
				return nil, err
			}
		}
		return docs, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		var v any
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("json: %w", err)
		}
		docs = append(docs, normalize(v))
	}
	return docs, nil
}

func decodeJSON(j []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normalize(v), nil
}

// normalize converts json.Number to int where it fits exactly, else float64,
// recursively; gojq accepts those.
func normalize(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.Atoi(x.String()); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = normalize(e)
		}
	case []any:
		for i, e := range x {
			x[i] = normalize(e)
		}
	case int64:
		return int(x)
	}
	return v
}

func writeValue(out *cappedBuffer, v any, raw bool) error {
	if s, ok := v.(string); ok && raw {
		return out.line(s)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := out.Write(b.Bytes())
	return err
}

// Leading timestamp layouts tried by logs.window when none is given.
var timestampLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"}

// stampReader reads the time a log line begins with.
type stampReader struct {
	layouts []string
	skip    int
	anchor  time.Time // zero when no anchor was given
	zone    *time.Location
}

// leadingTime parses the stamp at the start of line, after skip
// characters. A layout matches the line's prefix of as many fields as
// the layout has, so a space-padded day (_2) may be preceded by a run of
// spaces; text that closes the stamp, such as a bracket, need not be in
// the layout. A stamp whose layout has no zone is in the reader's zone; one
// with no year takes it from the anchor.
func (r stampReader) leadingTime(line string) (time.Time, bool) {
	for i := 0; i < r.skip; i++ {
		_, n := utf8.DecodeRuneInString(line)
		if n == 0 {
			return time.Time{}, false
		}
		line = line[n:]
	}
	for _, l := range r.layouts {
		candidate, ok := fieldPrefix(line, len(strings.Fields(l)))
		if !ok {
			continue
		}
		t, err := time.ParseInLocation(l, candidate, r.zone)
		if err != nil {
			trimmed := strings.TrimRightFunc(candidate, func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) })
			if trimmed == candidate || trimmed == "" {
				continue
			}
			if t, err = time.ParseInLocation(l, trimmed, r.zone); err != nil {
				continue
			}
		}
		if t.Year() == 0 && !r.anchor.IsZero() {
			if t, ok = r.dated(t); !ok {
				continue
			}
		}
		return t, true
	}
	return time.Time{}, false
}

// dated gives a yearless stamp the latest year that puts it no more than
// a day after the anchor. It looks at one line at a time, so it holds for
// unordered input; a stamp more than a year older than the anchor is
// misdated, as nothing on the line says otherwise.
func (r stampReader) dated(t time.Time) (time.Time, bool) {
	limit := r.anchor.Add(24 * time.Hour)
	for y := limit.Year(); y >= limit.Year()-8; y-- {
		d := time.Date(y, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
		if d.Month() != t.Month() || d.After(limit) {
			continue // 29 February in a common year, or after the anchor
		}
		return d, true
	}
	return time.Time{}, false
}

// fieldPrefix is line up to the end of its nth whitespace-separated field.
func fieldPrefix(line string, n int) (string, bool) {
	i := 0
	for f := 0; f < n; f++ {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i == len(line) {
			return "", false
		}
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
	}
	return line[:i], true
}

func newStampReader(p *windowParams) (stampReader, error) {
	r := stampReader{layouts: timestampLayouts, skip: p.Skip, zone: time.UTC}
	if p.Layout != "" {
		r.layouts = []string{p.Layout}
	}
	if p.Skip < 0 {
		return r, errors.New("logs.window: skip must not be negative")
	}
	if p.Anchor != "" {
		a, err := time.Parse(time.RFC3339, p.Anchor)
		if err != nil {
			return r, fmt.Errorf("logs.window: anchor: %w", err)
		}
		r.anchor = a
	}
	if p.Zone != "" {
		z, err := zone(p.Zone)
		if err != nil {
			return r, fmt.Errorf("logs.window: zone: %w", err)
		}
		r.zone = z
	}
	return r, nil
}

var reOffset = regexp.MustCompile(`^([+-])(\d{2}):?(\d{2})$`)

// zone is an IANA zone or a fixed offset, +HH:MM or +HHMM.
func zone(name string) (*time.Location, error) {
	if m := reOffset.FindStringSubmatch(name); m != nil {
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		off := h*3600 + mi*60
		if m[1] == "-" {
			off = -off
		}
		return time.FixedZone(name, off), nil
	}
	return time.LoadLocation(name)
}

// runWindow keeps lines whose leading timestamp is in [start, end). A line
// without a timestamp, such as a stack-trace continuation, follows the
// decision for the line before it. A file with lines of which none has a
// stamp the layout reads fails the stage: that is a layout that does not
// fit, and an empty window would read as no activity.
func runWindow(p *windowParams, e *primEnv) error {
	start, err := time.Parse(time.RFC3339, p.Start)
	if err != nil {
		return fmt.Errorf("logs.window: start: %w", err)
	}
	end, err := time.Parse(time.RFC3339, p.End)
	if err != nil {
		return fmt.Errorf("logs.window: end: %w", err)
	}
	if !end.After(start) {
		return errors.New("logs.window: end must be after start")
	}
	stamps, err := newStampReader(p)
	if err != nil {
		return err
	}
	srcs, err := e.sources(p.Path, "")
	if err != nil {
		return err
	}
	for i := range srcs {
		keep, lines, stamped := false, 0, 0
		if err := e.lines(&srcs[i], func(_ int, line string) error {
			lines++
			if t, ok := stamps.leadingTime(line); ok {
				stamped++
				keep = !t.Before(start) && t.Before(end)
			}
			if keep {
				return e.out.line(line)
			}
			return nil
		}); err != nil {
			return err
		}
		if lines > 0 && stamped == 0 {
			name := srcs[i].name
			if name == "-" {
				name = "the previous stage's output"
			}
			layout := "any default layout"
			if p.Layout != "" {
				layout = fmt.Sprintf("layout %q", p.Layout)
			}
			return fmt.Errorf("logs.window: no line of %s has a stamp %s reads (skip %d)", name, layout, p.Skip)
		}
	}
	return nil
}

var (
	reTimestamp = regexp.MustCompile(`^\S*\d{2}:\d{2}:\d{2}\S*\s+`)
	reVolatile  = regexp.MustCompile(`0x[0-9a-fA-F]+|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|\d+`)
)

// runCount groups lines that differ only in timestamps, numbers, hex values
// and UUIDs, and reports each group's count with its first and last
// occurrence.
func runCount(p *countParams, e *primEnv) error {
	var re *regexp.Regexp
	if p.Pattern != "" {
		var err error
		if re, err = regexp.Compile(p.Pattern); err != nil {
			return fmt.Errorf("logs.count: %w", err)
		}
	}
	max := p.MaxGroups
	if max <= 0 {
		max = 100
	}
	type group struct {
		Count  int    `json:"count"`
		First  string `json:"first"`
		Last   string `json:"last"`
		Sample string `json:"sample"`
		key    string
	}
	groups := map[string]*group{}
	srcs, err := e.sources(p.Path, "")
	if err != nil {
		return err
	}
	for i := range srcs {
		s := &srcs[i]
		if err := e.lines(s, func(n int, line string) error {
			if re != nil && !re.MatchString(line) {
				return nil
			}
			key := reVolatile.ReplaceAllString(reTimestamp.ReplaceAllString(line, ""), "#")
			loc := s.name + ":" + strconv.Itoa(n)
			g, ok := groups[key]
			if !ok {
				g = &group{First: loc, Sample: line, key: key}
				groups[key] = g
			}
			g.Count++
			g.Last = loc
			return nil
		}); err != nil {
			return err
		}
	}
	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].key < list[j].key
	})
	if len(list) > max {
		list = list[:max]
		e.out.truncated = true
	}
	for _, g := range list {
		if err := writeValue(e.out, map[string]any{"count": g.Count, "first": g.First, "last": g.Last, "sample": g.Sample}, false); err != nil {
			return err
		}
	}
	return nil
}

func runAggregate(p *aggregateParams, e *primEnv) error {
	if !e.hasInput {
		return errors.New("aggregate: needs a previous stage")
	}
	var code *gojq.Code
	if p.Field != "" {
		q, err := gojq.Parse(p.Field)
		if err != nil {
			return fmt.Errorf("aggregate: field: %w", err)
		}
		if code, err = gojq.Compile(q); err != nil {
			return fmt.Errorf("aggregate: field: %w", err)
		}
	}
	var keys []string
	sc := bufio.NewScanner(bytes.NewReader(e.input))
	sc.Buffer(make([]byte, 64*1024), e.maxLineSize)
	for sc.Scan() {
		line := sc.Text()
		if code == nil {
			keys = append(keys, line)
			continue
		}
		v, err := decodeJSON([]byte(line))
		if err != nil {
			return fmt.Errorf("aggregate: field needs JSON lines: %w", err)
		}
		iter := code.RunWithContext(e.ctx, v)
		for {
			r, ok := iter.Next()
			if !ok {
				break
			}
			if err, ok := r.(error); ok {
				return fmt.Errorf("aggregate: field: %w", err)
			}
			if s, ok := r.(string); ok {
				keys = append(keys, s)
			} else {
				j, _ := json.Marshal(r)
				keys = append(keys, string(j))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	switch p.Op {
	case "count":
		return e.out.line(strconv.Itoa(len(keys)))
	case "unique", "count_by":
		counts := map[string]int{}
		for _, k := range keys {
			counts[k]++
		}
		uniq := make([]string, 0, len(counts))
		for k := range counts {
			uniq = append(uniq, k)
		}
		sort.Strings(uniq)
		if p.Op == "unique" {
			for _, k := range uniq {
				if err := e.out.line(k); err != nil {
					return err
				}
			}
			return nil
		}
		sort.SliceStable(uniq, func(i, j int) bool { return counts[uniq[i]] > counts[uniq[j]] })
		for _, k := range uniq {
			if err := writeValue(e.out, map[string]any{"value": k, "count": counts[k]}, false); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("aggregate: op must be count, count_by or unique, not %q", p.Op)
}

package cli

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// The command line's output is painted for a terminal: headings bold,
// evidence IDs faint, warnings yellow. Painting only adds escape codes,
// so the text is the same; and it is applied only when the output is a
// terminal and NO_COLOR is unset (no-color.org), so pipes, scripts and
// --json see exactly the plain text.

const (
	sgrBold   = "\x1b[1m"
	sgrFaint  = "\x1b[2m"
	sgrYellow = "\x1b[33m"
	sgrReset  = "\x1b[0m"
)

// painter is a writer that paints each whole line it is given.
type painter struct {
	w   io.Writer
	buf []byte
}

// paintFor is a painter over out when out is a terminal, NO_COLOR is
// unset and the command prints neither JSON nor a view of its own; nil
// otherwise.
func paintFor(out io.Writer, args []string) *painter {
	f, ok := out.(*os.File)
	if !ok || !isTerminal(f) || os.Getenv("NO_COLOR") != "" || slices.Contains(args, "--json") || slices.Contains(args, "--show") {
		return nil
	}
	return &painter{w: out}
}

func (p *painter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := string(p.buf[:i])
		p.buf = p.buf[i+1:]
		if _, err := io.WriteString(p.w, paintLine(line)+"\n"); err != nil {
			return 0, err
		}
	}
}

// flush writes a last line that ended without a newline.
func (p *painter) flush() error {
	if len(p.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(p.w, paintLine(string(p.buf)))
	p.buf = nil
	return err
}

var (
	evidenceID  = regexp.MustCompile(`\bev-[0-9a-f]{16}\b`)
	tableHeader = regexp.MustCompile(`^[A-Z]{2,}( +[A-Z]{2,})+$`)
	labelOnly   = regexp.MustCompile(`^[^ :][^:]*:$`)
)

// warnings are the beginnings of lines that tell the reader to take care:
// no valid answer, what a model was not shown, what was dropped.
var warnings = []string{"no valid answer", "not shown:", "could not tell:", "dropped ", "rests on nothing", "unsupported:"}

// paintLine paints one line: a heading bold — a briefing's head, a line
// that is only a label, a table's header — a warning yellow, and evidence
// IDs faint.
func paintLine(line string) string {
	switch {
	case strings.HasPrefix(line, "── "), tableHeader.MatchString(line), labelOnly.MatchString(line):
		return sgrBold + line + sgrReset
	}
	for _, w := range warnings {
		if strings.HasPrefix(line, w) {
			return sgrYellow + line + sgrReset
		}
	}
	return evidenceID.ReplaceAllString(line, sgrFaint+"$0"+sgrReset)
}

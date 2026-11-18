// Package text holds the engine's small text helpers, for its command line
// and its clients.
package text

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/lowmagic/compa-engine/internal/escape"
)

// Safe escapes s for printing on a terminal: case and package text is
// untrusted, and an escape sequence printed raw would act on it.
func Safe(s string) string { return escape.Terminal(s) }

// Plural is n and its noun, in the plural as English writes it unless n
// is 1.
func Plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if stem, ok := strings.CutSuffix(noun, "y"); ok && stem != "" && !strings.ContainsAny(stem[len(stem)-1:], "aeiou") {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// FirstLine is s's first line, s trimmed of surrounding space first.
func FirstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// Ellipsis is s cut to n runes, its last an ellipsis, when it is longer.
func Ellipsis(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ReadLine reads one line, keeping at most limit bytes of it.
func ReadLine(br *bufio.Reader, limit int) (string, error) {
	var b []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if room := limit - len(b); room > 0 {
			b = append(b, chunk[:min(len(chunk), room)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return strings.TrimRight(string(b), "\r\n"), err
	}
}

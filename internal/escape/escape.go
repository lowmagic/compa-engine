// Package escape makes untrusted text safe to print on a terminal.
package escape

import (
	"fmt"
	"strings"
	"unicode"
)

// Terminal escapes control characters other than newline and tab. Case
// and package text is untrusted, and an escape sequence printed raw would
// act on the engineer's terminal.
func Terminal(s string) string {
	if strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafeRune(r) {
			fmt.Fprintf(&b, "\\x%02x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	return r != '\n' && r != '\t' && (unicode.IsControl(r) || r == unicode.ReplacementChar)
}

package chat

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// commandCompletion belongs to one chat model. Its state never enters the
// transcript; changing the input starts a fresh selection and reopens it.
type commandCompletion struct {
	input     string
	selected  int
	dismissed bool
	options   []commandSpec
}

// slashMatches only completes command names, not arguments or multiline input.
func slashMatches(input string) []commandSpec {
	if !strings.HasPrefix(input, "/") || strings.ContainsFunc(input, unicode.IsSpace) {
		return nil
	}
	prefix := input[1:]
	matches := make([]commandSpec, 0, len(commandCatalog))
	for _, c := range commandCatalog {
		if strings.HasPrefix(c.name, prefix) {
			matches = append(matches, c)
		}
	}
	return matches
}

func (c *commandCompletion) matches(input string) []commandSpec {
	if input != c.input {
		c.input, c.selected, c.dismissed = input, 0, false
		c.options = slashMatches(input)
	}
	if c.dismissed {
		return nil
	}
	return c.options
}

// key returns the replacement input, whether it consumed the key, and whether
// the caller should submit the replacement through its usual enter handler.
// It never executes a command itself. Exact typed commands pass Enter through,
// even when they accept arguments: /draft and /model still list their choices.
func (c *commandCompletion) key(input, key string) (value string, handled, submit bool) {
	matches := c.matches(input)
	if len(matches) == 0 {
		return input, false, false
	}
	c.selected = min(max(c.selected, 0), len(matches)-1)
	switch key {
	case "esc":
		c.dismissed = true
		return input, true, false
	case "up":
		c.selected = max(c.selected-1, 0)
		return input, true, false
	case "down":
		c.selected = min(c.selected+1, len(matches)-1)
		return input, true, false
	case "enter":
		if commandNamed(input[1:]).name != "" {
			return input, false, false
		}
	case "tab":
	default:
		return input, false, false
	}
	chosen := matches[c.selected]
	value = "/" + chosen.name
	if chosen.args != "" {
		value += " "
	}
	c.input, c.selected, c.dismissed = value, 0, true
	return value, true, key == "enter" && chosen.args == ""
}

// rows reserves its last row for keys and scrolls only the option window.
// room excludes the input, status and scrollback insertion row. With fewer
// than two available rows it draws nothing rather than displacing those rows.
func (c *commandCompletion) rows(input string, width, room int) []string {
	matches := c.matches(input)
	if len(matches) == 0 || room < 2 || width < 1 {
		return nil
	}
	c.selected = min(max(c.selected, 0), len(matches)-1)
	count := min(len(matches), room-1, 6)
	first := max(0, min(c.selected-count/2, len(matches)-count))
	rows := make([]string, 0, count+1)
	for i := first; i < first+count; i++ {
		mark := "  "
		if i == c.selected {
			mark = "› "
		}
		row := ansi.Truncate(mark+matches[i].label()+" — "+matches[i].description, width, "…")
		if i == c.selected {
			row = cursorW.Render(row)
		} else {
			row = faintW.Render(row)
		}
		rows = append(rows, row)
	}
	hint := "↑↓ select · tab complete · enter choose · esc close"
	rows = append(rows, faintW.Render(ansi.Truncate(hint, width, "…")))
	return rows
}

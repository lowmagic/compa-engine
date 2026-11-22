package chat

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// commandCompletion belongs to one chat model. Its state never enters the
// transcript; changing the input starts a fresh selection and reopens it.
// args, where set, gives a command's argument options once its name and a
// space are typed (argOptions); without it only names are completed.
type commandCompletion struct {
	input     string
	selected  int
	dismissed bool
	options   []completionOption
	args      func(input string) []completionOption
}

// completionOption is one row of the popover: what tab completes the
// input to, what enter runs, empty where enter completes as tab does, and
// how the row reads.
type completionOption struct {
	complete, run      string
	label, description string
}

// commandOption is a command's name as an option: completed with a space
// where it takes arguments, and run by enter where it takes none.
func commandOption(c commandSpec) completionOption {
	o := completionOption{complete: "/" + c.name, label: c.label(), description: c.description}
	if c.args != "" {
		o.complete += " "
	} else {
		o.run = o.complete
	}
	return o
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

// optionsFor are the options for input: the command names it begins, or,
// past a name and a space, the command's arguments it begins.
func (c *commandCompletion) optionsFor(input string) []completionOption {
	if names := slashMatches(input); len(names) > 0 {
		out := make([]completionOption, 0, len(names))
		for _, n := range names {
			out = append(out, commandOption(n))
		}
		return out
	}
	if c.args == nil || !strings.HasPrefix(input, "/") || strings.ContainsAny(input, "\n\t") {
		return nil
	}
	var out []completionOption
	for _, o := range c.args(input) {
		if strings.HasPrefix(o.complete, input) && o.complete != input && o.run != input {
			out = append(out, o)
		}
	}
	return out
}

// runs says whether input is an argument option typed whole, as /copy a1
// is where a10 is offered too, or /debug load with no expert: enter runs
// it as typed, as it does a command's name.
func (c *commandCompletion) runs(input string) bool {
	if c.args == nil {
		return false
	}
	for _, o := range c.args(input) {
		if o.run == input || strings.TrimSpace(o.complete) == input {
			return true
		}
	}
	return false
}

func (c *commandCompletion) matches(input string) []completionOption {
	if input != c.input {
		c.input, c.selected, c.dismissed = input, 0, false
		c.options = c.optionsFor(input)
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
// Enter on an option that runs submits it; tab completes it, and where the
// completion has options of its own, as /draft followed by a space, they
// show at once.
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
		if commandNamed(input[1:]).name != "" || c.runs(input) {
			return input, false, false
		}
	case "tab":
	default:
		return input, false, false
	}
	chosen := matches[c.selected]
	value, submit = chosen.complete, false
	if key == "enter" && chosen.run != "" {
		value, submit = chosen.run, true
	}
	c.input, c.selected = value, 0
	c.options = c.optionsFor(value)
	c.dismissed = submit || !strings.HasSuffix(value, " ") || len(c.options) == 0
	return value, true, submit
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
		row := mark + matches[i].label
		if matches[i].description != "" {
			row += " — " + matches[i].description
		}
		row = ansi.Truncate(row, width, "…")
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

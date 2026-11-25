package chat

import "charm.land/lipgloss/v2"

// The chat prints into the engineer's own terminal, whose background it
// does not know: its styles use the terminal's sixteen colours, faint and
// restrained bold (only headings, warnings and explicit **emphasis**), and
// paint no background of their own but the browser's marked lines, so they
// read on a light terminal and a dark one alike.
var (
	plainW   = lipgloss.NewStyle()
	faintW   = plainW.Faint(true)
	mutedW   = plainW.Foreground(lipgloss.BrightBlack)
	h1W      = plainW.Bold(true).Foreground(lipgloss.Blue)
	headingW = plainW.Bold(true).Foreground(lipgloss.Magenta)
	h3W      = plainW.Bold(true).Foreground(lipgloss.Yellow)
	nextW    = plainW.Bold(true).Foreground(lipgloss.Green)
	forYouW  = plainW.Bold(true).Foreground(lipgloss.Yellow)
	labelW   = plainW.Foreground(lipgloss.Yellow)
	codeW    = plainW.Foreground(lipgloss.Cyan)
	bulletW  = plainW.Foreground(lipgloss.BrightBlack)
	optionW  = plainW.Bold(true).Foreground(lipgloss.Green)
	toolW    = plainW.Foreground(lipgloss.Blue)
	linkW    = plainW.Foreground(lipgloss.Blue).Underline(true)
	citeW    = plainW.Foreground(lipgloss.BrightBlack)
	flagW    = plainW.Bold(true).Foreground(lipgloss.Red)
	youW     = plainW.Bold(true).Foreground(lipgloss.Green)
	warnW    = plainW.Foreground(lipgloss.Yellow)
	errorW   = plainW.Foreground(lipgloss.Red)
	stateW   = plainW.Foreground(lipgloss.Cyan)
	cursorW  = plainW.Reverse(true)
	markW    = plainW.Foreground(lipgloss.Black).Background(lipgloss.Yellow)
	citedW   = plainW.Underline(true).Foreground(lipgloss.Cyan)
	hitW     = plainW.Bold(true).Foreground(lipgloss.Yellow)
)

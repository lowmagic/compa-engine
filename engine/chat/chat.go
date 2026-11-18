// Package chat is the engine's chat: one conversation per case with the
// companion, inline in the engineer's terminal, printing into its own
// scrollback with the input and one status line beneath, and the evidence
// browser over it (docs/ARCHITECTURE.md §The chat).
package chat

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// Options are how the chat opens.
type Options struct {
	// NoOpen skips the opening turn (--no-open); a chat with no case has
	// none.
	NoOpen bool
}

// Run runs the chat on the session until the engineer quits: the opening
// turn first for a case, unless o.NoOpen.
func Run(ctx context.Context, s *engine.Session, o Options, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var p *tea.Program
	m := newModel(s, o, func(work func(context.Context, func(tea.Msg)) tea.Msg) context.CancelFunc {
		wctx, wcancel := context.WithCancel(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer wcancel()
			p.Send(work(wctx, p.Send))
		}()
		return wcancel
	})
	p = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	_, err := p.Run()
	// Quitting cancels what still runs, a turn included, and waits for it:
	// its records are kept as cancelled.
	cancel()
	wg.Wait()
	return err
}

// spawnFunc runs work in a goroutine of the chat's, under a context of its
// own that the returned function cancels: what work sends as it runs, then
// what it returns, reach Update in order.
type spawnFunc func(work func(ctx context.Context, send func(tea.Msg)) tea.Msg) context.CancelFunc

// model is the chat.
type model struct {
	s       *engine.Session
	noOpen  bool
	spawn   spawnFunc
	println func(string) tea.Cmd

	width, height int
	input         textarea.Model
	completion    commandCompletion
	activity      spinner.Model
	spinning      bool
	// Hold a popover's row extent until a scrollback write resets Bubble
	// Tea's relative cursor origin; shrinking it earlier leaves old rows.
	// inputHeight holds the input's the same way: a line deleted from it
	// would leave a copy of the status line for each row it gave up; and
	// liveHeight the live answer's: a revision taking an answer's place
	// would leave the answer on screen.
	menuHeight  int
	inputHeight int
	liveHeight  int
	// choice is the choice panel open, nil when none; news the reopened
	// case's news whose catch-up is offered once the reload is chosen.
	choice *choicePanel
	news   *engine.ReopenState

	// queue holds logical output; rows holds its wrapped remainder. Only one
	// terminal row is inserted at a time, acknowledged before the next.
	queue    []string
	rows     []string
	rowsAt   int
	printing bool

	// transcript is every line printed, kept to print again at a new width;
	// dropped counts the oldest let go past transcriptMax. sized says the
	// terminal's first size has come; resizes counts width changes, so a
	// reprint waits for the last; reprintDue that one is owed.
	transcript []string
	dropped    int
	sized      bool
	resizes    int
	reprintDue bool

	// turn is the turn running, nil when none; quitArmed is a first
	// ctrl+c, which cancelled it.
	turn      *turnState
	quitArmed bool
	quitting  bool
	// shells are the engineer's commands running.
	shells []string

	current engine.CompanionModel
	// records are this run's calls; usage is the stored case/chat total.
	records []engine.InvocationRecord
	usage   engine.UsageAggregate
	debug   bool

	// answers are the answers printed this run; lastTools the last turn's
	// tool calls, for ctrl+o.
	answers   []answer
	lastTools []toolRun

	// marks are the lines the engineer marked in the browser, which begin
	// their next message.
	marks []string
	// browser is the browser open; pending the one waiting for the prints
	// before it, the command's echo among them, to reach the scrollback;
	// resuming whether the prints wait for the chat's screen to be back.
	browser, pending *browser
	resuming         bool
	// sent counts what the engineer has sent, which chooses the input's
	// tip.
	sent int
	// reviewOffer is the item a draft was kept as, whose review the chat
	// offers on r after it, until the next thing is sent or the next turn
	// begins; empty when nothing is offered.
	reviewOffer string
	// checkOffer is the answer whose findings the chat offers in the
	// checks tab on f after it, as reviewOffer is offered.
	checkOffer string
	// judged are the verdicts kept since the browser opened, which its
	// closing sums up.
	judged []engine.SpanVerdict
}

// hint is what the empty input always says; tips are said one at a time
// after it, the next after each thing sent, the row cut where the
// terminal is narrower.
const hint = "a message, /command or !shell · alt+enter new line"

var tips = []string{
	"/evidence browses what the case holds",
	"v in the browser marks lines for your next message",
	"/pin keeps the last answer as an item",
	"/ledger shows the comments as the ledger reads them",
	"ctrl+o prints the last turn's tool calls in full",
	"/draft writes a draft for an intention",
	"/usage shows the case's tokens and cost",
	"ctrl+c stops a running turn",
}

// placeholder is the empty input's text after n things sent.
func placeholder(n int) string {
	return hint + " · " + tips[n%len(tips)]
}

func newModel(s *engine.Session, o Options, spawn spawnFunc) *model {
	in := textarea.New()
	in.ShowLineNumbers = false
	in.Prompt = "› "
	in.Placeholder = placeholder(0)
	in.DynamicHeight = true
	in.MinHeight = 1
	in.MaxHeight = 8
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	st := textarea.DefaultStyles(true)
	st.Focused.CursorLine = plainW
	st.Focused.Placeholder = faintW
	st.Focused.Prompt = youW
	st.Focused.EndOfBuffer = plainW
	// The terminal's own cursor colour, which the engineer chose.
	st.Cursor.Color = nil
	st.Blurred = st.Focused
	in.SetStyles(st)
	in.SetWidth(80)
	// The terminal's own cursor, which blinks as the engineer set it.
	in.SetVirtualCursor(false)
	in.Focus()
	m := &model{s: s, noOpen: o.NoOpen, spawn: spawn, input: in, width: 80, height: 24,
		println: func(s string) tea.Cmd { return tea.Println(s) }}
	m.activity = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(stateW))
	// An error here leaves the status line without the model; the first
	// turn says what is wrong.
	m.current, _ = s.CompanionModel()
	return m
}

// printedMsg says a print has reached the terminal.
type printedMsg struct{}

// resumeMsg says the chat's own screen is back after the browser, so the
// prints held meanwhile can go.
type resumeMsg struct{}

// resumeWait is how long the prints held while the browser was open wait
// after it closes. Bubble Tea v2 writes a print as soon as its loop takes
// it, but a frame, the one leaving the alternate screen included, only at
// its renderer's next tick, 60 a second by default, on a goroutine of its
// own that no message waits for; a print taken before that tick would be
// written into the alternate screen and lost. Three frames leave the tick
// time to come.
const resumeWait = 3 * time.Second / 60

// transcriptMax bounds the lines kept for reprinting after a width change.
const transcriptMax = 20_000

// print queues lines to print into the scrollback, and keeps them to print
// again should the terminal's width change.
func (m *model) print(lines ...string) {
	m.queue = append(m.queue, lines...)
	m.transcript = append(m.transcript, lines...)
	if over := len(m.transcript) - transcriptMax; over > 0 {
		m.transcript = append(m.transcript[:0:0], m.transcript[over:]...)
		m.dropped += over
	}
}

// flush inserts one physical row above the inline view. Bubble Tea v2.0.10's
// insertAbove first scrolls by the print's height, then moves back up by that
// height plus the last rendered frame's height. If their sum exceeds the
// terminal height, cursor-up clamps at the top and commits the old frame
// (streaming text and prompts included) into scrollback. View reserves one
// row, so a one-row insert fits even before its next asynchronous redraw.
// The browser holds prints until it has given the normal screen back.
func (m *model) flush() tea.Cmd {
	if m.printing || (len(m.queue) == 0 && len(m.rows) == 0) || m.browser != nil || m.resuming {
		return nil
	}
	if len(m.rows) == 0 {
		m.rows, m.queue, m.rowsAt = m.queue, nil, 0
	}
	width := m.cols()
	if m.rowsAt != width {
		m.rows = strings.Split(ansi.Wrap(strings.Join(m.rows, "\n"), width, ""), "\n")
		m.rowsAt = width
	}
	s := m.rows[0]
	m.rows[0] = ""
	m.rows = m.rows[1:]
	// insertAbove ignores an empty string, but a blank transcript row must
	// still occupy a row. An SGR reset has no visible text and is not empty.
	if s == "" {
		s = ansi.ResetStyle
	}
	m.printing = true
	return tea.Sequence(m.println(s), func() tea.Msg { return printedMsg{} })
}

// reprintWait is how long the terminal's width must stay put before the
// chat prints its transcript again: a window dragged narrower sends a size
// on every step.
const reprintWait = 200 * time.Millisecond

// resizedMsg is a width change settling; n is which one.
type resizedMsg struct{ n int }

// reprint clears the terminal's screen and scrollback and prints the
// transcript again at the new width. The inline renderer cannot redraw
// after a terminal rewraps on a width change: iTerm2, Ghostty, kitty and
// tmux rewrap long rows into more physical rows, the renderer moves up by
// the logical ones and leaves the top of the old frame behind on every
// narrowing (charmbracelet/bubbletea#1867), and tmux moves the cursor from
// its cell as it rewraps, so no move counted from the cursor is right on
// every terminal; a cleared terminal is the one place every terminal
// agrees on. What scrolled back before the chat began is cleared with it.
// The screen is erased before the history: tmux's scroll-on-clear, on by
// default, moves an erased screen into the history. The clear reaches the
// terminal at once and the frame at the renderer's next tick, so the prints
// wait as they do after the browser closes.
func (m *model) reprint() tea.Cmd {
	m.reprintDue = false
	m.queue = append([]string(nil), m.transcript...)
	if m.dropped > 0 {
		m.queue = append([]string{noteLine(fmt.Sprintf("… %s earlier, not kept to print again", text.Plural(m.dropped, "line")))}, m.queue...)
	}
	m.rows, m.rowsAt = nil, 0
	m.menuHeight, m.inputHeight, m.liveHeight = 0, 0, 0
	m.resuming = true
	return tea.Batch(
		tea.Raw(ansi.CursorHomePosition+ansi.EraseEntireScreen+ansi.EraseEntireDisplay),
		tea.ClearScreen,
		tea.Tick(resumeWait, func(time.Time) tea.Msg { return resumeMsg{} }))
}

// reveal shows the browser waiting to open once every print before it
// has reached the terminal: Bubble Tea writes a print when its loop takes
// it, before the message after it, so a print acknowledged is in the
// scrollback before the frame entering the alternate screen is drawn.
func (m *model) reveal() {
	if m.pending != nil && !m.printing && len(m.queue) == 0 && len(m.rows) == 0 {
		m.browser, m.pending = m.pending, nil
	}
}

// closeBrowser gives the chat its screen back, the prints held until a
// frame has left the alternate screen, the verdicts kept in the checks
// tab summed up first.
func (m *model) closeBrowser() tea.Cmd {
	m.browser, m.resuming = nil, true
	m.printJudged()
	return tea.Tick(resumeWait, func(time.Time) tea.Msg { return resumeMsg{} })
}

func (m *model) Init() tea.Cmd {
	m.opening()
	return tea.Batch(m.flush(), m.spin())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if m.browser == nil && m.pending == nil && m.menuHeight > 0 && m.choice == nil && len(m.completion.matches(m.input.Value())) == 0 &&
		!m.printing && len(m.queue) == 0 && len(m.rows) == 0 {
		// A blank separator gives a dismissed popover or choice panel the
		// same cursor reset as an ordinary transcript print, without
		// printing its options; it is not kept in the transcript.
		m.queue = append(m.queue, "")
	}
	var reprint tea.Cmd
	if m.reprintDue && !m.printing && m.browser == nil && m.pending == nil && !m.resuming {
		reprint = m.reprint()
	}
	flush := m.flush()
	m.reveal()
	return m, tea.Batch(cmd, reprint, flush, m.spin())
}

// spin schedules animation only while work runs; idle chats need no timer.
func (m *model) spin() tea.Cmd {
	if m.turn == nil || m.spinning {
		return nil
	}
	m.spinning = true
	return m.activity.Tick
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if m.turn == nil {
			m.spinning = false
			return nil
		}
		var cmd tea.Cmd
		m.activity, cmd = m.activity.Update(msg)
		return cmd
	case tea.WindowSizeMsg:
		widthChanged := m.sized && msg.Width != m.width
		m.width, m.height, m.sized = msg.Width, msg.Height, true
		// Leave room for the status line and a one-row scrollback insertion.
		m.input.MaxHeight = min(8, max(m.height-2, 1))
		m.input.SetWidth(m.cols())
		if !widthChanged {
			return nil
		}
		m.resizes++
		n := m.resizes
		return tea.Tick(reprintWait, func(time.Time) tea.Msg { return resizedMsg{n} })
	case resizedMsg:
		m.reprintDue = m.reprintDue || msg.n == m.resizes
		return nil
	case printedMsg:
		m.printing = false
		m.menuHeight, m.inputHeight, m.liveHeight = 0, 0, 0
		return nil
	case resumeMsg:
		m.resuming = false
		return nil
	case historyMsg:
		m.restoreHistory(msg)
		return nil
	case usageMsg:
		m.applyUsage(msg)
		return nil
	case eventMsg:
		m.event(msg.ev)
		return nil
	case stateMsg:
		if m.turn != nil {
			m.turn.status = msg.state
		}
		return nil
	case noteMsg:
		if msg.line != "" {
			m.print(msg.line)
		}
		return nil
	case ledgerCallMsg:
		m.called(msg.ev)
		return nil
	case ledgerRunningMsg:
		m.ledgerWatch(msg.job)
		return nil
	case recordMsg:
		m.record(msg)
		return nil
	case turnDone:
		return m.done(msg)
	case jobDone:
		return m.jobDone(msg)
	case shellDone:
		m.shellDone(msg)
		return nil
	case itemsMsg:
		m.browserItems(msg)
		return nil
	case verdictMsg:
		m.verdictDone(msg)
		return nil
	case tea.KeyPressMsg:
		if m.browser != nil {
			return m.browserKey(msg)
		}
		return m.key(msg)
	case tea.MouseMsg:
		// Main chat uses native terminal scrollback, not the textarea's
		// viewport. Do not let delivered wheel events move that viewport.
		return nil
	}
	if m.browser == nil {
		if _, ok := msg.(tea.PasteMsg); ok {
			// A paste begins a message, as a key does: the offers end.
			m.reviewOffer, m.checkOffer = "", ""
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd
	}
	return nil
}

// key is a key pressed in the chat.
func (m *model) key(k tea.KeyPressMsg) tea.Cmd {
	if m.choice != nil && m.choiceKey(k.String()) {
		return nil
	}
	// r takes the review offered, and f the findings offered in the tab,
	// only as the first key after the answer, on an empty input with
	// nothing else open; any other key ends both offers, so a letter of a
	// new message is never a paid review or a check begun.
	idle := m.turn == nil && m.choice == nil && m.input.Value() == ""
	switch {
	case idle && k.String() == "r" && m.reviewOffer != "":
		item := m.reviewOffer
		m.reviewOffer, m.checkOffer = "", ""
		m.print(commandLine("/review " + item))
		m.review(engine.ReviewAsk{Item: item})
		return nil
	case idle && k.String() == "f" && m.checkOffer != "":
		label := m.checkOffer
		m.reviewOffer, m.checkOffer = "", ""
		m.print(commandLine("/check " + label))
		m.checkFindings(label)
		return nil
	}
	m.reviewOffer, m.checkOffer = "", ""
	if value, handled, submit := m.completion.key(m.input.Value(), k.String()); handled {
		if value != m.input.Value() {
			m.input.SetValue(value)
			m.input.CursorEnd()
		}
		if submit {
			m.enter()
		}
		return nil
	}
	switch k.String() {
	case "ctrl+c":
		if m.turn != nil && !m.quitArmed {
			m.turn.cancel()
			m.turn.status = "cancelling"
			m.quitArmed = true
			return nil
		}
		m.quitting = true
		return tea.Quit
	case "ctrl+o":
		m.printTools()
		return nil
	case "esc":
		return nil
	case "enter":
		m.enter()
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

// enter sends what the engineer wrote: a command, a shell command or a
// message. What is refused stays in the input.
func (m *model) enter() {
	line := strings.TrimSpace(m.input.Value())
	if line == "" {
		return
	}
	m.reviewOffer, m.checkOffer = "", ""
	var sent bool
	switch {
	case strings.HasPrefix(line, "/"):
		sent = m.command(line)
	case strings.HasPrefix(line, "!"):
		sent = m.shell(strings.TrimSpace(line[1:]))
	default:
		sent = m.message(line)
	}
	if sent {
		m.input.Reset()
		m.sent++
		m.input.Placeholder = placeholder(m.sent)
	}
}

// message sends the engineer's words to the conversation, the lines they
// marked in the browser said first.
func (m *model) message(line string) bool {
	if m.turn != nil {
		m.print(errorLine("a turn is running: wait for its answer, or ctrl+c to cancel it"))
		return false
	}
	if len(m.marks) > 0 {
		line = "the engineer points at " + strings.Join(m.marks, ", ") + "\n\n" + line
		m.marks = nil
	}
	m.print("", youLine(line))
	s := m.s
	m.startTurn(kindAnswer, "calling the model", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
		reply, err := s.Talk(ctx, line, on)
		return turnDone{reply: reply, text: reply.Text, check: reply.Check, err: err}
	})
	return true
}

// youLine is the engineer's message as printed.
func youLine(s string) string {
	lines := strings.Split(text.Safe(s), "\n")
	for i := range lines {
		lead := "  "
		if i == 0 {
			lead = youW.Render("›") + " "
		}
		lines[i] = lead + lines[i]
	}
	return strings.Join(lines, "\n")
}

// commandLine is a command the engineer gave, as printed.
func commandLine(s string) string { return faintW.Render("› " + text.Safe(s)) }

// errorLine is an error or refusal, one line.
func errorLine(s string) string { return errorW.Render("✗ " + text.Safe(firstLine(s))) }

// warnLine is a warning, one line.
func warnLine(s string) string { return warnW.Render("! " + text.Safe(firstLine(s))) }

// noteLine is a dim line of the chat's own.
func noteLine(s string) string { return faintW.Render(text.Safe(s)) }

// firstLine is s on one line: its lines joined.
func firstLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// maxLive bounds the rows of a streaming answer the view shows: its end.
const maxLive = 40

func (m *model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	if m.browser != nil {
		v := tea.NewView(m.browserView())
		v.AltScreen = true
		return v
	}
	// Keep one physical row free for scrollback insertion. On a tiny
	// terminal, the input takes precedence over the status and popover.
	limit := max(m.height-1, 0)
	input := strings.Split(m.input.View(), "\n")
	if len(input) > limit {
		input = input[:limit]
	}
	footer := make([]string, 0, len(input)+1)
	if len(input) < limit {
		footer = append(footer, m.statusLine())
	}
	barRows := len(footer)
	footer = append(footer, input...)
	// Rows the input gave up, kept blank above the status line until the
	// next print.
	m.inputHeight = min(max(m.inputHeight, len(input)), limit)
	held := min(m.inputHeight-len(input), max(limit-len(footer), 0))
	room := limit - len(footer) - held
	var options []string
	if m.choice != nil {
		options = m.choice.rows(m.cols(), room)
	} else {
		options = m.completion.rows(m.input.Value(), m.cols(), room)
	}
	m.menuHeight = min(max(m.menuHeight, len(options)), room)
	for len(options) < m.menuHeight {
		options = append(options, "")
	}
	var rows []string
	live := max(min(maxLive, limit-len(footer)-held-len(options)), 0)
	if t := m.turn; t != nil && t.live.Len() > 0 {
		rows = strings.Split(t.rendered(m.cols()), "\n")
		room := live
		// An answer the editor checks says so above it, where there is
		// room for a row besides.
		mark := t.mark != "" && room > 1
		if mark {
			room--
		}
		if len(rows) > room {
			rows = rows[len(rows)-room:]
		}
		if mark {
			rows = append([]string{faintW.Render("⋯ " + t.mark)}, rows...)
		}
	}
	m.liveHeight = min(max(m.liveHeight, len(rows)), live)
	for len(rows) < m.liveHeight {
		rows = append(rows, "")
	}
	rows = append(rows, options...)
	for range held {
		rows = append(rows, "")
	}
	inputOffset := len(rows) + barRows
	rows = append(rows, footer...)
	for i := range rows {
		// Extremely narrow terminals can be smaller than the textarea's
		// minimum width; never let implicit wrapping add unmanaged rows.
		rows[i] = ansi.Truncate(rows[i], m.cols(), "")
	}
	v := tea.NewView(strings.Join(rows, "\n"))
	v.MouseMode = tea.MouseModeNone
	if c := m.input.Cursor(); c != nil && c.Y < len(input) {
		c.X = min(c.X, max(m.cols()-1, 0))
		c.Y += inputOffset
		v.Cursor = c
	}
	return v
}

// cols is the width the chat draws and prints at: one column short of the
// terminal's, so no row fills it (charmbracelet/bubbletea#1885).
func (m *model) cols() int { return max(m.width-1, 1) }

// statusLine is the bar above the input: the case, model and effort,
// current work, cumulative tokens/cache/spend, and pending evidence marks.
func (m *model) statusLine() string {
	where := "case " + m.s.CaseID()
	if m.s.IsChat() {
		where = "chat " + m.s.CaseID()
	}
	parts := make([]string, 0, 6)
	if m.turn != nil {
		stop := "ctrl+c to stop"
		if m.quitArmed {
			stop = "ctrl+c to quit"
		}
		activity := m.activity.View() + " " + stop + " · " + stateW.Render(m.doing())
		parts = append(parts, activity)
	}
	parts = append(parts, faintW.Render(where))
	if len(m.marks) > 0 {
		parts = append(parts, warnW.Render("pointing at "+strings.Join(m.marks, ", ")))
	}
	if m.current.Name != "" {
		parts = append(parts, faintW.Render(m.current.Name+" "+engine.EffortShown(m.current.Effort)))
	}
	if m.turn == nil {
		parts = append(parts, stateW.Render(m.doing()))
	}
	if m.debug {
		parts = append(parts, faintW.Render("debug"))
	}
	width := m.cols()
	left := strings.Join(parts, faintW.Render(" · "))
	right := usageShown(m.usage)
	if right != "" {
		right = faintW.Render("total " + right)
		// Keep cumulative spend/cache visible when long work or mark
		// descriptions compete for the same row.
		leftRoom := width - ansi.StringWidth(right) - 5
		if leftRoom > 0 {
			left = ansi.Truncate(left, leftRoom, "…")
			gap := max(width-ansi.StringWidth(left)-ansi.StringWidth(right)-4, 1)
			return ansi.Truncate(faintW.Render("─ ")+left+" "+faintW.Render(strings.Repeat("─", gap))+" "+right, width, "…")
		}
		left += faintW.Render(" · ") + right
	}
	left = ansi.Truncate(faintW.Render("─ ")+left, width, "…")
	return left + faintW.Render(strings.Repeat("─", max(width-ansi.StringWidth(left), 0)))
}

// doing is what is running now, said precisely.
func (m *model) doing() string {
	switch {
	case m.turn != nil:
		return m.turn.status
	case len(m.shells) > 0:
		return "running $ " + m.shells[0]
	}
	return "waiting for you"
}

// usageShown is the case/chat's cumulative tokens, cost and cache share.
func usageShown(a engine.UsageAggregate) string {
	s := a.Compact()
	if s == "" {
		return ""
	}
	// The canonical categories of docs/MODEL_API.md §Usage accounting.
	in, read, write := a.Tokens["input_tokens"], a.Tokens["cache_read_tokens"], a.Tokens["cache_write_tokens"]
	if in == nil || read == nil || write == nil {
		return s
	}
	prompt := in.Reported + read.Reported + write.Reported
	if prompt > 0 && in.Unavailable == 0 && read.Unavailable == 0 && write.Unavailable == 0 &&
		in.Estimated == 0 && read.Estimated == 0 && write.Estimated == 0 {
		return s + " · " + percent(read.Reported, prompt) + " cached"
	}
	if read.Unavailable > 0 && read.Reported == 0 && read.Estimated == 0 {
		return s
	}
	cache := engine.Thousands(read.Reported) + " cached tok"
	if read.Estimated > 0 {
		cache += " + " + engine.Thousands(read.Estimated) + " est"
	}
	if read.Unavailable > 0 {
		cache = "≥" + cache
	}
	s += " · " + cache
	return s
}

// percent is part of whole in whole percent, rounded.
func percent(part, whole int64) string {
	return strconv.FormatInt((part*100+whole/2)/whole, 10) + "%"
}

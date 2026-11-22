package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// commandSpec is shared by help, name completion and the busy-turn guard.
// args uses the same optional/required notation as the command's help.
type commandSpec struct {
	name, args, description string
	waitsForTurn            bool
	idleWithoutArgs         bool
}

var commandCatalog = []commandSpec{
	{name: "draft", args: "[mode] [intention]", description: "Write a case comment, internal, update or kcs, steered by what follows; with none, list the modes", waitsForTurn: true, idleWithoutArgs: true},
	{name: "copy", args: "[aN]", description: "Copy the last answer or draft, or aN, to the clipboard as Markdown"},
	{name: "turns", description: "Browse the conversation's turns, to read, select or copy one"},
	{name: "ledger", description: "Show the case ledger, reading new comments first", waitsForTurn: true},
	{name: "evidence", args: "[eN[:lines]]", description: "Browse evidence, comments, turns and checks"},
	{name: "check", args: "[aN]", description: "Open the checks tab at the last answer's findings, or an answer's", waitsForTurn: true},
	{name: "review", args: "[--ranked] [aN]", description: "Have the reviewer review the last answer, or an answer/draft; --ranked adds evidence used, not cited", waitsForTurn: true},
	{name: "model", args: "[model] [effort]", description: "Show or change the model", waitsForTurn: true, idleWithoutArgs: true},
	{name: "compact", description: "Write a handoff and go on in a new conversation from the case's state", waitsForTurn: true},
	{name: "debug", args: "[on|off|usage|experts|load <expert>|ranked]", description: "Call diagnostics on or off, usage, the experts, loading one, the ranked evidence"},
}

// debugOption is one of /debug's arguments.
type debugOption struct {
	name, description string
}

// debugOptions are /debug's arguments, in the order it lists them.
var debugOptions = []debugOption{
	{"on", "each model call's input, cache share, output and timing, and each tool call's question and answers, inline"},
	{"off", "the quiet chat again"},
	{"usage", "recorded tokens and cost by conversation, consult and model"},
	{"experts", "the installed experts, those loaded marked"},
	{"load <expert>", "load an installed expert into the conversation"},
	{"ranked", "the evidence the conversation used most, ranked, with no model call"},
}

func (c commandSpec) label() string {
	if c.args != "" {
		return "/" + c.name + " " + c.args
	}
	return "/" + c.name
}

// waits says whether the command with arg waits for the turn running:
// /debug only to load an expert.
func (c commandSpec) waits(arg string) bool {
	if c.name == "debug" {
		sub, _, _ := strings.Cut(arg, " ")
		return sub == "load"
	}
	return c.waitsForTurn && (arg != "" || !c.idleWithoutArgs)
}

func commandNamed(name string) commandSpec {
	for _, c := range commandCatalog {
		if c.name == name {
			return c
		}
	}
	return commandSpec{}
}

func commandHelp() string {
	names := make([]string, 0, len(commandCatalog)+1)
	for _, c := range commandCatalog {
		names = append(names, c.label())
	}
	return strings.Join(append(names, "!<command>"), ", ")
}

// command runs a command the engineer gave, reporting whether it was
// taken; one refused stays in the input.
func (m *model) command(line string) bool {
	name, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	arg = strings.TrimSpace(arg)
	if m.turn != nil && commandNamed(name).waits(arg) {
		m.print(errorLine("/" + name + " waits for the turn running: wait for its answer, or ctrl+c to cancel it"))
		return false
	}
	m.print(commandLine(line))
	s := m.s
	switch name {
	case "draft":
		if arg == "" {
			m.printIntentions()
			break
		}
		m.startTurn(kindDraft, "calling the model for the draft", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
			it, reply, err := s.Draft(ctx, arg, on)
			return turnDone{item: it, reply: reply, text: reply.Text, check: reply.Check, err: err}
		})
	case "copy":
		m.copyAnswer(arg)
	case "turns":
		m.openTab(tabTurns)
	case "ledger":
		m.ledger()
	case "evidence":
		m.openEvidence(arg)
	case "check":
		m.checkFindings(arg)
	case "review":
		ask, err := reviewAsk(arg)
		if err != nil {
			m.print(errorLine(err.Error()))
			break
		}
		m.review(ask)
	case "model":
		m.chooseModel(strings.Fields(arg))
	case "compact":
		m.compact()
	case "debug":
		m.debugCommand(arg)
	default:
		m.print(errorLine("there is no command /" + name + "; the commands are " + commandHelp()))
	}
	return true
}

// argOptions are the argument options for input, a command's name and a
// space, then what of its arguments is typed: /draft's modes, /copy's
// answers newest first, /model's models then a model's efforts, and
// /debug's arguments. Each mode, answer, model or argument runs on
// enter; tab completes it, a mode to take steering after it and a model
// with efforts to offer them.
func (m *model) argOptions(input string) []completionOption {
	name, rest, ok := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	if !ok {
		return nil
	}
	var out []completionOption
	switch name {
	case "draft":
		if strings.Contains(rest, " ") {
			return nil
		}
		for _, in := range m.s.Intentions() {
			v := "/draft " + in.ID
			out = append(out, completionOption{complete: v + " ", run: v, label: in.ID, description: in.Label + ", " + audienceShown(in.Audience)})
		}
	case "copy":
		for i := len(m.answers) - 1; i >= 0; i-- {
			a := m.answers[i]
			v, label := "/copy "+a.label, a.label
			if a.draft != "" {
				label += " · draft: " + a.draft
			}
			out = append(out, completionOption{complete: v, run: v, label: label, description: a.preview})
		}
	case "model":
		models := m.s.CompanionModels()
		if chosen, effort, picked := strings.Cut(rest, " "); picked {
			if strings.Contains(effort, " ") {
				return nil
			}
			for _, e := range models[chosen] {
				v := "/model " + chosen + " " + e
				out = append(out, completionOption{complete: v, run: v, label: e})
			}
			return out
		}
		names := make([]string, 0, len(models))
		for n := range models {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v := "/model " + n
			o := completionOption{complete: v, run: v, label: n, description: "takes no effort"}
			if len(models[n]) > 0 {
				o.complete, o.description = v+" ", "efforts "+strings.Join(models[n], ", ")
			}
			out = append(out, o)
		}
	case "debug":
		if strings.Contains(rest, " ") {
			return nil
		}
		for _, o := range debugOptions {
			if sub, _, more := strings.Cut(o.name, " "); more {
				out = append(out, completionOption{complete: "/debug " + sub + " ", label: o.name, description: o.description})
				continue
			}
			v := "/debug " + o.name
			out = append(out, completionOption{complete: v, run: v, label: o.name, description: o.description})
		}
	}
	return out
}

// preview is the start of an answer's text, its Markdown marks at the
// start of each line dropped, on one line, for /copy's options.
func preview(s string) string {
	var words []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimLeft(strings.TrimSpace(l), "#->*` ")
		if l != "" {
			words = append(words, l)
		}
		if len(words) > 3 {
			break
		}
	}
	return firstLine(strings.Join(words, " "))
}

// compact writes a handoff and goes on in a new conversation, as a turn:
// in a case, begun from the case as it is now, the ledger, the handoff
// and what the engine carries from the old one (engine.ReloadSummary); in
// a chat, which has no case, from the handoff (engine.Session.Compact).
func (m *model) compact() {
	s := m.s
	if s.IsChat() {
		m.startTurn(kindCompact, "calling the model for a handoff", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
			handoff := ""
			_, err := s.Compact(ctx, func(ev engine.TalkEvent) {
				if ev.Kind == engine.TalkCompacted {
					handoff = ev.Text
				}
				on(ev)
			})
			return turnDone{text: handoff, err: err}
		})
		return
	}
	m.startTurn(kindReload, "calling the model for a handoff", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
		r, err := s.Reload(ctx, engine.ReloadSummary, on)
		return turnDone{reloaded: &r, text: r.Handoff, err: err}
	})
}

// debugCommand takes /debug's argument: diagnostics on or off, the usage,
// the experts, loading one, the ranked evidence; with none, the state and
// the arguments.
func (m *model) debugCommand(arg string) {
	sub, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	s := m.s
	switch sub {
	case "":
		state := "off"
		if m.debug {
			state = "on"
		}
		m.print(noteLine("debug is " + state + "; /debug takes one of:"))
		for _, o := range debugOptions {
			m.print(text.Safe("  " + o.name + " — " + o.description))
		}
	case "on":
		m.debug = true
		m.print(noteLine("debug: each model call's input, cache share, output and timing, and each tool call's question and answers, from now on"))
	case "off":
		m.debug = false
		m.print(noteLine("debug off"))
	case "usage":
		m.printUsage()
	case "experts":
		m.job(func(ctx context.Context) jobDone { return experts(ctx, s) })
	case "load":
		if rest == "" {
			m.print(errorLine("/debug load names an expert; /debug experts lists them"))
			return
		}
		m.startTurn(kindLoad, "loading "+rest, func(ctx context.Context, _ func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
			_, err := s.LoadExpert(ctx, rest)
			return turnDone{loaded: rest, err: err}
		})
	case "ranked":
		m.job(func(ctx context.Context) jobDone {
			ranked, err := s.RankedEvidence(ctx, 0)
			if err != nil {
				return jobDone{err: err}
			}
			var lines []string
			for _, l := range strings.Split(strings.TrimRight(engine.RenderRanked(ranked), "\n"), "\n") {
				lines = append(lines, text.Safe(l))
			}
			return jobDone{lines: lines}
		})
	default:
		m.print(errorLine("/debug takes on, off, usage, experts, load <expert> or ranked"))
	}
}

// copyAnswer copies the last answer or draft, or the one label names, as
// the engineer was shown it (engine.AnswerViews), a draft's citations
// taken out as it was shown; as Markdown, unrendered.
func (m *model) copyAnswer(label string) {
	s := m.s
	label = strings.ToLower(label)
	m.job(func(ctx context.Context) jobDone {
		history, err := s.ConversationHistory(ctx)
		if err != nil {
			return jobDone{err: err}
		}
		v, err := pickAnswer(engine.AnswerViews(history), label)
		if err != nil {
			return jobDone{err: err}
		}
		body := v.Text
		if v.Draft {
			body = engine.StripDraftCites(body)
		}
		return jobDone{copied: &copied{name: v.Label, text: strings.TrimSpace(body)}}
	})
}

// pickAnswer is the answer label names, or the last.
func pickAnswer(views []engine.AnswerView, label string) (engine.AnswerView, error) {
	if len(views) == 0 {
		return engine.AnswerView{}, errors.New("there is no answer to copy yet")
	}
	if label == "" {
		return views[len(views)-1], nil
	}
	for _, v := range views {
		if v.Label == label {
			return v, nil
		}
	}
	return engine.AnswerView{}, fmt.Errorf("there is no answer %s; the answers are a1 to %s", label, views[len(views)-1].Label)
}

// jobDone is a command's work done: lines to print, or why it failed, or
// an answer to copy.
type jobDone struct {
	lines  []string
	err    error
	copied *copied
}

// copied is an answer to copy: its name and its text.
type copied struct {
	name, text string
}

// job runs work beside the chat and the turn.
func (m *model) job(work func(ctx context.Context) jobDone) {
	m.spawn(func(ctx context.Context, _ func(tea.Msg)) tea.Msg { return work(ctx) })
}

func (m *model) jobDone(d jobDone) tea.Cmd {
	if d.err != nil {
		m.print(errorLine(d.err.Error()))
		return nil
	}
	m.print(d.lines...)
	if d.copied != nil {
		return m.copyText(d.copied.name, d.copied.text)
	}
	return nil
}

// copyText asks the terminal to put text on the clipboard, through OSC
// 52, which reaches the engineer's own terminal over SSH; whether it does
// is the terminal's to say.
func (m *model) copyText(name, body string) tea.Cmd {
	m.print(noteLine("asked the terminal to copy " + name + " to the clipboard (OSC 52); a terminal that does not take it copies nothing"))
	return tea.SetClipboard(body)
}

// printIntentions lists what /draft can write.
func (m *model) printIntentions() {
	ins := m.s.Intentions()
	if len(ins) == 0 {
		m.print(errorLine("no intentions are configured (comments.intentions), so there is nothing to draft"))
		return
	}
	m.print(noteLine("/draft <mode> [intention], the mode one of:"))
	for _, in := range ins {
		m.print(text.Safe(fmt.Sprintf("  %s — %s, %s: %s", in.ID, in.Label, audienceShown(in.Audience), in.Purpose)))
	}
}

// audienceShown says whom a draft is for.
func audienceShown(audience string) string {
	if audience == "internal" {
		return "an internal note"
	}
	return "for the " + audience
}

// experts lists the installed experts, those loaded marked.
func experts(ctx context.Context, s *engine.Session) jobDone {
	xs, err := s.Experts(ctx)
	if err != nil {
		return jobDone{err: err}
	}
	if len(xs) == 0 {
		return jobDone{lines: []string{noteLine("no experts are installed")}}
	}
	lines := make([]string, 0, len(xs))
	for _, x := range xs {
		mark := "  "
		if x.Loaded {
			mark = youW.Render("●") + " "
		}
		l := x.ID + " " + x.Version
		if x.Loaded {
			l += " (loaded)"
		}
		l += " — " + x.Summary
		if len(x.RoutingKeys) > 0 {
			l += " (primary for " + strings.Join(x.RoutingKeys, ", ") + ")"
		}
		lines = append(lines, mark+text.Safe(firstLine(l)))
	}
	return jobDone{lines: lines}
}

// chooseModel shows the model and the choices, or switches to one.
func (m *model) chooseModel(args []string) {
	if len(args) == 0 {
		cur, err := m.s.CompanionModel()
		if err != nil {
			m.print(errorLine(err.Error()))
		} else {
			m.print(noteLine("the model: " + cur.Name + " " + engine.EffortShown(cur.Effort) + " (" + cur.Source + ")"))
		}
		models := m.s.CompanionModels()
		names := make([]string, 0, len(models))
		for n := range models {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			efforts := "takes no effort"
			if len(models[n]) > 0 {
				efforts = "efforts " + strings.Join(models[n], ", ")
			}
			m.print(text.Safe("  " + n + " — " + efforts))
		}
		m.print(noteLine("/model <model> [effort] switches"))
		return
	}
	if len(args) > 2 {
		m.print(errorLine("/model takes a model and an effort"))
		return
	}
	effort := ""
	if len(args) == 2 {
		effort = args[1]
	}
	if err := m.s.ChooseCompanionModel(args[0], effort); err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	cur, err := m.s.CompanionModel()
	if err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	m.current = cur
	m.print(noteLine("the next calls use " + cur.Name + " " + engine.EffortShown(cur.Effort) + "; the conversation goes on"))
}

// printUsage prints stored usage by conversation and model, and its total.
func (m *model) printUsage() {
	aggs, err := m.s.CompanionUsage()
	if err != nil {
		m.print(errorLine(err.Error()))
		return
	}
	if len(aggs) == 0 {
		m.print(noteLine("no model calls yet"))
		return
	}
	for _, a := range aggs {
		l := a.Key[engine.UsageByConversation] + " · " + a.Key[engine.UsageByModel] + " · " + a.Totals()
		m.print(text.Safe(l), noteLine("  "+strings.Join(a.Completeness(), "; ")))
	}
	m.print(noteLine("total: " + m.usage.Totals()))
}

// Bounds on the engineer's commands: the output kept as evidence, cut
// past it and said so, and the lines printed of it.
const (
	maxShellBytes   = 8 << 20
	shownShellLines = 20
	// shellWait is how long a command's output may stay open after its
	// shell ends, as a child left running holds it.
	shellWait = 2 * time.Second
)

// shellDone is the engineer's command run: its output, its exit status,
// and the name it is kept under.
type shellDone struct {
	command, output, name string
	status                int
	cut                   bool
	err                   error
}

// shell runs a command in the engineer's shell beside the chat and keeps
// its output as evidence.
func (m *model) shell(command string) bool {
	if command == "" {
		m.print(errorLine("! runs a command in your shell, as !journalctl -u kubelet"))
		return false
	}
	m.print(commandLine("$ " + command))
	m.shells = append(m.shells, command)
	s := m.s
	m.spawn(func(ctx context.Context, _ func(tea.Msg)) tea.Msg { return runShell(ctx, s, command) })
	return true
}

// runShell runs command with $SHELL -c, or sh -c, and ingests what it
// printed, as location "$ <command>".
func runShell(ctx context.Context, s *engine.Session, command string) shellDone {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "sh"
	}
	c := exec.CommandContext(ctx, sh, "-c", command)
	out := &capped{limit: maxShellBytes}
	c.Stdout, c.Stderr = out, out
	c.WaitDelay = shellWait
	err := c.Run()
	d := shellDone{command: command, output: out.b.String(), cut: out.cut}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		d.err = ctx.Err()
		return d
	case errors.As(err, &exit):
		d.status = exit.ExitCode()
	case err != nil:
		d.err = err
		return d
	}
	note := fmt.Sprintf("exit status %d", d.status)
	if d.cut {
		note += fmt.Sprintf("; output cut at %d bytes", maxShellBytes)
	}
	it, _, err := s.Ingest(strings.NewReader(d.output), "$ "+command, note)
	if err != nil {
		d.err = fmt.Errorf("the output was not kept: %w", err)
		return d
	}
	if d.name, err = s.NameEvidence(it.Ref.ID); err != nil {
		d.err = fmt.Errorf("the output was kept but not named: %w", err)
	}
	return d
}

func (m *model) shellDone(d shellDone) {
	for i, c := range m.shells {
		if c == d.command {
			m.shells = append(m.shells[:i], m.shells[i+1:]...)
			break
		}
	}
	if errors.Is(d.err, context.Canceled) {
		m.print(noteLine("$ " + d.command + ": cancelled"))
		return
	}
	lines := strings.Split(strings.TrimRight(d.output, "\n"), "\n")
	if d.output == "" {
		lines = nil
		m.print(noteLine("(no output)"))
	}
	for _, l := range lines[:min(len(lines), shownShellLines)] {
		l = text.Safe(strings.ReplaceAll(l, "\t", "    "))
		if m.width > 0 {
			l = ansi.Truncate(l, m.cols(), "…")
		}
		m.print(l)
	}
	if d.err != nil {
		m.print(errorLine(d.err.Error()))
		return
	}
	foot := fmt.Sprintf("kept as %s · exit status %d", d.name, d.status)
	if n := len(lines) - shownShellLines; n > 0 {
		foot = fmt.Sprintf("%d more lines in %s · ", n, d.name) + foot
	}
	if d.cut {
		foot += fmt.Sprintf(" · output cut at %d bytes", maxShellBytes)
	}
	m.print(noteLine(foot))
}

// capped keeps what is written up to its limit and says whether it cut
// any.
type capped struct {
	b     bytes.Buffer
	limit int
	cut   bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.limit - c.b.Len(); room < len(p) {
		c.cut = true
		p = p[:max(room, 0)]
	}
	c.b.Write(p)
	return n, nil
}

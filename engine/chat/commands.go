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
	{name: "analyze", description: "Report the observations and open questions", waitsForTurn: true},
	{name: "draft", args: "[intention]", description: "List intentions or write a draft", waitsForTurn: true, idleWithoutArgs: true},
	{name: "review", args: "[--ranked] [item]", description: "Have the reviewer review the last answer, or an item; --ranked adds evidence used, not cited", waitsForTurn: true},
	{name: "check", args: "[aN]", description: "Open the checks tab at the last answer's findings, or an answer's", waitsForTurn: true},
	{name: "load", args: "<expert>", description: "Load an expert into the conversation", waitsForTurn: true},
	{name: "experts", description: "List installed experts"},
	{name: "evidence", args: "[eN[:lines]]", description: "Browse evidence"},
	{name: "comments", args: "[N]", description: "Browse case comments"},
	{name: "ledger", description: "Show the case ledger, reading new comments first", waitsForTurn: true},
	{name: "pin", description: "Keep the last answer as an item"},
	{name: "items", description: "Browse kept answers and drafts"},
	{name: "copy", args: "[item]", description: "Copy an item to the clipboard"},
	{name: "model", args: "[model] [effort]", description: "Show or change the model", waitsForTurn: true, idleWithoutArgs: true},
	{name: "compact", description: "Compact the conversation into a handoff", waitsForTurn: true},
	{name: "reload", args: "[full|suggested|summary]", description: "List the ways on once the cache has expired, or take one", waitsForTurn: true, idleWithoutArgs: true},
	{name: "catchup", description: "Catch up on the comments and attachments new since you last worked the case", waitsForTurn: true},
	{name: "ranked", description: "Show the evidence the conversation used most, ranked"},
	{name: "usage", description: "Show recorded model usage and cost"},
	{name: "debug", description: "Toggle model-call diagnostics"},
}

func (c commandSpec) label() string {
	if c.args != "" {
		return "/" + c.name + " " + c.args
	}
	return "/" + c.name
}

func (c commandSpec) waits(arg string) bool {
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
	case "analyze":
		m.startTurn(kindAnalysis, "calling the model", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
			reply, err := s.Analyze(ctx, on)
			return turnDone{reply: reply, text: reply.Text, check: reply.Check, err: err}
		})
	case "draft":
		if arg == "" {
			m.printIntentions()
			break
		}
		m.startTurn(kindDraft, "calling the model for the draft", func(ctx context.Context, on func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
			it, reply, err := s.Draft(ctx, arg, on)
			return turnDone{item: it, reply: reply, text: reply.Text, check: reply.Check, err: err}
		})
	case "review":
		ask, err := reviewAsk(arg)
		if err != nil {
			m.print(errorLine(err.Error()))
			break
		}
		m.review(ask)
	case "check":
		m.checkFindings(arg)
	case "load":
		if arg == "" {
			m.print(errorLine("/load names an expert; /experts lists them"))
			break
		}
		m.startTurn(kindLoad, "loading "+arg, func(ctx context.Context, _ func(engine.TalkEvent), _ func(tea.Msg)) turnDone {
			_, err := s.LoadExpert(ctx, arg)
			return turnDone{loaded: arg, err: err}
		})
	case "compact":
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
	case "reload":
		if arg == "" {
			m.job(func(ctx context.Context) jobDone { return reloadChoices(ctx, s) })
			break
		}
		m.reload(engine.ReloadMode(arg))
	case "catchup":
		m.catchUp()
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
	case "experts":
		m.job(func(ctx context.Context) jobDone { return experts(ctx, s) })
	case "evidence":
		m.openEvidence(arg)
	case "comments":
		m.openComments(arg)
	case "ledger":
		m.ledger()
	case "items":
		m.openItems()
	case "pin":
		m.job(func(ctx context.Context) jobDone {
			it, err := s.Pin(ctx)
			if err != nil {
				return jobDone{err: err}
			}
			return jobDone{lines: []string{noteLine("the last answer kept as " + it.Name() + "; /copy " + it.Name() + " copies it")}}
		})
	case "copy":
		m.job(func(ctx context.Context) jobDone {
			items, err := s.Items(ctx)
			if err != nil {
				return jobDone{err: err}
			}
			it, err := pick(items, arg)
			if err != nil {
				return jobDone{err: err}
			}
			return jobDone{copied: &it}
		})
	case "model":
		m.chooseModel(strings.Fields(arg))
	case "usage":
		m.printUsage()
	case "debug":
		m.debug = !m.debug
		if m.debug {
			m.print(noteLine("debug: each model call's input, cache share, output and timing, and each tool call's question and answers, from now on"))
		} else {
			m.print(noteLine("debug off"))
		}
	default:
		m.print(errorLine("there is no command /" + name + "; the commands are " + commandHelp()))
	}
	return true
}

// jobDone is a command's work done: lines to print, or why it failed, or
// an item to copy.
type jobDone struct {
	lines  []string
	err    error
	copied *engine.CompanionItem
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
		return m.copyItem(*d.copied)
	}
	return nil
}

// copyItem asks the terminal to put an item on the clipboard, through
// OSC 52, which reaches the engineer's own terminal over SSH; whether it
// does is the terminal's to say.
func (m *model) copyItem(it engine.CompanionItem) tea.Cmd {
	m.print(noteLine("asked the terminal to copy " + it.Name() + " to the clipboard (OSC 52); a terminal that does not take it copies nothing"))
	return tea.SetClipboard(it.Text)
}

// pick is the item named, or the last.
func pick(items []engine.CompanionItem, name string) (engine.CompanionItem, error) {
	if len(items) == 0 {
		return engine.CompanionItem{}, errors.New("there are no items: /pin keeps the last answer, /draft writes a draft")
	}
	if name == "" {
		return items[len(items)-1], nil
	}
	for _, it := range items {
		if it.Name() == name {
			return it, nil
		}
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name())
	}
	return engine.CompanionItem{}, fmt.Errorf("there is no item %s; the items are %s", name, strings.Join(names, ", "))
}

// printIntentions lists what /draft can write.
func (m *model) printIntentions() {
	ins := m.s.Intentions()
	if len(ins) == 0 {
		m.print(errorLine("no intentions are configured (comments.intentions), so there is nothing to draft"))
		return
	}
	m.print(noteLine("/draft <intention>, one of:"))
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

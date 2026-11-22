// Package cli is compa's command line: a deployment's main hands it the
// arguments and the engine's options, and it runs the commands over the
// engine's public API, and the chat, engine/chat, on a terminal.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/chat"
	"github.com/lowmagic/compa-engine/engine/expertcheck"
	"github.com/lowmagic/compa-engine/engine/text"
)

const usage = `usage: compa [--config file] <command>

Configuration is the built-in defaults, then $XDG_CONFIG_HOME/compa/config.yaml
(~/.config/compa/config.yaml) if it exists, then --config if given; each holds
only what it changes.

commands:
  <case> [--no-open]                  open a case in the chat; its opening turn runs
                                      unless --no-open is given
  chat [--dir path] [chat]            a troubleshooting chat with no case, over the
                                      directory's artifacts when given; a chat's
                                      name resumes it, over the directory it
                                      began with
  ask <case> <question>               one turn of the case's conversation; prints
                                      the answer
  case open <case>                    open a case, acquiring its metadata and comments
  case show <case>                    show what is stored for a case
  case trajectory <case> [--json]     what each turn of the case's conversation
                                      did: its calls, what became of the evidence
                                      they returned, its experts, its input
                                      tokens as recorded, the editor's check
  case resend <case> <call> [--model name [--effort effort]]
                                      send a stored model call's request again, to
                                      its model or a configured one, and print
                                      both answers; the call is named as case
                                      trajectory --json gives it, and nothing in
                                      the conversation changes
  case allow-models <case>            where the barred-account check could not be
                                      made, show why and the instructions, then
                                      allow model calls once the account's name is
                                      typed; a match on the list cannot be overridden
  case revoke-models <case>           withdraw that override
  case comments <case> [--exclude-comment id | --exclude-author name |
      --exclude-older id | --exclude-private | --exclude-public |
      --exclude-support | --exclude-customer | --exclude-kind kind |
      --exclude-all | --keep id | --include rule]
                                      the case's comments and which models read;
                                      a rule leaves some out, --exclude-kind those
                                      by authors of a kind the source reports,
                                      --keep reads one whatever else, --include
                                      removes a rule
  case comments <case> --show         every comment whole, newest first, flush and
                                      unwrapped, for copying
  evidence <case> [eN]                the case's named evidence, newest first, or one
                                      item's lines under their numbers
  evidence list <case> [--limit n]    list a case's evidence
  evidence show <case> <evidence-id> [--max-bytes n]
                                      show one piece of evidence
  in <case> [--note text] [file …]
                                      store files, or standard input, as evidence
  expert check [--fail-on-warning] [--json] [--update] <root> [root …]
                                      check expert and catalog packages as the
                                      engine loads them and run their fixture
                                      cases; exit 1 if any fails
  expert routing --keys <file> --library <file> [--write [--revision r]] <root> [root …]
                                      compile a routing library from its keys
                                      and the experts' routing keys; without
                                      --write, exit 1 unless the file holds it
  expert versions [--base <revision>] <root> [root …]
                                      in a git checkout, exit 1 if a package
                                      changed since the base kept its version
`

// errUsage marks a mistake in the command line, exit status 2.
var errUsage = errors.New("usage")

// Main runs the compa command line with the deployment's options and returns
// the process's exit status.
func Main(args []string, o engine.Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, args, o, os.Stdin, os.Stdout, os.Stderr)
}

// Run is Main with its context, input and output given, for tests and for a
// deployment that wraps the command line.
func Run(ctx context.Context, args []string, o engine.Options, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("compa", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	configPath := fl.String("config", "", "configuration file")
	if err := fl.Parse(args); err != nil {
		fmt.Fprintf(stderr, "compa: %v\n\n%s", err, usage)
		return 2
	}
	e, err := engine.New(o)
	if err == nil {
		err = e.LayerConfig(*configPath)
	}
	if err == nil {
		err = (&commandLine{e: e, artifacts: o.Artifacts}).run(ctx, fl.Args(), stdin, stdout)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage), errors.Is(err, expertcheck.ErrUsage):
		fmt.Fprintf(stderr, "compa: %v\n\n%s", err, usage)
		return 2
	default:
		fmt.Fprintf(stderr, "compa: %s\n", text.Safe(err.Error()))
		return 1
	}
}

// commandLine runs one command on the engine, the deployment's artifact
// source at hand for the offer to fetch a case's artifacts.
type commandLine struct {
	e         *engine.Engine
	artifacts engine.ArtifactSource
}

func (l *commandLine) run(ctx context.Context, args []string, in io.Reader, out io.Writer) (err error) {
	if len(args) == 1 && args[0] == "version" {
		_, err := fmt.Fprintln(out, engine.BuildVersion())
		return err
	}
	if len(args) >= 1 && args[0] == "in" {
		r, err := parseIn(args[1:], os.Getenv("COMPA_CASE"), os.Getenv("COMPA_INGEST"))
		if err != nil {
			return err
		}
		return l.ingest(ctx, r, in, out)
	}
	if len(args) >= 1 && args[0] == "chat" {
		fl := flag.NewFlagSet("chat", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		dir := fl.String("dir", "", "")
		pos, err := parseInterleaved(fl, args[1:])
		if err != nil || len(pos) > 1 {
			return fmt.Errorf("%w: chat [--dir path] [chat]", errUsage)
		}
		return l.chatAlone(ctx, strings.Join(pos, ""), *dir, in, out)
	}
	if len(args) >= 1 && args[0] == "ask" {
		if len(args) < 3 {
			return fmt.Errorf("%w: ask <case> <question>", errUsage)
		}
		return l.askOnce(ctx, args[1], strings.Join(args[2:], " "), in, out)
	}
	if len(args) >= 1 && !commands[args[0]] && (len(args) == 1 || strings.HasPrefix(args[1], "-")) {
		fl := flag.NewFlagSet("case", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		noOpen := fl.Bool("no-open", false, "")
		pos, err := parseInterleaved(fl, args)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("%w: <case> [--no-open]", errUsage)
		}
		return l.chatOn(ctx, pos[0], chat.Options{NoOpen: *noOpen}, in, out)
	}
	// A terminal sees the output painted; anything else, the plain text.
	// The offer to fetch a case's artifacts asks on the terminal itself,
	// since the painter holds a line until it ends.
	terminal := out
	if p := paintFor(out, args); p != nil {
		out = p
		defer func() {
			if ferr := p.flush(); err == nil {
				err = ferr
			}
		}()
	}
	if len(args) < 2 {
		return fmt.Errorf("%w: give a command", errUsage)
	}
	if args[0] == "evidence" && args[1] != "list" && args[1] != "show" {
		if len(args) > 3 {
			return fmt.Errorf("%w: evidence <case> [eN]", errUsage)
		}
		return l.evidenceNamed(ctx, args[1], strings.Join(args[2:], ""), out)
	}
	switch args[0] + " " + args[1] {
	case "expert check":
		return expertcheck.Command(ctx, args[2:], out)
	case "expert routing":
		return expertcheck.Routing(ctx, args[2:], out)
	case "expert versions":
		return expertcheck.Versions(ctx, args[2:], out)
	case "case open":
		id, err := oneArg(args[2:], "case open <case>")
		if err != nil {
			return err
		}
		return l.caseOpen(ctx, id, l.fetchOffer(in, terminal), out)
	case "case comments":
		fl := flag.NewFlagSet("case comments", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		comment := fl.String("exclude-comment", "", "")
		author := fl.String("exclude-author", "", "")
		private := fl.Bool("exclude-private", false, "")
		public := fl.Bool("exclude-public", false, "")
		support := fl.Bool("exclude-support", false, "")
		customer := fl.Bool("exclude-customer", false, "")
		authorKind := fl.String("exclude-kind", "", "")
		all := fl.Bool("exclude-all", false, "")
		keep := fl.String("keep", "", "")
		older := fl.String("exclude-older", "", "")
		include := fl.String("include", "", "")
		show := fl.Bool("show", false, "")
		pos, err := parseInterleaved(fl, args[2:])
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("%w: case comments <case> [--exclude-comment id | --exclude-author name | --exclude-older id | --exclude-private | --exclude-public | --exclude-support | --exclude-customer | --exclude-kind kind | --exclude-all | --keep id | --include rule | --show]", errUsage)
		}
		var kind, value string
		n := 0
		for _, o := range []struct {
			on          bool
			kind, value string
		}{{*comment != "", engine.ExcludeComment, *comment}, {*author != "", engine.ExcludeAuthor, *author}, {*private, engine.ExcludePrivate, ""}, {*public, engine.ExcludePublic, ""},
			{*support, engine.ExcludeSupport, ""}, {*customer, engine.ExcludeCustomer, ""}, {*authorKind != "", engine.ExcludeKind, *authorKind}, {*all, engine.ExcludeAll, ""}, {*keep != "", engine.KeepComment, *keep}, {*older != "", engine.ExcludeOlder, *older}, {*include != "", "include", *include}} {
			if o.on {
				kind, value = o.kind, o.value
				n++
			}
		}
		if n > 1 {
			return fmt.Errorf("%w: one rule at a time", errUsage)
		}
		if *show {
			if n > 0 {
				return fmt.Errorf("%w: case comments <case> --show, with no rule", errUsage)
			}
			return l.printComments(ctx, pos[0], out)
		}
		return l.caseComments(ctx, pos[0], kind, value, out)
	case "case allow-models":
		id, err := oneArg(args[2:], "case allow-models <case>")
		if err != nil {
			return err
		}
		return l.caseAllowModels(ctx, id, in, out)
	case "case revoke-models":
		id, err := oneArg(args[2:], "case revoke-models <case>")
		if err != nil {
			return err
		}
		return l.caseRevokeModels(ctx, id, out)
	case "case show":
		id, err := oneArg(args[2:], "case show <case>")
		if err != nil {
			return err
		}
		return l.caseShow(ctx, id, out)
	case "case resend":
		fl := flag.NewFlagSet("case resend", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		modelName := fl.String("model", "", "")
		effort := fl.String("effort", "", "")
		pos, err := parseInterleaved(fl, args[2:])
		if err != nil || len(pos) != 2 || *effort != "" && *modelName == "" {
			return fmt.Errorf("%w: case resend <case> <call> [--model name [--effort effort]]", errUsage)
		}
		return l.caseResend(ctx, pos[0], pos[1], *modelName, *effort, out)
	case "case trajectory":
		fl := flag.NewFlagSet("case trajectory", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		asJSON := fl.Bool("json", false, "")
		pos, err := parseInterleaved(fl, args[2:])
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("%w: case trajectory <case> [--json]", errUsage)
		}
		return l.caseTrajectory(ctx, pos[0], *asJSON, out)
	case "evidence list":
		fl := flag.NewFlagSet("evidence list", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		limit := fl.Int("limit", 50, "")
		pos, err := parseInterleaved(fl, args[2:])
		if err != nil || len(pos) != 1 || *limit < 1 {
			return fmt.Errorf("%w: evidence list <case> [--limit n]", errUsage)
		}
		return l.evidenceList(ctx, pos[0], *limit, out)
	case "evidence show":
		fl := flag.NewFlagSet("evidence show", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		maxBytes := fl.Int("max-bytes", 64<<10, "")
		pos, err := parseInterleaved(fl, args[2:])
		if err != nil || len(pos) != 2 || *maxBytes < 1 {
			return fmt.Errorf("%w: evidence show <case> <evidence-id> [--max-bytes n]", errUsage)
		}
		return l.evidenceShow(ctx, pos[0], pos[1], *maxBytes, out)
	}
	return fmt.Errorf("%w: unknown command %q", errUsage, strings.Join(args[:2], " "))
}

// commands are the first words of the subcommands; any other first
// argument is a case to open in the chat.
var commands = map[string]bool{"chat": true, "ask": true, "in": true, "case": true, "evidence": true, "expert": true, "version": true}

func oneArg(args []string, form string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("%w: %s", errUsage, form)
	}
	return args[0], nil
}

// parseInterleaved lets flags follow positional arguments, as
// "evidence list 00000001 --limit 5" reads naturally; the standard
// library's flag stops at the first positional one.
func parseInterleaved(fl *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, err
		}
		if fl.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fl.Arg(0))
		args = fl.Args()[1:]
	}
}

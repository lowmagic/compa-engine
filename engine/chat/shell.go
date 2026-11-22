package chat

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/text"
)

// shellDoneMsg is the engineer's shell ended, and how.
type shellDoneMsg struct{ err error }

// subshell is the engineer's shell as Bubble Tea runs it in the terminal
// it gives up: the banner first, then the shell.
type subshell struct {
	cmd    *exec.Cmd
	banner string
}

func (s *subshell) SetStdin(r io.Reader)  { s.cmd.Stdin = r }
func (s *subshell) SetStdout(w io.Writer) { s.cmd.Stdout = w }
func (s *subshell) SetStderr(w io.Writer) { s.cmd.Stderr = w }

func (s *subshell) Run() error {
	if s.cmd.Stdout != nil {
		if _, err := io.WriteString(s.cmd.Stdout, s.banner); err != nil {
			return err
		}
	}
	return s.cmd.Run()
}

// shell opens the engineer's shell in the case's artifact folder, the
// chat giving it the terminal until it ends: COMPA_CASE names the case and
// COMPA_INGEST the socket compa in sends to, which this chat serves, and
// the compa running the chat is first on the PATH. Nothing the shell runs
// is kept unless piped to compa in. Text after ! is refused, saying how.
func (m *model) shell(rest string) (bool, tea.Cmd) {
	if rest != "" {
		m.print(errorLine("! alone opens your shell, where " + rest + " | compa in \"what does this answer?\" keeps its output"))
		return false, nil
	}
	if m.turn != nil {
		m.print(errorLine("a turn is running: wait for its answer, or ctrl+c to cancel it"))
		return false, nil
	}
	m.print(commandLine("!"))
	return true, tea.Exec(m.subshellFor(), func(err error) tea.Msg { return shellDoneMsg{err} })
}

// subshellFor is the engineer's shell as ! opens it.
func (m *model) subshellFor() *subshell {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	// The shell is the engineer's to end, not the chat's: no context of
	// the chat's cancels it.
	c := exec.CommandContext(context.Background(), sh)
	info := m.s.Store()
	c.Dir = info.ArtifactRoot
	c.Env = append(os.Environ(), "COMPA_CASE="+m.s.CaseID())
	name, reach := "compa", ""
	if exe, err := os.Executable(); err == nil {
		name = filepath.Base(exe)
		c.Env = append(c.Env, "PATH="+filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if m.ingest != nil {
		c.Env = append(c.Env, "COMPA_INGEST="+m.ingest.Socket())
	} else if m.ingestErr != nil {
		reach = m.ingestErr.Error()
	}
	return &subshell{cmd: c, banner: shellBanner(name, m.s.CaseID(), c.Dir, reach, m.cols())}
}

// shellBanner is what the shell opens under: loud, and three lines, the
// case and where the shell is, how to keep what it shows, and how to come
// back; where compa in cannot reach the chat, why.
func shellBanner(name, id, dir, unreachable string, width int) string {
	width = max(min(width, 80), 40)
	head := "━━ compa shell · " + id
	// The folder is shortened from its start to keep the line one row.
	if room := width - len([]rune(head)) - 6; dir != "" && room > 1 {
		if r := []rune(dir); len(r) > room {
			dir = "…" + string(r[len(r)-room+1:])
		}
		head += " · " + dir
	}
	head += " "
	rule := strings.Repeat("━", width)
	keep := "  Keep evidence:  <command> | " + name + " in \"what does this answer?\" [-t tag,tag]"
	if unreachable != "" {
		keep = "  " + name + " in cannot reach this chat: " + unreachable
	}
	lines := []string{
		headingW.Render(head + strings.Repeat("━", max(width-len([]rune(head)), 2))),
		keep,
		"  Back to chat:   exit (or ctrl+d)",
		headingW.Render(rule),
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// shellDone is the chat back from the shell: what was kept meanwhile
// listed, which the next message names.
func (m *model) shellDone(err error) {
	if err != nil {
		m.print(warnLine("the shell ended: " + err.Error()))
	}
	got := m.takeBrought()
	if len(got) == 0 {
		m.print(noteLine("back from the shell: nothing kept; <command> | compa in keeps its output"))
		return
	}
	m.print(noteLine("back from the shell: " + text.Plural(len(got), "item") + " kept, which your next message names"))
}

// takeBrought takes what compa in kept since it was last taken, lists it,
// and keeps it for the next message to name.
func (m *model) takeBrought() []engine.IngestedItem {
	if m.ingest == nil {
		return nil
	}
	got := m.ingest.Take()
	for _, it := range got {
		m.print(noteLine("kept " + broughtShown(it)))
	}
	m.brought = append(m.brought, got...)
	return got
}

// broughtShown is an item compa in kept, as the chat and the next message
// name it: its name, where it came from, its lines and the engineer's
// note.
func broughtShown(it engine.IngestedItem) string {
	l := it.Name + " (" + it.Location + ", " + text.Plural(int(it.Lines), "line")
	if it.Note != "" {
		l += ": " + it.Note
	}
	return text.Safe(l + ")")
}

package cli

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// The command line paints a terminal's output and nothing else: headings
// bold, warnings yellow, evidence IDs faint, each painted line the plain
// one once its escape codes are taken out. A pipe or a buffer gets no
// painter; neither does --json or --show.
func TestCLIPaintsOnlyATerminal(t *testing.T) {
	for _, c := range []struct{ line, sgr string }{
		{"── 1/3 · answer · 2026-10-27 09:00 · fixture.storage", sgrBold},
		{"rests on:", sgrBold},
		{"ID                   TYPE               BYTES  LINES  CREATED               LOCATION", sgrBold},
		{"not shown: given by reference only, not read: #1", sgrYellow},
		{"no valid answer: schema_violation", sgrYellow},
		{"latest case metadata, ev-cc92e8f2c89cd30e:", sgrBold},
		{"answer: it rests on ev-cc92e8f2c89cd30e and ev-2a12b9e6e248a789.", sgrFaint},
		{"question: Why will the volume not attach?", ""},
		{"  [A] Read the CSI driver's events.", ""},
	} {
		got := paintLine(c.line)
		if sgr.ReplaceAllString(got, "") != c.line {
			t.Errorf("%q painted changes its text: %q", c.line, got)
		}
		if c.sgr == "" && got != c.line || c.sgr != "" && !strings.Contains(got, c.sgr) {
			t.Errorf("%q painted %q, want %q", c.line, got, c.sgr)
		}
	}
	if n := strings.Count(paintLine("answer: ev-cc92e8f2c89cd30e and ev-2a12b9e6e248a789"), sgrFaint); n != 2 {
		t.Errorf("each evidence ID is faint: %d", n)
	}

	var out bytes.Buffer
	p := &painter{w: &out}
	p.Write([]byte("── 1/1 · ans"))
	p.Write([]byte("wer\nplain\nnot shown: x"))
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	if want := sgrBold + "── 1/1 · answer" + sgrReset + "\nplain\n" + sgrYellow + "not shown: x" + sgrReset; out.String() != want {
		t.Errorf("the painter wrote %q, want %q", out.String(), want)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if paintFor(&out, nil) != nil || paintFor(w, nil) != nil {
		t.Error("a buffer or a pipe is painted")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		t.Log("no terminal to check --json, --show and NO_COLOR against")
		return
	}
	defer tty.Close()
	if paintFor(tty, []string{"case", "ask", "--json"}) != nil || paintFor(tty, []string{"case", "comments", "--show"}) != nil {
		t.Error("--json or --show is painted")
	}
	t.Setenv("NO_COLOR", "1")
	if paintFor(tty, nil) != nil {
		t.Error("NO_COLOR is painted")
	}
}

package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/evidence"
)

func comment(id, body string) evidence.Input {
	text := "case: 00000001\ncomment: " + id + "\n\n" + body
	return evidence.Input{Ref: evidence.Ref{ID: "ev-" + id, Type: TypeCaseComment}, Text: text, Inline: true, Size: evidence.SizeOf(text, false)}
}

// TestDigestComments: each rule on synthetic comments, newest first as a
// request carries them — a signature found in three comments kept in the
// oldest and cut from the others, naming it, a repeated comment pointing to the oldest, a quoted reply of an earlier comment and a
// run of quoted lines cut, a long log run keeping its head and tail —
// and anything that is not a comment passed as it is, sizes kept.
func TestDigestComments(t *testing.T) {
	sig := "Regards,\nJane Doe\nSenior Engineer, Example Corp\n+1 555 0100"
	var log []string
	for i := 0; i < 100; i++ {
		log = append(log, fmt.Sprintf("2026-10-01T09:%02d:%02dZ worker-1 kubelet[2211]: event %d", i/60, i%60, i))
	}
	meta := evidence.Input{Ref: evidence.Ref{ID: "ev-meta", Type: TypeCaseMetadata}, Text: "case: 00000001\n\n" + sig, Inline: true}
	in := []evidence.Input{
		meta,
		comment("c5", "One more thing.\n> you said\n> earlier\nThat is all."),
		comment("c4", "Here is the kubelet log:\n"+strings.Join(log, "\n")),
		comment("c3", "It happened again.\n\nOn Tue, Oct 1, 2026 at 9:00 AM Jane Doe <jane@example.com> wrote:\n> Pods fail to start\n> after the upgrade.\n>\n> More."),
		comment("c2", "Pods  fail to start\nafter the upgrade.\n\n"+sig),
		comment("c1", "Pods fail to start after the upgrade.\n\n"+sig),
		comment("c0", "First contact.\n\n"+sig),
	}
	out, st := digestComments(in)
	text := map[string]string{}
	for _, e := range out {
		_, body, _ := strings.Cut(e.Text, "\n\n")
		text[e.Ref.ID] = body
	}
	if out[0].Text != meta.Text {
		t.Errorf("metadata changed: %q", out[0].Text)
	}
	if !strings.Contains(text["ev-c0"], "Jane Doe") {
		t.Errorf("signature not kept in the oldest: %q", text["ev-c0"])
	}
	if strings.Contains(text["ev-c1"], "Jane Doe") || !strings.Contains(text["ev-c1"], "[1 paragraph also in ev-c0 cut here]") {
		t.Errorf("signature not cut: %q", text["ev-c1"])
	}
	if text["ev-c2"] != "[repeats ev-c1]" || !strings.HasPrefix(text["ev-c1"], "Pods fail to start after the upgrade.") {
		t.Errorf("repeat: c2 %q, c1 %q", text["ev-c2"], text["ev-c1"])
	}
	if strings.Contains(text["ev-c3"], "Pods fail") || !strings.Contains(text["ev-c3"], "It happened again.") || !strings.Contains(text["ev-c3"], "[an earlier message quoted here was cut]") {
		t.Errorf("quoted reply: %q", text["ev-c3"])
	}
	if !strings.Contains(text["ev-c5"], "[2 quoted lines cut]") || !strings.Contains(text["ev-c5"], "That is all.") {
		t.Errorf("quoted lines: %q", text["ev-c5"])
	}
	if !strings.Contains(text["ev-c4"], "event 0\n") || !strings.Contains(text["ev-c4"], "event 99") || strings.Contains(text["ev-c4"], "event 50") || !strings.Contains(text["ev-c4"], "[70 log lines cut]") {
		t.Errorf("log run: %q", text["ev-c4"])
	}
	for i := range in {
		if out[i].Size != in[i].Size {
			t.Errorf("%s: size changed", in[i].Ref.ID)
		}
	}
	if st.Comments != 6 || st.Repeats != 1 || st.Quoted != 2 || st.Boilerplate != 2 || st.LogLines != 70 || st.After >= st.Before {
		t.Errorf("stats %+v", st)
	}
	if in[1].Text == out[1].Text {
		t.Error("the input slice was changed in place, or nothing was cut")
	}
}

// A paragraph repeated across comments is kept once, in the oldest, and
// the others point to it: repeated symptoms are evidence.
func TestDigestKeepsOneCopy(t *testing.T) {
	body := "The API server is still returning HTTP 503 after the rollback."
	out, _ := digestComments([]evidence.Input{comment("c3", body), comment("c2", body), comment("c1", body)})
	if !strings.Contains(out[2].Text, body) {
		t.Errorf("the oldest copy was cut: %q", out[2].Text)
	}
	for _, e := range out[:2] {
		if strings.Contains(e.Text, body) || !strings.Contains(e.Text, "ev-c1") {
			t.Errorf("%s does not point to the copy kept: %q", e.Ref.ID, e.Text)
		}
	}
}

// A line such as "On Tuesday the node wrote:" does not make what follows
// a quoted reply: it is cut only when an earlier comment holds it.
func TestDigestKeepsAReportItQuotesFromNowhere(t *testing.T) {
	report := "2026-10-20T10:00:00Z kernel: Out of memory: Killed process 732"
	out, st := digestComments([]evidence.Input{comment("q", "On Tuesday the node wrote:\n"+report)})
	if !strings.Contains(out[0].Text, "Out of memory") || st.Quoted != 0 {
		t.Errorf("the report was cut as a quoted reply: %q", out[0].Text)
	}
}

// Comments differing only in case are not repeats: a changed identifier
// is what tells them apart.
func TestDigestKeepsCase(t *testing.T) {
	out, st := digestComments([]evidence.Input{comment("new", "Secret PROD is missing."), comment("old", "Secret prod is missing.")})
	if !strings.Contains(out[0].Text, "Secret PROD is missing.") || st.Repeats != 0 {
		t.Errorf("a comment differing in case was taken as a repeat: %q", out[0].Text)
	}
}

// Every line a digested comment keeps carries its stored number, so a
// model cites the comment's stored lines; a line the digest wrote is
// numbered 0, and a comment the digest left whole needs no numbers.
func TestDigestNumbersStoredLines(t *testing.T) {
	sig := "Regards,\nJane Doe\nSenior Engineer, Example Corp\n+1 555 0100"
	var log []string
	for i := 0; i < 100; i++ {
		log = append(log, fmt.Sprintf("worker-1 kubelet: event %d", i))
	}
	in := []evidence.Input{
		comment("c5", "One more thing.\n> you said\n> earlier\nThat is all."),
		comment("c4", "Here is the kubelet log:\n"+strings.Join(log, "\n")+"\nend"),
		comment("c3", "It happened again.\n\nOn Tue, Oct 1, 2026 at 9:00 AM Jane Doe <jane@example.com> wrote:\n> Pods fail to start\n> after the upgrade."),
		comment("c2", "Pods fail to start\nafter the upgrade.\n\n"+sig),
		comment("c1", "Something else.\n\n"+sig),
		comment("c0", "First contact.\n\n"+sig),
	}
	stored := map[string][]string{}
	for _, e := range in {
		stored[e.Ref.ID] = strings.Split(e.Text, "\n")
	}
	out, _ := digestComments(in)
	numbered := 0
	for _, e := range out {
		if len(e.Lines) == 0 {
			continue
		}
		numbered++
		got := strings.Split(e.Text, "\n")
		if len(got) != len(e.Lines) {
			t.Fatalf("%s: %d lines, %d numbers", e.Ref.ID, len(got), len(e.Lines))
		}
		for i, n := range e.Lines {
			if n > 0 && stored[e.Ref.ID][n-1] != got[i] {
				t.Errorf("%s line %d says it is stored line %d: %q, stored %q", e.Ref.ID, i, n, got[i], stored[e.Ref.ID][n-1])
			}
		}
	}
	if numbered < 3 {
		t.Errorf("only %d comments numbered", numbered)
	}
}

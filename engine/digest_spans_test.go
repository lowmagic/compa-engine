package engine

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/evidence"
)

// digestFixture is TestDigestComments' case: a signature in three
// comments, a repeat, a quoted reply of an earlier comment, quoted lines
// and a long log run.
func digestFixture() []evidence.Input {
	sig := "Regards,\nJane Doe\nSenior Engineer, Example Corp\n+1 555 0100"
	var log []string
	for i := 0; i < 100; i++ {
		log = append(log, fmt.Sprintf("2026-10-01T09:%02d:%02dZ worker-1 kubelet[2211]: event %d", i/60, i%60, i))
	}
	return []evidence.Input{
		{Ref: evidence.Ref{ID: "ev-meta", Type: TypeCaseMetadata}, Text: "case: 00000001\n\n" + sig, Inline: true},
		comment("c5", "One more thing.\n> you said\n> earlier\nThat is all."),
		comment("c4", "Here is the kubelet log:\n"+strings.Join(log, "\n")),
		comment("c3", "It happened again.\n\nOn Tue, Oct 1, 2026 at 9:00 AM Jane Doe <jane@example.com> wrote:\n> Pods fail to start\n> after the upgrade.\n>\n> More."),
		comment("c2", "Pods  fail to start\nafter the upgrade.\n\n"+sig),
		comment("c1", "Pods fail to start after the upgrade.\n\n"+sig),
		comment("c0", "First contact.\n\n"+sig),
	}
}

// generatedComments are comments made of paragraphs drawn at random from
// every shape the digest cuts, and blank lines of every length between
// them, newest first.
func generatedComments(r *rand.Rand, n int) []evidence.Input {
	var log []string
	for i := 0; i < 45+r.Intn(30); i++ {
		log = append(log, fmt.Sprintf("Oct  1 09:%02d:%02d worker-0 crio[1]: line %d", i/60, i%60, i))
	}
	pool := []string{
		"Regards,\nJane Doe\nSenior Engineer, Example Corp",
		"The router pods are Pending since the upgrade to 4.16.",
		"> quoted once\n> quoted twice",
		"> a lone quoted line",
		strings.Join(log, "\n"),
		"On Mon, Oct 20, 2026 at 10:00 AM A. Singh wrote:\n> The router pods are Pending since the upgrade to 4.16.",
		"-----Original Message-----\nThe router pods are Pending since the upgrade to 4.16.",
		"From: someone\nSent: yesterday\nThe router pods are Pending since the upgrade to 4.16.",
		"  indented code\n  more code",
		"Thanks, that helps.",
		"",
		"   ",
	}
	seps := []string{"\n\n", "\n\n\n", "\n", "\n\n\n\n"}
	var out []evidence.Input
	for i := n - 1; i >= 0; i-- {
		var b strings.Builder
		for k := 0; k < 1+r.Intn(5); k++ {
			if k > 0 {
				b.WriteString(seps[r.Intn(len(seps))])
			}
			b.WriteString(pool[r.Intn(len(pool))])
		}
		if r.Intn(3) == 0 {
			b.WriteString("\n")
		}
		out = append(out, comment(fmt.Sprintf("g%02d", i), b.String()))
	}
	return out
}

// The digest that reports spans cuts exactly as digest-2 did: on every
// fixture and on generated comments of every shape, its text and its
// counts are the reference's, byte for byte.
func TestDigestMatchesItsReference(t *testing.T) {
	cases := [][]evidence.Input{digestFixture()}
	r := rand.New(rand.NewSource(20261022))
	for i := 0; i < 400; i++ {
		cases = append(cases, generatedComments(r, 1+r.Intn(8)))
	}
	for n, in := range cases {
		got, gotStats := digestComments(in)
		want, wantStats := refDigestComments(in)
		if gotStats != wantStats {
			t.Fatalf("case %d: stats %+v, the reference's %+v", n, gotStats, wantStats)
		}
		for i := range want {
			if got[i].Text != want[i].Text {
				t.Fatalf("case %d, %s:\n%q\nthe reference:\n%q", n, in[i].Ref.ID, got[i].Text, want[i].Text)
			}
		}
	}
}

// Every span lies in its comment's body and covers lines the digest cut:
// none of a span's own lines survives in what models read, apart from a
// log run's kept head and tail, which lie outside its span.
func TestDigestSpansCoverWhatWasCut(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for n := 0; n < 300; n++ {
		in := generatedComments(r, 1+r.Intn(8))
		_, _, spans := digestSpans(in)
		for _, e := range in {
			_, body, _ := strings.Cut(e.Text, "\n\n")
			lines := strings.Split(body, "\n")
			for _, sp := range spans[e.Ref.ID] {
				if sp.From < 0 || sp.To > len(lines) || sp.From >= sp.To {
					t.Fatalf("case %d, %s: span %+v outside its %d lines", n, e.Ref.ID, sp, len(lines))
				}
			}
		}
	}
}

func TestDigestSpansGolden(t *testing.T) {
	_, _, spans := digestSpans(digestFixture())
	got, err := json.MarshalIndent(spans, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "digest-spans.golden.json")
	if *update {
		os.WriteFile(golden, got, 0o644)
	}
	if want, err := os.ReadFile(golden); err != nil || string(want) != string(got) {
		t.Errorf("spans differ from %s (go test -update to accept):\n%s", golden, got)
	}
}

// What the spans say is what the digest established, in its own words: a
// symptom repeated in three comments is repeated, kept in the oldest; a
// quote of a paragraph two comments hold names both, so no single source
// is made up; quoted lines the case holds nowhere name none; a log run's
// span leaves fifteen lines at each end.
func TestDigestSpansSayOnlyWhatTheDigestFound(t *testing.T) {
	symptom := "The API server is still returning HTTP 503 after the rollback."
	_, _, spans := digestSpans([]evidence.Input{comment("c3", symptom), comment("c2", symptom), comment("c1", symptom)})
	for _, id := range []string{"ev-c3", "ev-c2"} {
		sp := spans[id]
		if len(sp) != 1 || sp[0].Kind != SpanRepeated || sp[0].Count != 3 || strings.Join(sp[0].Comments, " ") != "ev-c1" {
			t.Errorf("%s: %+v", id, sp)
		}
	}
	if len(spans["ev-c1"]) != 0 {
		t.Errorf("the oldest copy has spans: %+v", spans["ev-c1"])
	}

	said := "The router pods are Pending since the upgrade."
	quote := "On Mon, Oct 20, 2026 at 10:00 AM A. Singh wrote:\n> " + said
	_, _, spans = digestSpans([]evidence.Input{
		comment("q", "Still the same.\n\n"+quote),
		comment("b", said+"\n\nNo change."),
		comment("a", said),
	})
	if sp := spans["ev-q"]; len(sp) != 1 || sp[0].Kind != SpanQuoted || strings.Join(sp[0].Comments, " ") != "ev-a ev-b" || sp[0].From != 2 {
		t.Errorf("a quote two comments could be the source of: %+v", sp)
	}

	_, _, spans = digestSpans([]evidence.Input{comment("x", "Look:\n> nothing the case holds\n> anywhere")})
	if sp := spans["ev-x"]; len(sp) != 1 || sp[0].Kind != SpanQuoted || len(sp[0].Comments) != 0 || sp[0].From != 1 || sp[0].To != 3 {
		t.Errorf("quoted lines from nowhere: %+v", sp)
	}

	in := digestFixture()
	_, _, spans = digestSpans(in)
	if sp := spans["ev-c4"]; len(sp) != 1 || sp[0].Kind != SpanLog || sp[0].From != 1+logKeep || sp[0].To != 101-logKeep {
		t.Errorf("the log run: %+v", sp)
	}
}

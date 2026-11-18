package engine

// This is the digest as it stood at digest-2 before it reported spans,
// kept as the reference the digest must still match byte for
// byte: a viewer folding what the digest cuts must not change what models
// read.

import (
	"fmt"
	"strings"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/evidence"
)

func refDigestComments(in []evidence.Input) ([]evidence.Input, DigestStats) {
	var st DigestStats
	type parsed struct {
		header string
		paras  []string
	}
	out := append([]evidence.Input(nil), in...)
	bodies := map[int]*parsed{}
	for i, e := range out {
		if e.Ref.Type != TypeCaseComment || !e.Inline || e.Text == "" {
			continue
		}
		st.Comments++
		st.Before += len(e.Text)
		header, body, _ := strings.Cut(e.Text, "\n\n")
		bodies[i] = &parsed{header: header, paras: refSplitParas(body)}
	}
	// Boilerplate: a paragraph found in several comments.
	seenIn := map[string]int{}
	for _, p := range bodies {
		mine := map[string]bool{}
		for _, para := range p.paras {
			if k := refNorm(para); len(k) >= boilerplateMin && !mine[k] {
				mine[k] = true
				seenIn[k]++
			}
		}
	}
	// Repeats point to the oldest, and a recurring paragraph stays in the
	// oldest holding it; inputs come newest first.
	first := map[string]int{}
	holder := map[string]int{}
	// earlier holds the paragraphs of the comments before this one, quote
	// marks taken off, so a quoted message is cut only when the case
	// already holds what it quotes.
	earlier := map[string]bool{}
	for i := len(out) - 1; i >= 0; i-- {
		p := bodies[i]
		if p == nil {
			continue
		}
		// kept is what the comment shows; own is what it says apart from
		// recurring paragraphs, which decides whether it repeats another.
		var kept, own []string
		var holders []string
		for _, para := range p.paras {
			k := refNorm(para)
			if seenIn[k] < boilerplateComments {
				kept, own = append(kept, para), append(own, para)
				continue
			}
			j, held := holder[k]
			if !held {
				holder[k] = i
				kept = append(kept, para)
				continue
			}
			if j != i {
				holders = append(holders, out[j].Ref.ID)
			}
		}
		st.Boilerplate += len(holders)
		body, quoted := refCutQuotes(strings.Join(kept, "\n\n"), earlier)
		st.Quoted += quoted
		body, logCut := refCutLogRuns(body)
		st.LogLines += logCut
		if len(holders) > 0 {
			body = strings.TrimRight(body, "\n") + fmt.Sprintf("\n\n[%s also in %s cut here]", text.Plural(len(holders), "paragraph"), strings.Join(refUnique(holders), ", "))
		}
		ownBody, _ := refCutQuotes(strings.Join(own, "\n\n"), earlier)
		ownBody, _ = refCutLogRuns(ownBody)
		key := refNorm(ownBody)
		if j, ok := first[key]; ok && key != "" {
			body = fmt.Sprintf("[repeats %s]", out[j].Ref.ID)
			st.Repeats++
		} else if key != "" {
			first[key] = i
		}
		out[i].Text = p.header + "\n\n" + body
		st.After += len(out[i].Text)
		for _, para := range p.paras {
			earlier[refNorm(refUnquote(para))] = true
		}
	}
	return out, st
}

// cutQuotes cuts a quoted earlier message and runs of quoted lines. A
// line such as "On Tuesday the node wrote:" is not evidence enough of a
// quoted reply: what follows it is cut only when its first paragraph is
// one an earlier comment holds, so a report the case does not otherwise
// carry stays.
func refCutQuotes(body string, earlier map[string]bool) (string, int) {
	n := 0
	if loc := quoteStart.FindStringIndex(body); loc != nil && earlier[refNorm(refFirstPara(body[loc[1]:]))] {
		body = strings.TrimRight(body[:loc[0]], "\n") + "\n\n[an earlier message quoted here was cut]"
		n++
	}
	body, runs := refCutQuotedLines(body)
	return body, n + runs
}

// firstPara is the first paragraph of a quoted message, its quote marks
// taken off first, since a line of ">" alone ends a quoted paragraph.
func refFirstPara(s string) string {
	for _, p := range strings.Split(refUnquote(s), "\n\n") {
		if strings.TrimSpace(p) != "" {
			return p
		}
	}
	return ""
}

// unquote takes the quote marks off each line.
func refUnquote(para string) string {
	lines := strings.Split(para, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(strings.TrimSpace(l), "> ")
	}
	return strings.Join(lines, "\n")
}

func refUnique(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func refSplitParas(body string) []string {
	var out []string
	for _, p := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// norm compares texts apart from their whitespace. Case is kept: PROD
// and prod may name two different things.
func refNorm(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// cutQuotedLines replaces each run of two or more ">"-quoted lines.
func refCutQuotedLines(body string) (string, int) {
	lines := strings.Split(body, "\n")
	var out []string
	runs := 0
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), ">") {
			j++
		}
		if j-i >= 2 {
			out = append(out, fmt.Sprintf("[%d quoted lines cut]", j-i))
			runs++
			i = j
			continue
		}
		if j > i {
			out = append(out, lines[i:j]...)
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n"), runs
}

// cutLogRuns keeps the head and tail of each long run of log lines.
func refCutLogRuns(body string) (string, int) {
	lines := strings.Split(body, "\n")
	var out []string
	cut := 0
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && logLine.MatchString(lines[j]) {
			j++
		}
		if n := j - i; n > logRunMax {
			out = append(out, lines[i:i+logKeep]...)
			out = append(out, fmt.Sprintf("[%d log lines cut]", n-2*logKeep))
			out = append(out, lines[j-logKeep:j]...)
			cut += n - 2*logKeep
			i = j
			continue
		}
		if j > i {
			out = append(out, lines[i:j]...)
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n"), cut
}

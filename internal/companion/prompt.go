package companion

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed prompt.md
var prompt string

// CustomerDraft is what a draft for the customer is asked to hold to:
// the certainty review, the companion's
// prompt carrying only the base discipline. It goes with each such request
// rather than in the system prompt, so a conversation begun before it has
// it too, and an internal note does not.
//
//go:embed customer.md
var CustomerDraft string

// ExpertLine is one installed expert as the index lists it.
type ExpertLine struct {
	ID      string
	Summary string
	Keys    []string
}

// Intention is one purpose a case comment may be drafted for.
type Intention struct {
	ID, Label, Audience, Purpose string
}

// System is the system prompt: the companion's method, then the expert
// index, the deployment's house style for case comments and its house
// section. Every part is the same for every case of a deployment, so one
// cached prefix serves them all; experts and intentions are listed in the
// order given, which the caller keeps stable. house is the deployment's
// own text, appended as it is given: no part of it is interpreted, and
// none of the method is replaced by it.
func System(experts []ExpertLine, style string, intentions []Intention, house string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(prompt))
	b.WriteString("\n\n## Installed experts\n\n")
	if len(experts) == 0 {
		b.WriteString("None are installed.\n")
	}
	for _, e := range experts {
		fmt.Fprintf(&b, "- `%s`: %s", e.ID, oneLine(e.Summary))
		if len(e.Keys) > 0 {
			fmt.Fprintf(&b, " (routing keys: %s)", strings.Join(e.Keys, ", "))
		}
		b.WriteString("\n")
	}
	if style != "" || len(intentions) > 0 {
		b.WriteString("\n## Case comments: house style and intentions\n\n")
		if style != "" {
			b.WriteString(oneLine(style))
			b.WriteString("\n\n")
		}
		for _, in := range intentions {
			fmt.Fprintf(&b, "- `%s`, %s, for %s: %s\n", in.ID, in.Label, audience(in.Audience), oneLine(in.Purpose))
		}
	}
	if house = strings.TrimSpace(house); house != "" {
		b.WriteString("\n## This deployment: its support organization, knowledge sources and tool conventions\n\n")
		b.WriteString(house)
		b.WriteString("\n")
	}
	return b.String()
}

func audience(a string) string {
	if a == "internal" {
		return "an internal note"
	}
	return "the customer"
}

// oneLine collapses text's whitespace, so configuration's folded YAML and
// a manifest's line breaks render the same.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

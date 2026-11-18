package check

import (
	_ "embed"
	"strings"

	"github.com/lowmagic/compa-engine/internal/companion"
)

var (
	//go:embed editor.md
	editor string
	//go:embed reviewer.md
	reviewer string
)

// withRules is a check's prompt with the rules of the certainty review
// as the companion's customer drafts carry them in its {customer.md}
// place, included from that one file rather than restated.
func withRules(prompt string) string {
	return strings.Replace(strings.TrimSpace(prompt), "{customer.md}", strings.TrimSpace(companion.CustomerDraft), 1)
}

// editorPrompt is the editor's system prompt: its task, then the rules.
var editorPrompt = withRules(editor)

// reviewerPrompt is the reviewer's system prompt: the adversarial
// review and its form, then the rules.
var reviewerPrompt = withRules(reviewer)

// EditorPrompt is the editor's system prompt, the same bytes on every call.
func EditorPrompt() string { return editorPrompt }

// ReviewerPrompt is the reviewer's system prompt, the same bytes on every
// call.
func ReviewerPrompt() string { return reviewerPrompt }

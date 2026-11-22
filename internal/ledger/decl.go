package ledger

import (
	_ "embed"

	"github.com/lowmagic/compa-engine/internal/model"
)

// ToolName is the one tool the ledger's model is given, and must call with
// its update as the arguments.
const ToolName = "ledger_update"

//go:embed prompt.md
var prompt string

// Prompt is the ledger's system prompt, the same bytes on every call.
func Prompt() string { return prompt }

// Decl declares ledger_update: the update's schema written in the keywords
// both providers accepted live, so without the references, array bounds
// and lengths the public schema adds, which Validate checks.
func Decl() model.Tool {
	list := func(vals ...string) []any {
		out := make([]any, len(vals))
		for i, v := range vals {
			out[i] = v
		}
		return out
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	enum := func(desc string, vals ...string) map[string]any {
		return map[string]any{"type": "string", "description": desc, "enum": list(vals...)}
	}
	comment := "A comment by its name in the request, such as e12."
	item := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             list("kind", "who", "text", "comment", "status"),
		"properties": map[string]any{
			"kind":       enum("What the comment does.", Asks, Commits, Reports, Provides),
			"who":        enum("Whose comment it is, from its author kind.", "customer", "support"),
			"text":       map[string]any{"type": "string", "description": "The item in one line, in your words: what, where and when, without opening with asks, reports, commits, provides, customer or support.", "maxLength": 300},
			"comment":    str(comment),
			"due":        str("For a commitment, when it was promised by, as the comment says it."),
			"attachment": str("For what was provided, the attachment exactly as the comment names it."),
			"status": enum("open, answered or ruled_out for an ask; pending, done or dropped for a commitment; none or superseded for what was provided; none for a report.",
				Open, Answered, RuledOut, Pending, Done, Dropped, Superseded, none),
			"because": str("For a settled or superseded status, the later comment that makes it so."),
		},
	}
	change := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             list("item", "status", "because"),
		"properties": map[string]any{
			"item":    map[string]any{"type": "string", "description": "The ledger item's ID, such as l3.", "pattern": "^l[1-9][0-9]*$"},
			"status":  enum("The item's new status.", Answered, RuledOut, Done, Dropped, Superseded, Withdrawn),
			"because": str("The later comment that makes the change, or for withdrawn the edited comment."),
			"note":    map[string]any{"type": "string", "description": "Why, in one line, where the comment does not plainly say.", "maxLength": 300},
		},
	}
	nothing := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             list("comment", "why"),
		"properties": map[string]any{
			"comment": str(comment),
			"why":     enum("Why the comment holds nothing to record.", NoneCourtesy, NoneAutomated, NoneRepeat, NoneNothingNew),
		},
	}
	return model.Tool{
		Name:        ToolName,
		Description: "Record the ledger update for the comments given: new items, changes to items the ledger holds, and the comments with nothing to record. Call it once, with the whole update.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             list("items", "updates"),
			"properties": map[string]any{
				"items":   map[string]any{"type": "array", "description": "What the new comments ask, commit to, report or provide.", "items": item},
				"updates": map[string]any{"type": "array", "description": "Changes to items the ledger already holds.", "items": change},
				"none":    map[string]any{"type": "array", "description": "The comments given that hold nothing to record.", "items": nothing},
				"notes":   map[string]any{"type": "array", "description": "Anything the update could not say, briefly.", "items": map[string]any{"type": "string", "maxLength": 500}},
			},
		},
	}
}

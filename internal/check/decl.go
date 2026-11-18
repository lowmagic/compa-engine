package check

import "github.com/lowmagic/compa-engine/internal/model"

// ToolName is the one tool the editor's and the reviewer's model is given,
// and must call with its findings as the arguments.
const ToolName = "check_findings"

// Decl declares check_findings: the findings' schema written in the
// keywords both providers accepted live, so without the references, array
// bounds and lengths the public schema adds, which Validate checks.
func Decl() model.Tool {
	list := func(vals ...string) []any {
		out := make([]any, len(vals))
		for i, v := range vals {
			out[i] = v
		}
		return out
	}
	text := func(desc string, most int) map[string]any {
		return map[string]any{"type": "string", "description": desc, "maxLength": most}
	}
	statement := map[string]any{"type": "string", "description": "A statement by the ID the request gave it, such as s3.", "pattern": "^s[1-9][0-9]*$"}
	finding := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             list("statement", "kind", "quote", "why"),
		"properties": map[string]any{
			"statement": statement,
			"kind": map[string]any{"type": "string", "enum": list(Unsupported, Overstated, Misattributed, ListingAsState, UnestablishedLink, QualifierDropped, CitationMismatch, Undetermined, Certainty),
				"description": "unsupported: nothing given holds it; overstated: the lines hold a weaker statement; misattributed: attributed or inferred stated as observed, or the wrong source; listing_as_state: a file listing cited for what runs, ran or is mounted; unestablished_link: a causal or temporal link the lines do not make; qualifier_dropped: an earlier statement's qualifier gone (the editor only); citation_mismatch: the cited lines exist and do not say it, and where other quoted lines do the editor corrects the citation to them; undetermined: the lines given cannot settle it; certainty: certainty or causal wording in the writer's own voice beyond what the lines and their attribution justify."},
			"quote": text("The words at fault, exactly as the statement has them.", 2000),
			"why":   text("Why, in one line.", 600),
			"evidence": map[string]any{"type": "array", "description": "The lines it rests on, among those the request quoted: e12, e12:41 or e12:41-58.",
				"items": map[string]any{"type": "string", "pattern": "^e[1-9][0-9]*(:[1-9][0-9]*(-[1-9][0-9]*)?)?$"}},
			"suggested":     text("The maximum justified wording: a phrase on the wording ladder, an attributed phrase, or unresolved; on an anchor, what fulfils it.", 1000),
			"alternative":   text("The reviewer's only: the strongest material alternative still consistent with the record, or none where none is meaningful.", 1000),
			"discriminator": text("The reviewer's only: the evidence that would distinguish the claim from the alternative, and whether it is given.", 1000),
			"anchor":        map[string]any{"type": "boolean", "description": "The editor's only: true where the finding needs the conversation — who said it, or lines the request did not quote — and is left to the main model's revision; say in suggested what fulfils it."},
		},
	}
	skipped := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             list("statement", "why"),
		"properties": map[string]any{
			"statement": statement,
			"why":       map[string]any{"type": "string", "description": "An ask of the customer, a step to take, or a question.", "enum": list(SkipRequest, SkipProcedure, SkipQuestion)},
		},
	}
	return model.Tool{
		Name:        ToolName,
		Description: "Record the check of the statements given: each one with a finding, supported as worded, or skipped with nothing to check. Call it once, with every statement accounted for.",
		Parameters: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             list("findings", "supported"),
			"properties": map[string]any{
				"findings":  map[string]any{"type": "array", "description": "What is wrong with which statements.", "items": finding},
				"supported": map[string]any{"type": "array", "description": "The statements supported as worded by the lines given.", "items": statement},
				"skipped":   map[string]any{"type": "array", "description": "The statements with nothing to check.", "items": skipped},
				"corrected": text("The editor's corrected text of the whole answer, where it changed wording or a citation for a finding that is not an anchor, its statements as many and in the same order; the reviewer writes none.", 32000),
				"notes":     map[string]any{"type": "array", "description": "Anything the check could not say, briefly.", "items": text("A note.", 1000)},
			},
		},
	}
}

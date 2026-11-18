package check_test

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/api"
	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// vc is a synthetic request: three statements of an invented answer, e7
// a find listing quoted at lines 2 to 4, e9 a log search quoted at 1 to 3.
func vc(role check.Role) check.Context {
	return check.Context{
		Role: role,
		Statements: []check.Statement{
			{ID: "s1", Text: "The MONs ran on worker-1 [e7:2-4]."},
			{ID: "s2", Text: "The CSI socket refused   connections [e9:1]."},
			{ID: "s3", Text: "Send the kubelet log."},
			{ID: "s4", Text: "The OSD stopped at 08:16 [e9:3]."},
		},
		Quoted:   map[string][]check.Lines{"e7": {{From: 2, To: 4}}, "e9": {{From: 1, To: 3}}},
		Listings: map[string]bool{"e7": true},
	}
}

func args(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestValidate: findings are each checked against the request — a
// statement given, a quote it holds, lines it quoted, a kind the check may
// report, a listing for listing_as_state — and what fails is dropped with
// why; every statement given is accounted for or said unaccounted.
func TestValidate(t *testing.T) {
	f, dropped, err := check.Validate(vc(check.Reviewer), args(t, `{
		"findings": [
			{"statement": "s1", "kind": "listing_as_state", "quote": "The MONs ran on worker-1", "why": "a listing of log files", "evidence": ["e7:2-4"], "suggested": "worker-1 holds MON logs."},
			{"statement": "s2", "kind": "overstated", "quote": "refused connections", "why": "whitespace differs only", "evidence": ["e9"]},
			{"statement": "s9", "kind": "unsupported", "quote": "x", "why": "no such statement"},
			{"statement": "s1", "kind": "overstated", "quote": "the MONs crashed", "why": "not in s1"},
			{"statement": "s1", "kind": "unsupported", "quote": "MONs", "why": "lines outside", "evidence": ["e7:1-3"]},
			{"statement": "s1", "kind": "unsupported", "quote": "MONs", "why": "item not quoted", "evidence": ["e5"]},
			{"statement": "s2", "kind": "listing_as_state", "quote": "socket", "why": "cites no listing", "evidence": ["e9:1"]},
			{"statement": "s2", "kind": "qualifier_dropped", "quote": "refused", "why": "the reviewer has no earlier statements"}
		],
		"supported": ["s1", "s7"],
		"skipped": [{"statement": "s3", "why": "request"}, {"statement": "s2", "why": "question"}],
		"corrected": "A reviewer's rewrite."
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, fi := range f.Findings {
		got = append(got, fi.Statement+" "+fi.Kind)
	}
	if d := cmp.Diff([]string{"s1 listing_as_state", "s2 overstated"}, got); d != "" {
		t.Errorf("findings kept (-want +got):\n%s", d)
	}
	if len(f.Supported) != 0 || len(f.Skipped) != 1 || f.Skipped[0].Statement != "s3" || f.Corrected != "" {
		t.Errorf("supported %v, skipped %v, corrected %q", f.Supported, f.Skipped, f.Corrected)
	}
	if d := cmp.Diff([]string{"s4"}, f.Unaccounted); d != "" {
		t.Errorf("unaccounted (-want +got):\n%s", d)
	}
	var why []string
	for _, r := range dropped {
		why = append(why, r.String())
	}
	want := []string{
		"findings[2]: s9 is no statement the request gave",
		`findings[3]: "the MONs crashed" is not in s1`,
		"findings[4]: e7:1-3 is not among the lines given",
		"findings[5]: e5 is not among the lines given",
		"findings[6]: a listing_as_state finding cites no listing given",
		"findings[7]: only the editor, given the earlier statements, reports a dropped qualifier",
		"supported[0]: s1 is accounted for already",
		"supported[1]: s7 is no statement the request gave",
		"skipped[1]: s2 is accounted for already",
		"corrected: the reviewer writes no corrected text",
	}
	if d := cmp.Diff(want, why); d != "" {
		t.Errorf("dropped (-want +got):\n%s", d)
	}

	// The editor reports a dropped qualifier and writes the corrected text.
	f, dropped, err = check.Validate(vc(check.Editor), args(t, `{
		"findings": [{"statement": "s2", "kind": "qualifier_dropped", "quote": "refused connections", "why": "an earlier answer said likely"}],
		"supported": ["s1", "s4"], "skipped": [{"statement": "s3", "why": "request"}],
		"corrected": "The CSI socket likely refused connections [e9:1]."
	}`))
	if err != nil || len(dropped) != 0 || len(f.Findings) != 1 || f.Corrected == "" || len(f.Unaccounted) != 0 {
		t.Errorf("the editor's findings: %+v, dropped %v, %v", f, dropped, err)
	}
}

// TestValidateDuplicatesAndEmptyQuotes: a statement listed supported twice
// counts once, the second dropped with why, and a quote of whitespace
// alone, which every statement would hold, is dropped as empty.
func TestValidateDuplicatesAndEmptyQuotes(t *testing.T) {
	f, dropped, err := check.Validate(vc(check.Editor), args(t, `{
		"findings": [{"statement": "s2", "kind": "overstated", "quote": "   ", "why": "an empty quote"}],
		"supported": ["s1", "s1", "s4"],
		"skipped": [{"statement": "s3", "why": "request"}, {"statement": "s3", "why": "procedure"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]string{"s1", "s4"}, f.Supported); d != "" {
		t.Errorf("supported (-want +got):\n%s", d)
	}
	var why []string
	for _, r := range dropped {
		why = append(why, r.String())
	}
	want := []string{
		"findings[0]: the quote is empty",
		"supported[1]: s1 is accounted for already",
		"skipped[1]: s3 is accounted for already",
	}
	if d := cmp.Diff(want, why); d != "" {
		t.Errorf("dropped (-want +got):\n%s", d)
	}
	if d := cmp.Diff([]string{"s2"}, f.Unaccounted); d != "" {
		t.Errorf("unaccounted (-want +got):\n%s", d)
	}
}

// TestValidateRoles: each role sends only its own fields — the editor an
// anchor and corrected text, the reviewer an alternative and a
// discriminator — the other's dropped with why and the finding kept; an
// anchor saying nothing of what fulfils it is dropped; and corrected text
// no finding but an anchor calls for is dropped.
func TestValidateRoles(t *testing.T) {
	const both = `{
		"findings": [
			{"statement": "s1", "kind": "certainty", "quote": "ran on worker-1", "why": "a listing", "evidence": ["e7:2-4"], "suggested": "worker-1 holds MON log files", "alternative": "the MONs ran elsewhere", "discriminator": "the pods' placement, not given"},
			{"statement": "s2", "kind": "unsupported", "quote": "refused", "why": "who saw it is not given", "suggested": "name who saw it", "anchor": true},
			{"statement": "s4", "kind": "unsupported", "quote": "stopped at 08:16", "why": "the rule is not given", "anchor": true}
		],
		"supported": [], "skipped": [{"statement": "s3", "why": "request"}],
		"corrected": "worker-1 holds MON log files [e7:2-4]."
	}`
	reasons := func(dropped []check.Rejected) []string {
		var out []string
		for _, r := range dropped {
			out = append(out, r.String())
		}
		return out
	}
	f, dropped, err := check.Validate(vc(check.Editor), args(t, both))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"findings[0].alternative: only the reviewer names an alternative",
		"findings[0].discriminator: only the reviewer names a discriminator",
		"findings[2]: an anchor says in suggested what fulfils it",
	}
	if d := cmp.Diff(want, reasons(dropped)); d != "" {
		t.Errorf("the editor's dropped (-want +got):\n%s", d)
	}
	if len(f.Findings) != 2 || f.Findings[0].Alternative != "" || f.Findings[0].Discriminator != "" || !f.Findings[1].Anchor || f.Corrected == "" {
		t.Errorf("the editor's findings: %+v", f)
	}
	if d := cmp.Diff([]string{"s4"}, f.Unaccounted); d != "" {
		t.Errorf("unaccounted (-want +got):\n%s", d)
	}

	f, dropped, err = check.Validate(vc(check.Reviewer), args(t, both))
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"findings[1].anchor: only the editor, given no conversation, leaves an anchor",
		"findings[2].anchor: only the editor, given no conversation, leaves an anchor",
		"corrected: the reviewer writes no corrected text",
	}
	if d := cmp.Diff(want, reasons(dropped)); d != "" {
		t.Errorf("the reviewer's dropped (-want +got):\n%s", d)
	}
	if len(f.Findings) != 3 || f.Findings[0].Alternative == "" || f.Findings[0].Discriminator == "" || f.Findings[1].Anchor || f.Corrected != "" {
		t.Errorf("the reviewer's findings: %+v", f)
	}

	f, dropped, err = check.Validate(vc(check.Editor), args(t, `{
		"findings": [{"statement": "s2", "kind": "unsupported", "quote": "refused", "why": "who saw it is not given", "suggested": "name who saw it", "anchor": true}],
		"supported": ["s1", "s4"], "skipped": [{"statement": "s3", "why": "request"}],
		"corrected": "A rewrite no finding asked for."
	}`))
	if err != nil || f.Corrected != "" || !slices.Equal(reasons(dropped), []string{"corrected: no finding kept that is not an anchor calls for a correction"}) {
		t.Errorf("corrected text with only an anchor: %+v, dropped %v, %v", f, dropped, err)
	}
}

// TestValidateRefusesWhole: findings the schema refuses are refused whole,
// saying why, so the model can call again.
func TestValidateRefusesWhole(t *testing.T) {
	for _, s := range []string{
		`{"findings": []}`,
		`{"findings": [{"statement": "s1", "kind": "wrong", "quote": "x", "why": "y"}], "supported": []}`,
		`{"findings": [], "supported": ["S1"]}`,
		`{"findings": [], "supported": [], "verdict": "fine"}`,
		`{"findings": [{"statement": "s1", "kind": "unsupported", "quote": "x", "why": "y", "evidence": ["e7:"]}], "supported": []}`,
	} {
		if _, _, err := check.Validate(vc(check.Editor), args(t, s)); err == nil {
			t.Errorf("%s was accepted", s)
		}
	}
}

// TestValidateTakesMeasuredLengths: a why and a note as long as the
// longest the live benchmark saw a model write, 405 and 727
// characters, which a 300 and a 500 bound refused whole, are taken; one
// past the bound still refuses the call.
func TestValidateTakesMeasuredLengths(t *testing.T) {
	call := func(why, note string) map[string]any {
		return map[string]any{
			"findings":  []any{map[string]any{"statement": "s4", "kind": "overstated", "quote": "stopped", "why": why, "evidence": []any{"e9:3"}}},
			"supported": []any{"s1", "s2"},
			"notes":     []any{note},
		}
	}
	f, _, err := check.Validate(vc(check.Reviewer), call(strings.Repeat("w", 405), strings.Repeat("n", 727)))
	if err != nil || len(f.Findings) != 1 || len(f.Notes) != 1 {
		t.Errorf("the measured lengths: %+v, %v", f, err)
	}
	if _, _, err := check.Validate(vc(check.Reviewer), call(strings.Repeat("w", 601), "n")); err == nil {
		t.Error("a why past its bound was taken")
	}
}

// TestValidateTakesLongFindings: a model's findings run long. A quote or
// a wording past a 300 bound refused whole calls that were otherwise
// valid, the longest 437 and 381 characters. A 430-character quote of a
// long statement and 381-character wordings are taken; one past the
// bound still refuses the call.
func TestValidateTakesLongFindings(t *testing.T) {
	long := "The OSD stopped at 08:16 " + strings.Repeat("while the cluster rebalanced ", 14) + "[e9:3]."
	c := vc(check.Reviewer)
	c.Statements = append(c.Statements, check.Statement{ID: "s5", Text: long})
	call := func(quote, wording string) map[string]any {
		return map[string]any{
			"findings": []any{map[string]any{"statement": "s5", "kind": "overstated", "quote": quote, "why": "the lines hold less",
				"evidence": []any{"e9:3"}, "suggested": wording, "alternative": wording, "discriminator": wording}},
			"supported": []any{"s1", "s2", "s4"},
		}
	}
	quote := strings.TrimSuffix(long, " [e9:3].")
	f, _, err := check.Validate(c, call(quote, strings.Repeat("w", 381)))
	if err != nil || len(f.Findings) != 1 {
		t.Errorf("a %d-character quote and 381-character wordings: %+v, %v", len(quote), f, err)
	}
	if _, _, err := check.Validate(c, call(quote, strings.Repeat("w", 1001))); err == nil {
		t.Error("a wording past its bound was taken")
	}
}

// TestValidateTakesElevenEvidenceLines: a finding resting on 11 evidence
// lines, past a bound of 10 the declaration cannot carry to the model,
// refused its whole call. Eleven are taken; one past the bound still
// refuses the call.
func TestValidateTakesElevenEvidenceLines(t *testing.T) {
	call := func(evidence []any) map[string]any {
		return map[string]any{
			"findings":  []any{map[string]any{"statement": "s4", "kind": "overstated", "quote": "stopped", "why": "the lines hold less", "evidence": evidence}},
			"supported": []any{"s1", "s2"},
		}
	}
	eleven := []any{"e9:1", "e9:2", "e9:3", "e9:1-2", "e9:2-3", "e9:1-3", "e9", "e7:2", "e7:3", "e7:4", "e7:2-4"}
	if f, _, err := check.Validate(vc(check.Reviewer), call(eleven)); err != nil || len(f.Findings) != 1 {
		t.Errorf("a finding on 11 evidence lines: %+v, %v", f, err)
	}
	many := make([]any, 41)
	for i := range many {
		many[i] = "e9:3"
	}
	if _, _, err := check.Validate(vc(check.Reviewer), call(many)); err == nil {
		t.Error("evidence past its bound was taken")
	}
}

// TestValidateListingOnlyStatement: listing_as_state is taken on a
// statement whose every citation is a file listing, the request says,
// though the finding names no evidence or names its citation only in the
// quote; on a statement citing what is no listing it is dropped still.
func TestValidateListingOnlyStatement(t *testing.T) {
	c := vc(check.Editor)
	c.ListingOnly = map[string]bool{"s1": true}
	f, dropped, err := check.Validate(c, args(t, `{
		"findings": [
			{"statement": "s1", "kind": "listing_as_state", "quote": "The MONs ran on worker-1 [e7:2-4].", "why": "a listing of log files"},
			{"statement": "s2", "kind": "listing_as_state", "quote": "socket", "why": "not a listing", "evidence": ["e9:1"]},
			{"statement": "s4", "kind": "listing_as_state", "quote": "stopped", "why": "nothing cited"}
		],
		"supported": []
	}`))
	var got []string
	for _, d := range dropped {
		got = append(got, d.String())
	}
	if err != nil || len(f.Findings) != 1 || f.Findings[0].Statement != "s1" || !slices.Equal(got, []string{
		"findings[1]: a listing_as_state finding cites no listing given",
		"findings[2]: a listing_as_state finding cites no listing given",
	}) {
		t.Errorf("findings %+v, dropped %v, %v", f.Findings, got, err)
	}
}

// TestValidateNoChangeIsSupported: a finding whose suggestion keeps the
// statement as it is, in the forms a reviewer was seen filing live, counts
// its statement as supported and is dropped saying so; a suggestion that
// only repeats the quote stays a finding, as a citation_mismatch may keep
// the wording and dispute the citation.
func TestValidateNoChangeIsSupported(t *testing.T) {
	f, dropped, err := check.Validate(vc(check.Reviewer), args(t, `{
		"findings": [
			{"statement": "s1", "kind": "overstated", "quote": "ran on worker-1", "why": "the lines support it", "evidence": ["e7:2-4"], "suggested": "Supported as worded.", "alternative": "none", "discriminator": "none"},
			{"statement": "s2", "kind": "citation_mismatch", "quote": "refused", "why": "the line supports it", "evidence": ["e9:1"], "suggested": "supported", "alternative": "none", "discriminator": "none"},
			{"statement": "s4", "kind": "citation_mismatch", "quote": "stopped at 08:16", "why": "e9:3 holds 08:61", "evidence": ["e9:3"], "suggested": "stopped at 08:16", "alternative": "a typo", "discriminator": "the log line"}
		],
		"supported": [],
		"skipped": [{"statement": "s3", "why": "request"}]
	}`))
	var got []string
	for _, d := range dropped {
		got = append(got, d.String())
	}
	if err != nil || len(f.Findings) != 1 || f.Findings[0].Statement != "s4" || !slices.Equal(f.Supported, []string{"s1", "s2"}) || len(f.Unaccounted) != 0 || !slices.Equal(got, []string{
		`findings[0]: "Supported as worded." asks for no change: s1 is counted as supported`,
		`findings[1]: "supported" asks for no change: s2 is counted as supported`,
	}) {
		t.Errorf("findings %+v, supported %v, unaccounted %v, dropped %v, %v", f.Findings, f.Supported, f.Unaccounted, got, err)
	}
}

// TestDeclMatchesSchema: check_findings' parameters use only the keywords
// both providers accepted live, and offer the same kinds and reasons as
// the public schema.
func TestDeclMatchesSchema(t *testing.T) {
	accepted := map[string]bool{}
	for _, k := range vertexgemini.Keywords {
		if slices.Contains(vertexanthropic.Keywords, k) {
			accepted[k] = true
		}
	}
	d := check.Decl()
	if d.Name != check.ToolName {
		t.Errorf("declared as %q", d.Name)
	}
	for _, k := range model.Keywords(d.Parameters) {
		if !accepted[k] {
			t.Errorf("check_findings uses %q, which no live call accepted on both providers", k)
		}
	}
	raw, err := fs.ReadFile(api.Schemas, "check/v1alpha1/findings.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var sch map[string]any
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff(enums(sch), enums(d.Parameters)); d != "" {
		t.Errorf("the declaration's enums differ from the schema's (-schema +declaration):\n%s", d)
	}
	if d := cmp.Diff(patterns(sch), patterns(d.Parameters)); d != "" {
		t.Errorf("the declaration's patterns differ from the schema's (-schema +declaration):\n%s", d)
	}
	if d := cmp.Diff(required(sch), required(d.Parameters)); d != "" {
		t.Errorf("the declaration's required lists differ from the schema's (-schema +declaration):\n%s", d)
	}
	if d := cmp.Diff(maxLengths(sch), maxLengths(d.Parameters)); d != "" {
		t.Errorf("the declaration's maxLengths differ from the schema's (-schema +declaration):\n%s", d)
	}
}

// maxLengths are a schema's string bounds, by the property each bounds.
func maxLengths(s map[string]any) map[string]float64 {
	out := map[string]float64{}
	walk(s, "", func(name string, m map[string]any) {
		switch n := m["maxLength"].(type) {
		case float64:
			out[name] = n
		case int:
			out[name] = float64(n)
		}
	})
	return out
}

// walk visits every object of a schema with the name of the property it
// is, the last property name on its path.
func walk(v any, name string, visit func(name string, m map[string]any)) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	visit(name, m)
	for k, sub := range m {
		next := name
		if k != "properties" && k != "$defs" && k != "items" {
			next = k
		}
		walk(sub, next, visit)
	}
}

func enums(s map[string]any) map[string][]any {
	out := map[string][]any{}
	walk(s, "", func(name string, m map[string]any) {
		if e, ok := m["enum"].([]any); ok {
			out[name] = e
		}
	})
	return out
}

func patterns(s map[string]any) []string {
	var out []string
	walk(s, "", func(_ string, m map[string]any) {
		if p, ok := m["pattern"].(string); ok && !slices.Contains(out, p) {
			out = append(out, p)
		}
	})
	slices.Sort(out)
	return out
}

func required(s map[string]any) []string {
	var out []string
	walk(s, "", func(_ string, m map[string]any) {
		if r, ok := m["required"].([]any); ok {
			var names []string
			for _, n := range r {
				if name, ok := n.(string); ok {
					names = append(names, name)
				}
			}
			out = append(out, strings.Join(names, ","))
		}
	})
	slices.Sort(out)
	return out
}

// TestEditorPromptCarriesTheRules: the editor's prompt holds the customer
// drafts' rules, the certainty review's, from their one file,
// whole and not restated, beside its own task.
func TestEditorPromptCarriesTheRules(t *testing.T) {
	p := check.EditorPrompt()
	if !strings.Contains(p, strings.TrimSpace(companion.CustomerDraft)) || strings.Contains(p, "{customer.md}") {
		t.Errorf("the editor's prompt does not carry customer.md whole:\n%s", p)
	}
	for _, want := range []string{"`anchor` true", "`corrected`", check.ToolName} {
		if !strings.Contains(p, want) {
			t.Errorf("the editor's prompt lacks %q", want)
		}
	}
}

// TestReviewerPromptCarriesTheRules: [E3] the reviewer's prompt holds the
// customer drafts' rules from their one file, whole and once, beside the
// review's form: each finding's claim, support, alternative,
// discriminator and maximum justified wording.
func TestReviewerPromptCarriesTheRules(t *testing.T) {
	p := check.ReviewerPrompt()
	rules := strings.TrimSpace(companion.CustomerDraft)
	if strings.Count(p, rules) != 1 || strings.Contains(p, "{customer.md}") || p == check.EditorPrompt() {
		t.Errorf("the reviewer's prompt does not carry customer.md whole, once:\n%s", p)
	}
	for _, want := range []string{"`quote`", "`why`", "`alternative`", "`discriminator`", "`suggested`", "four questions", check.ToolName} {
		if !strings.Contains(p, want) {
			t.Errorf("the reviewer's prompt lacks %q", want)
		}
	}
}

// testCite reads the citations the tests below write as the engine reads
// them: [e7:2-4], [e7:2], [e7], and an artifact path with a line.
var testCite = regexp.MustCompile(`\[(e\d+)(?::(\d+)(?:-(\d+))?)?\]|` + "`" + `([\w./-]+/[\w.-]+):(\d+)` + "`")

func testCites(text string) []check.Cite {
	var out []check.Cite
	for _, m := range testCite.FindAllStringSubmatch(text, -1) {
		c := check.Cite{Name: m[1], Path: m[4]}
		c.From, _ = strconv.Atoi(m[2] + m[5])
		c.To, _ = strconv.Atoi(m[3])
		out = append(out, c)
	}
	return out
}

// editorContext is an editor's request about answer: e7 quoted whole, its
// 6 lines, and e9, of 20 lines, quoted at 1 to 3 and 10 to 14.
func editorContext(answer string) check.Context {
	return check.Context{
		Role:       check.Editor,
		Statements: check.Split(answer),
		Quoted:     map[string][]check.Lines{"e7": {{From: 1, To: 6}}, "e9": {{From: 1, To: 3}, {From: 10, To: 14}}},
		Items:      map[string]int{"e7": 6, "e9": 20},
		Answer:     answer,
		Cites:      testCites,
	}
}

// TestValidateCorrectedCitations: a citation the editor's corrected text
// gives a statement is kept where it names lines the request quoted; one
// naming lines it did not quote, lines past the item's end, an item not
// quoted or an artifact path is refused, its finding dropped with why and
// its statement put back as the answer has it, line breaks and all, the
// others' corrections kept. A citation the answer had, quoted or not, is
// never refused. Statements that do not pair, adding a citation refused,
// drop the corrected text whole.
func TestValidateCorrectedCitations(t *testing.T) {
	answer := "The MONs ran on worker-1 [e7:2]. The CSI socket refused connections [e9:1]. The OSD stopped at 08:16 [e9:3]. The pod was evicted [e9:12]. " +
		"Kubelet restarted [e5:1]. Its log is `nodes/worker-1/kubelet.log:3`."
	corrected := "The MONs ran on worker-1 [e7:4]. The CSI socket refused connections [e9:7]. The OSD stopped at 08:16 [e9:25]. The pod was evicted [e3:2]. " +
		"Kubelet restarted [e5:1]. Its log is `nodes/worker-1/kubelet.log:4`."
	fix := func(s, quote, to string) map[string]any {
		return map[string]any{"statement": s, "kind": "citation_mismatch", "quote": quote, "why": "other lines hold it", "suggested": to}
	}
	f, dropped, err := check.Validate(editorContext(answer), map[string]any{
		"findings": []any{
			fix("s1", "[e7:2]", "[e7:4]"), fix("s2", "[e9:1]", "[e9:7]"), fix("s3", "[e9:3]", "[e9:25]"), fix("s4", "[e9:12]", "[e3:2]"),
			fix("s6", "`nodes/worker-1/kubelet.log:3`", "`nodes/worker-1/kubelet.log:4`"),
		},
		"supported": []any{"s5"},
		"corrected": corrected,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "The MONs ran on worker-1 [e7:4]. The CSI socket refused connections [e9:1]. The OSD stopped at 08:16 [e9:3]. The pod was evicted [e9:12]. " +
		"Kubelet restarted [e5:1]. Its log is `nodes/worker-1/kubelet.log:3`."
	if f.Corrected != want || len(f.Findings) != 1 || f.Findings[0].Statement != "s1" || !slices.Equal(f.Unaccounted, []string{"s2", "s3", "s4", "s6"}) {
		t.Errorf("corrected %q, findings %+v, unaccounted %v", f.Corrected, f.Findings, f.Unaccounted)
	}
	wantDropped := []check.Rejected{
		{Element: "findings[1]", Reason: "the corrected text cites [e9:7] for s2, and the request did not quote those lines: s2 is kept as written"},
		{Element: "findings[2]", Reason: "the corrected text cites [e9:25] for s3, and e9 holds 20 lines: s3 is kept as written"},
		{Element: "findings[3]", Reason: "the corrected text cites [e3:2] for s4, and e3 is no item the request quoted: s4 is kept as written"},
		{Element: "findings[4]", Reason: "the corrected text cites nodes/worker-1/kubelet.log:4 for s6, and an artifact's path is not what the request quoted: s6 is kept as written"},
	}
	if d := cmp.Diff(wantDropped, dropped); d != "" {
		t.Errorf("dropped (-want +got):\n%s", d)
	}
	if got := check.CorrectedCites(answer, f.Corrected, testCites); !slices.Equal(got, []check.Cite{{Name: "e7", From: 4}}) {
		t.Errorf("the citations corrected: %+v", got)
	}

	list := "- The MONs ran on worker-1,\n  as the listing shows [e7:2].\n- The OSD stopped at 08:16 [e9:3]."
	f, _, err = check.Validate(editorContext(list), map[string]any{
		"findings":  []any{fix("s1", "[e7:2]", "[e7:9]"), fix("s2", "[e9:3]", "[e9:2]")},
		"supported": []any{},
		"corrected": "- The MONs ran on worker-1, as the listing shows [e7:9].\n- The OSD stopped at 08:16 [e9:2].",
	})
	if want := "- The MONs ran on worker-1,\n  as the listing shows [e7:2].\n- The OSD stopped at 08:16 [e9:2]."; err != nil || f.Corrected != want {
		t.Errorf("the list corrected %q, %v; want %q", f.Corrected, err, want)
	}

	f, dropped, err = check.Validate(editorContext(answer), map[string]any{
		"findings":  []any{fix("s1", "[e7:2]", "[e7:9]")},
		"supported": []any{"s2", "s3", "s4", "s5", "s6"},
		"corrected": "The MONs ran on worker-1 [e7:9], and the CSI socket refused connections [e9:1]. The OSD stopped at 08:16 [e9:3]. The pod was evicted [e9:12]. " +
			"Kubelet restarted [e5:1]. Its log is `nodes/worker-1/kubelet.log:3`.",
	})
	if err != nil || f.Corrected != "" || len(dropped) != 1 || dropped[0].Element != "corrected" ||
		dropped[0].Reason != "it cites [e7:9], and e7 holds 6 lines; its statements do not pair with the answer's, so it is dropped" {
		t.Errorf("unpaired: corrected %q, dropped %v, %v", f.Corrected, dropped, err)
	}
}

// TestValidateCitationOnlyCorrection: a correction that only gives
// statements citations of lines the request quoted needs no finding, as
// when the editor cites the lines that hold a draft's uncited statements
// or a comment's lines in place of a case-ledger item cited; one that also
// changes a word does, and one citing lines not quoted is put back as
// written.
func TestValidateCitationOnlyCorrection(t *testing.T) {
	answer := "The MONs ran on worker-1. The CSI socket refused connections."
	f, dropped, err := check.Validate(editorContext(answer), args(t, `{
		"findings": [], "supported": ["s1", "s2"],
		"corrected": "The MONs ran on worker-1 [e7:2]. The CSI socket refused connections [e9:1]."
	}`))
	if err != nil || len(dropped) != 0 || f.Corrected != "The MONs ran on worker-1 [e7:2]. The CSI socket refused connections [e9:1]." {
		t.Errorf("citations only: corrected %q, dropped %v, %v", f.Corrected, dropped, err)
	}
	f, dropped, err = check.Validate(editorContext(answer), args(t, `{
		"findings": [], "supported": ["s1", "s2"],
		"corrected": "The MONs ran on worker-2 [e7:2]. The CSI socket refused connections [e9:1]."
	}`))
	if err != nil || f.Corrected != "" || len(dropped) != 1 {
		t.Errorf("a word changed too: corrected %q, dropped %v, %v", f.Corrected, dropped, err)
	}
	f, _, err = check.Validate(editorContext(answer), args(t, `{
		"findings": [], "supported": ["s1", "s2"],
		"corrected": "The MONs ran on worker-1 [e7:2]. The CSI socket refused connections [e9:7]."
	}`))
	if err != nil || f.Corrected != "The MONs ran on worker-1 [e7:2]. The CSI socket refused connections." {
		t.Errorf("a citation not quoted: corrected %q, %v", f.Corrected, err)
	}
	ledger := "The customer restarted the MONs [l3]. The CSI socket refused connections."
	f, dropped, err = check.Validate(editorContext(ledger), args(t, `{
		"findings": [], "supported": ["s1", "s2"],
		"corrected": "The customer restarted the MONs [e7:2]. The CSI socket refused connections."
	}`))
	if err != nil || len(dropped) != 0 || f.Corrected != "The customer restarted the MONs [e7:2]. The CSI socket refused connections." {
		t.Errorf("a ledger item's citation rewritten: corrected %q, dropped %v, %v", f.Corrected, dropped, err)
	}
}

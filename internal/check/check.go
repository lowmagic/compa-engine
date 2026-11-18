// Package check is the epistemic checks' model side and their pure text
// work: the statements a text is split into, what a statement holds that a
// check looks at, the findings the editor and the reviewer answer with,
// the declaration of their one tool, and the validation of findings
// against what the request gave. The engine quotes the
// cited lines and builds the requests; what a model writes here is its
// reading of a model's text, never evidence.
package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lowmagic/compa-engine/internal/schema"
)

// Role says which check made the findings: the editor, after every
// answer, or the reviewer, on request.
type Role string

// The two checks.
const (
	Editor   Role = "editor"
	Reviewer Role = "reviewer"
)

// Kinds of finding.
const (
	Unsupported       = "unsupported"
	Overstated        = "overstated"
	Misattributed     = "misattributed"
	ListingAsState    = "listing_as_state"
	UnestablishedLink = "unestablished_link"
	QualifierDropped  = "qualifier_dropped"
	CitationMismatch  = "citation_mismatch"
	Undetermined      = "undetermined"
	// Certainty is certainty or causal wording in the writer's own voice
	// beyond what the lines and their attribution justify.
	Certainty = "certainty"
)

// Why a statement holds nothing to check.
const (
	SkipRequest   = "request"
	SkipProcedure = "procedure"
	SkipQuestion  = "question"
)

// Findings are one check's answer: what is wrong with which statements,
// those supported as worded, and those with nothing to check.
type Findings struct {
	Findings  []Finding `json:"findings"`
	Supported []string  `json:"supported"`
	Skipped   []Skipped `json:"skipped,omitempty"`
	// Corrected is the editor's text of the whole answer, corrected.
	Corrected string   `json:"corrected,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	// Unaccounted are the statements the request gave that the findings
	// say nothing of, in their order; set by Validate.
	Unaccounted []string `json:"-"`
}

// Finding is what is wrong with one statement. Suggested is the maximum
// justified wording, or on an anchor what fulfils it; Alternative and
// Discriminator are the reviewer's only, Anchor the editor's.
type Finding struct {
	Statement     string   `json:"statement"`
	Kind          string   `json:"kind"`
	Quote         string   `json:"quote"`
	Why           string   `json:"why"`
	Evidence      []string `json:"evidence,omitempty"`
	Suggested     string   `json:"suggested,omitempty"`
	Alternative   string   `json:"alternative,omitempty"`
	Discriminator string   `json:"discriminator,omitempty"`
	// Anchor says the finding needs the conversation, which the editor is
	// not given, and is left to the main model's revision.
	Anchor bool `json:"anchor,omitempty"`
}

// Skipped is a statement with nothing to check, and why.
type Skipped struct {
	Statement string `json:"statement"`
	Why       string `json:"why"`
}

// Lines are a range of an evidence item's lines, first and last, from 1.
type Lines struct{ From, To int }

// Context is what findings are validated against: the request's
// statements and the lines it quoted.
type Context struct {
	Role       Role
	Statements []Statement
	// Quoted are the lines the request quoted, by evidence name.
	Quoted map[string][]Lines
	// Items are how many lines each item the request quoted holds, by
	// evidence name.
	Items map[string]int
	// Listings are the names of the quoted items that are file listings.
	Listings map[string]bool
	// ListingOnly are the statements, by ID, whose every citation names
	// a file listing: listing_as_state rests on those citations whether
	// or not the finding names them again.
	ListingOnly map[string]bool
	// Answer is the editor's text as written, whose statements
	// Statements are; Cites reads a text's citations as the engine reads
	// them. With both, the citations the corrected text changes are
	// checked (correctedCites).
	Answer string
	Cites  func(text string) []Cite
}

// Cite is a citation as the engine reads it: an evidence item by name,
// or an artifact by path, with the lines it names, From 0 for a whole
// item and To 0 for one line.
type Cite struct {
	Name, Path string
	From, To   int
}

// Place is the citation as one place: [e12:41-58], [e12:41], [e12], or
// an artifact's path:41.
func (c Cite) Place() string {
	s := c.Name + c.Path
	switch {
	case c.From > 0 && c.To > 0:
		s += ":" + strconv.Itoa(c.From) + "-" + strconv.Itoa(c.To)
	case c.From > 0:
		s += ":" + strconv.Itoa(c.From)
	}
	if c.Path != "" {
		return s
	}
	return "[" + s + "]"
}

// same says whether two citations name the same place.
func (c Cite) same(o Cite) bool {
	return c.Name == o.Name && c.Path == o.Path && c.From == o.From && max(c.To, c.From) == max(o.To, o.From)
}

// Rejected is an element of the findings validation took out, and why.
type Rejected struct {
	Element, Reason string
}

func (r Rejected) String() string { return r.Element + ": " + r.Reason }

// Validate checks a check_findings call's arguments: against the schema,
// whole, or the findings are refused with why; then each element against
// the request. A finding names a statement given, quotes words that
// statement has, rests on lines the request quoted, and is of a kind its
// check may report; listing_as_state cites a listing given, or is of a
// statement citing only listings (ListingOnly). Only the
// editor leaves an anchor, which says in suggested what fulfils it, and
// writes corrected text, which a finding that is not an anchor must call
// for; a citation it corrects is checked (correctedCites). Only the
// reviewer names an alternative and a discriminator. An element failing
// is dropped with its reason, the rest kept; the statements no element
// accounts for come back as Unaccounted.
func Validate(vc Context, args map[string]any) (Findings, []Rejected, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return Findings{}, nil, fmt.Errorf("the arguments: %w", err)
	}
	if err := schema.ValidateJSON(schema.CheckFindings, raw); err != nil {
		return Findings{}, nil, err
	}
	var f Findings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Findings{}, nil, fmt.Errorf("the arguments: %w", err)
	}
	var dropped []Rejected
	drop := func(el, format string, a ...any) {
		dropped = append(dropped, Rejected{Element: el, Reason: fmt.Sprintf(format, a...)})
	}
	texts := make(map[string]string, len(vc.Statements))
	for _, s := range vc.Statements {
		texts[s.ID] = s.Text
	}
	accounted := map[string]bool{}
	var kept []Finding
	// keptAt are the kept findings' indices in the call, for an element's
	// name.
	var keptAt []int
	var noChange []string
	for i, fi := range f.Findings {
		el := fmt.Sprintf("findings[%d]", i)
		text, ok := texts[fi.Statement]
		if !ok {
			drop(el, "%s is no statement the request gave", fi.Statement)
			continue
		}
		quote := oneLine(fi.Quote)
		if quote == "" {
			drop(el, "the quote is empty")
			continue
		}
		if !strings.Contains(oneLine(text), quote) {
			drop(el, "%q is not in %s", fi.Quote, fi.Statement)
			continue
		}
		if fi.Kind == QualifierDropped && vc.Role != Editor {
			drop(el, "only the editor, given the earlier statements, reports a dropped qualifier")
			continue
		}
		if bad := ungiven(vc.Quoted, fi.Evidence); bad != "" {
			drop(el, "%s is not among the lines given", bad)
			continue
		}
		if fi.Kind == ListingAsState && !citesListing(vc.Listings, fi.Evidence) && !vc.ListingOnly[fi.Statement] {
			drop(el, "a listing_as_state finding cites no listing given")
			continue
		}
		if fi.Anchor && vc.Role != Editor {
			drop(el+".anchor", "only the editor, given no conversation, leaves an anchor")
			fi.Anchor = false
		}
		if fi.Alternative != "" && vc.Role != Reviewer {
			drop(el+".alternative", "only the reviewer names an alternative")
			fi.Alternative = ""
		}
		if fi.Discriminator != "" && vc.Role != Reviewer {
			drop(el+".discriminator", "only the reviewer names a discriminator")
			fi.Discriminator = ""
		}
		if fi.Anchor && oneLine(fi.Suggested) == "" {
			drop(el, "an anchor says in suggested what fulfils it")
			continue
		}
		if !fi.Anchor && asksNoChange(fi.Suggested) {
			drop(el, "%q asks for no change: %s is counted as supported", fi.Suggested, fi.Statement)
			noChange = append(noChange, fi.Statement)
			continue
		}
		accounted[fi.Statement] = true
		kept = append(kept, fi)
		keptAt = append(keptAt, i)
	}
	var supported []string
	for i, id := range f.Supported {
		el := fmt.Sprintf("supported[%d]", i)
		switch _, ok := texts[id]; {
		case !ok:
			drop(el, "%s is no statement the request gave", id)
		case accounted[id]:
			drop(el, "%s is accounted for already", id)
		default:
			accounted[id] = true
			supported = append(supported, id)
		}
	}
	var skipped []Skipped
	for i, sk := range f.Skipped {
		el := fmt.Sprintf("skipped[%d]", i)
		switch _, ok := texts[sk.Statement]; {
		case !ok:
			drop(el, "%s is no statement the request gave", sk.Statement)
		case accounted[sk.Statement]:
			drop(el, "%s is accounted for already", sk.Statement)
		default:
			accounted[sk.Statement] = true
			skipped = append(skipped, sk)
		}
	}
	// A statement whose only findings asked for no change is supported,
	// unless the call accounted for it otherwise.
	for _, id := range noChange {
		if !accounted[id] {
			accounted[id] = true
			supported = append(supported, id)
		}
	}
	if f.Corrected != "" && vc.Role == Editor && vc.Cites != nil {
		var removed map[int]bool
		f.Corrected, removed = correctedCites(vc, f.Corrected, kept, keptAt, drop)
		if len(removed) > 0 {
			var rest []Finding
			for k, fi := range kept {
				if !removed[k] {
					rest = append(rest, fi)
				}
			}
			for k := range removed {
				id := kept[k].Statement
				if !slices.ContainsFunc(rest, func(fi Finding) bool { return fi.Statement == id }) {
					accounted[id] = false
				}
			}
			kept = rest
		}
	}
	switch {
	case f.Corrected != "" && vc.Role != Editor:
		drop("corrected", "the reviewer writes no corrected text")
		f.Corrected = ""
	case f.Corrected != "" && !slices.ContainsFunc(kept, func(fi Finding) bool { return !fi.Anchor }) && (vc.Cites == nil || !citesOnly(vc.Answer, f.Corrected)):
		drop("corrected", "no finding kept that is not an anchor calls for a correction")
		f.Corrected = ""
	}
	f.Findings, f.Supported, f.Skipped = kept, supported, skipped
	for _, s := range vc.Statements {
		if !accounted[s.ID] {
			f.Unaccounted = append(f.Unaccounted, s.ID)
		}
	}
	return f, dropped, nil
}

// asksNoChange says a finding's suggested wording keeps the statement as it
// is: "supported", "supported as worded" or "supported as written", which a
// reviewer has filed as a finding though its rule says to list it in
// supported.
func asksNoChange(suggested string) bool {
	switch strings.ToLower(strings.TrimRight(oneLine(suggested), ". ")) {
	case "supported", "supported as worded", "supported as written":
		return true
	}
	return false
}

// ungiven is the first of refs naming lines the request did not quote, or
// nothing: a bare name needs the item quoted at all, a range lines quoted
// within one range given.
func ungiven(quoted map[string][]Lines, refs []string) string {
	for _, ref := range refs {
		name, rng, hasRange := strings.Cut(ref, ":")
		given := quoted[name]
		if len(given) == 0 {
			return ref
		}
		if !hasRange {
			continue
		}
		fromS, toS, _ := strings.Cut(rng, "-")
		from, _ := strconv.Atoi(fromS)
		to := from
		if toS != "" {
			to, _ = strconv.Atoi(toS)
		}
		if !within(given, from, to) {
			return ref
		}
	}
	return ""
}

// within says whether lines from to to lie within one range of given.
func within(given []Lines, from, to int) bool {
	for _, l := range given {
		if from >= l.From && to <= l.To && from <= to {
			return true
		}
	}
	return false
}

// correctedCites checks the citations the editor's corrected text adds:
// each must name an item the request quoted, lines that item holds and
// lines the request quoted of it (citeRefused). The answer's statements,
// all of them however few the request asked about, and the corrected
// text's pair in order (citeChanges). Where a statement
// adds a citation refused, its citation_mismatch findings are dropped
// with why, returned by their index in kept — or, with none, the
// correction of it is — and the statement is put back as the answer has
// it. Where the statements do not pair and a citation added is refused,
// or a statement cannot be put back, the corrected text is dropped whole.
func correctedCites(vc Context, corrected string, kept []Finding, keptAt []int, drop func(el, format string, a ...any)) (string, map[int]bool) {
	as, cs := Split(vc.Answer), Split(corrected)
	changes, paired := citeChanges(as, cs, vc.Cites)
	removed := map[int]bool{}
	var back []int
	for _, ch := range changes {
		for _, c := range ch.added {
			why := citeRefused(vc, c)
			if why == "" {
				continue
			}
			if !paired {
				drop("corrected", "it cites %s, and %s; its statements do not pair with the answer's, so it is dropped", c.Place(), why)
				return "", nil
			}
			id := as[ch.at].ID
			mismatch := false
			for k, fi := range kept {
				if fi.Statement == id && fi.Kind == CitationMismatch && !fi.Anchor {
					drop(fmt.Sprintf("findings[%d]", keptAt[k]), "the corrected text cites %s for %s, and %s: %s is kept as written", c.Place(), id, why, id)
					removed[k], mismatch = true, true
				}
			}
			if !mismatch {
				drop("corrected", "it cites %s for %s, and %s: %s is kept as written", c.Place(), id, why, id)
			}
			back = append(back, ch.at)
			break
		}
	}
	if len(back) == 0 {
		return corrected, removed
	}
	ap, aok := placeStatements(vc.Answer, as)
	cp, cok := placeStatements(corrected, cs)
	if !aok || !cok {
		drop("corrected", "%s cannot be put back as written, so the corrected text is dropped", as[back[0]].ID)
		return "", removed
	}
	for i := len(back) - 1; i >= 0; i-- {
		k := back[i]
		corrected = corrected[:cp[k][0]] + vc.Answer[ap[k][0]:ap[k][1]] + corrected[cp[k][1]:]
	}
	return corrected, removed
}

// bracketCite is a citation in brackets with the space before it, as a
// correction adds one to a statement or rewrites one naming a case-ledger
// item, [l26, e45].
var bracketCite = regexp.MustCompile(`\s*\[[el]\d+[^\]\n]*\]`)

// citesOnly says corrected differs from answer only by citations in
// brackets given its statements, which correctedCites has checked, so it
// needs no finding: as a draft citing nothing is given, so the editor may
// cite the lines that hold its statements.
func citesOnly(answer, corrected string) bool {
	as, cs := Split(answer), Split(corrected)
	if len(as) != len(cs) {
		return false
	}
	changed := false
	for i := range as {
		a, c := oneLine(as[i].Text), oneLine(cs[i].Text)
		if a == c {
			continue
		}
		if oneLine(bracketCite.ReplaceAllString(a, "")) != oneLine(bracketCite.ReplaceAllString(c, "")) {
			return false
		}
		changed = true
	}
	return changed
}

// Splice is base — the answer as written or the editor's corrected text —
// with the statements asked, by ID as Split numbers the answer's, as
// corrected has them, the three texts' statements paired in order. It
// fails, saying why, where they do not pair, or where corrected changes
// a statement it was not asked about from both the answer and base, its
// whitespace aside.
func Splice(answer, base, corrected string, asked []string) (string, error) {
	as, bs, cs := Split(answer), Split(base), Split(corrected)
	if len(cs) != len(as) || len(bs) != len(as) {
		return "", fmt.Errorf("its %d statements do not pair with the answer's %d", len(cs), len(as))
	}
	var at []int
	for i, s := range as {
		changed := oneLine(cs[i].Text) != oneLine(s.Text)
		switch {
		case slices.Contains(asked, s.ID):
			if changed {
				at = append(at, i)
			}
		case changed && oneLine(cs[i].Text) != oneLine(bs[i].Text):
			return "", fmt.Errorf("it changed %s, which it was not asked about", s.ID)
		}
	}
	if len(at) == 0 {
		return base, nil
	}
	bp, bok := placeStatements(base, bs)
	cp, cok := placeStatements(corrected, cs)
	if !bok || !cok {
		return "", errors.New("its statements cannot be placed in the text")
	}
	for i := len(at) - 1; i >= 0; i-- {
		k := at[i]
		base = base[:bp[k][0]] + corrected[cp[k][0]:cp[k][1]] + base[bp[k][1]:]
	}
	return base, nil
}

// citeChange is a statement the corrected text gives citations it did not
// have: its index among the statements, -1 where they do not pair, and
// those citations, each once.
type citeChange struct {
	at    int
	added []Cite
}

// citeChanges are the citations corrected gives answer's statements that
// they did not have, the statements paired in order, as the editor is
// asked to keep them. Where they are not as many, they do not pair, and
// one change at -1 holds what the whole text adds.
func citeChanges(answer, corrected []Statement, cites func(string) []Cite) ([]citeChange, bool) {
	added := func(at int, had, has []Cite) []citeChange {
		ch := citeChange{at: at}
		for _, c := range has {
			if !slices.ContainsFunc(had, c.same) && !slices.ContainsFunc(ch.added, c.same) {
				ch.added = append(ch.added, c)
			}
		}
		if len(ch.added) == 0 {
			return nil
		}
		return []citeChange{ch}
	}
	if len(answer) != len(corrected) {
		var had, has []Cite
		for _, s := range answer {
			had = append(had, cites(s.Text)...)
		}
		for _, s := range corrected {
			has = append(has, cites(s.Text)...)
		}
		return added(-1, had, has), false
	}
	var out []citeChange
	for i := range answer {
		out = append(out, added(i, cites(answer[i].Text), cites(corrected[i].Text))...)
	}
	return out, true
}

// citeRefused says why the editor's corrected text may not cite c, or
// nothing: it names an item the request quoted, lines the item holds, and
// lines the request quoted of it; a whole item the request quoted any of.
func citeRefused(vc Context, c Cite) string {
	if c.Path != "" {
		return "an artifact's path is not what the request quoted"
	}
	n, ok := vc.Items[c.Name]
	last := max(c.From, c.To)
	switch {
	case !ok:
		return c.Name + " is no item the request quoted"
	case last > n && n == 1:
		return c.Name + " holds 1 line"
	case last > n:
		return fmt.Sprintf("%s holds %d lines", c.Name, n)
	case c.From == 0:
		return ""
	case c.To != 0 && c.To < c.From:
		return "its lines run backwards"
	case !within(vc.Quoted[c.Name], c.From, last):
		return "the request did not quote those lines"
	}
	return ""
}

// placeStatements finds each of ss in text, in order, as the byte offsets
// of its first and last word: its words as Split read them, apart by any
// whitespace and a quote mark opening a line; false where one is not
// found.
func placeStatements(text string, ss []Statement) ([][2]int, bool) {
	out := make([][2]int, len(ss))
	at := 0
	for i, s := range ss {
		words := strings.Fields(s.Text)
		for j, w := range words {
			words[j] = regexp.QuoteMeta(w)
		}
		loc := regexp.MustCompile(strings.Join(words, `\s+(?:>\s*)?`)).FindStringIndex(text[at:])
		if loc == nil {
			return nil, false
		}
		out[i] = [2]int{at + loc[0], at + loc[1]}
		at += loc[1]
	}
	return out, true
}

// CorrectedCites are the citations the editor's corrected text adds to
// the statements of answer it pairs with, in its order, each once: those
// an answer's footer counts. Where the statements do not pair, they are
// those the whole text adds.
func CorrectedCites(answer, corrected string, cites func(string) []Cite) []Cite {
	changes, _ := citeChanges(Split(answer), Split(corrected), cites)
	var out []Cite
	for _, ch := range changes {
		for _, c := range ch.added {
			if !slices.ContainsFunc(out, c.same) {
				out = append(out, c)
			}
		}
	}
	return out
}

// citesListing reports whether one of refs names a listing given.
func citesListing(listings map[string]bool, refs []string) bool {
	for _, ref := range refs {
		name, _, _ := strings.Cut(ref, ":")
		if listings[name] {
			return true
		}
	}
	return false
}

// oneLine collapses text's whitespace, so a quote matches its statement
// however a model rewrapped it.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

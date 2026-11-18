package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/store"
)

// The chat's view of the companion: what the engineer's commands ask of
// the session beside talking (docs/ARCHITECTURE.md §The chat).

// CompanionItem is an answer the engineer pinned or a draft written for
// them, named i1, i2, …; kept to copy, never posted.
type CompanionItem = companion.Item

// The kinds of item.
const (
	ItemPin   = companion.ItemPin
	ItemDraft = companion.ItemDraft
)

// CompanionModel is the model and effort the session's conversation calls,
// by its name under reasoning.models, and who chose it: the configuration
// or the engineer.
type CompanionModel struct {
	Name, Effort, Source string
}

// analyzeAsk is the engine's words for /analyze.
const analyzeAsk = "Give a structured analysis of the case now: the structured analysis your instructions describe."

// profile is the model the session's calls use: the engineer's choice
// where they made one, else the companion's profile.
func (s *Session) profile() (config.ProfileChoice, error) {
	s.mu.Lock()
	chosen := s.chosen
	s.mu.Unlock()
	if chosen != nil {
		return *chosen, nil
	}
	pc := s.e.config.Profile(companionTask)
	if !pc.Defined {
		return pc, fmt.Errorf("no model is configured for the companion (reasoning.profiles.%s)", pc.Class)
	}
	return pc, nil
}

// CompanionModel is the model and effort the next call uses.
func (s *Session) CompanionModel() (CompanionModel, error) {
	pc, err := s.profile()
	if err != nil {
		return CompanionModel{}, err
	}
	return CompanionModel{Name: pc.ModelName, Effort: pc.Effort, Source: pc.Source}, nil
}

// CompanionModels are the configured models by name, each with the efforts
// it takes.
func (s *Session) CompanionModels() map[string][]string {
	out := map[string][]string{}
	for name, m := range s.e.config.Reasoning.Models {
		out[name] = slices.Clone(m.Efforts)
	}
	return out
}

// ChooseCompanionModel has the session's calls use a configured model, by
// name, at an effort it takes, or its profile's where left empty; each
// call records the engineer's choice. The conversation is kept.
func (s *Session) ChooseCompanionModel(name, effort string) error {
	pc, err := s.e.config.WithModel(s.e.config.Profile(companionTask), name, effort)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.chosen = &pc
	s.mu.Unlock()
	return nil
}

// Analyze asks for a structured analysis of the case.
func (s *Session) Analyze(ctx context.Context, on func(TalkEvent)) (TalkReply, error) {
	return s.turn(ctx, companion.KindEngine, analyzeAsk, 0, on)
}

// Draft has the model write a draft for the configured intention id, in
// the deployment's house style, and keeps it as an item: its text as the
// engineer is shown it, the editor's correction or the model's revision
// where the editor ran. It returns the item with the turn's reply, which
// says what the check made of it; a turn cancelled once the model has
// answered keeps no item, its reply holding the answer as shown.
func (s *Session) Draft(ctx context.Context, id string, on func(TalkEvent)) (CompanionItem, TalkReply, error) {
	in, ok := s.intentionByID(id)
	if !ok {
		var ids []string
		for _, i := range s.e.config.Comments.Intentions {
			ids = append(ids, i.ID)
		}
		return CompanionItem{}, TalkReply{}, fmt.Errorf("no intention %q; the intentions are %s", id, strings.Join(ids, ", "))
	}
	reply, err := s.turn(ctx, companion.KindEngine, draftWords(in, s.SupportName()), 0, on)
	if err != nil {
		return CompanionItem{}, reply, err
	}
	it, err := companion.Store{DB: s.c.st.DB()}.AddItem(ctx, CompanionItem{
		Kind: ItemDraft, Conversation: reply.Conversation, Intention: in.ID, Audience: in.Audience,
		Text: reply.Text, CreatedAt: s.c.now(),
	})
	return it, reply, err
}

func (s *Session) intentionByID(id string) (config.Intention, bool) {
	for _, in := range s.e.config.Comments.Intentions {
		if in.ID == id {
			return in, true
		}
	}
	return config.Intention{}, false
}

// draftWords is the engine's words for a draft: the intention, for whom, and
// what it is for; the house style is in the system prompt. A draft the
// customer will read carries the certainty review it is held to
// (companion.CustomerDraft); an internal note does not.
func draftWords(in config.Intention, supportName string) string {
	audience := "the customer, as a case comment"
	if in.Audience == "internal" {
		audience = supportName + " only, as an internal note"
	}
	words := fmt.Sprintf(draftLead+"%s (%s), for %s: %s Write it from this conversation, in the house style, ready to post: only the text, no preamble.", in.Label, in.ID, audience, in.Purpose)
	if in.Audience == "internal" {
		return words
	}
	return words + "\n\n" + companion.CustomerDraft
}

// SupportName is the configured name of the support organization.
func (s *CaseStore) SupportName() string { return s.e.config.SupportName() }

// SupportRole is what the deployment calls a support author as a person.
func (s *CaseStore) SupportRole() string { return s.e.config.SupportRole() }

// Pin keeps the conversation's last answer as an item.
func (s *Session) Pin(ctx context.Context) (CompanionItem, error) {
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	if err != nil {
		return CompanionItem{}, errors.New("there is no answer to pin yet")
	}
	text, err := st.LastAnswer(ctx, conv.ID)
	if err != nil {
		return CompanionItem{}, err
	}
	if text == "" {
		return CompanionItem{}, errors.New("there is no answer to pin yet")
	}
	return st.AddItem(ctx, CompanionItem{Kind: ItemPin, Conversation: conv.ID, Text: text, CreatedAt: s.c.now()})
}

// Items are the case's pinned answers and drafts, oldest first.
func (s *Session) Items(ctx context.Context) ([]CompanionItem, error) {
	return companion.Store{DB: s.c.st.DB()}.Items(ctx)
}

// carried is the items a compaction carries into the new conversation.
func carried(ctx context.Context, c *openCase) (string, error) {
	items, err := companion.Store{DB: c.st.DB()}.Items(ctx)
	if err != nil || len(items) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Retained model answers and drafts\n\nModel answers the engineer pinned and drafts written for them, kept verbatim. Retention alone does not verify their claims or make them the engineer's conclusions:")
	for _, it := range items {
		what := "pinned answer"
		if it.Kind == ItemDraft {
			what = "draft for " + it.Intention
		}
		fmt.Fprintf(&b, "\n\n## %s · %s\n\n%s", it.Name(), what, strings.TrimSpace(it.Text))
	}
	return b.String(), nil
}

// LoadExpert loads an installed expert's guidance into the conversation,
// as the engine's own call, and returns what the model is given.
func (s *Session) LoadExpert(ctx context.Context, id string) (string, error) {
	l, err := s.loop(ctx)
	if err != nil {
		return "", err
	}
	conv, err := s.conversation(ctx, l)
	if err != nil {
		return "", err
	}
	return l.Preload(ctx, conv, "load_expert", map[string]any{"id": id})
}

// CompanionExpert is one installed expert as the index lists it, and
// whether the conversation has loaded it.
type CompanionExpert struct {
	ID, Version, Summary string
	RoutingKeys          []string
	Loaded               bool
}

// Experts are the installed experts, by ID.
func (s *Session) Experts(ctx context.Context) ([]CompanionExpert, error) {
	loaded := map[string]bool{}
	st := companion.Store{DB: s.c.st.DB()}
	conv, err := st.Latest(ctx)
	switch {
	case err == nil:
		for _, raw := range conv.Pins {
			var p expert.Pin
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			loaded[p.ID] = true
		}
	case !errors.Is(err, companion.ErrNoConversation):
		return nil, err
	}
	idx := s.e.experts().idx
	var out []CompanionExpert
	for _, d := range s.e.installedExperts() {
		x := CompanionExpert{ID: d.ID, Version: d.Version, Summary: d.Summary, Loaded: loaded[d.ID]}
		if entry, ok := idx.Describe(d.ID, d.Version); ok {
			for _, r := range entry.Manifest.Spec.Routing.Keys {
				if r.Relationship == "primary" {
					x.RoutingKeys = append(x.RoutingKeys, r.ID)
				}
			}
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CompanionUsage is the case's model usage by conversation (a consult's
// is its own) and model.
func (s *Session) CompanionUsage() ([]UsageAggregate, error) {
	return invocations.AggregateUsage(s.c.st.DB(), s.c.id, invocations.ByConversation, invocations.ByModel)
}

// NamedEvidence is one piece of the case's evidence by its short name, as
// the browser lists it: its description, without its content.
type NamedEvidence struct {
	Name string
	Item EvidenceItem
	// By says what produced it: a tool and its parameters, a command, the
	// engineer, or the case's source.
	By string
}

// Evidences are the case's named evidence, newest name first.
func (s *CaseStore) Evidences() ([]NamedEvidence, error) {
	// The names are read first and the rows closed: the store has one
	// connection, which describing each item needs.
	ns, err := evidenceNames(s.c)
	if err != nil {
		return nil, err
	}
	out := make([]NamedEvidence, 0, len(ns))
	for _, n := range ns {
		it, err := s.c.ev.Describe(n.id)
		if err != nil {
			return nil, err
		}
		out = append(out, NamedEvidence{Name: "e" + strconv.FormatInt(n.seq, 10), Item: it, By: ProducedBy(it)})
	}
	return out, nil
}

// namedID is one evidence name: its number and the item it names.
type namedID struct {
	seq int64
	id  string
}

// evidenceNames are the case's evidence names, newest first.
func evidenceNames(c *openCase) ([]namedID, error) {
	rows, err := c.st.DB().Query(`SELECT seq, evidence_id FROM evidence_names ORDER BY seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ns []namedID
	for rows.Next() {
		var n namedID
		if err := rows.Scan(&n.seq, &n.id); err != nil {
			return nil, err
		}
		ns = append(ns, n)
	}
	return ns, rows.Err()
}

// EvidenceNamed is the evidence a short name names, with its content.
func (s *CaseStore) EvidenceNamed(name string) (EvidenceItem, error) {
	id, err := evidenceByName(s.c, name)
	if err != nil {
		return EvidenceItem{}, err
	}
	return s.c.ev.Get(id)
}

// NameEvidence gives a stored item its short name, or returns the one it
// has.
func (s *Session) NameEvidence(id string) (string, error) { return evidenceName(s.c, id) }

// ProducedBy says what made an item: a tool and its parameters, a
// command, the engineer, or the case's source.
func ProducedBy(it EvidenceItem) string {
	switch {
	case it.Collection != nil && it.Collection.What.TestID != "":
		return it.Collection.What.TestID + "@" + it.Collection.What.TestVersion + " " + compact(it.Collection.What.Params)
	case it.Collection != nil:
		var stages []string
		for _, st := range it.Collection.What.Pipeline {
			if len(st.Argv) > 0 {
				stages = append(stages, strings.Join(st.Argv, " "))
				continue
			}
			stages = append(stages, st.Primitive+" "+compact(st.Params))
		}
		return strings.Join(stages, " | ")
	case it.Ref.Type == evidence.TypeIngested:
		return "the engineer: " + strings.TrimSpace(it.Location+" "+it.Note)
	case it.Note != "":
		return it.Note
	}
	return caseItemTitle(it.Ref.Type)
}

// Cite is one place an answer cites: evidence by short name, or an
// artifact by path, with the lines it names (To 0 for one line); or a
// case-ledger item a bracket names among them (Ledger, l26), which is a
// model's reading of a comment and never evidence, so CheckCites flags it
// and nothing that reads evidence takes it.
type Cite struct {
	Text     string
	Name     string
	Path     string
	Ledger   string
	From, To int
}

var (
	// evidenceCite is a bracket of names, each an evidence name with its
	// lines and any bare lines after it, or a ledger item's ID. A range
	// accepts a hyphen or the typographic en dash models also write.
	evidenceCite = regexp.MustCompile(`\[(?:e\d+(?::\d+(?:[-–]\d+)?)?(?:,\s*\d+(?:[-–]\d+)?)*|l\d+)(?:,\s*(?:e\d+(?::\d+(?:[-–]\d+)?)?(?:,\s*\d+(?:[-–]\d+)?)*|l\d+))*\]`)
	pathCite     = regexp.MustCompile("`?((?:[A-Za-z0-9_.@+-]+/)+[A-Za-z0-9_.@+-]+):(\\d+)(?:[-–](\\d+))?`?")
	nameWord     = regexp.MustCompile(`\be\d+\b`)
)

// Cites are the places an answer cites, in order: [e12:41-58],
// [e12:41], [e12:2-3, 7], [e12] and [e1, e4], case-ledger items among
// them, [l26, e45], and artifact paths with a line.
func Cites(text string) []Cite {
	var out []Cite
	for _, m := range evidenceCite.FindAllString(text, -1) {
		// A bare number belongs to the evidence name before it: in
		// [e35:4, e40:18, 24], line 24 is e40's.
		name := ""
		for _, part := range strings.Split(m[1:len(m)-1], ",") {
			part = strings.TrimSpace(part)
			switch {
			case strings.HasPrefix(part, "l"):
				out = append(out, Cite{Text: m, Ledger: part})
				continue
			case strings.HasPrefix(part, "e"):
				n, lines, ok := strings.Cut(part, ":")
				if name = n; !ok {
					out = append(out, Cite{Text: m, Name: name})
					continue
				}
				part = lines
			}
			from, to := part, ""
			if at := strings.IndexAny(part, "-–"); at >= 0 {
				from = part[:at]
				width := 1
				if part[at] != '-' {
					width = len("–")
				}
				to = part[at+width:]
			}
			c := Cite{Text: m, Name: name}
			c.From, _ = strconv.Atoi(from)
			c.To, _ = strconv.Atoi(to)
			out = append(out, c)
		}
	}
	for _, m := range pathCite.FindAllStringSubmatch(text, -1) {
		c := Cite{Text: m[0], Path: m[1]}
		c.From, _ = strconv.Atoi(m[2])
		c.To, _ = strconv.Atoi(m[3])
		out = append(out, c)
	}
	return out
}

// CiteCheck is whether a citation resolves: the evidence or artifact
// it names and its line count, or why it is flagged.
type CiteCheck struct {
	OK bool
	// Why says what is wrong with a flagged citation.
	Why string
	// Via says, of a citation that resolves on lines the conversation
	// itself was not shown, whose showing it rests on: a consult's read,
	// or names alone where the conversation's exposure is unknown.
	Via        string
	EvidenceID string
	Lines      int
}

// CheckCites resolves an answer's citations against the case: evidence
// whose lines the conversation was shown, an artifact under the case's
// root, lines within the end. A case-ledger item is flagged, never
// resolved as evidence, saying which comment it reads. A flagged citation
// is shown flagged, never dropped.
func (s *Session) CheckCites(ctx context.Context, cites []Cite) ([]CiteCheck, error) {
	x, err := s.exposure(ctx)
	if err != nil {
		return nil, err
	}
	var ledger map[string]string
	if slices.ContainsFunc(cites, func(c Cite) bool { return c.Ledger != "" }) {
		if ledger, err = s.ledgerCiteWhys(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]CiteCheck, len(cites))
	for i, c := range cites {
		if c.Ledger != "" {
			why, ok := ledger[c.Ledger]
			if !ok {
				why = c.Ledger + " names nothing: the case ledger has no item " + c.Ledger
			}
			out[i] = CiteCheck{Why: why}
			continue
		}
		out[i] = s.checkCite(c, x)
	}
	return out, nil
}

// ledgerCiteWhys are why a citation of each case-ledger item is flagged,
// by ID: an item is the ledger's model's reading of a comment, never
// evidence (docs/MODEL_API.md §The case ledger), so what to cite is the
// comment's own lines, named by its number and evidence name; an item
// from a comment the engineer's rules leave out names no comment.
func (s *Session) ledgerCiteWhys(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if s.c.chat {
		return out, nil
	}
	all, err := ledgerItems(s.c)
	if err != nil {
		return nil, err
	}
	for _, it := range all {
		out[it.ID] = it.ID + " is the case ledger's reading of a comment left out of what models read, not evidence"
	}
	shown, err := s.Ledger(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range shown {
		comment := it.Comment
		if it.Number > 0 {
			comment = fmt.Sprintf("c#%d (%s)", it.Number, it.Comment)
		}
		out[it.ID] = it.ID + " is the case ledger's reading of comment " + comment + ", not evidence: cite that comment's lines instead"
	}
	return out, nil
}

func (s *Session) checkCite(c Cite, x exposure) CiteCheck {
	last := max(c.From, c.To)
	if c.Path != "" {
		n, err := artifactLines(s.c.root, c.Path)
		if err != nil {
			return CiteCheck{Why: "no artifact " + c.Path}
		}
		if last > n {
			return CiteCheck{Why: fmt.Sprintf("%s has %d lines", c.Path, n), Lines: n}
		}
		return CiteCheck{OK: true, Lines: n}
	}
	id, err := evidenceByName(s.c, c.Name)
	if err != nil {
		return CiteCheck{Why: "no evidence " + c.Name}
	}
	if !x.shown[c.Name] {
		why := c.Name + " was never shown in this conversation"
		if x.unknown {
			why += " (" + exposureUnknown + ")"
		}
		return CiteCheck{Why: why, EvidenceID: id}
	}
	it, err := s.c.ev.Describe(id)
	if err != nil {
		return CiteCheck{Why: err.Error(), EvidenceID: id}
	}
	n := int(it.Size.Lines)
	if last > n {
		return CiteCheck{Why: fmt.Sprintf("%s has %d lines", c.Name, n), EvidenceID: id, Lines: n}
	}
	via, ok := x.covers(c.Name, c.From, last)
	switch {
	case ok:
		return CiteCheck{OK: true, Via: via, EvidenceID: id, Lines: n}
	case x.named[c.Name]:
		return CiteCheck{OK: true, Via: exposureUnknown, EvidenceID: id, Lines: n}
	}
	return CiteCheck{Why: x.outside(c.Name, c.From, last), EvidenceID: id, Lines: n}
}

// outside says that lines from to to of name, an item the conversation
// was shown lines of, were never shown it, with the lines that were.
func (x exposure) outside(name string, from, to int) string {
	what := "line " + strconv.Itoa(from) + " was"
	if to > from {
		what = fmt.Sprintf("lines %d–%d were", from, to)
	}
	return fmt.Sprintf("%s's %s never shown in this conversation, only %s", name, what, x.ranges(name))
}

// reads says whether lines from to to of name, from 1, may be read as
// shown: carried to the conversation or its consults, or the item named
// where its exposure is unknown; and if not, why.
func (x exposure) reads(name string, from, to int) (string, bool) {
	if !x.shown[name] {
		return name + " is no evidence this conversation saw", false
	}
	if _, ok := x.covers(name, from, to); ok || x.named[name] {
		return "", true
	}
	return x.outside(name, from, to), false
}

// whole says whether every line of name may be read as shown: the item
// was carried whole, or named where the exposure is unknown.
func (x exposure) whole(name string) bool {
	return x.named[name] || slices.ContainsFunc(x.lines[name], func(r carriedRead) bool { return r.from == 0 })
}

// exposureUnknown says a citation was judged by names, its conversation,
// or one it continues, stored before the engine recorded what each call
// carried.
const exposureUnknown = "exposure unknown: the conversation was stored before the engine recorded what each call showed, so it is judged by the names it holds"

// exposure is what the case's current conversation, the ones it was
// compacted from and their consults were shown of the evidence: as the
// engine recorded what each request carried (companion.Store.Exposure),
// or, for a conversation stored before that record, by the names its
// messages hold (unrecordedNames).
type exposure struct {
	// shown are the items any of whose content a conversation was shown,
	// by short name, those an unrecorded conversation names among them.
	shown map[string]bool
	// lines are the lines carried, by short name.
	lines map[string][]carriedRead
	// named are the names an unrecorded conversation holds; unknown says
	// there was one.
	named   map[string]bool
	unknown bool
}

// carriedRead is lines from to to of an item a request carried, both 0
// for the whole; consult names the expert whose consult was shown them,
// empty for the conversation's own.
type carriedRead struct {
	from, to int
	consult  string
}

// covers reports whether the lines from to to of name, or the item where
// from is 0, were carried: by the conversation itself, via empty, or with
// its consults, via naming the consults that read them. A whole item cited
// is covered by any of its lines.
func (x exposure) covers(name string, from, to int) (via string, ok bool) {
	reads := x.lines[name]
	own := slices.DeleteFunc(slices.Clone(reads), func(r carriedRead) bool { return r.consult != "" })
	if spans(own, from, to) {
		return "", true
	}
	if !spans(reads, from, to) {
		return "", false
	}
	var experts []string
	for _, r := range reads {
		if r.consult != "" && (from == 0 || r.from == 0 || (r.from <= to && r.to >= from)) && !slices.Contains(experts, r.consult) {
			experts = append(experts, r.consult)
		}
	}
	return "read in the consult of " + strings.Join(experts, " and ") + ", not in this conversation", true
}

// spans reports whether reads hold every line from to to, or, where from
// is 0, any line.
func spans(reads []carriedRead, from, to int) bool {
	if len(reads) == 0 {
		return false
	}
	if from == 0 || slices.ContainsFunc(reads, func(r carriedRead) bool { return r.from == 0 }) {
		return true
	}
	sorted := slices.SortedFunc(slices.Values(reads), func(a, b carriedRead) int { return a.from - b.from })
	next := from
	for _, r := range sorted {
		if r.from > next {
			break
		}
		next = max(next, r.to+1)
	}
	return next > to
}

// ranges are the lines of name carried, merged, as the flag says them:
// "lines 1–40, 52–60", at most a few ranges, the rest counted.
func (x exposure) ranges(name string) string {
	sorted := slices.SortedFunc(slices.Values(x.lines[name]), func(a, b carriedRead) int { return a.from - b.from })
	var merged []carriedRead
	for _, r := range sorted {
		if k := len(merged) - 1; k >= 0 && r.from <= merged[k].to+1 {
			merged[k].to = max(merged[k].to, r.to)
			continue
		}
		merged = append(merged, r)
	}
	const most = 4
	var parts []string
	for i, r := range merged {
		if i == most {
			parts = append(parts, fmt.Sprintf("and %d more", len(merged)-most))
			break
		}
		if r.from == r.to {
			parts = append(parts, strconv.Itoa(r.from))
		} else {
			parts = append(parts, fmt.Sprintf("%d–%d", r.from, r.to))
		}
	}
	if len(merged) == 1 && merged[0].from == merged[0].to {
		return "line " + parts[0]
	}
	return "lines " + strings.Join(parts, ", ")
}

// exposure is what the case's current conversation, the ones it was
// compacted from and their consults were shown of the evidence. A
// conversation's own requests are read from the record of what each
// carried, which an engine-authored message gives with its content: an
// item of the case block or a case update under its heading, the lines a
// tool's result gives under their numbers, what a restart carries, the
// lines a verdict quotes. A name alone — an older comment's line, an item
// too large to give, an evidence listing, the engineer's words — shows
// nothing, and nothing a model wrote does. A checker's conversation is
// never one of them, so what an editor or a reviewer was quoted shows
// the conversation nothing. A conversation stored before the record has
// its exposure unknown, and is judged by the names it holds
// (unrecordedNames), said so.
func (s *Session) exposure(ctx context.Context) (exposure, error) {
	st := companion.Store{DB: s.c.st.DB()}
	x := exposure{shown: map[string]bool{}, lines: map[string][]carriedRead{}, named: map[string]bool{}}
	conv, err := st.Latest(ctx)
	if errors.Is(err, companion.ErrNoConversation) {
		return x, nil
	}
	if err != nil {
		return x, err
	}
	for id := conv.ID; id != ""; {
		c, err := st.Conversation(ctx, id)
		if err != nil {
			return x, err
		}
		consults, err := st.Consults(ctx, id)
		if err != nil {
			return x, err
		}
		for _, cv := range append([]companion.Conversation{c}, consults...) {
			if !cv.Exposure {
				x.unknown = true
				if err := unrecordedNames(ctx, st, cv.ID, x.named); err != nil {
					return x, err
				}
				continue
			}
			shown, err := st.Exposure(ctx, cv.ID)
			if err != nil {
				return x, err
			}
			for _, sh := range shown {
				x.lines[sh.Name] = append(x.lines[sh.Name], carriedRead{from: sh.From, to: sh.To, consult: cv.Expert})
			}
		}
		id = c.CompactedFrom
	}
	for n := range x.lines {
		x.shown[n] = true
	}
	for n := range x.named {
		x.shown[n] = true
	}
	return x, nil
}

// unrecordedNames adds to names those a conversation stored before the
// record of what each call carried shows by the rule kept for it: an item
// the case block or a case update shows under its heading, with its
// content, a name in the engineer's words, or one in a tool's result. A
// name alone is no showing: an older comment the case block names on its
// line, an item too large to give, named with its size, and the comments
// the ledger's items name are not shown by them. What a model wrote — an
// answer, a correction, a handoff, a review, a consult's question or the
// answer a consult returned — shows nothing, so a citation never shows
// its own item.
func unrecordedNames(ctx context.Context, st companion.Store, conv string, names map[string]bool) error {
	msgs, err := st.Messages(ctx, conv)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		switch m.Kind {
		case companion.KindCase, companion.KindUpdate:
			for _, n := range shownUnderHeadings(m.Text) {
				names[n] = true
			}
		case companion.KindEngineer:
			for _, n := range nameWord.FindAllString(m.Text, -1) {
				names[n] = true
			}
		case companion.KindResults:
			for _, r := range m.Results {
				if r.Name == consultTool {
					continue
				}
				for _, n := range nameWord.FindAllString(r.Text, -1) {
					names[n] = true
				}
			}
		}
	}
	return nil
}

// itemHeading is the heading caseItems gives each item, its name first.
var itemHeading = regexp.MustCompile(`^## (e\d+) · `)

// notShown begins what caseItems writes in place of an item too large to
// give.
const notShown = "Not shown here, "

// shownUnderHeadings are the names of the items a case block or case
// update gives under their headings (caseItems), with their content: not
// one written in its place as too large, nor a name elsewhere in the text.
func shownUnderHeadings(text string) []string {
	lines := strings.Split(text, "\n")
	var out []string
	for i, l := range lines {
		m := itemHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		j := i + 1
		for j < len(lines) && strings.HasPrefix(lines[j], "! ") {
			j++
		}
		if j < len(lines) && strings.HasPrefix(lines[j], notShown) {
			continue
		}
		out = append(out, m[1])
	}
	return out
}

// maxCountedBytes bounds the read that counts an artifact's lines for a
// citation; past it the count is a floor.
const maxCountedBytes = 256 << 20

// artifactLines counts a file's lines under the case's artifact root,
// streaming, confined to the root.
func artifactLines(root, p string) (int, error) {
	if root == "" {
		return 0, errors.New("no artifacts")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	f, err := r.Open(path.Clean(p))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n, read := 0, 0
	for sc.Scan() && read < maxCountedBytes {
		n++
		read += len(sc.Bytes()) + 1
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return n, err
	}
	return n, nil
}

// metaChatDir is the store's meta key holding a chat's artifact root by
// its real path, empty for a chat with no directory. A chat stored before
// it was kept has none, and keeps the first directory it is given.
const metaChatDir = "chat_dir"

// StartChat opens a troubleshooting chat with no case: id names an earlier
// chat to resume, or a new one when empty; dir, when given, is its
// artifact root, by its real path. Its store is under the home directory's
// .local/share/compa/chats/<id>, so id is checked as a case ID is. The
// directory is kept with the chat: resumed without one, it is over its
// own again, and resumed with another, or with one where it began with
// none, it is refused. Close closes it.
func (e *Engine) StartChat(ctx context.Context, id, dir string) (*Session, error) {
	if id != "" {
		if err := CheckCaseID(id); err != nil {
			return nil, fmt.Errorf("chat name: %w", err)
		}
	}
	root := ""
	if dir != "" {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			resolved, err = filepath.Abs(resolved)
		}
		if fi, serr := os.Stat(resolved); err != nil || serr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("no directory %s", dir)
		}
		root = resolved
	}
	if id == "" {
		id = "chat-" + time.Now().UTC().Format("20060102-150405")
	}
	storeDir := filepath.Join(e.home, ".local", "share", "compa", "chats", id)
	_, err := os.Stat(storeDir)
	resumed := err == nil
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(storeDir, id, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("chat %s: %w", id, err)
	}
	if root, err = chatRoot(ctx, st, id, root, resumed); err != nil {
		st.Close()
		return nil, err
	}
	c := newOpenCase(id, root, storeDir, true, st)
	c.chat = true
	return &Session{caseStore: &CaseStore{e: e, c: c}}, nil
}

// chatRoot is the artifact root of chat id, given root, its directory as
// the engineer gave it now, if any: the one kept with the chat, which a
// different one may not replace, else root, kept from now on.
func chatRoot(ctx context.Context, st *store.Store, id, root string, resumed bool) (string, error) {
	kept, ok, err := st.Meta(ctx, metaChatDir)
	switch {
	case err != nil:
		return "", fmt.Errorf("chat %s: its directory: %w", id, err)
	case !ok && (root != "" || !resumed):
		return root, st.SetMeta(ctx, metaChatDir, root)
	case !ok, root == "", root == kept:
		return kept, nil
	case kept == "":
		return "", fmt.Errorf("chat %s has no directory; resume it without --dir, or start a new chat over %s", id, root)
	}
	return "", fmt.Errorf("chat %s is over %s, not %s; resume it without --dir", id, kept, root)
}

// IsChat is true for a troubleshooting chat with no case.
func (s *Session) IsChat() bool { return s.c.chat }

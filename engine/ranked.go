package engine

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/engine/text"
)

// What each use of an evidence item adds to its rank, before decay, in
// descending order: lines the
// engineer pointed at, a citation in a pinned answer or a draft, a
// citation in another of the model's answers, an excerpt a tool returned.
// Each counts once per message, or per item or tool result, however many
// ranges of the item it names.
const (
	rankPointed  = 5.0
	rankPinned   = 4.0
	rankCited    = 3.0
	rankReturned = 1.0
	// rankDecay is what a use is worth for each turn since: one in the
	// newest turn counts whole, one ten turns ago about a fifth.
	rankDecay = 0.85
)

// pointsAt opens an engineer's message that points at lines the browser
// marked, its first line naming them: "the engineer points at e12:41-58,
// e3:7". The chat writes it.
const pointsAt = "the engineer points at "

// caseBlockTypes are the kinds of item the case block holds, every version:
// the case's details, its attachments' overview and inventory, and its
// comments, which are always reachable by name and which the ledger covers.
// The ranked list leaves them out.
var caseBlockTypes = map[string]bool{
	TypeCaseMetadata:      true,
	TypeArtifactOverview:  true,
	TypeArtifactInventory: true,
	TypeCaseComment:       true,
}

// LineRange is lines From to To of an evidence item, both included; one
// line has To equal to From.
type LineRange struct{ From, To int }

// RankedEvidence is one evidence item the conversation used, ranked by
// what was done with it and how recently.
type RankedEvidence struct {
	Name       string
	EvidenceID string
	Score      float64
	// Ranges are the lines pointed at or cited, sorted, overlapping and
	// adjacent ones merged; none where only the whole item was named.
	Ranges []LineRange
	// Why says what was done with it, newest first, each once: "pointed at
	// by the engineer", "cited in i2", "cited in a3", "returned by grep".
	Why []string
	// LastTurn is how many turns ago it was last used: 0 for the newest.
	LastTurn int
}

// rankUse is one use of an evidence item by name.
type rankUse struct {
	name   string
	weight float64
	age    int
	why    string
	ranges []LineRange
}

// RankedEvidence is the evidence the conversation used, through its
// compaction ancestors and not its consults, best first, at most limit
// items (all where limit is not positive). Each use scores its weight
// decayed by the turns since, a turn being an engineer's or the engine's
// message; items the case block holds are left out, as are names no
// evidence has. Ties go to the more recent use, then the lower name. It
// is derived from the stored messages and items each time: it writes
// nothing and calls no model.
func (s *Session) RankedEvidence(ctx context.Context, limit int) ([]RankedEvidence, error) {
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return nil, err
	}
	return s.rankHistory(ctx, history, limit)
}

// rankHistory is RankedEvidence over history, the conversation's as
// ConversationHistory reads it.
func (s *Session) rankHistory(ctx context.Context, history []ConversationMessage, limit int) ([]RankedEvidence, error) {
	if len(history) == 0 {
		return nil, nil
	}
	items, err := s.Items(ctx)
	if err != nil {
		return nil, err
	}
	uses := rankUses(history, items)
	if len(uses) == 0 {
		return nil, nil
	}
	named, err := evidenceNames(s.c)
	if err != nil {
		return nil, err
	}
	seqs := make(map[string]int64, len(named))
	ids := make(map[string]string, len(named))
	for _, n := range named {
		name := fmt.Sprintf("e%d", n.seq)
		seqs[name], ids[name] = n.seq, n.id
	}
	byName := map[string]*RankedEvidence{}
	skip := map[string]bool{}
	var out []*RankedEvidence
	// Newest first, so Why reads newest first.
	for i := len(uses) - 1; i >= 0; i-- {
		u := uses[i]
		if skip[u.name] {
			continue
		}
		r := byName[u.name]
		if r == nil {
			id, ok := ids[u.name]
			if !ok {
				skip[u.name] = true
				continue
			}
			it, err := s.c.ev.Describe(id)
			if err != nil {
				return nil, err
			}
			if caseBlockTypes[it.Ref.Type] {
				skip[u.name] = true
				continue
			}
			r = &RankedEvidence{Name: u.name, EvidenceID: id, LastTurn: u.age}
			byName[u.name] = r
			out = append(out, r)
		}
		r.Score += u.weight * math.Pow(rankDecay, float64(u.age))
		r.Ranges = append(r.Ranges, u.ranges...)
		if !slices.Contains(r.Why, u.why) {
			r.Why = append(r.Why, u.why)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.LastTurn != b.LastTurn {
			return a.LastTurn < b.LastTurn
		}
		return seqs[a.Name] < seqs[b.Name]
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	ranked := make([]RankedEvidence, len(out))
	for i, r := range out {
		r.Ranges = mergeRanges(r.Ranges)
		ranked[i] = *r
	}
	return ranked, nil
}

// rankUses are the uses of evidence names in history, oldest first, each
// with its age in turns. An answer counts as the engineer was shown it
// (AnswerViews): its revision's or the editor's correction's citations in
// its place, the latter said so, the words it replaced not counted. A
// pinned answer's or draft's citations count as the item's, in place of
// the answer's; an item whose answer is not in history counts where it
// was made.
func rankUses(history []ConversationMessage, items []CompanionItem) []rankUse {
	ages := make([]int, len(history))
	turn := 0
	for i := len(history) - 1; i >= 0; i-- {
		ages[i] = turn
		if k := history[i].Kind; k == MessageEngineer || k == MessageEngine {
			turn++
		}
	}
	shown := map[int]AnswerView{}
	replaced := map[int]bool{}
	for _, v := range AnswerViews(history) {
		shown[v.At] = v
		for i := v.Original; i < v.At; i++ {
			if history[i].Kind == MessageModel {
				replaced[i] = true
			}
		}
	}
	// The items each message carries: the newest answer shown with the
	// same text (or stripped draft text), else the newest message made by
	// the time the item was.
	type keptItem struct {
		item CompanionItem
		text string
	}
	kept := map[int][]keptItem{}
	asAnswer := map[int]bool{}
	for _, it := range items {
		at := -1
		citeText := it.Text
		for i := len(history) - 1; i >= 0; i-- {
			if v, ok := shown[i]; ok && (strings.TrimSpace(v.Text) == strings.TrimSpace(it.Text) ||
				(it.Kind == ItemDraft && v.Draft && strings.TrimSpace(StripDraftCites(v.Text)) == strings.TrimSpace(it.Text))) {
				at = i
				citeText = v.Text
				asAnswer[i] = true
				break
			}
		}
		if at < 0 {
			at = 0
			for i := len(history) - 1; i >= 0; i-- {
				if !history[i].CreatedAt.After(it.CreatedAt) {
					at = i
					break
				}
			}
		}
		kept[at] = append(kept[at], keptItem{item: it, text: citeText})
	}
	var uses []rankUse
	for i, m := range history {
		age := ages[i]
		switch v, isShown := shown[i]; {
		case isShown:
			// Numbered as the chat numbers the answers it restores.
			why := "cited in " + v.Label
			if v.Corrected {
				why += ", as the editor corrected it"
			}
			if !asAnswer[i] {
				uses = append(uses, citeUses(Cites(v.Text), rankCited, age, why)...)
			}
		case m.Kind == MessageEngineer:
			if head, ok := strings.CutPrefix(m.Text, pointsAt); ok {
				head, _, _ = strings.Cut(head, "\n")
				var cites []Cite
				for _, mark := range strings.Split(head, ",") {
					cites = append(cites, Cites("["+strings.TrimSpace(mark)+"]")...)
				}
				uses = append(uses, citeUses(cites, rankPointed, age, "pointed at by the engineer")...)
			}
		case m.Kind == MessageModel && !replaced[i] && !IsAnswer(m):
			uses = append(uses, citeUses(Cites(m.Text), rankCited, age, "cited by the model")...)
		case m.Kind == MessageResults:
			for _, r := range m.Results {
				if r.IsError {
					continue
				}
				for _, name := range ExcerptEvidence(r.Text) {
					uses = append(uses, rankUse{name: name, weight: rankReturned, age: age, why: "returned by " + r.Name})
				}
			}
		}
		for _, k := range kept[i] {
			uses = append(uses, citeUses(Cites(k.text), rankPinned, age, "cited in "+k.item.Name())...)
		}
	}
	return uses
}

// citeUses is one use for each evidence name cites name, with the lines
// they name.
func citeUses(cites []Cite, weight float64, age int, why string) []rankUse {
	var uses []rankUse
	at := map[string]int{}
	for _, c := range cites {
		if c.Name == "" {
			continue
		}
		i, ok := at[c.Name]
		if !ok {
			i = len(uses)
			at[c.Name] = i
			uses = append(uses, rankUse{name: c.Name, weight: weight, age: age, why: why})
		}
		if c.From > 0 {
			uses[i].ranges = append(uses[i].ranges, LineRange{From: c.From, To: max(c.From, c.To)})
		}
	}
	return uses
}

// mergeRanges sorts ranges and merges those that overlap or adjoin.
func mergeRanges(rs []LineRange) []LineRange {
	if len(rs) == 0 {
		return nil
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].From != rs[j].From {
			return rs[i].From < rs[j].From
		}
		return rs[i].To < rs[j].To
	})
	out := []LineRange{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.From <= last.To+1 {
			last.To = max(last.To, r.To)
			continue
		}
		out = append(out, r)
	}
	return out
}

// RenderRanked is the ranked list as /ranked shows it, one line an item:
// its name, score, lines, why and when it was last used.
func RenderRanked(entries []RankedEvidence) string {
	if len(entries) == 0 {
		return "No evidence ranked yet: the conversation has used none beyond the case's own.\n"
	}
	var b strings.Builder
	for _, r := range entries {
		fmt.Fprintf(&b, "%s · %.2f", r.Name, r.Score)
		if len(r.Ranges) > 0 {
			parts := make([]string, len(r.Ranges))
			for i, rg := range r.Ranges {
				parts[i] = fmt.Sprint(rg.From)
				if rg.To > rg.From {
					parts[i] += fmt.Sprintf("-%d", rg.To)
				}
			}
			b.WriteString(" · lines " + strings.Join(parts, ", "))
		}
		b.WriteString(" · " + strings.Join(r.Why, ", "))
		if r.LastTurn == 0 {
			b.WriteString(" · this turn\n")
		} else {
			b.WriteString(" · " + text.Plural(r.LastTurn, "turn") + " ago\n")
		}
	}
	return b.String()
}

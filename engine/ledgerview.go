package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/ledger"
)

// LedgerComment is a case comment as the ledger accounts for it, for the
// engineer's view of the ledger (docs/MODEL_API.md §The case ledger).
type LedgerComment struct {
	// Number is the comment's number in the case, c#N; Name its evidence
	// name, eN; Head its header as the source gave it, the body empty.
	Number int
	Name   string
	Head   CaseComment
	// Outcome is what the ledger made of it: OutcomeItems, OutcomeChanges,
	// "none: " and the model's why, OutcomeUnaccounted, OutcomeNotShown,
	// OutcomeUnread, OutcomeLeftOut, or OutcomeEarlier.
	Outcome string
}

// What the ledger made of a comment, beside "none: <why>".
const (
	OutcomeItems       = "items"
	OutcomeChanges     = "changes"
	OutcomeUnaccounted = "unaccounted"
	// OutcomeNotShown is a comment too long to give the ledger's model,
	// marked by the engine: not read, and not read again until edited.
	OutcomeNotShown = "not shown"
	// OutcomeUnread is a comment the ledger has yet to read.
	OutcomeUnread = "unread"
	// OutcomeLeftOut is a comment the engineer's rules leave out of what
	// models read.
	OutcomeLeftOut = "left out"
	// OutcomeEarlier is a comment saved again with a text the ledger read
	// in an earlier version, which is not read again.
	OutcomeEarlier = "earlier"
)

// LedgerComments are the case's comments, numbered as the case lists
// them, oldest first, each with what the ledger made of it.
func (s *Session) LedgerComments(context.Context) ([]LedgerComment, error) {
	if s.c.chat {
		return nil, nil
	}
	outcomes, err := ledgerOutcomes(s.c)
	if err != nil {
		return nil, err
	}
	rules, err := exclusions(s.c)
	if err != nil {
		return nil, err
	}
	unread, err := ledgerUnread(s.c)
	if err != nil {
		return nil, err
	}
	isUnread := make(map[string]bool, len(unread))
	for _, cm := range unread {
		isUnread[cm.EvidenceID] = true
	}
	nums := commentNumbers(s.c)
	all := caseCommentsAll(s.c)
	out := make([]LedgerComment, 0, len(all))
	for _, cm := range all {
		name, err := evidenceName(s.c, cm.EvidenceID)
		if err != nil {
			return nil, err
		}
		o := outcomes[cm.EvidenceID]
		switch {
		case len(rules) > 0 && ExcludedBy(rules, cm) != nil:
			o = OutcomeLeftOut
		case o != "":
		case isUnread[cm.EvidenceID]:
			o = OutcomeUnread
		default:
			o = OutcomeEarlier
		}
		cm.Body, cm.Raw = "", ""
		out = append(out, LedgerComment{Number: nums[cm.ID], Name: name, Head: cm, Outcome: o})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// ledgerOutcomes are what the ledger made of each comment it read, by
// evidence ID.
func ledgerOutcomes(c *openCase) (map[string]string, error) {
	rows, err := c.st.DB().Query(`SELECT evidence_id, outcome FROM ledger_read WHERE investigation_id = ?`, c.id)
	if err != nil {
		return nil, fmt.Errorf("the comments the ledger read: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, o string
		if err := rows.Scan(&id, &o); err != nil {
			return nil, fmt.Errorf("the comments the ledger read: %w", err)
		}
		out[id] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the comments the ledger read: %w", err)
	}
	return out, nil
}

// ledgerViewIntro opens the engineer's view of the ledger.
const ledgerViewIntro = "A model's reading of the case's comments: what each asks, commits to, reports or provides, and what later comments made of it. Not verified and never evidence."

// RenderLedger is the case ledger as the engineer is shown it: the open
// asks grouped by who is awaited, the pending commitments grouped by side,
// then every comment in the case's order under its number, author, who they
// are, visibility, time in loc and evidence name, with compact-tag items
// ([ask], [commit], [report], [provide]) and their statuses in trailing
// parentheses; a comment that gave none is one line saying why. Items the
// engineer marked wrong are left out. Nothing is left out for length.
func RenderLedger(comments []LedgerComment, items []LedgerItem, loc *time.Location, supportName, supportRole string) string {
	supportName = strings.Join(strings.Fields(supportName), " ")
	supportRole = strings.Join(strings.Fields(supportRole), " ")
	var awaitCustomer, awaitSupport, commitSupport, commitCustomer []LedgerItem
	byNumber := map[int][]LedgerItem{}
	settles := map[int][]string{}
	var unnumbered []LedgerItem
	for _, it := range items {
		if it.Corrected == "wrong" {
			continue
		}
		switch {
		case it.Open() && it.Kind == ledger.Asks && it.Who == AuthorSupport:
			awaitCustomer = append(awaitCustomer, it)
		case it.Open() && it.Kind == ledger.Asks:
			awaitSupport = append(awaitSupport, it)
		case it.Open() && it.Who == AuthorSupport:
			commitSupport = append(commitSupport, it)
		case it.Open():
			commitCustomer = append(commitCustomer, it)
		}
		if it.StatusByNumber > 0 {
			settles[it.StatusByNumber] = append(settles[it.StatusByNumber], settledRef(it))
		}
		if it.Number == 0 {
			unnumbered = append(unnumbered, it)
			continue
		}
		byNumber[it.Number] = append(byNumber[it.Number], it)
	}
	var b strings.Builder
	b.WriteString("# Case ledger\n\n" + ledgerViewIntro + "\n")
	if len(awaitCustomer)+len(awaitSupport) > 0 {
		b.WriteString("\n## Open asks\n")
		for _, g := range []struct {
			who   string
			items []LedgerItem
		}{{"customer", awaitCustomer}, {supportName, awaitSupport}} {
			if len(g.items) == 0 {
				continue
			}
			b.WriteString("\nAwaiting " + g.who + ":\n")
			for _, it := range g.items {
				b.WriteString(viewLine(it, true, supportName) + "\n")
			}
		}
	}
	if len(commitSupport)+len(commitCustomer) > 0 {
		b.WriteString("\n## Pending commitments\n")
		for _, g := range []struct {
			who   string
			items []LedgerItem
		}{{supportName, commitSupport}, {"customer", commitCustomer}} {
			if len(g.items) == 0 {
				continue
			}
			b.WriteString("\nBy " + g.who + ":\n")
			for _, it := range g.items {
				b.WriteString(viewLine(it, true, supportName) + "\n")
			}
		}
	}
	if len(comments)+len(unnumbered) == 0 {
		b.WriteString("\nThe case has no comments.\n")
		return b.String()
	}
	// One blank row after the heading, whichever kind of comment is first:
	// a comment with items begins with its own.
	b.WriteString("\n## Comments\n")
	gap := true
	for _, cm := range comments {
		head := strings.Join(append(append([]string{commentNumber(cm.Number)}, commentFacts(cm.Head, loc, supportRole)...), cm.Name), " · ")
		its := byNumber[cm.Number]
		if len(its) == 0 {
			if gap {
				b.WriteString("\n")
				gap = false
			}
			b.WriteString(head + " · " + outcomeShown(cm.Outcome, settles[cm.Number]) + "\n")
			continue
		}
		b.WriteString("\n### " + head + "\n\n")
		for _, it := range its {
			b.WriteString(viewLine(it, false, supportName) + "\n")
		}
		if s := settles[cm.Number]; len(s) > 0 {
			b.WriteString("- settles " + strings.Join(s, ", ") + "\n")
		}
		gap = true
	}
	if len(unnumbered) > 0 {
		b.WriteString("\n### Comments no longer in the case\n\n")
		for _, it := range unnumbered {
			b.WriteString(viewLine(it, true, supportName) + "\n")
		}
	}
	return b.String()
}

// settledRef describes an earlier item settled by a later comment without
// exposing internal lN identifiers.
func settledRef(it LedgerItem) string {
	noun := kindNoun(it.Kind)
	status := strings.ReplaceAll(it.Status, "_", " ")
	if ref := commentRef(it.Number, it.Comment); ref != "" {
		return ref + " " + noun + " (" + status + ")"
	}
	return noun + " (" + status + ")"
}

func kindNoun(kind string) string {
	switch kind {
	case ledger.Asks:
		return "ask"
	case ledger.Commits:
		return "commit"
	case ledger.Reports:
		return "report"
	case ledger.Provides:
		return "provide"
	}
	return kind
}

// outcomeShown says in a few words why a comment gave no item.
func outcomeShown(o string, settled []string) string {
	switch {
	case o == OutcomeChanges && len(settled) > 0:
		return "settles " + strings.Join(settled, ", ")
	case o == OutcomeChanges:
		return "changed earlier items"
	case strings.HasPrefix(o, "none: "):
		return "nothing: " + strings.ReplaceAll(strings.TrimPrefix(o, "none: "), "_", " ")
	case o == OutcomeUnaccounted:
		return "not accounted for by the ledger's model"
	case o == OutcomeNotShown:
		return "not read: too long to show the ledger's model"
	case o == OutcomeUnread:
		return "not read yet"
	case o == OutcomeLeftOut:
		return "left out by an exclusion rule"
	case o == OutcomeEarlier:
		return "read as an earlier version"
	case o == OutcomeItems:
		return "its items were withdrawn or marked wrong"
	}
	return o
}

// viewLine is an item as the engineer's view shows it: compact tag ([ask],
// [commit], [report], [provide]), no lN ID or per-line actor, cleaned of
// duplicate leading verbs, with comment reference or settled status in
// trailing parentheses.
func viewLine(it LedgerItem, withComment bool, supportName string) string {
	var b strings.Builder
	b.WriteString("- [" + kindNoun(it.Kind) + "] ")
	body := cleanLedgerText(it.Text, supportName)
	switch {
	case it.Attachment != "" && body != "":
		b.WriteString(it.Attachment + ": " + body)
	case it.Attachment != "":
		b.WriteString(it.Attachment)
	default:
		b.WriteString(body)
	}
	if it.Due != "" {
		b.WriteString(" (due " + strings.Join(strings.Fields(it.Due), " ") + ")")
	}
	if withComment {
		if ref := commentRef(it.Number, it.Comment); ref != "" {
			b.WriteString(" (" + ref + ")")
		}
	}
	if it.Status != "" && !it.Open() {
		b.WriteString(" (" + strings.ReplaceAll(it.Status, "_", " ") + statusInShown(it) + ")")
	}
	if it.Corrected != "" {
		b.WriteString(" [corrected by the engineer]")
	}
	return b.String()
}

// cleanLedgerText collapses whitespace and strips redundant leading actor and
// kind-verb prefixes ("asks why...", "reports no...", "customer reports: ...")
// from stored ledger item text.
func cleanLedgerText(raw, supportName string) string {
	s := strings.Join(strings.Fields(raw), " ")
	if s == "" {
		return ""
	}
	actors := []string{"the customer ", "customer ", "support "}
	if supportName != "" {
		actors = append([]string{supportName + " "}, actors...)
	}
	for {
		changed := false
		lower := strings.ToLower(s)
		trimmedActor := s
		for _, a := range actors {
			if al := strings.ToLower(a); strings.HasPrefix(lower, al) {
				trimmedActor = strings.TrimSpace(s[len(a):])
				break
			}
		}
		for _, candidate := range []string{trimmedActor, s} {
			cl := strings.ToLower(candidate)
			for _, prefix := range []string{
				"asks: ", "asks ",
				"reports that ", "reports: ", "reports ",
				"commits to ", "commits: ",
				"provides: ", "provides ",
			} {
				if strings.HasPrefix(cl, prefix) {
					if rest := strings.TrimSpace(candidate[len(prefix):]); rest != "" {
						s, changed = rest, true
						break
					}
				}
			}
			if changed {
				break
			}
		}
		if !changed {
			return s
		}
	}
}

// statusInShown formats who or which comment settled an item for trailing
// parentheticals such as "(answered in c#3)".
func statusInShown(it LedgerItem) string {
	s := ""
	switch {
	case it.StatusByID == "engineer":
		s = " by the engineer"
	case it.StatusBy != "":
		s = " in " + commentRef(it.StatusByNumber, it.StatusBy)
	}
	if it.StatusNote != "" {
		s += ": " + strings.Join(strings.Fields(it.StatusNote), " ")
	}
	return s
}

// commentRef names a comment by its number in the case, else by its
// evidence name.
func commentRef(n int, name string) string {
	if n > 0 {
		return commentNumber(n)
	}
	return name
}

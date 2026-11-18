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
// asks by who owes them and the pending commitments, then every comment
// in the case's order under its number, author, who they are, visibility,
// time in loc and evidence name, with its items and their statuses; a
// comment that gave none is one line saying why. Items the engineer marked
// wrong are left out. Nothing is left out for length.
func RenderLedger(comments []LedgerComment, items []LedgerItem, loc *time.Location, supportName, supportRole string) string {
	var owedCustomer, owedSupport, pending []LedgerItem
	byNumber := map[int][]LedgerItem{}
	settles := map[int][]string{}
	var unnumbered []LedgerItem
	for _, it := range items {
		if it.Corrected == "wrong" {
			continue
		}
		switch {
		case it.Open() && it.Kind == ledger.Asks && it.Who == AuthorSupport:
			owedCustomer = append(owedCustomer, it)
		case it.Open() && it.Kind == ledger.Asks:
			owedSupport = append(owedSupport, it)
		case it.Open():
			pending = append(pending, it)
		}
		if it.StatusByNumber > 0 {
			settles[it.StatusByNumber] = append(settles[it.StatusByNumber], it.ID+" "+strings.ReplaceAll(it.Status, "_", " "))
		}
		if it.Number == 0 {
			unnumbered = append(unnumbered, it)
			continue
		}
		byNumber[it.Number] = append(byNumber[it.Number], it)
	}
	var b strings.Builder
	b.WriteString("# Case ledger\n\n" + ledgerViewIntro + "\n")
	if len(owedCustomer)+len(owedSupport) > 0 {
		b.WriteString("\n## Open asks\n")
		for _, g := range []struct {
			who   string
			items []LedgerItem
		}{{"the customer", owedCustomer}, {supportName, owedSupport}} {
			if len(g.items) == 0 {
				continue
			}
			b.WriteString("\nOwed by " + g.who + ":\n")
			for _, it := range g.items {
				b.WriteString(viewLine(it, true, supportName) + "\n")
			}
		}
	}
	if len(pending) > 0 {
		b.WriteString("\n## Pending commitments\n\n")
		for _, it := range pending {
			b.WriteString(viewLine(it, true, supportName) + "\n")
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

// viewLine is an item as the engineer's view shows it, its comment by
// number where whose, its status said where it has one.
func viewLine(it LedgerItem, whose bool, supportName string) string {
	var b strings.Builder
	b.WriteString("- " + it.ID)
	if whose {
		b.WriteString(" · " + commentRef(it.Number, it.Comment))
	}
	b.WriteString(" · " + whoShown(it.Who, supportName) + " " + it.Kind)
	if it.Attachment != "" {
		b.WriteString(" " + it.Attachment)
	}
	if it.Due != "" {
		b.WriteString(", due " + it.Due)
	}
	if it.Status != "" && !it.Open() && it.Status != ledger.Superseded {
		b.WriteString(", " + strings.ReplaceAll(it.Status, "_", " ") + statusByShown(it))
	}
	b.WriteString(": " + strings.Join(strings.Fields(it.Text), " "))
	if it.Status == ledger.Superseded {
		b.WriteString(" (superseded" + statusByShown(it) + ")")
	}
	if it.Corrected != "" {
		b.WriteString(" [corrected by the engineer]")
	}
	return b.String()
}

// statusByShown is what settled an item, its comment by number.
func statusByShown(it LedgerItem) string {
	s := ""
	switch {
	case it.StatusByID == "engineer":
		s = " by the engineer"
	case it.StatusBy != "":
		s = " by " + commentRef(it.StatusByNumber, it.StatusBy)
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

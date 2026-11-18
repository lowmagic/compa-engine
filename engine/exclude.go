package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/expert"
)

// Exclusion kinds: one comment, every comment by an author, a comment and
// every one older, the private or the public ones, those by support, by
// the customer or by authors of any other kind the source reports, or all
// of them; and keep, which reads one comment whatever else would leave it
// out.
const (
	ExcludeComment  = "comment"
	ExcludeAuthor   = "author"
	ExcludeOlder    = "older"
	ExcludePrivate  = "private"
	ExcludePublic   = "public"
	ExcludeSupport  = "support"
	ExcludeCustomer = "customer"
	ExcludeKind     = "kind"
	ExcludeAll      = "all"
	KeepComment     = "keep"
)

// Exclusion is a rule of the engineer's leaving case comments out of what
// models read: the case block and the evidence tools. Value is the
// comment ID for comment and older, the author for author, the author
// kind for kind.
type Exclusion struct {
	ID, Kind, Value string
	// Before is, for older, the excluded comment's time: it and every
	// comment no newer are left out.
	Before    time.Time
	CreatedAt time.Time
}

// CaseComment is a comment as the engineer chooses among them. Author is
// the author as the evidence names them, "name (ID)" where the source gave
// both, which the author rule matches; AuthorName and AuthorID are its
// parts. Body is trimmed; Raw is the body as stored, whose lines the
// digest's spans count.
type CaseComment struct {
	EvidenceID, ID, Author, AuthorKind, Visibility string
	AuthorName, AuthorID                           string
	Automated                                      bool
	Created, Updated                               time.Time
	Body, Raw                                      string
}

// Private reports a comment only support sees; a partner's counts as
// public, since the customer's side sees it.
func (cm CaseComment) Private() bool { return cm.Visibility == string(VisibilityPrivate) }

// commentOf reads a comment's fields from its evidence.
func commentOf(evidenceID string, content []byte) CaseComment {
	cm := CaseComment{EvidenceID: evidenceID}
	head, body, _ := strings.Cut(string(content), "\n\n")
	cm.Body, cm.Raw = strings.TrimSpace(body), body
	for _, l := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(l, ": ")
		switch k {
		case "comment":
			cm.ID = v
		case "author":
			cm.Author = v
		case "author kind":
			cm.AuthorKind = v
		case "automated":
			cm.Automated = v == "yes"
		case "visibility":
			cm.Visibility = v
		case "created":
			cm.Created, _ = time.Parse(time.RFC3339, v)
		case "updated":
			cm.Updated, _ = time.Parse(time.RFC3339, v)
		}
	}
	cm.AuthorName, cm.AuthorID = SplitParty(cm.Author)
	return cm
}

// SplitParty takes a party as text.party wrote it apart: "name (ID)" where
// both were given, else the one that was. A name alone that ends in a
// parenthesis reads as a name and an ID; the owner mark, which compares
// IDs, then fails to match, which is the safe way to be wrong.
func SplitParty(s string) (name, id string) {
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		return s[:i], s[i+2 : len(s)-1]
	}
	return s, ""
}

// sameAuthor compares two authors by their IDs where both have one, since
// a source may write a name otherwise later, as one's decoding of
// escaped names did, and by the whole text otherwise.
func sameAuthor(a, b string) bool {
	_, ida := SplitParty(a)
	_, idb := SplitParty(b)
	return a == b || ida != "" && ida == idb
}

// ExcludedBy is the first rule leaving a comment out, or nil: none when
// the engineer keeps it. A comment whose author kind the source does not
// give is left out by neither the support rule nor the customer one.
func ExcludedBy(rules []Exclusion, cm CaseComment) *Exclusion {
	for _, r := range rules {
		if r.Kind == KeepComment && r.Value == cm.ID {
			return nil
		}
	}
	for i, r := range rules {
		var out bool
		switch r.Kind {
		case ExcludeComment:
			out = cm.ID != "" && cm.ID == r.Value
		case ExcludeAuthor:
			out = cm.Author != "" && sameAuthor(cm.Author, r.Value)
		case ExcludePrivate:
			out = cm.Private()
		case ExcludePublic:
			out = !cm.Private()
		case ExcludeSupport:
			out = cm.AuthorKind == AuthorSupport
		case ExcludeCustomer:
			out = cm.AuthorKind == AuthorCustomer
		case ExcludeKind:
			out = cm.AuthorKind != "" && cm.AuthorKind == r.Value
		case ExcludeAll:
			out = true
		case ExcludeOlder:
			out = cm.ID == r.Value || !cm.Created.IsZero() && !cm.Created.After(r.Before)
		}
		if out {
			return &rules[i]
		}
	}
	return nil
}

// KeptBy is the rule by which the engineer keeps a comment whatever else,
// or nil.
func KeptBy(rules []Exclusion, id string) *Exclusion {
	for i, r := range rules {
		if r.Kind == KeepComment && r.Value == id {
			return &rules[i]
		}
	}
	return nil
}

func exclusions(c *openCase) ([]Exclusion, error) {
	// Rules in the order they were made: by rowid, which grows with each
	// insert, not by created_at, whose RFC 3339 text drops a fraction's
	// trailing zeros and so does not sort as time does.
	rows, err := c.st.DB().Query(`SELECT id, kind, value, created_at FROM comment_exclusions ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Exclusion
	for rows.Next() {
		var r Exclusion
		var at string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Value, &at); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// An older rule takes its time from its comment.
	for i, r := range out {
		if r.Kind == ExcludeOlder {
			for _, cm := range caseCommentsAll(c) {
				if cm.ID == r.Value {
					out[i].Before = cm.Created
				}
			}
		}
	}
	return out, nil
}

// caseCommentsAll are the case's comments as models are given them —
// each the version the latest acquisition holds — newest first, excluded
// or not.
func caseCommentsAll(c *openCase) []CaseComment {
	latest, order, err := caseCurrent(c)
	if err != nil {
		return nil
	}
	out := make([]CaseComment, 0, len(order))
	for _, key := range order {
		id := latest[key]
		if full, err := c.ev.Get(id); err == nil {
			out = append(out, commentOf(id, full.Content))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// excludeComments records a rule. A rule already held is not
// recorded twice.
func excludeComments(c *openCase, kind, value string) (Exclusion, error) {
	switch kind {
	case ExcludeComment, ExcludeOlder, ExcludeAuthor, ExcludeKind, KeepComment:
		if strings.TrimSpace(value) == "" {
			return Exclusion{}, fmt.Errorf("excluding by %s needs a value", kind)
		}
	case ExcludePrivate, ExcludePublic, ExcludeSupport, ExcludeCustomer, ExcludeAll:
		value = ""
	default:
		return Exclusion{}, fmt.Errorf("no exclusion %q", kind)
	}
	rules, err := exclusions(c)
	if err != nil {
		return Exclusion{}, err
	}
	for _, r := range rules {
		if r.Kind == kind && r.Value == value {
			return r, nil
		}
	}
	r := Exclusion{ID: c.newID("exclude"), Kind: kind, Value: value, CreatedAt: c.now().UTC()}
	if _, err := c.st.DB().Exec(`INSERT INTO comment_exclusions (id, kind, value, created_at) VALUES (?, ?, ?, ?)`, r.ID, r.Kind, r.Value, r.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return Exclusion{}, err
	}
	return r, nil
}

// includeComments removes a rule.
func includeComments(c *openCase, ruleID string) error {
	rules, err := exclusions(c)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.ID == ruleID {
			if _, err := c.st.DB().Exec(`DELETE FROM comment_exclusions WHERE id = ?`, ruleID); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("no exclusion %s", ruleID)
}

// Describe says what a rule leaves out, using the deployment's support name.
func (r Exclusion) Describe(supportName string) string {
	switch r.Kind {
	case ExcludeComment:
		return "comment " + r.Value
	case ExcludeAuthor:
		return "every comment by " + r.Value
	case ExcludePrivate:
		return "every private comment"
	case ExcludePublic:
		return "every public comment"
	case ExcludeSupport:
		return "every comment by " + supportName
	case ExcludeCustomer:
		return "every comment by the customer"
	case ExcludeKind:
		return "every comment whose author kind is " + r.Value
	case ExcludeAll:
		return "every comment"
	case KeepComment:
		return "nothing: comment " + r.Value + " is kept whatever else leaves it out"
	case ExcludeOlder:
		return "comment " + r.Value + " and every one older"
	}
	return r.Kind
}

// withdrawn reports evidence the engineer has left out of what models
// read: every stored version of a case comment the rules leave out. A
// conversation that disclosed one is begun again.
func withdrawn(c *openCase) func(string) bool {
	rules, err := exclusions(c)
	if err != nil || len(rules) == 0 {
		return func(string) bool { return false }
	}
	all, err := c.ev.List()
	if err != nil {
		return func(string) bool { return false }
	}
	out := map[string]bool{}
	for _, it := range all {
		if it.Ref.Type != TypeCaseComment {
			continue
		}
		if full, err := c.ev.Get(it.Ref.ID); err == nil && ExcludedBy(rules, commentOf(it.Ref.ID, full.Content)) != nil {
			out[it.Ref.ID] = true
		}
	}
	// What the engine's evidence tools read from an item left out rests on
	// it, and is left out with it. Items are listed oldest first, so what
	// was read from such a result is found too.
	for _, it := range all {
		if !strings.HasPrefix(it.Ref.Provenance.Source, expert.ReservedTestPrefix) {
			continue
		}
		for _, src := range strings.Split(it.Location, "\n") {
			if out[src] {
				out[it.Ref.ID] = true
				break
			}
		}
	}
	return func(id string) bool { return out[id] }
}

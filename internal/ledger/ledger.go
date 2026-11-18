// Package ledger is the case ledger's model side: its update, the
// declaration of the one tool the ledger's model calls with it, its system
// prompt, and the validation of an update against what the request gave
// and what the ledger holds (docs/MODEL_API.md §The case ledger). The
// engine reads the comments, runs the call through the companion's loop,
// numbers the items and keeps them; a model only adds items and moves
// statuses, and what it writes is its reading, never evidence.
package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/lowmagic/compa-engine/internal/schema"
)

// Item kinds.
const (
	Asks     = "asks"
	Commits  = "commits"
	Reports  = "reports"
	Provides = "provides"
)

// Statuses. An ask is open until answered or ruled out, a commitment
// pending until done or dropped, and what was provided stands, with no
// status, until superseded; any item may be withdrawn by an edit of its
// own comment.
const (
	Open       = "open"
	Answered   = "answered"
	RuledOut   = "ruled_out"
	Pending    = "pending"
	Done       = "done"
	Dropped    = "dropped"
	Superseded = "superseded"
	Withdrawn  = "withdrawn"
	// none is an item's status as the model writes one with no status to
	// keep: a report, or what was provided.
	none = "none"
)

// Why a comment holds nothing to record.
const (
	NoneCourtesy   = "courtesy"
	NoneAutomated  = "automated"
	NoneRepeat     = "repeat"
	NoneNothingNew = "nothing_new"
)

// Update is one ledger call's answer: new items, changes to items the
// ledger holds, and the comments that hold nothing to record.
type Update struct {
	Items   []Item   `json:"items"`
	Updates []Change `json:"updates"`
	None    []None   `json:"none,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// Item is one thing a comment asks, commits to, reports or provides,
// citing its comment; Status and Because where a later comment among
// those given already settles or supersedes it. Comment and Because are
// evidence IDs once validated.
type Item struct {
	Kind       string `json:"kind"`
	Who        string `json:"who"`
	Text       string `json:"text"`
	Comment    string `json:"comment"`
	Due        string `json:"due,omitempty"`
	Attachment string `json:"attachment,omitempty"`
	Status     string `json:"status"`
	Because    string `json:"because,omitempty"`
}

// Change moves an item the ledger holds, by its ID, citing the later
// comment that moves it.
type Change struct {
	Item    string `json:"item"`
	Status  string `json:"status"`
	Because string `json:"because"`
	Note    string `json:"note,omitempty"`
}

// None is a comment that holds nothing to record, and why.
type None struct {
	Comment string `json:"comment"`
	Why     string `json:"why"`
}

// Entry is an item the ledger holds, as validation needs it.
type Entry struct {
	ID, Kind, Status, Comment string
}

// Rejected is an element of an update validation took out, and why.
type Rejected struct {
	Element, Reason string
}

func (d Rejected) String() string { return d.Element + ": " + d.Reason }

// Settles says whether a status settles an item of a kind: an ask is
// answered or ruled out, a commitment done or dropped, and what was
// provided superseded.
func Settles(kind, status string) bool {
	switch kind {
	case Asks:
		return status == Answered || status == RuledOut
	case Commits:
		return status == Done || status == Dropped
	case Provides:
		return status == Superseded
	}
	return false
}

// Unsettled is the status an item of a kind starts in: open for an ask,
// pending for a commitment, none for the rest.
func Unsettled(kind string) string {
	switch kind {
	case Asks:
		return Open
	case Commits:
		return Pending
	}
	return ""
}

// Context is what an update is validated against: the request's comments
// and the ledger so far.
type Context struct {
	// Names maps each name the request gave a comment, and each given
	// comment's own ID at the source, to its evidence ID.
	Names map[string]string
	// Written is each comment's time, by evidence ID: those given and those
	// the held items cite.
	Written map[string]time.Time
	// Held are the items the ledger holds that the request showed.
	Held []Entry
	// Earlier maps each given comment that is an edit to the versions of it
	// the ledger read before, by evidence ID.
	Earlier map[string][]string
}

// Validate checks a ledger_update call's arguments: against the schema,
// whole, or the update is refused with why; then each element against the
// request and the ledger. An item cites a comment the request gave; a
// status settles or supersedes only as its kind allows, citing a given
// comment written after the item's own; a change names a held item not
// settled yet, or withdraws one by an edited version of its own comment.
// An element failing is dropped with its reason, the rest kept; comments
// come back as evidence IDs.
func Validate(vc Context, args map[string]any) (Update, []Rejected, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return Update{}, nil, fmt.Errorf("the arguments: %w", err)
	}
	if err := schema.ValidateJSON(schema.LedgerUpdate, raw); err != nil {
		return Update{}, nil, err
	}
	var u Update
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		return Update{}, nil, fmt.Errorf("the arguments: %w", err)
	}
	var dropped []Rejected
	drop := func(el, format string, a ...any) {
		dropped = append(dropped, Rejected{Element: el, Reason: fmt.Sprintf(format, a...)})
	}
	given := func(ref string) (string, bool) {
		id, ok := vc.Names[ref]
		return id, ok
	}
	later := func(a, b string) bool {
		ta, okA := vc.Written[a]
		tb, okB := vc.Written[b]
		return okA && okB && tb.After(ta)
	}
	var items []Item
	for i, it := range u.Items {
		el := fmt.Sprintf("items[%d]", i)
		id, ok := given(it.Comment)
		if !ok {
			drop(el, "%s is no comment the request gave", it.Comment)
			continue
		}
		it.Comment = id
		if it.Attachment != "" && it.Kind != Provides {
			drop(el+".attachment", "a %s item names no attachment", it.Kind)
			it.Attachment = ""
		}
		// Unsettled — open, pending, none — the item keeps its kind's own
		// status.
		if it.Status == Open || it.Status == Pending || it.Status == none {
			if it.Because != "" {
				drop(el, "because without a settling status")
				continue
			}
			it.Status = Unsettled(it.Kind)
			items = append(items, it)
			continue
		}
		by, ok := given(it.Because)
		switch {
		case !Settles(it.Kind, it.Status):
			drop(el, "a %s item cannot be %s", it.Kind, it.Status)
			continue
		case !ok:
			drop(el, "%s is no comment the request gave", it.Because)
			continue
		case !later(it.Comment, by):
			drop(el, "%s was not written after %s", it.Because, it.Comment)
			continue
		}
		it.Because = by
		items = append(items, it)
	}
	held := map[string]Entry{}
	for _, e := range vc.Held {
		held[e.ID] = e
	}
	moved := map[string]bool{}
	var changes []Change
	for i, c := range u.Updates {
		el := fmt.Sprintf("updates[%d]", i)
		e, ok := held[c.Item]
		by, isGiven := given(c.Because)
		switch {
		case !ok:
			drop(el, "the ledger holds no item %s", c.Item)
			continue
		case moved[c.Item]:
			drop(el, "%s is moved twice", c.Item)
			continue
		case !isGiven:
			drop(el, "%s is no comment the request gave", c.Because)
			continue
		case e.Status == Withdrawn:
			drop(el, "%s is withdrawn", c.Item)
			continue
		case c.Status == Withdrawn:
			// Withdrawn only by an edited version of the item's own comment.
			if !slices.Contains(vc.Earlier[by], e.Comment) {
				drop(el, "%s is no edited version of %s's comment", c.Because, c.Item)
				continue
			}
		case !Settles(e.Kind, c.Status):
			drop(el, "%s is a %s item and cannot be %s", c.Item, e.Kind, c.Status)
			continue
		case e.Status != Unsettled(e.Kind):
			drop(el, "%s is already %s", c.Item, e.Status)
			continue
		case !later(e.Comment, by):
			drop(el, "%s was not written after %s's comment", c.Because, c.Item)
			continue
		}
		c.Because = by
		moved[c.Item] = true
		changes = append(changes, c)
	}
	gave := map[string]bool{}
	for _, it := range items {
		gave[it.Comment] = true
	}
	var nones []None
	for i, n := range u.None {
		el := fmt.Sprintf("none[%d]", i)
		id, ok := given(n.Comment)
		switch {
		case !ok:
			drop(el, "%s is no comment the request gave", n.Comment)
		case gave[id]:
			drop(el, "%s gave items", n.Comment)
		default:
			gave[id] = true
			n.Comment = id
			nones = append(nones, n)
		}
	}
	u.Items, u.Updates, u.None = items, changes, nones
	return u, dropped, nil
}

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/sources"
	"github.com/lowmagic/compa-engine/internal/store"
)

const (
	TypeCaseMetadata = "case_metadata"
	TypeCaseComment  = "case_comment"
)

// metaAccountNumber is the store's meta key for the case's account number
// as its source last reported it, empty where it reported none.
const metaAccountNumber = "account_number"

// openCase is a case's store, held until Close.
type openCase struct {
	id string
	// root is the case's artifact directory, empty when it has none.
	root string
	// chat is a troubleshooting chat with no case, its id the chat's.
	chat     bool
	storeDir string
	inHome   bool
	st       *store.Store
	ev       *evidence.Store
	src      *sources.Store
	// now and newID are the case's clock and ID maker; tests set them.
	now   func() time.Time
	newID func(prefix string) string
	// rootErr says why the case has no artifact directory, where it has
	// none.
	rootErr error
	// notes say what opening did to the case's stores, for the engineer.
	notes []string
	// reading orders the readings of the case made while it is open, as
	// its comments and its counts arrive apart; walked counts the
	// attachments its counts walked.
	reading sync.Mutex
	walked  int
}

func (c *openCase) Close() error {
	return c.st.Close()
}

// errNotOpened is returned for a case that has never been opened, so a
// command that only reads does not create a store.
var errNotOpened = errors.New("has not been opened")

// open resolves where a case's store lives and opens it. The store goes in
// the case's artifact directory, the case folder where there is one,
// and under the home directory when there is none. With create
// false, a case with no store yet is errNotOpened.
func (e *Engine) open(ctx context.Context, id string, create bool) (*openCase, error) {
	root, err := e.artifacts.Root(ctx, id)
	var rootErr error
	if errors.Is(err, ErrNoArtifacts) {
		root, rootErr, err = "", err, nil
	}
	if err != nil {
		return nil, err
	}
	dir, inHome, err := store.Location(root, e.home, id)
	if err != nil {
		return nil, err
	}
	var notes []string
	if !inHome {
		if notes, err = e.adoptHomeStore(id, dir); err != nil {
			return nil, err
		}
	}
	if !create {
		if _, err := os.Stat(filepath.Join(dir, "case.db")); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("case %s %w; run compa case open %s", id, errNotOpened, id)
		}
	}
	st, err := store.Open(dir, id, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("case %s: %w", id, err)
	}
	c := newOpenCase(id, root, dir, inHome, st)
	c.rootErr, c.notes = rootErr, notes
	return c, nil
}

// adoptHomeStore moves a case's home store into its case folder, when the
// folder has appeared since the case was opened without it, so the work
// done then is not split from the work to come, nor swept away later. If
// the folder already holds a store, the two are not merged: the home one
// is kept from the sweep while the case is opened, and the engineer told.
func (e *Engine) adoptHomeStore(id, dir string) ([]string, error) {
	home, _, err := store.Location("", e.home, id)
	if err != nil {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(home, "case.db")); err != nil {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "case.db")); err == nil {
		note := fmt.Sprintf("an earlier store for this case is in %s, from before its folder was found; it is not merged, and is kept while the case is opened", home)
		if err := store.Keep(home, id); err != nil {
			note += fmt.Sprintf(" (marking it used: %v)", err)
		}
		return []string{note}, nil
	}
	if err := store.Move(home, dir, id, store.Options{}); err != nil {
		return nil, fmt.Errorf("case %s: moving its store from %s into the case folder: %w", id, home, err)
	}
	return []string{"moved the store made before the case folder was found from " + home}, nil
}

func newOpenCase(id, root, dir string, inHome bool, st *store.Store) *openCase {
	return &openCase{id: id, root: root, storeDir: dir, inHome: inHome, st: st, ev: evidence.New(st), src: sources.New(st),
		now: func() time.Time { return time.Now().UTC() }, newID: newID}
}

// idSeq makes IDs made in the same nanosecond distinct: background work
// and the engineer's own make IDs at once.
var idSeq atomic.Int64

// newID makes an ID with prefix, unique within the process and ordered by
// when it was made.
func newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), idSeq.Add(1))
}

type acquired struct {
	total int
	added []string
	// artifacts is what the counts found; nil when the case has no
	// artifact directory or nothing has counted it yet.
	artifacts *overviewTotals
	// atts are the attachments the artifact source reported.
	atts []Attachment
}

func (a *acquired) and(b acquired) {
	a.total += b.total
	a.added = append(a.added, b.added...)
	if b.artifacts != nil {
		a.artifacts = b.artifacts
	}
}

// acquire reads the whole case at once, as compa case open does: its details
// and attachment list, its comments, and its artifacts' counts.
func (e *Engine) acquire(ctx context.Context, c *openCase, kase Case, comments []Comment) (acquired, error) {
	a, err := e.readList(ctx, c, kase)
	if err != nil {
		return a, err
	}
	b, err := e.readComments(c, kase, comments)
	if err != nil {
		return a, err
	}
	a.and(b)
	n, err := e.readCounts(ctx, c, a.atts)
	a.and(n)
	return a, err
}

// readList is the reading of a case made before anything else: its
// details, and an overview of its attachments as the artifact source
// reports them, which costs no walk. The comments the last reading found
// are carried forward until the fresh ones are read, so the case never
// seems to have lost them. Each item's ID derives from its content and
// origin, so an unchanged case adds nothing and a changed one is a new item
// beside the old. What it found is recorded as the acquisition, which says
// what is current: an earlier version coming back adds no evidence, and a
// comment or attachment gone adds nothing at all. A case with no artifact
// directory has no attachments.
func (e *Engine) readList(ctx context.Context, c *openCase, kase Case) (acquired, error) {
	c.reading.Lock()
	defer c.reading.Unlock()
	var a acquired
	items := []evidence.Item{caseItem(e.cases.Name(), kase)}
	var now sources.Acquisition
	if c.root != "" {
		now.Origin = e.artifacts.Name()
		atts, err := e.attachments(ctx, c.id)
		if err != nil {
			return a, err
		}
		if err := recordAttachments(c.src, e.artifacts.Name(), atts); err != nil {
			return a, err
		}
		for _, at := range atts {
			now.Attachments = append(now.Attachments, at.Name)
		}
		items, a.atts = append(items, overview(e.artifacts.Name(), c.id, atts)), atts
	}
	if err := a.store(c, items); err != nil {
		return a, err
	}
	// The account number is kept for the barred-accounts check alone, so
	// it is not in the case's evidence, which models read.
	if err := c.st.SetMeta(ctx, metaAccountNumber, kase.Account.Number); err != nil {
		return a, err
	}
	kept, err := readingOf(c, TypeCaseComment, TypeArtifactCounts, TypeArtifactInventory)
	if err != nil {
		return a, err
	}
	for _, it := range items {
		now.Evidence = append(now.Evidence, it.Ref.ID)
	}
	now.Evidence = append(now.Evidence, kept...)
	return a, c.src.Acquired(now)
}

// readComments is the reading that adds the case's comments as its source
// gives them now, in place of those the last reading found.
func (e *Engine) readComments(c *openCase, kase Case, comments []Comment) (acquired, error) {
	var items []evidence.Item
	for _, cm := range comments {
		items = append(items, commentItem(e.cases.Name(), kase.ID, cm))
	}
	return replaceInReading(c, TypeCaseComment, items)
}

// readCounts counts the attachments' files and bytes, walking only those
// the last count did not count as the source reports them now, and records
// the counts as the case's current ones. Cancelled, it records nothing.
func (e *Engine) readCounts(ctx context.Context, c *openCase, atts []Attachment) (acquired, error) {
	if c.root == "" {
		return acquired{}, nil
	}
	prev := map[string]counted{}
	if ids, err := readingOf(c, TypeArtifactCounts); err == nil && len(ids) > 0 {
		if it, err := c.ev.Get(ids[0]); err == nil {
			prev = countsOf(string(it.Content))
		}
	}
	confined := func(dir string) bool { return realDir(c.root, dir) }
	it, tot, walked, err := artifactCounts(ctx, os.DirFS(c.root), confined, e.artifacts.Name(), c.id, atts, prev)
	if err != nil {
		return acquired{}, err
	}
	c.reading.Lock()
	c.walked += walked
	c.reading.Unlock()
	a, err := replaceInReading(c, TypeArtifactCounts, []evidence.Item{it})
	a.artifacts = &tot
	return a, err
}

// store adds the items the store does not hold yet.
func (a *acquired) store(c *openCase, items []evidence.Item) error {
	for _, it := range items {
		a.total++
		if c.ev.Exists(it.Ref.ID) {
			continue
		}
		if err := c.ev.Add(it); err != nil {
			return err
		}
		a.added = append(a.added, it.Ref.ID)
	}
	return nil
}

// replaceInReading records a reading of the case that is the latest with
// its items of one type replaced by these.
func replaceInReading(c *openCase, typ string, items []evidence.Item) (acquired, error) {
	c.reading.Lock()
	defer c.reading.Unlock()
	var a acquired
	if err := a.store(c, items); err != nil {
		return a, err
	}
	last, ok, err := c.src.Latest()
	if err != nil {
		return a, err
	}
	types, err := evidenceTypes(c)
	if err != nil {
		return a, err
	}
	now := sources.Acquisition{Origin: last.Origin, Attachments: last.Attachments}
	if !ok {
		now = sources.Acquisition{}
	}
	for _, id := range last.Evidence {
		if types[id] != typ {
			now.Evidence = append(now.Evidence, id)
		}
	}
	for _, it := range items {
		now.Evidence = append(now.Evidence, it.Ref.ID)
	}
	return a, c.src.Acquired(now)
}

// readingOf is the latest reading's items of the given types, in order.
func readingOf(c *openCase, typs ...string) ([]string, error) {
	last, ok, err := c.src.Latest()
	if err != nil || !ok {
		return nil, err
	}
	types, err := evidenceTypes(c)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range last.Evidence {
		for _, t := range typs {
			if types[id] == t {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

func evidenceTypes(c *openCase) (map[string]string, error) {
	all, err := c.ev.List()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for _, it := range all {
		out[it.Ref.ID] = it.Ref.Type
	}
	return out, nil
}

func caseItem(source string, c Case) evidence.Item {
	var b fieldText
	b.field("case", c.ID)
	b.field("source id", c.SourceID)
	b.field("subject", c.Subject)
	b.field("status", c.Status)
	b.field("internal status", c.InternalStatus)
	if c.WaitingOnSupport {
		b.field("waiting on support", "yes")
	}
	b.field("severity", c.Severity)
	b.field("product", c.Product)
	b.field("product version", c.ProductVersion)
	b.field("cluster id", c.ClusterID)
	b.field("cluster version", c.ClusterVersion)
	b.field("routing keys", strings.Join(c.RoutingKeys, ", "))
	if len(c.Components) > 0 {
		j, _ := json.Marshal(c.Components)
		b.field("routing components", string(j))
	}
	if len(c.Tags) > 0 {
		j, _ := json.Marshal(c.Tags)
		b.field("routing tags", string(j))
	}
	b.party("account", c.Account)
	b.party("owner", c.Owner)
	b.time("created", c.Created)
	b.time("updated", c.Updated)
	b.body(c.Description)
	return item(TypeCaseMetadata, source, "case "+c.ID, b.String())
}

func commentItem(source, caseID string, cm Comment) evidence.Item {
	var b fieldText
	b.field("case", caseID)
	b.field("comment", cm.ID)
	b.party("author", cm.Author)
	b.field("author kind", cm.AuthorKind)
	// Written only when set, so a comment from a source that does not say
	// keeps the evidence ID it had before the field existed.
	if cm.Automated {
		b.field("automated", "yes")
	}
	b.field("visibility", string(cm.Visibility))
	b.time("created", cm.Created)
	b.time("updated", cm.Updated)
	b.body(cm.Body)
	return item(TypeCaseComment, source, "case "+caseID+" comment "+cm.ID, b.String())
}

func item(typ, source, location, content string) evidence.Item {
	h := sha256.New()
	for _, s := range []string{typ, source, location, content} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return evidence.Item{
		Ref: evidence.Ref{
			ID:         "ev-" + hex.EncodeToString(h.Sum(nil))[:16],
			Type:       typ,
			Provenance: evidence.Provenance{Source: source, CollectedBy: "engine"},
		},
		Location: location,
		Size:     *evidence.SizeOf(content, false),
		Content:  []byte(content),
	}
}

// fieldText renders evidence as "key: value" lines, then a blank line and the
// body: fewer tokens than JSON, and readable as it is. Empty fields are
// left out, so a source that lacks one does not add noise.
type fieldText struct{ strings.Builder }

func (t *fieldText) field(k, v string) {
	if v != "" {
		// A value is one line; a newline in it would read as another field.
		fmt.Fprintf(t, "%s: %s\n", k, strings.ReplaceAll(v, "\n", " "))
	}
}

func (t *fieldText) party(k string, p Party) {
	switch {
	case p.Name != "" && p.ID != "":
		t.field(k, p.Name+" ("+p.ID+")")
	default:
		t.field(k, p.Name+p.ID)
	}
}

func (t *fieldText) time(k string, v time.Time) {
	if !v.IsZero() {
		t.field(k, v.UTC().Format(time.RFC3339))
	}
}

func (t *fieldText) body(s string) {
	if s != "" {
		t.WriteString("\n" + s)
		if !strings.HasSuffix(s, "\n") {
			t.WriteString("\n")
		}
	}
}

// caseCurrent is what the case is now: the latest metadata and artifact
// overview, by type, and its comments, oldest first, each the version the
// latest acquisition holds — by location in a store from before
// acquisitions were recorded. latest maps each type and each comment to
// its evidence.
func caseCurrent(c *openCase) (latest map[string]string, comments []string, err error) {
	all, err := c.ev.List()
	if err != nil {
		return nil, nil, err
	}
	latest = map[string]string{}
	acq, ok, err := c.src.Latest()
	if err != nil {
		return nil, nil, err
	}
	if ok {
		// The latest acquisition says what the case is now.
		types := map[string]string{}
		for _, it := range all {
			types[it.Ref.ID] = it.Ref.Type
		}
		for _, id := range acq.Evidence {
			switch t := types[id]; t {
			case TypeCaseMetadata, TypeArtifactOverview:
				latest[t] = id
			case TypeCaseComment:
				comments = append(comments, id)
				latest[id] = id
			}
		}
	} else {
		// A store from before acquisitions were recorded: oldest first,
		// so the last item at a location is its latest version.
		for _, it := range all {
			switch it.Ref.Type {
			case TypeCaseMetadata, TypeArtifactOverview:
				latest[it.Ref.Type] = it.Ref.ID
			case TypeCaseComment:
				if _, ok := latest[it.Location]; !ok {
					comments = append(comments, it.Location)
				}
				latest[it.Location] = it.Ref.ID
			}
		}
	}
	return latest, comments, nil
}

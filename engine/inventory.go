package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	apischema "github.com/lowmagic/compa-engine/internal/schema"
)

// The artifact inventory:
// each attachment with the captures it holds, their targets and capture
// times, and their currency, made without a model. Capture identity is
// the catalogs' knowledge: a catalog's capture kind may name an identity
// tool, which the engine runs through the runner over each attachment
// holding the kind's marks. The engine only applies the rules of
// currency to what the tools return.

// TypeArtifactInventory is the inventory, one item, made again only when
// an attachment as its source reports it or the installed identity tools
// change.
const TypeArtifactInventory = "artifact_inventory"

// A capture's currency.
const (
	// CaptureCurrent is the latest capture of its kind and target.
	CaptureCurrent = "current"
	// CaptureOutdated is an earlier one: still evidence of what was true
	// when it was taken, never merged with the current one.
	CaptureOutdated = "outdated"
	// CaptureDuplicate is the same report extracted again.
	CaptureDuplicate = "duplicate"
	// CaptureUncertain is a capture whose identity or time could not be
	// established, or conflicts with another.
	CaptureUncertain = "uncertain"
)

// localTimeLayout is an identity's local_time: a capture time without a
// zone.
const localTimeLayout = "2006-01-02T15:04:05.999999999"

// Inventory is the case's attachments with the captures each holds.
type Inventory struct {
	// Evidence is the stored item's ID; empty for a case with no
	// artifacts.
	Evidence    string
	Attachments []InventoryAttachment
	// Omitted counts the attachments past the bound, not inventoried.
	Omitted int
}

// InventoryAttachment is one attachment as its source reports it, the
// capture kinds whose marks lie beneath its contents, the identity tools
// run over it and the captures they found.
type InventoryAttachment struct {
	Attachment Attachment
	Kinds      []string
	Runs       []IdentityRun
	Captures   []InventoryCapture
	// Note says why its contents were not looked in, where something
	// stopped it.
	Note string
}

// IdentityRun is one identity tool run over one attachment's directory,
// its record stored as evidence.
type IdentityRun struct {
	Kind string
	// Tool is the tool's full ID and version.
	Tool     string
	Evidence string
	Status   string
	Reason   string
}

// InventoryCapture is one capture an identity tool found, with its
// currency among the captures of its kind and target.
type InventoryCapture struct {
	Kind string
	// Capture is the capture's root or report directory, relative to the
	// artifact root; empty where the tool's output named none the
	// inventory could use.
	Capture    string
	TargetKind string
	Target     string
	// Time is the capture time as stated, with its offset; LocalTime is
	// one stated without a zone; RawTime is as the capture holds it.
	Time      string
	LocalTime string
	RawTime   string
	Problems  []string
	Currency  string
	// SupersededBy names the current capture an outdated one is older
	// than; DuplicateOf the capture a duplicate repeats.
	SupersededBy string
	DuplicateOf  string
	// Why says what makes a capture uncertain.
	Why string

	at   time.Time // parsed Time or LocalTime
	zone bool      // at is from Time
}

// ReadInventory identifies the captures in the attachments StartSession
// listed and records the inventory as the case's current one, the one the
// session's conversations are shown in place of the attachment overview.
// It runs no model: identity tools are collection, under the runner's
// sandbox, allowlist and limits, so a case barred from AI processing has
// one too. Cancelled, it records nothing.
func (s *Session) ReadInventory(ctx context.Context) (Inventory, error) {
	inv, err := s.e.readInventory(ctx, s.c, s.atts)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("the attachments could not be inventoried: %w", err)
		}
		return inv, err
	}
	s.mu.Lock()
	s.inv = &inv
	s.mu.Unlock()
	return inv, nil
}

// identityTool is an installed catalog's identity tool for one kind.
type identityTool struct {
	kind  string
	marks []string
	pkg   *expert.Package
	test  expert.Test
}

func (t identityTool) label() string { return t.test.ID + "@" + t.test.Version }

// identityTools are the identity tools of the highest installed version
// of each catalog, by kind and then tool.
func (e *Engine) identityTools() []identityTool {
	idx := e.experts().idx
	var out []identityTool
	seen := map[string]bool{}
	for _, d := range idx.List() {
		// List gives each ID's highest version first.
		if d.Kind != expert.KindCatalog || seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		entry, ok := idx.Describe(d.ID, d.Version)
		if !ok {
			continue
		}
		for _, c := range entry.Manifest.Spec.Captures {
			if c.Identity == "" {
				continue
			}
			if t, ok := entry.Test(d.ID + "." + c.Identity); ok {
				out = append(out, identityTool{kind: c.Kind, marks: c.Marks, pkg: entry.Package, test: t})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].test.ID < out[j].test.ID
	})
	return out
}

// identityToolsLine names the identity tools an inventory ran, so a later
// one can tell whether its runs stand.
func identityToolsLine(tools []identityTool) string {
	if len(tools) == 0 {
		return "none"
	}
	parts := make([]string, len(tools))
	for i, t := range tools {
		parts[i] = t.kind + " " + t.label()
	}
	return strings.Join(parts, ", ")
}

// readInventory runs each identity tool once over each attachment holding
// its kind's marks, reusing the runs of the last inventory for an
// attachment its source reports unchanged when the identity tools are
// unchanged too, applies the rules of currency, and records the result.
func (e *Engine) readInventory(ctx context.Context, c *openCase, atts []Attachment) (Inventory, error) {
	if c.root == "" {
		return Inventory{}, nil
	}
	tools := e.identityTools()
	toolsLine := identityToolsLine(tools)
	prev := map[string]inventoried{}
	if ids, err := readingOf(c, TypeArtifactInventory); err == nil && len(ids) > 0 {
		if it, err := c.ev.Get(ids[0]); err == nil {
			prev = inventoryOf(string(it.Content), toolsLine)
		}
	}
	var decl []expert.Capture
	for _, t := range tools {
		decl = append(decl, expert.Capture{Kind: t.kind, Marks: t.marks})
	}
	var inv Inventory
	for i, a := range atts {
		if i == maxOverviewAttachments {
			inv.Omitted = len(atts) - i
			break
		}
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		ia := InventoryAttachment{Attachment: a}
		switch {
		case a.Dir == "" || len(tools) == 0:
		case !realDir(c.root, a.Dir):
			ia.Note = "not looked in: the directory is reached through a link"
		default:
			outs, ok := reused(c, prev, a)
			if ok {
				ia.Kinds = prev[a.Name].kinds
			} else {
				var err error
				if ia.Kinds, outs, err = e.identify(ctx, c, a, tools, decl); err != nil {
					return Inventory{}, err
				}
			}
			for _, o := range outs {
				ia.Runs = append(ia.Runs, o.run)
				ia.Captures = append(ia.Captures, identities(a.Dir, o)...)
			}
		}
		inv.Attachments = append(inv.Attachments, ia)
	}
	applyCurrency(inv.Attachments)
	it := inventoryItem(e.artifacts.Name(), c.id, toolsLine, inv)
	if _, err := replaceInReading(c, TypeArtifactInventory, []evidence.Item{it}); err != nil {
		return Inventory{}, err
	}
	inv.Evidence = it.Ref.ID
	return inv, nil
}

// identityOutput is one identity run with what it wrote.
type identityOutput struct {
	run       IdentityRun
	output    []byte
	truncated bool
}

// identify looks for the kinds' marks beneath one attachment's directory,
// a bounded walk, and runs each marked kind's identity tool with the
// directory, storing each record as evidence as run_tool does.
func (e *Engine) identify(ctx context.Context, c *openCase, a Attachment, tools []identityTool, decl []expert.Capture) ([]string, []identityOutput, error) {
	have := captureKinds(filepath.Join(c.root, filepath.FromSlash(a.Dir)), decl)
	var kinds []string
	var outs []identityOutput
	for _, t := range tools {
		if !have[t.kind] {
			continue
		}
		if len(kinds) == 0 || kinds[len(kinds)-1] != t.kind {
			kinds = append(kinds, t.kind)
		}
		rec := e.runner().RunTest(ctx, c.root, t.pkg, t.test.ID, map[string]any{expert.IdentityParam: a.Dir})
		// A run the context ended is not the attachment's identity.
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := c.ev.Put(rec); err != nil {
			return nil, nil, err
		}
		outs = append(outs, identityOutput{
			run:    IdentityRun{Kind: t.kind, Tool: t.label(), Evidence: rec.ID, Status: rec.Status, Reason: rec.Reason},
			output: rec.Output, truncated: rec.Truncated,
		})
	}
	return kinds, outs, nil
}

// reused is the last inventory's runs over an attachment its source
// reports unchanged, each read back from its evidence; false when any is
// missing or did not complete, as one that timed out, so the attachment is
// identified again.
func reused(c *openCase, prev map[string]inventoried, a Attachment) ([]identityOutput, bool) {
	p, ok := prev[a.Name]
	if !ok || p.key != countKey(a) {
		return nil, false
	}
	outs := make([]identityOutput, 0, len(p.runs))
	for _, r := range p.runs {
		it, err := c.ev.Get(r.Evidence)
		if err != nil || it.Collection == nil || it.Collection.Status != collect.StatusOK {
			return nil, false
		}
		r.Status, r.Reason = it.Collection.Status, it.Collection.Reason
		outs = append(outs, identityOutput{run: r, output: it.Content, truncated: it.Collection.Truncated})
	}
	return outs, true
}

// identityLine is an identity tool's output line, decoded once it has
// passed the schema.
type identityLine struct {
	Capture    string   `json:"capture"`
	TargetKind string   `json:"target_kind"`
	Target     *string  `json:"target"`
	Time       *string  `json:"time"`
	LocalTime  *string  `json:"local_time"`
	RawTime    *string  `json:"raw_time"`
	Problems   []string `json:"problems"`
}

// identities are the captures one run found beneath dir. A run that did
// not complete, a line that is not a valid identity, or one naming a
// capture outside dir is an uncertain capture saying so.
func identities(dir string, o identityOutput) []InventoryCapture {
	uncertain := func(why string) InventoryCapture {
		return InventoryCapture{Kind: o.run.Kind, Currency: CaptureUncertain, Why: why}
	}
	if o.run.Status != collect.StatusOK {
		return []InventoryCapture{uncertain(fmt.Sprintf("its identity could not be read: %s %s: %s", o.run.Tool, o.run.Status, text.Ellipsis(o.run.Reason, 200)))}
	}
	var out []InventoryCapture
	lines := strings.Split(strings.TrimRight(string(o.output), "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if o.truncated && i == len(lines)-1 {
			out = append(out, uncertain(fmt.Sprintf("%s's output was cut at its limit; captures past line %d are not listed", o.run.Tool, i)))
			break
		}
		c, err := identityCapture(dir, l)
		if err != nil {
			out = append(out, uncertain(fmt.Sprintf("line %d of %s is not a valid identity: %s", i+1, o.run.Tool, text.Ellipsis(err.Error(), 300))))
			continue
		}
		c.Kind = o.run.Kind
		out = append(out, c)
	}
	return out
}

// identityCapture validates one output line against the identity schema
// and reads it.
func identityCapture(dir, line string) (InventoryCapture, error) {
	doc, err := apischema.DecodeJSON([]byte(line))
	if err != nil {
		return InventoryCapture{}, errors.New("not JSON")
	}
	if err := apischema.Validate(apischema.ExpertIdentity, doc); err != nil {
		return InventoryCapture{}, err
	}
	var l identityLine
	if err := json.Unmarshal([]byte(line), &l); err != nil {
		return InventoryCapture{}, err
	}
	if l.Capture != path.Clean(l.Capture) || (l.Capture != dir && !strings.HasPrefix(l.Capture, dir+"/")) {
		return InventoryCapture{}, fmt.Errorf("capture %q is not beneath the attachment's directory %s", l.Capture, dir)
	}
	c := InventoryCapture{Capture: l.Capture, TargetKind: l.TargetKind, Target: deref(l.Target), Time: deref(l.Time),
		LocalTime: deref(l.LocalTime), RawTime: deref(l.RawTime), Problems: l.Problems}
	switch {
	case c.Time != "":
		if c.at, err = time.Parse(time.RFC3339Nano, c.Time); err != nil {
			return InventoryCapture{}, fmt.Errorf("time %q: %w", c.Time, err)
		}
		c.zone = true
	case c.LocalTime != "":
		if c.at, err = time.Parse(localTimeLayout, c.LocalTime); err != nil {
			return InventoryCapture{}, fmt.Errorf("local_time %q: %w", c.LocalTime, err)
		}
	}
	return c, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// applyCurrency applies the rules of currency over every attachment's
// captures: within one kind and target, the latest by
// capture time is current and the earlier ones outdated; the same report
// of one host found twice is a duplicate; a capture whose target or time
// cannot be read, whose times cannot be compared with the others', or
// whose identity two lines disagree on is uncertain. Attachment names and
// order are never used.
func applyCurrency(atts []InventoryAttachment) {
	var all []*InventoryCapture
	for i := range atts {
		for j := range atts[i].Captures {
			if atts[i].Captures[j].Currency == "" {
				all = append(all, &atts[i].Captures[j])
			}
		}
	}
	all = withoutConflicts(all)
	groups := map[string][]*InventoryCapture{}
	var keys []string
	for _, c := range all {
		switch {
		case c.Target == "":
			c.Currency, c.Why = CaptureUncertain, "its "+targetNoun(c.TargetKind)+" could not be read"
			continue
		case c.at.IsZero():
			c.Currency, c.Why = CaptureUncertain, "its capture time could not be read"
			continue
		}
		k := c.Kind + "\x00" + c.TargetKind + "\x00" + c.Target
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], c)
	}
	sort.Strings(keys)
	for _, k := range keys {
		order(groups[k])
	}
}

// withoutConflicts makes uncertain every capture two lines identify
// differently, and marks a line repeating another exactly as found twice;
// what is left is one line a capture.
func withoutConflicts(all []*InventoryCapture) []*InventoryCapture {
	byPath := map[string][]*InventoryCapture{}
	for _, c := range all {
		byPath[c.Capture] = append(byPath[c.Capture], c)
	}
	var out []*InventoryCapture
	for _, c := range all {
		same := byPath[c.Capture]
		if same[0] != c {
			continue
		}
		conflict := false
		for _, o := range same[1:] {
			conflict = conflict || !identical(c, o)
		}
		if !conflict {
			for _, o := range same[1:] {
				o.Currency, o.DuplicateOf = CaptureDuplicate, o.Capture
			}
			out = append(out, c)
			continue
		}
		says := make([]string, len(same))
		for i, o := range same {
			says[i] = o.identity()
		}
		for _, o := range same {
			o.Currency, o.Why = CaptureUncertain, "conflicting identities: "+strings.Join(says, "; ")
		}
	}
	return out
}

func identical(a, b *InventoryCapture) bool {
	return a.Kind == b.Kind && a.TargetKind == b.TargetKind && a.Target == b.Target && a.Time == b.Time &&
		a.LocalTime == b.LocalTime && a.RawTime == b.RawTime
}

// identity is what a capture's line says it is, in one phrase.
func (c *InventoryCapture) identity() string {
	s := c.Kind + " of " + targetNoun(c.TargetKind) + " " + orUnread(c.Target)
	switch {
	case c.Time != "":
		s += " at " + c.Time
	case c.LocalTime != "":
		s += " at " + c.LocalTime + " local time"
	}
	return s
}

func targetNoun(kind string) string {
	if kind == "host" {
		return "host name"
	}
	return "cluster ID"
}

func orUnread(s string) string {
	if s == "" {
		return "(unread)"
	}
	return s
}

// order settles one kind and target's captures: duplicates first, then by
// time where every capture states one with its zone, or by local time
// where every one states it without and none with; otherwise their times
// cannot be compared and every one is uncertain.
func order(g []*InventoryCapture) {
	sort.SliceStable(g, func(i, j int) bool { return g[i].Capture < g[j].Capture })
	if g[0].TargetKind == "host" {
		// One sosreport extracted twice: the same host and the same
		// capture time, as read.
		first := map[string]*InventoryCapture{}
		kept := g[:0:0]
		for _, c := range g {
			k := c.Time + "\x00" + c.LocalTime + "\x00" + c.RawTime
			if f, ok := first[k]; ok {
				c.Currency, c.DuplicateOf = CaptureDuplicate, f.Capture
				continue
			}
			first[k] = c
			kept = append(kept, c)
		}
		g = kept
	}
	zoned := 0
	for _, c := range g {
		if c.zone {
			zoned++
		}
	}
	if zoned != 0 && zoned != len(g) {
		for _, c := range g {
			var others []string
			for _, o := range g {
				if o.zone != c.zone {
					others = append(others, o.Capture)
				}
			}
			c.Currency, c.Why = CaptureUncertain, "its capture time cannot be compared with that of "+strings.Join(others, ", ")+": one states its zone and the other does not"
		}
		return
	}
	sort.SliceStable(g, func(i, j int) bool { return g[i].at.Before(g[j].at) })
	last := g[len(g)-1]
	var tied []*InventoryCapture
	for _, c := range g {
		if c.at.Equal(last.at) {
			tied = append(tied, c)
		}
	}
	latest := tied[0]
	if len(tied) > 1 {
		for _, c := range tied {
			var others []string
			for _, o := range tied {
				if o != c {
					others = append(others, o.Capture)
				}
			}
			c.Currency, c.Why = CaptureUncertain, "captured at the same time as "+strings.Join(others, ", ")+", so which is later cannot be told"
		}
	} else {
		latest.Currency = CaptureCurrent
	}
	for _, c := range g {
		if c.Currency == "" {
			c.Currency, c.SupersededBy = CaptureOutdated, latest.Capture
		}
	}
}

// captureLine is one capture as the inventory says it.
func captureLine(c InventoryCapture) string {
	var b strings.Builder
	b.WriteString(c.Kind)
	if c.Capture != "" {
		b.WriteString(" " + c.Capture)
	}
	if c.TargetKind != "" {
		fmt.Fprintf(&b, " · %s %s", c.TargetKind, orUnread(c.Target))
	}
	switch {
	case c.Time != "":
		b.WriteString(" · captured " + c.Time)
	case c.LocalTime != "":
		b.WriteString(" · captured " + c.LocalTime + " local time, zone unknown")
	}
	if c.RawTime != "" && c.RawTime != c.Time && c.RawTime != c.LocalTime {
		fmt.Fprintf(&b, " (%q as read)", c.RawTime)
	}
	b.WriteString(" · " + c.Currency)
	switch {
	case c.SupersededBy != "":
		b.WriteString(", superseded by " + c.SupersededBy)
	case c.DuplicateOf != "" && c.DuplicateOf == c.Capture:
		b.WriteString(", found twice")
	case c.DuplicateOf != "":
		b.WriteString(" of " + c.DuplicateOf)
	case c.Why != "":
		b.WriteString(": " + c.Why)
	}
	if len(c.Problems) > 0 {
		b.WriteString(" · problems: " + strings.Join(c.Problems, "; "))
	}
	return strings.ReplaceAll(b.String(), "\n", " ")
}

// inventoryItem is the inventory as one deterministic item: each
// attachment as its source reports it, the kinds marked in it, each
// identity run with its evidence, and each capture with its currency; an
// attachment none was found in says it has no currency.
func inventoryItem(source, caseID, toolsLine string, inv Inventory) evidence.Item {
	var b fieldText
	fmt.Fprintf(&b, "artifact inventory of case %s from %s: %s\n", caseID, source, text.Plural(len(inv.Attachments)+inv.Omitted, "attachment"))
	b.field("identity tools", toolsLine)
	for _, ia := range inv.Attachments {
		b.WriteString("\n")
		attachmentFields(&b, ia.Attachment)
		if ia.Attachment.Dir != "" && ia.Note == "" && toolsLine != "none" {
			marked := "none"
			if len(ia.Kinds) > 0 {
				marked = strings.Join(ia.Kinds, ", ")
			}
			b.field("marked", marked)
		}
		for _, r := range ia.Runs {
			b.field("identity", strings.Join([]string{r.Kind, r.Tool, r.Status, r.Evidence}, " "))
		}
		for _, c := range ia.Captures {
			b.field("capture", captureLine(c))
		}
		b.field("note", ia.Note)
		if len(ia.Captures) == 0 {
			b.field("currency", "none: no capture identified")
		}
	}
	if inv.Omitted > 0 {
		fmt.Fprintf(&b, "\n… %d more attachments not inventoried\n", inv.Omitted)
	}
	return item(TypeArtifactInventory, source, "case "+caseID+" artifact inventory", b.String())
}

// inventoried is what an earlier inventory ran over one attachment, with
// what the source reported of the attachment then.
type inventoried struct {
	key   string
	kinds []string
	runs  []IdentityRun
}

// inventoryOf reads back an inventory item's runs; none when it was made
// with other identity tools than toolsLine names, since its runs then do
// not stand.
func inventoryOf(content, toolsLine string) map[string]inventoried {
	out := map[string]inventoried{}
	blocks := strings.Split(content, "\n\n")
	if !strings.HasSuffix(strings.TrimSuffix(blocks[0], "\n"), "\nidentity tools: "+toolsLine) {
		return out
	}
	for _, block := range blocks[1:] {
		var a Attachment
		a.Size = -1
		var p inventoried
		marked := false
		for _, l := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(l, ": ")
			switch k {
			case "attachment":
				a.Name = v
			case "state":
				a.State = AttachmentState(v)
			case "kind":
				a.Kind = v
			case "size":
				if _, err := fmt.Sscanf(v, "%d bytes", &a.Size); err != nil {
					a.Size = -1
				}
			case "file":
				a.Path = v
			case "contents in":
				a.Dir = strings.TrimSuffix(v, "/")
			case "marked":
				marked = true
				if v != "none" {
					p.kinds = strings.Split(v, ", ")
				}
			case "identity":
				f := strings.Fields(v)
				if len(f) == 4 {
					p.runs = append(p.runs, IdentityRun{Kind: f[0], Tool: f[1], Status: f[2], Evidence: f[3]})
				}
			}
		}
		if a.Name == "" || !marked {
			continue
		}
		if a.Dir == "" {
			a.Dir = a.Name
		}
		p.key = countKey(a)
		out[a.Name] = p
	}
	return out
}

// Text is the inventory as the case block shows it: one line an
// attachment, with its kind, size and state, and one a capture, with its
// target, capture time and currency, and the evidence each identity is
// read from.
func (inv Inventory) Text() string {
	var b strings.Builder
	n := len(inv.Attachments) + inv.Omitted
	fmt.Fprintf(&b, "Artifact inventory: %s\n", text.Plural(n, "attachment"))
	for _, ia := range inv.Attachments {
		a := ia.Attachment
		var about []string
		if a.Kind != "" {
			about = append(about, a.Kind)
		}
		if a.Size >= 0 {
			about = append(about, text.Plural(int(a.Size), "byte"))
		}
		if a.State != "" {
			about = append(about, string(a.State))
		}
		fmt.Fprintf(&b, "- %s", a.Name)
		if len(about) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(about, ", "))
		}
		switch {
		case ia.Note != "":
			b.WriteString(": " + ia.Note)
		case len(ia.Captures) == 0:
			b.WriteString(": no capture identified, no currency")
		}
		var evs []string
		for _, r := range ia.Runs {
			evs = append(evs, r.Evidence)
		}
		if len(evs) > 0 {
			b.WriteString("; identity in " + strings.Join(evs, ", "))
		}
		b.WriteString("\n")
		for _, c := range ia.Captures {
			b.WriteString("  - " + captureLine(c) + "\n")
		}
	}
	if inv.Omitted > 0 {
		fmt.Fprintf(&b, "… %d more attachments not inventoried\n", inv.Omitted)
	}
	return b.String()
}

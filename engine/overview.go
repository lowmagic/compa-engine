package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/sources"
	"github.com/lowmagic/compa-engine/internal/store"
)

const (
	TypeArtifactOverview = "artifact_overview"
	// TypeArtifactCounts is what a walk of the attachments counted, kept
	// apart from the overview so a count never changes it.
	TypeArtifactCounts = "artifact_counts"
)

const (
	// maxOverviewAttachments bounds how many attachments the overview lists.
	maxOverviewAttachments = 200
	// maxWalkEntries bounds the entries one overview counts, so a huge or
	// slow tree yields a lower bound rather than an open-ended wait.
	maxWalkEntries = 5_000_000
	// readDirBatch is how many entries the walk holds from one directory
	// at a time, so one directory of a million files costs no more memory
	// than one of a hundred.
	readDirBatch = 256
)

var errWalkLimit = errors.New("walk limit reached")

// tally is what a walk found beneath one directory.
type tally struct {
	files, bytes int64
	// unreadable counts directories the walk could not list.
	unreadable int
	// cut means the walk stopped at its limit, so the counts are a floor.
	cut bool
}

// walker counts regular files and their sizes from directory entries
// alone: it opens directories to list them and never opens a file.
// Symlinks are not followed: only entries that are directories themselves
// are descended into, so a walk from a real directory stays beneath it.
type walker struct {
	ctx  context.Context
	fsys fs.FS
	left int
}

// realDir reports whether dir, under root, is a directory reached through
// no link, which is what lets the walk use plain directory opens: os.Root
// resolves every component of every path with a system call of its own,
// which made the walk ten times slower on a must-gather's deep trees.
func realDir(root, dir string) bool {
	p := filepath.Join(root, dir)
	real, err := filepath.EvalSymlinks(p)
	return err == nil && real == p
}

func (w *walker) count(dir string) (tally, error) {
	var t tally
	err := w.dir(dir, &t)
	if errors.Is(err, errWalkLimit) {
		t.cut, err = true, nil
	}
	return t, err
}

// dir recurses as it reads, rather than queueing subdirectories, so what it
// holds grows with the tree's depth and not its width.
func (w *walker) dir(name string, t *tally) error {
	f, err := w.fsys.Open(name)
	if err != nil {
		t.unreadable++
		return nil
	}
	defer f.Close()
	rd, ok := f.(fs.ReadDirFile)
	if !ok {
		t.unreadable++
		return nil
	}
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		entries, err := rd.ReadDir(readDirBatch)
		for _, e := range entries {
			if w.left == 0 {
				return errWalkLimit
			}
			w.left--
			switch {
			case e.IsDir():
				if err := w.dir(path.Join(name, e.Name()), t); err != nil {
					return err
				}
			case e.Type().IsRegular():
				t.files++
				if info, err := e.Info(); err == nil {
					t.bytes += info.Size()
				}
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(entries) == 0) {
			return nil
		}
		if err != nil {
			t.unreadable++
			return nil
		}
	}
}

// excluded reports whether an attachment lies in an entry collection never
// reads: the store, or one the deployment names.
func (e *Engine) excluded(a Attachment) bool {
	names := append([]string{store.DirName}, e.config.Collection.Exclude...)
	for _, n := range names {
		for _, p := range []string{a.Name, a.Path, a.Dir} {
			if top, _, _ := strings.Cut(p, "/"); top == n {
				return true
			}
		}
	}
	return false
}

// attachments asks the artifact source what the case holds, leaving out
// excluded entries.
func (e *Engine) attachments(ctx context.Context, id string) ([]Attachment, error) {
	all, err := e.artifacts.Attachments(ctx, id)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, a := range all {
		if !e.excluded(a) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func recordAttachments(st *sources.Store, origin string, atts []Attachment) error {
	items := make([]sources.Source, len(atts))
	for i, a := range atts {
		items[i] = sources.Source{Origin: origin, Name: a.Name, Kind: sources.KindAttachment, Path: a.Path, Dir: a.Dir,
			Size: a.Size, State: string(a.State), Reported: a.Kind}
	}
	return st.Record(items)
}

// overviewTotals sums what the counts found: the files in each
// attachment's contents, and each attachment that is a file of its own.
type overviewTotals struct {
	attachments int
	tally
}

// overview is the case's artifacts as one bounded, deterministic item:
// each attachment as its source reports it. It is what the case block
// shows, and it holds nothing a walk finds, so it is known
// before any walk and a count changing never changes it.
func overview(source, caseID string, atts []Attachment) evidence.Item {
	var b fieldText
	fmt.Fprintf(&b, "artifacts of case %s from %s: %s\n", caseID, source, text.Plural(len(atts), "attachment"))
	for i, a := range atts {
		if i == maxOverviewAttachments {
			fmt.Fprintf(&b, "\n… %d more attachments not listed\n", len(atts)-i)
			break
		}
		b.WriteString("\n")
		attachmentFields(&b, a)
	}
	return item(TypeArtifactOverview, source, "case "+caseID+" artifacts", b.String())
}

func attachmentFields(b *fieldText, a Attachment) {
	b.field("attachment", a.Name)
	b.field("state", string(a.State))
	b.field("kind", a.Kind)
	if a.Size >= 0 {
		b.field("size", fmt.Sprintf("%d bytes", a.Size))
	}
	b.field("file", a.Path)
	if a.Dir != "" && a.Dir != a.Name {
		b.field("contents in", a.Dir+"/")
	}
}

// counted is one attachment's contents as a walk counted them, kept with
// what the source reported of the attachment then, so a later count can
// tell it unchanged and reuse it.
type counted struct {
	key   string
	line  string
	tally tally
}

// countKey is what must be unchanged for an attachment's count to stand.
func countKey(a Attachment) string {
	return strings.Join([]string{a.Name, string(a.State), a.Kind, fmt.Sprint(a.Size), a.Path, a.Dir}, "\x00")
}

// artifactCounts counts each extracted attachment's files and bytes, the
// item requests carry beside the overview: those prev counted from the
// same attachment as the source reports it now are reused and not walked
// again. walked is how many attachments it walked. A walk cancelled ends
// it with the context's error and no item.
func artifactCounts(ctx context.Context, fsys fs.FS, confined func(dir string) bool, source, caseID string, atts []Attachment, prev map[string]counted) (evidence.Item, overviewTotals, int, error) {
	w := &walker{ctx: ctx, fsys: fsys, left: maxWalkEntries}
	var b fieldText
	tot := overviewTotals{attachments: len(atts)}
	walked := 0
	fmt.Fprintf(&b, "file counts of the attachments of case %s from %s\n", caseID, source)
	for i, a := range atts {
		if i == maxOverviewAttachments {
			fmt.Fprintf(&b, "\n… %d more attachments not counted\n", len(atts)-i)
			tot.cut = true
			break
		}
		if a.Dir == "" {
			// Counted as the one file it is; an extracted archive counts
			// its contents instead, so nothing is counted twice.
			if a.Path != "" {
				tot.files++
				tot.bytes += max(a.Size, 0)
			}
			continue
		}
		b.WriteString("\n")
		attachmentFields(&b, a)
		if confined != nil && !confined(a.Dir) {
			b.field("contents", "not counted: the directory is reached through a link")
			continue
		}
		t, line := tally{}, ""
		if p, ok := prev[a.Name]; ok && p.key == countKey(a) {
			t, line = p.tally, p.line
		} else {
			var err error
			if t, err = w.count(a.Dir); err != nil {
				return evidence.Item{}, tot, walked, err
			}
			if err := ctx.Err(); err != nil {
				return evidence.Item{}, tot, walked, err
			}
			walked++
			line = countLine(t)
		}
		tot.files, tot.bytes, tot.unreadable = tot.files+t.files, tot.bytes+t.bytes, tot.unreadable+t.unreadable
		tot.cut = tot.cut || t.cut
		b.field("contents", line)
		b.field("files", fmt.Sprint(t.files))
		b.field("bytes", fmt.Sprint(t.bytes))
		if t.cut {
			b.field("cut", "yes")
		}
		if t.unreadable > 0 {
			b.field("unreadable", fmt.Sprint(t.unreadable))
		}
	}
	return item(TypeArtifactCounts, source, "case "+caseID+" artifact counts", b.String()), tot, walked, nil
}

func countLine(t tally) string {
	counts := fmt.Sprintf("%s, %d bytes", text.Plural(int(t.files), "file"), t.bytes)
	if t.cut {
		counts = "at least " + counts + " (the count stopped at its limit)"
	}
	if t.unreadable == 1 {
		counts += "; 1 directory could not be listed"
	} else if t.unreadable > 1 {
		counts += fmt.Sprintf("; %d directories could not be listed", t.unreadable)
	}
	return counts
}

// countsOf reads back a counts item: each attachment's count with what
// the source reported of it then.
func countsOf(content string) map[string]counted {
	out := map[string]counted{}
	for _, block := range strings.Split(content, "\n\n")[1:] {
		var a Attachment
		a.Size = -1
		var c counted
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
				fmt.Sscanf(v, "%d bytes", &a.Size)
			case "file":
				a.Path = v
			case "contents in":
				a.Dir = strings.TrimSuffix(v, "/")
			case "contents":
				c.line = v
			case "files":
				fmt.Sscan(v, &c.tally.files)
			case "bytes":
				fmt.Sscan(v, &c.tally.bytes)
			case "cut":
				c.tally.cut = v == "yes"
			case "unreadable":
				fmt.Sscan(v, &c.tally.unreadable)
			}
		}
		if a.Name == "" || c.line == "" || strings.HasPrefix(c.line, "not counted") {
			continue
		}
		if a.Dir == "" {
			a.Dir = a.Name
		}
		c.key = countKey(a)
		out[a.Name] = c
	}
	return out
}

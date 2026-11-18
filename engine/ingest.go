package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/lowmagic/compa-engine/internal/evidence"
)

// lineCounter counts the lines in what passes through it, as SizeOf would.
type lineCounter struct {
	lines int64
	last  byte
	any   bool
}

func (l *lineCounter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			l.lines++
		}
	}
	if len(p) > 0 {
		l.last, l.any = p[len(p)-1], true
	}
	return len(p), nil
}

func (l *lineCounter) total() int64 {
	if l.any && l.last != '\n' {
		return l.lines + 1
	}
	return l.lines
}

// ingest stores what r yields as evidence the engineer brought in, streamed
// so its size never matters to memory. The ID derives from the content and
// where it came from: the same file twice is one item, the same text from
// two places is two. It reports whether the item is new.
func ingest(c *openCase, r io.Reader, location, note string) (evidence.Item, bool, error) {
	var lc lineCounter
	digest, n, err := c.st.PutBlobFrom(io.TeeReader(r, &lc))
	if err != nil {
		return evidence.Item{}, false, err
	}
	h := sha256.New()
	for _, s := range []string{evidence.TypeIngested, "engineer", location, digest} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	it := evidence.Item{
		Ref: evidence.Ref{
			ID:         "ev-" + hex.EncodeToString(h.Sum(nil))[:16],
			Type:       evidence.TypeIngested,
			Provenance: evidence.Provenance{Source: "engineer", CollectedBy: "engineer"},
		},
		Location: location,
		Size:     evidence.Size{Bytes: n, Lines: lc.total()},
		Note:     note,
	}
	if c.ev.Exists(it.Ref.ID) {
		return it, false, nil
	}
	return it, true, c.ev.AddStored(it, digest)
}

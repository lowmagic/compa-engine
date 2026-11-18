package evidence

import "strings"

// Ref names a piece of evidence: its ID, its type and where it came from.
type Ref struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Scope      string     `json:"scope,omitempty"`
	Provenance Provenance `json:"provenance"`
	// Size is the whole evidence's size, so a model can tell what it has
	// not seen and how large it is; absent when the engine does not know
	// it.
	Size *Size `json:"size,omitempty"`
	// Note is what a reader must know before the text, such as where a
	// collection stopped; it is not one of the text's lines.
	Note string `json:"note,omitempty"`
}

// Provenance is where evidence came from: its source, its location there,
// and who collected it.
type Provenance struct {
	Source      string `json:"source"`
	Location    string `json:"location,omitempty"`
	CollectedBy string `json:"collected_by,omitempty"`
}

// Size measures a piece of evidence in bytes and lines, not tokens.
// LowerBound means the evidence was cut before it could be measured, as
// when a collection reached the runner's output limit, so the real size is
// larger.
type Size struct {
	Bytes      int64 `json:"bytes"`
	Lines      int64 `json:"lines"`
	LowerBound bool  `json:"lower_bound,omitempty"`
}

// SizeOf measures text.
func SizeOf(text string, lowerBound bool) *Size {
	lines := int64(strings.Count(text, "\n"))
	if text != "" && !strings.HasSuffix(text, "\n") {
		lines++
	}
	return &Size{Bytes: int64(len(text)), Lines: lines, LowerBound: lowerBound}
}

// Input is evidence as a model is offered it: its reference and its text,
// inlined, or its reference and size alone.
type Input struct {
	Ref    Ref
	Text   string
	Inline bool
	// Lines is each line's stored number where Text's lines are not the
	// stored lines in order, as a digest's are.
	Lines []int
	// Size is the evidence's size where the engine knows it without the
	// text, as for evidence too large to load; otherwise it is measured
	// from Text.
	Size *Size
}

package companion

// A model's output that degenerates into a loop repeats one fragment until
// the output cap stops it (seen live: 34, 129 and 1,059 repeats of a fragment).
// An answer that only ran long does not end in a repeat; looped tells the
// two apart from the text alone.
const (
	// loopMinRepeats and loopMinSpan are how often, and over how many bytes,
	// the tail must repeat to count as a loop; loopMaxPeriod is the longest
	// fragment looked for, and loopWindow how much of the tail is read.
	loopMinRepeats = 8
	loopMinSpan    = 256
	loopMaxPeriod  = 1024
	loopWindow     = 64 << 10
)

// looped reports whether text ends in one fragment repeated at least
// loopMinRepeats times over at least loopMinSpan bytes: the fragment's
// length, the shortest that repeats, and how many times it does.
func looped(text []byte) (period, repeats int, ok bool) {
	if len(text) > loopWindow {
		text = text[len(text)-loopWindow:]
	}
	n := len(text)
	for p := 1; p <= loopMaxPeriod && 2*p <= n; p++ {
		// The tail has period p for as far back as each byte equals the
		// one p after it.
		i := n - 1 - p
		for i >= 0 && text[i] == text[i+p] {
			i--
		}
		span := n - 1 - i
		if r := span / p; r >= loopMinRepeats && span >= loopMinSpan {
			return p, r, true
		}
	}
	return 0, 0, false
}

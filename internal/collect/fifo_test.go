//go:build unix

package collect

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO among the artifacts never holds a run until a writer appears: read,
// which opens the path it is given, refuses it as not a regular file at
// once, and the primitives that walk pass it by.
func TestFIFOIsRefusedNotWaitedOn(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skip(err)
	}
	for _, st := range []struct {
		name   string
		params map[string]any
	}{
		{"read", map[string]any{"path": "pipe"}},
		{"grep", map[string]any{"path": "pipe", "pattern": "x"}},
		{"jq", map[string]any{"path": "pipe", "filter": "."}},
	} {
		done := make(chan Record, 1)
		go func() { done <- run(t, root, prim(st.name, st.params)) }()
		select {
		case rec := <-done:
			if st.name == "read" && (rec.Status == StatusOK || !strings.Contains(rec.Reason, "not a regular file")) {
				t.Errorf("read of a FIFO: %s %s %q", rec.Status, rec.Reason, rec.Output)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s of a FIFO did not return", st.name)
		}
	}
}

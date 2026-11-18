package collect

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// With prlimit, a command's address space is held to MemoryBytes: dd
// cannot allocate a block larger than it, and can without the limit.
func TestBubblewrapBoundsMemory(t *testing.T) {
	sb, err := DefaultSandbox()
	if err != nil {
		SkipWithoutSandbox(t, err)
	}
	if sb.Name() != "bwrap+prlimit" {
		SkipWithoutSandbox(t, fmt.Sprintf("%s does not bound memory: %s is not installed", sb.Name(), prlimitPath))
	}
	run := func(memory int64) error {
		scratch, _ := filepath.EvalSymlinks(t.TempDir())
		var stderr bytes.Buffer
		cmd, err := sb.Command(context.Background(), SandboxSpec{
			Argv:    []string{"/usr/bin/dd", "if=/dev/zero", "of=/dev/null", "bs=256M", "count=1"},
			Scratch: scratch, Env: []string{"PATH=/usr/bin:/bin"}, MemoryBytes: memory, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = cmd.Run()
		t.Logf("memory %d: %v %s", memory, err, stderr.String())
		return err
	}
	if err := run(0); err != nil {
		t.Fatalf("with no limit, dd failed: %v", err)
	}
	if err := run(128 << 20); err == nil {
		t.Error("dd allocated 256 MiB within a 128 MiB limit")
	}
}

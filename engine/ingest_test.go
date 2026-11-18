package engine

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/store"
)

func testCase(t *testing.T, maxBlob int64) *openCase {
	t.Helper()
	dir := filepath.Join(t.TempDir(), store.DirName)
	st, err := store.Open(dir, "00000001", store.Options{MaxBlobBytes: maxBlob})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newOpenCase("00000001", "", dir, false, st)
}

func TestIngestTooLargeLeavesNothing(t *testing.T) {
	c := testCase(t, 1000)
	if _, _, err := ingest(c, strings.NewReader(strings.Repeat("x", 1001)), "stdin", ""); !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("ingest of 1001 bytes: %v", err)
	}
	if items, _ := c.ev.List(); len(items) != 0 {
		t.Errorf("evidence after a refused ingest: %+v", items)
	}
	var blobs int
	c.st.DB().QueryRow(`SELECT count(*) FROM blobs`).Scan(&blobs)
	files := 0
	filepath.WalkDir(filepath.Join(c.storeDir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if blobs != 0 || files != 0 {
		t.Errorf("%d blob rows and %d files left", blobs, files)
	}
}

func TestIngestCountsLines(t *testing.T) {
	c := testCase(t, 1<<20)
	for in, want := range map[string]int64{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n\n": 2} {
		it, _, err := ingest(c, strings.NewReader(in), "stdin-"+in, "")
		if err != nil || it.Size.Lines != want || it.Size.Bytes != int64(len(in)) {
			t.Errorf("ingest %q: %+v %v, want %d lines", in, it.Size, err, want)
		}
	}
}

// repeated yields n bytes of a log line, over and over.
type repeated struct {
	left int64
	line []byte
	off  int
}

func (r *repeated) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	k := 0
	for k < len(p) && r.left > 0 {
		c := copy(p[k:], r.line[r.off:])
		c = int(min(int64(c), r.left))
		k, r.off, r.left = k+c, (r.off+c)%len(r.line), r.left-int64(c)
	}
	return k, nil
}

// sampled reads through to r, sampling the heap every 256 reads.
type sampled struct {
	r     io.Reader
	reads int
	peak  uint64
}

func (s *sampled) Read(p []byte) (int, error) {
	if s.reads++; s.reads%256 == 0 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		s.peak = max(s.peak, m.HeapAlloc)
	}
	return s.r.Read(p)
}

// TestIngestHeapIsBounded ingests 64 MB, or a 2 GB input with
// COMPA_BENCH_LARGE=1, and records the heap's peak above where it started.
func TestIngestHeapIsBounded(t *testing.T) {
	size := int64(64 << 20)
	if os.Getenv("COMPA_BENCH_LARGE") == "1" {
		size = 2 << 30
	}
	c := testCase(t, size)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	in := &sampled{r: &repeated{left: size, line: []byte("2026-10-01T00:00:00Z worker-1 kubelet[2211]: PLEG is not healthy\n")}}
	it, _, err := ingest(c, in, "stdin", "")
	if err != nil || it.Size.Bytes != size {
		t.Fatalf("ingest: %+v %v", it.Size, err)
	}
	growth := in.peak - min(in.peak, base.HeapAlloc)
	t.Logf("ingested %d MiB, %d lines: peak heap %d MiB above the start", size>>20, it.Size.Lines, growth>>20)
	if growth > 64<<20 {
		t.Errorf("the heap grew by %d MiB while ingesting %d MiB", growth>>20, size>>20)
	}
}

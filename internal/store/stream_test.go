package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPutBlobFrom(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), DirName), "0420", Options{})
	defer s.Close()
	data := []byte(strings.Repeat("2026-10-01T00:00:00Z kubelet: line\n", 1000))
	whole, err := s.PutBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	streamed, n, err := s.PutBlobFrom(bytes.NewReader(data))
	if err != nil || streamed != whole || n != int64(len(data)) {
		t.Fatalf("PutBlobFrom = %s %d %v; PutBlob gave %s", streamed, n, err, whole)
	}
	other, _, err := s.PutBlobFrom(strings.NewReader("other"))
	if err != nil {
		t.Fatal(err)
	}
	for digest, want := range map[string][]byte{whole: data, other: []byte("other")} {
		if got, err := s.Blob(digest); err != nil || !bytes.Equal(got, want) {
			t.Errorf("Blob(%s) = %d bytes, %v", digest, len(got), err)
		}
		r, err := s.OpenBlob(digest)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("OpenBlob(%s) = %d bytes, %v", digest, len(got), err)
		}
	}
	if left := tempFiles(t, s); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestPutBlobFromTooLarge(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), DirName), "0420", Options{MaxBlobBytes: 1000})
	defer s.Close()
	var before int
	s.DB().QueryRow(`SELECT count(*) FROM blobs`).Scan(&before)
	if _, _, err := s.PutBlobFrom(strings.NewReader(strings.Repeat("x", 1001))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("1001 bytes: %v, want ErrTooLarge", err)
	}
	var after int
	s.DB().QueryRow(`SELECT count(*) FROM blobs`).Scan(&after)
	if after != before {
		t.Errorf("blob rows %d, then %d", before, after)
	}
	if left := tempFiles(t, s); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
	if _, n, err := s.PutBlobFrom(strings.NewReader(strings.Repeat("x", 1000))); err != nil || n != 1000 {
		t.Errorf("exactly the limit: %d %v", n, err)
	}
}

func TestOpenBlobDetectsDamage(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), DirName), "0420", Options{})
	defer s.Close()
	digest, _, err := s.PutBlobFrom(strings.NewReader("good content"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.blobPath(digest), s.enc.EncodeAll([]byte("bad content!"), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := s.OpenBlob(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("reading a damaged blob: %v", err)
	}
}

// tempFiles lists files under the blob directory that are not blobs.
func tempFiles(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(filepath.Join(s.dir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasSuffix(p, ".zst") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// genStream yields n bytes of log-like text, one 1 MiB block at a time
// with each block numbered, so it compresses as logs do without being one
// repeated block.
type genStream struct {
	left  int64
	block []byte
	off   int
	n     int
}

func newGenStream(n int64) *genStream {
	return &genStream{left: n}
}

func (g *genStream) Read(p []byte) (int, error) {
	if g.left == 0 {
		return 0, io.EOF
	}
	if g.off == len(g.block) {
		var b bytes.Buffer
		for b.Len() < 1<<20 {
			fmt.Fprintf(&b, "2026-10-01T00:00:%02dZ node-%d kubelet[%d]: block %d probe ok\n", b.Len()%60, b.Len()%7, b.Len()%9000, g.n)
		}
		g.block, g.off = b.Bytes(), 0
		g.n++
	}
	k := copy(p, g.block[g.off:])
	if int64(k) > g.left {
		k = int(g.left)
	}
	g.off += k
	g.left -= int64(k)
	return k, nil
}

// heapSampler reads through to r, sampling the heap every 256 reads.
type heapSampler struct {
	r     io.Reader
	reads int
	peak  uint64
}

func (h *heapSampler) Read(p []byte) (int, error) {
	if h.reads++; h.reads%256 == 0 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		h.peak = max(h.peak, m.HeapAlloc)
	}
	return h.r.Read(p)
}

// TestPutBlobFromHeapIsBounded streams 64 MB, or with COMPA_BENCH_LARGE=1
// 2 GB, and records the peak heap; it fails only past a
// generous bound, since the point is that the heap does not follow the
// input's size.
func TestPutBlobFromHeapIsBounded(t *testing.T) {
	size := int64(64 << 20)
	if os.Getenv("COMPA_BENCH_LARGE") == "1" {
		size = 2 << 30
	}
	s := mustOpen(t, filepath.Join(t.TempDir(), DirName), "0420", Options{MaxBlobBytes: size})
	defer s.Close()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	h := &heapSampler{r: newGenStream(size)}
	digest, n, err := s.PutBlobFrom(h)
	if err != nil || n != size {
		t.Fatalf("PutBlobFrom: %d %v", n, err)
	}
	info, _ := os.Stat(s.blobPath(digest))
	growth := h.peak - min(h.peak, base.HeapAlloc)
	t.Logf("%d MiB in: peak heap %d MiB above the start, stored as %d MiB", size>>20, growth>>20, info.Size()>>20)
	if growth > 64<<20 {
		t.Errorf("the heap grew by %d MiB while streaming %d MiB", growth>>20, size>>20)
	}
}

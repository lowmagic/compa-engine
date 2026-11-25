package engine_test

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/cli"
	"github.com/lowmagic/compa-engine/engine/localdir"
	"github.com/lowmagic/compa-engine/engine/stubcases"
	"github.com/lowmagic/compa-engine/internal/benchcase"
	"github.com/lowmagic/compa-engine/internal/store"
)

// maxRSS is the process's peak resident set so far, in bytes.
func maxRSS() int64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	if runtime.GOOS == "darwin" {
		return ru.Maxrss // bytes on macOS
	}
	return ru.Maxrss << 10 // kilobytes on Linux
}

func dirBytes(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// TestLargeCase measures acquisition on a generated case: 32 MB always, so
// the measurement keeps working, and with COMPA_BENCH_LARGE=1 one of
// 2 GB and 100,000 files. It records, and sets no budget: none is set
// before a baseline exists (docs/standards.md). Peak RSS is the
// process's high-water mark, so each figure includes what came before it.
// The page cache is not dropped, which needs root: the first inventory
// reads directories the generator has just written.
func TestLargeCase(t *testing.T) {
	size, files := int64(32<<20), 2_000
	if os.Getenv("COMPA_BENCH_LARGE") == "1" {
		size, files = 2<<30, 100_000
	} else if testing.Short() {
		t.Skip("generates a case")
	}
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	cases, home := filepath.Join(tmp, "cases"), filepath.Join(tmp, "home")
	folder := filepath.Join(cases, "00000001")
	start := time.Now()
	st, err := benchcase.Generate(folder, benchcase.Options{Bytes: size, Files: files, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("machine: %s/%s, %d CPUs, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	t.Logf("generated %d files, %d MiB in %s; largest %d MiB", st.Files, st.Bytes>>20, time.Since(start).Round(time.Millisecond), st.LargestBytes>>20)

	o := engine.Options{Cases: stubcases.New("../testdata/cases"), Artifacts: localdir.New(cases), Home: home}
	run := func(args ...string) (string, time.Duration, uint64) {
		var out, errb bytes.Buffer
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		began := time.Now()
		code := cli.Run(context.Background(), args, o, strings.NewReader(""), &out, &errb)
		took := time.Since(began)
		runtime.ReadMemStats(&after)
		if code != 0 {
			t.Fatalf("compa %v: %d %s", args, code, errb.String())
		}
		return out.String(), took, after.Mallocs - before.Mallocs
	}
	out, first, allocs := run("case", "open", "00000001")
	t.Logf("case open, first: %s, %d allocations (%.1f per file), peak RSS %d MiB", first.Round(time.Millisecond), allocs, float64(allocs)/float64(st.Files), maxRSS()>>20)
	if !strings.Contains(out, "attachments: 1, holding ") {
		t.Errorf("case open:\n%s", out)
	}
	_, second, allocs := run("case", "open", "00000001")
	t.Logf("case open, repeat: %s, %d allocations, peak RSS %d MiB", second.Round(time.Millisecond), allocs, maxRSS()>>20)

	storeDir := filepath.Join(home, store.DirName, "cases", "00000001")
	before := dirBytes(storeDir)
	_, took, allocs := run("in", "00000001", filepath.Join(folder, st.Largest))
	grew := dirBytes(storeDir) - before
	t.Logf("ingest of the largest file, %d MiB: %s, %.0f MiB/s, %d allocations, peak RSS %d MiB; the store grew by %d MiB (%.1f%% of the input)",
		st.LargestBytes>>20, took.Round(time.Millisecond), float64(st.LargestBytes)/(1<<20)/took.Seconds(), allocs, maxRSS()>>20, grew>>20, 100*float64(grew)/float64(st.LargestBytes))

	// A hundred stores under the home directory, half of them expired.
	old := time.Now().Add(-30 * 24 * time.Hour)
	for i := range 100 {
		id := "9" + strings.Repeat("0", 4) + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "1"
		opts := store.Options{}
		if i%2 == 0 {
			opts.Now = func() time.Time { return old }
		}
		s, err := store.Open(filepath.Join(home, store.DirName, "cases", id), id, opts)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	began := time.Now()
	removed, err := store.Sweep(home, 14*24*time.Hour, time.Now())
	if err != nil || len(removed) != 50 {
		t.Fatalf("sweep removed %d: %v", len(removed), err)
	}
	t.Logf("sweep of 100 home stores, 50 expired: %s", time.Since(began).Round(time.Millisecond))
}

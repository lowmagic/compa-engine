package engine

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fixtureAttachments are what the local-directory source reports for
// testdata/artifacts.
var fixtureAttachments = []Attachment{
	{Name: "README.md", Path: "README.md", Size: 0, State: AttachmentPresent},
	{Name: "must-gather", Dir: "must-gather", Size: -1, State: AttachmentPresent},
}

func TestOverviewGolden(t *testing.T) {
	root, err := os.OpenRoot("../testdata/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	atts := append([]Attachment(nil), fixtureAttachments...)
	info, _ := root.Stat("README.md")
	atts[0].Size = info.Size()
	it := overview("local directory", "00000001", atts)
	counts, tot, walked, err := artifactCounts(context.Background(), root.FS(), nil, "local directory", "00000001", atts, nil)
	if err != nil {
		t.Fatal(err)
	}
	for golden, got := range map[string][]byte{"testdata/artifact-overview.golden": it.Content, "testdata/artifact-counts.golden": counts.Content} {
		if *update {
			os.WriteFile(golden, got, 0o644)
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("differs from %s (go test -update to accept):\n%s", golden, got)
		}
	}
	again := overview("local directory", "00000001", atts)
	if again.Ref.ID != it.Ref.ID || it.Ref.Type != TypeArtifactOverview || counts.Ref.Type != TypeArtifactCounts {
		t.Errorf("overview IDs %s and %s, types %s and %s", it.Ref.ID, again.Ref.ID, it.Ref.Type, counts.Ref.Type)
	}
	if tot.attachments != 2 || tot.files < 2 || tot.cut || walked != 1 {
		t.Errorf("totals %+v, %d walked", tot, walked)
	}
	// A count reused from the last one walks nothing and says the same.
	reused, _, walked, _ := artifactCounts(context.Background(), emptyFS{}, nil, "local directory", "00000001", atts, countsOf(string(counts.Content)))
	if walked != 0 || reused.Ref.ID != counts.Ref.ID {
		t.Errorf("reused counts: %d walked, %s\n%s", walked, reused.Ref.ID, reused.Content)
	}
}

func TestOverviewShowsWhatTheSourceReports(t *testing.T) {
	atts := []Attachment{
		{Name: "0010-must-gather.tgz", Path: "0010-must-gather.tgz", Size: 1234, State: AttachmentNotExtracted, Kind: "must-gather"},
		{Name: "0020-sosreport.tar.xz", Path: "0020-sosreport.tar.xz", Size: -1, State: AttachmentPending},
	}
	it := overview("case source", "00000001", atts)
	_, tot, _, err := artifactCounts(context.Background(), emptyFS{}, nil, "case source", "00000001", atts, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "attachment: 0010-must-gather.tgz\nstate: not_extracted\nkind: must-gather\nsize: 1234 bytes\nfile: 0010-must-gather.tgz\n\n" +
		"attachment: 0020-sosreport.tar.xz\nstate: pending\nfile: 0020-sosreport.tar.xz\n"
	if !strings.HasSuffix(string(it.Content), want) {
		t.Errorf("overview:\n%s", it.Content)
	}
	if tot.files != 2 || tot.bytes != 1234 {
		t.Errorf("totals %+v", tot)
	}
}

func TestExcludedAttachments(t *testing.T) {
	e := &Engine{config: mustConfig(t, "collection: {exclude: [.statedir]}")}
	for a, want := range map[Attachment]bool{
		{Name: ".compa", Dir: ".compa"}:             true,
		{Name: ".statedir", Dir: ".statedir"}:       true,
		{Name: "x", Dir: ".statedir/x"}:             true,
		{Name: "0010-mg", Dir: "0010-mg"}:           false,
		{Name: ".compafoo", Dir: ".compafoo"}:       false,
		{Name: "0010-mg", Dir: "0010-mg/.statedir"}: false,
	} {
		if got := e.excluded(a); got != want {
			t.Errorf("excluded(%+v) = %v, want %v", a, got, want)
		}
	}
}

func TestWalkReadsNoFileContents(t *testing.T) {
	fsys := &countingFS{FS: os.DirFS("../testdata/artifacts")}
	w := &walker{ctx: context.Background(), fsys: fsys, left: maxWalkEntries}
	got, err := w.count(".")
	if err != nil {
		t.Fatal(err)
	}
	if got.files == 0 || got.bytes == 0 {
		t.Fatalf("walk found %+v", got)
	}
	if fsys.files != 0 {
		t.Errorf("the walk opened %d files", fsys.files)
	}
}

func TestWalkStopsAtItsLimit(t *testing.T) {
	w := &walker{ctx: context.Background(), fsys: genFS{dirs: 1, files: 100}, left: 10}
	got, err := w.count(".")
	if err != nil || !got.cut || got.files > 10 {
		t.Errorf("count = %+v, %v; want a cut floor of at most 10 files", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&walker{ctx: ctx, fsys: genFS{dirs: 1, files: 100}, left: 1000}).count("."); err == nil {
		t.Error("a cancelled walk succeeded")
	}
}

// TestWalkMemoryIsFlat walks generated trees of 1,000 and 100,000 files,
// in one directory and spread over many, and requires the live heap at its
// peak not to grow with them. Allocations per file are recorded, not bound.
func TestWalkMemoryIsFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("walks 200,000 generated entries")
	}
	for _, shape := range []struct{ dirs, perDir int }{{1, 1}, {100, 1}} {
		peak := func(n int) (uint64, float64) {
			fsys := genFS{dirs: shape.dirs, files: n / shape.dirs * shape.perDir, sample: &sampler{}}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			w := &walker{ctx: context.Background(), fsys: fsys, left: maxWalkEntries}
			got, err := w.count(".")
			if err != nil || got.files != int64(n) {
				t.Fatalf("walk of %d: %+v, %v", n, got, err)
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			return fsys.sample.peak - min(fsys.sample.peak, before.HeapAlloc), float64(after.Mallocs-before.Mallocs) / float64(n)
		}
		small, perSmall := peak(1_000)
		large, perLarge := peak(100_000)
		t.Logf("%d dirs: peak live heap growth %d bytes over 1,000 files, %d over 100,000; %.1f and %.1f allocations per file", shape.dirs, small, large, perSmall, perLarge)
		if large > small+1<<20 {
			t.Errorf("%d dirs: the walk's live heap grew by %d bytes from 1,000 to 100,000 files", shape.dirs, large-small)
		}
	}
}

func mustConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	e, err := New(Options{Cases: nopCases{}, Artifacts: nopArtifacts{}, Home: t.TempDir(), Config: []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n" + body + "\n")})
	if err != nil {
		t.Fatal(err)
	}
	return e.config
}

type nopCases struct{}

func (nopCases) Name() string                                        { return "nop" }
func (nopCases) Case(context.Context, string) (Case, error)          { return Case{}, nil }
func (nopCases) Comments(context.Context, string) ([]Comment, error) { return nil, nil }

type nopArtifacts struct{}

func (nopArtifacts) Name() string                                 { return "nop" }
func (nopArtifacts) Root(context.Context, string) (string, error) { return "", ErrNoArtifacts }
func (nopArtifacts) Attachments(context.Context, string) ([]Attachment, error) {
	return nil, ErrNoArtifacts
}

// countingFS counts the files opened, as distinct from directories.
type countingFS struct {
	fs.FS
	files int
}

func (c *countingFS) Open(name string) (fs.File, error) {
	f, err := c.FS.Open(name)
	if err == nil {
		if info, err := f.Stat(); err == nil && !info.IsDir() {
			c.files++
		}
	}
	return f, err
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }

// genFS is a generated tree: dirs directories under the root, each holding
// files files of 10 bytes, produced as they are read and never stored.
type genFS struct {
	dirs, files int
	sample      *sampler
}

type sampler struct {
	reads int
	peak  uint64
}

// note samples the live heap every 1,024 calls: directory reads and entry
// lookups, so a batch is sampled while the walk holds it.
func (s *sampler) note() {
	if s == nil {
		return
	}
	if s.reads++; s.reads%1024 == 0 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		s.peak = max(s.peak, m.HeapAlloc)
	}
}

func (g genFS) Open(name string) (fs.File, error) {
	switch {
	case name == ".":
		if g.dirs == 1 {
			return &genDir{g: g, n: g.files, file: true}, nil
		}
		return &genDir{g: g, n: g.dirs}, nil
	case strings.HasPrefix(name, "d") && !strings.Contains(name, "/"):
		return &genDir{g: g, n: g.files, file: true}, nil
	}
	return nil, fs.ErrNotExist
}

type genDir struct {
	g    genFS
	n    int
	next int
	file bool // entries are files rather than directories
}

func (d *genDir) Stat() (fs.FileInfo, error) { return genInfo{name: ".", dir: true}, nil }
func (d *genDir) Read([]byte) (int, error)   { return 0, io.EOF }
func (d *genDir) Close() error               { return nil }

func (d *genDir) ReadDir(n int) ([]fs.DirEntry, error) {
	d.g.sample.note()
	if d.next == d.n {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	k := d.n - d.next
	if n > 0 {
		k = min(n, k)
	}
	out := make([]fs.DirEntry, k)
	for i := range out {
		if d.file {
			out[i] = genEntry{genInfo{name: fmt.Sprintf("f%d", d.next+i), size: 10}, d.g.sample}
		} else {
			out[i] = genEntry{genInfo{name: fmt.Sprintf("d%d", d.next+i), dir: true}, d.g.sample}
		}
	}
	d.next += k
	return out, nil
}

type genEntry struct {
	info   genInfo
	sample *sampler
}

func (e genEntry) Name() string               { return e.info.Name() }
func (e genEntry) IsDir() bool                { return e.info.dir }
func (e genEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e genEntry) Info() (fs.FileInfo, error) { e.sample.note(); return e.info, nil }

type genInfo struct {
	name string
	size int64
	dir  bool
}

func (i genInfo) Name() string       { return path.Base(i.name) }
func (i genInfo) Size() int64        { return i.size }
func (i genInfo) ModTime() time.Time { return time.Time{} }
func (i genInfo) IsDir() bool        { return i.dir }
func (i genInfo) Sys() any           { return nil }
func (i genInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func TestOverviewDoesNotWalkThroughLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.log"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(root, "real"), 0o700)
	os.WriteFile(filepath.Join(root, "real", "a.log"), []byte("abc"), 0o600)
	os.Symlink(outside, filepath.Join(root, "link"))
	os.Symlink(outside, filepath.Join(root, "real", "inner-link"))
	real, _ := filepath.EvalSymlinks(root)
	confined := func(dir string) bool { return realDir(real, dir) }
	atts := []Attachment{{Name: "link", Dir: "link", Size: -1, State: AttachmentPresent}, {Name: "real", Dir: "real", Size: -1, State: AttachmentPresent}}
	it, tot, _, err := artifactCounts(context.Background(), os.DirFS(real), confined, "local directory", "00000001", atts, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := string(it.Content)
	if !strings.Contains(c, "attachment: link\nstate: present\ncontents: not counted: the directory is reached through a link\n") ||
		!strings.Contains(c, "attachment: real\nstate: present\ncontents: 1 file, 3 bytes\n") || tot.files != 1 {
		t.Errorf("overview:\n%s", c)
	}
}

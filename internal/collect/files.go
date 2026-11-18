package collect

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// walk visits dir and everything beneath it in lexical order without
// following symlinks; symlinks are skipped.
func walk(a *artifactRoot, dir string, fn func(rel string, isDir bool, size int64, mod time.Time) error) error {
	return fs.WalkDir(a.root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if a.excludedTop(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) || !d.IsDir() && a.credentialsOnly(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(p, d.IsDir(), info.Size(), info.ModTime())
	})
}

func matchBase(glob, rel string) (bool, error) {
	return path.Match(glob, path.Base(rel))
}

// maxDiffLines bounds each side of a diff, keeping it within memory.
const maxDiffLines = 20000

// runDiff writes a line diff of a and b: "-" for lines only in a, "+" for
// lines only in b, " " for common lines.
func runDiff(p *diffParams, e *primEnv) error {
	read := func(name string) ([]string, error) {
		var out []string
		s := source{name: name}
		if c, err := e.root.path(name); err != nil {
			return nil, err
		} else {
			s.name = c
		}
		err := e.lines(&s, func(n int, line string) error {
			if n > maxDiffLines {
				return fmt.Errorf("diff: %s has more than %d lines", name, maxDiffLines)
			}
			out = append(out, line)
			return nil
		})
		return out, err
	}
	a, err := read(p.A)
	if err != nil {
		return err
	}
	b, err := read(p.B)
	if err != nil {
		return err
	}
	if err := e.out.line("--- " + p.A); err != nil {
		return err
	}
	if err := e.out.line("+++ " + p.B); err != nil {
		return err
	}
	for _, op := range diffLines(a, b) {
		if err := e.out.line(op); err != nil {
			return err
		}
	}
	return nil
}

// diffLines is Myers' O((N+M)D) algorithm, returning the edit script.
func diffLines(a, b []string) []string {
	n, m := len(a), len(b)
	max := n + m
	v := make([]int, 2*max+2)
	var trace [][]int
	found := false
	for d := 0; d <= max && !found; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[max+k-1] < v[max+k+1]) {
				x = v[max+k+1]
			} else {
				x = v[max+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[max+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	// Backtrack from the end to recover the script.
	var script []string
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[max+k-1] < v[max+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[max+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			script = append(script, " "+a[x-1])
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				script = append(script, "+"+b[y-1])
			} else {
				script = append(script, "-"+a[x-1])
			}
		}
		x, y = prevX, prevY
	}
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return script
}

// runArchiveList lists the members of a tar, gzipped tar or zip archive,
// sorted by name, without extracting anything.
func runArchiveList(p *archiveParams, e *primEnv) error {
	max := p.MaxEntries
	if max <= 0 {
		max = 5000
	}
	f, err := e.root.open(p.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	type entry struct {
		name string
		size int64
		kind string
	}
	var entries []entry
	more := false
	// Listing stops at the entry cap rather than reading on to learn
	// whether more follow: the next header lies past the last member's
	// content, which may be large. A full list is marked truncated.
	add := func(name string, size int64, kind string) error {
		entries = append(entries, entry{name, size, kind})
		if len(entries) == max {
			more = true
			return errOutputFull
		}
		return nil
	}
	name := strings.ToLower(p.Path)
	switch {
	case strings.HasSuffix(name, ".zip"):
		info, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return fmt.Errorf("archive.list: %w", err)
		}
		for _, zf := range zr.File {
			kind := "file"
			if zf.FileInfo().IsDir() {
				kind = "dir"
			}
			if err := add(zf.Name, int64(zf.UncompressedSize64), kind); err != nil {
				break
			}
		}
	case strings.HasSuffix(name, ".tar"), strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		var r io.Reader = ctxReader{e.ctx, f}
		if !strings.HasSuffix(name, ".tar") {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return fmt.Errorf("archive.list: %w", err)
			}
			defer gz.Close()
			r = ctxReader{e.ctx, gz}
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("archive.list: %w", err)
			}
			kind := "file"
			switch h.Typeflag {
			case tar.TypeDir:
				kind = "dir"
			case tar.TypeSymlink, tar.TypeLink:
				kind = "link"
			}
			if err := add(h.Name, h.Size, kind); err != nil {
				break
			}
		}
	default:
		return fmt.Errorf("archive.list: %s: unsupported archive type (tar, tar.gz, tgz, zip)", p.Path)
	}
	if more {
		e.out.truncated = true
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	for _, en := range entries {
		if err := e.out.line(fmt.Sprintf("%s\t%d\t%s", en.kind, en.size, en.name)); err != nil {
			return err
		}
	}
	return nil
}

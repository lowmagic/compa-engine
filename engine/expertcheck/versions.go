package expertcheck

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// pkgVersion is a package as the version check compares it: its manifest's
// version and a digest of its content.
type pkgVersion struct {
	dir, version, digest string
}

// Versions is compa expert versions, given the arguments after "expert
// versions", run in a git checkout: it fails when a package under the
// roots, by its manifest's ID, has the version it had at the base revision
// but other content. A package's content is every file of its directory
// but the directory's own Go files, which embed it. The base is --base, or
// the merge base with origin/main, or HEAD's parent where that is HEAD.
func Versions(ctx context.Context, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("expert versions", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	base := fl.String("base", "", "")
	roots, err := parseInterleaved(fl, args)
	if err != nil || len(roots) == 0 {
		return fmt.Errorf("%w: expert versions [--base <revision>] <root> [root …]", ErrUsage)
	}
	if *base == "" {
		if *base, err = defaultBase(ctx); err != nil {
			return err
		}
	}
	before := map[string]pkgVersion{}
	for _, r := range roots {
		files, err := exportRoot(ctx, *base, r)
		if err != nil {
			return err
		}
		if err := packagesOf(files, before); err != nil {
			return fmt.Errorf("%s at %s: %w", r, *base, err)
		}
	}
	now := map[string]pkgVersion{}
	for _, r := range roots {
		files, err := readRoot(r)
		if err != nil {
			return err
		}
		if err := packagesOf(files, now); err != nil {
			return fmt.Errorf("%s: %w", r, err)
		}
	}
	var unbumped []string
	for id, p := range now {
		if b, ok := before[id]; ok && b.version == p.version && b.digest != p.digest {
			unbumped = append(unbumped, fmt.Sprintf("%s (%s %s)", p.dir, id, p.version))
		}
	}
	sort.Strings(unbumped)
	if len(unbumped) > 0 {
		return fmt.Errorf("expert versions %w: content changed since %s with the version unchanged: %s", ErrFailed, short(*base), strings.Join(unbumped, ", "))
	}
	fmt.Fprintf(out, "%d packages against %s; every changed one has a new version\n", len(now), short(*base))
	return nil
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

func git(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

// defaultBase is the merge base of HEAD with origin/main, or, on a commit
// that is its own merge base, its parent.
func defaultBase(ctx context.Context) (string, error) {
	head, err := git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	mb, err := git(ctx, "merge-base", "HEAD", "origin/main")
	if err != nil {
		return "", err
	}
	if bytes.Equal(head, mb) {
		parent, err := git(ctx, "rev-parse", "HEAD~1")
		return strings.TrimSpace(string(parent)), err
	}
	return strings.TrimSpace(string(mb)), nil
}

// exportRoot is the files under root at rev, by their paths relative to
// root; none where root did not exist then.
func exportRoot(ctx context.Context, rev, root string) (map[string][]byte, error) {
	rel := filepath.ToSlash(filepath.Clean(root))
	listed, err := git(ctx, "ls-tree", "--name-only", rev, "--", rel)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(listed)) == 0 {
		return map[string][]byte{}, nil
	}
	data, err := git(ctx, "archive", "--format=tar", rev, rel)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[strings.TrimPrefix(h.Name, rel+"/")] = b
	}
}

// readRoot is the files under root in the working tree, by their paths
// relative to root.
func readRoot(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	return files, err
}

// packagesOf adds to into the packages among files: each directory
// directly under the root holding expert.yaml or catalog.yaml, by its
// manifest's ID.
func packagesOf(files map[string][]byte, into map[string]pkgVersion) error {
	type content struct{ paths []string }
	dirs := map[string]*content{}
	for p := range files {
		dir, rest, ok := strings.Cut(p, "/")
		if !ok {
			continue
		}
		if dirs[dir] == nil {
			dirs[dir] = &content{}
		}
		// The directory's own Go files embed the package; they are no part
		// of what it ships.
		if !strings.Contains(rest, "/") && path.Ext(rest) == ".go" {
			continue
		}
		dirs[dir].paths = append(dirs[dir].paths, rest)
	}
	for dir, c := range dirs {
		manifest, ok := files[dir+"/expert.yaml"]
		if !ok {
			manifest, ok = files[dir+"/catalog.yaml"]
		}
		if !ok {
			continue
		}
		var m struct {
			Metadata struct{ ID, Version string }
		}
		if err := yaml.Unmarshal(manifest, &m); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		sort.Strings(c.paths)
		h := sha256.New()
		for _, p := range c.paths {
			b := files[dir+"/"+p]
			fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
			h.Write(b)
		}
		if prev, dup := into[m.Metadata.ID]; dup {
			return fmt.Errorf("%s and %s are both %s", prev.dir, dir, m.Metadata.ID)
		}
		into[m.Metadata.ID] = pkgVersion{dir: dir, version: m.Metadata.Version, digest: hex.EncodeToString(h.Sum(nil))}
	}
	return nil
}

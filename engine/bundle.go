package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/lowmagic/compa-engine/internal/store"
)

// A bundle is the expert set a deployment builds into its binary: package
// directories under experts/ and the routing library at routing/library.yaml.
// Anything else it holds, such as a term dictionary, is copied and not
// read.
const (
	bundleExperts = "experts"
	bundleRouting = "routing/library.yaml"
)

// bundled writes the deployment's bundle under the home directory the first
// time it is needed, in a directory named by its content, and makes it the
// bundled expert root and the routing library unless configuration names
// others. The loader reads a directory, and pins name package digests, so
// a copy on disk changes nothing a unit pinned.
func (e *Engine) bundled() error {
	if e.bundle == nil || e.bundleDir != "" {
		return nil
	}
	dir, err := materialize(e.bundle, filepath.Join(e.home, store.DirName, "bundle"))
	if err != nil {
		return fmt.Errorf("the bundled experts: %w", err)
	}
	e.bundleDir = dir
	if e.config.Experts.Bundled == "" {
		if _, err := fs.Stat(e.bundle, bundleExperts); err == nil {
			e.config.Experts.Bundled = filepath.Join(dir, bundleExperts)
		}
	}
	if e.config.RoutingLibrary == "" {
		if _, err := fs.Stat(e.bundle, bundleRouting); err == nil {
			e.config.RoutingLibrary = filepath.Join(dir, filepath.FromSlash(bundleRouting))
		}
	}
	return nil
}

// materialize copies fsys under parent/<digest>, the digest over every
// file's path and content, so one bundle is written once and a new one
// beside it. It is written to a temporary directory and renamed into place.
func materialize(fsys fs.FS, parent string) (string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		} else if !d.IsDir() {
			return fmt.Errorf("%s: only files and directories are bundled", p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, p := range files {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
	}
	dir := filepath.Join(parent, hex.EncodeToString(h.Sum(nil))[:16])
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".writing-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for _, p := range files {
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := copyFile(fsys, p, dst); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another compa wrote the same bundle first.
		if info, serr := os.Stat(dir); serr == nil && info.IsDir() {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

func copyFile(fsys fs.FS, src, dst string) error {
	in, err := fsys.Open(path.Clean(src))
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

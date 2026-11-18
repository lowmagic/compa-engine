package expert

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store keeps an immutable snapshot of each pinned package, keyed by
// digest, so an investigation keeps its pinned expert after the installed
// copy is upgraded or removed.
type Store struct {
	Dir    string
	Loader Loader
}

func (s Store) path(digest string) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 || strings.Trim(hex, "0123456789abcdef") != "" {
		return "", fmt.Errorf("malformed digest %q", digest)
	}
	return filepath.Join(s.Dir, hex), nil
}

// Save snapshots pkg unless a snapshot with its digest already exists.
func (s Store) Save(pkg *Package) error {
	target, err := s.path(pkg.Digest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		_, err := s.load(target, pkg.pin())
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.Dir, ".snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, rel := range pkg.Files {
		if err := copyFile(filepath.Join(pkg.Dir, filepath.FromSlash(rel)), filepath.Join(tmp, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}
	// The copy is verified before it is published: if the installed package
	// changed after it was loaded, the digest no longer matches.
	if _, err := s.load(tmp, pkg.pin()); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// SaveResolved snapshots the expert and each catalog version it resolved.
func (s Store) SaveResolved(r Resolved) error {
	if err := s.Save(r.Package); err != nil {
		return err
	}
	for _, rc := range r.Catalogs {
		if rc.Package == nil {
			continue
		}
		if err := s.Save(rc.Package); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) Load(pin Pin) (*Package, error) {
	dir, err := s.path(pin.Digest)
	if err != nil {
		return nil, err
	}
	return s.load(dir, pin)
}

func (s Store) load(dir string, pin Pin) (*Package, error) {
	pkg, err := s.Loader.Load(dir)
	if err != nil {
		return nil, err
	}
	if pkg.Digest != pin.Digest || pkg.ID() != pin.ID || pkg.Version() != pin.Version {
		return nil, fmt.Errorf("snapshot %s holds %s %s (%s), not the pinned %s %s (%s)", dir, pkg.ID(), pkg.Version(), pkg.Digest, pin.ID, pin.Version, pin.Digest)
	}
	return pkg, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

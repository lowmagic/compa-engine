// Package localdir is an ArtifactSource over a local directory: a base
// directory holding one folder per case, or one explicit directory.
package localdir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lowmagic/compa-engine/engine"
)

type Source struct {
	base string
	dir  string
}

// New maps a case ID to <base>/<case ID>. That path may be a symlink to the
// case folder.
func New(base string) *Source { return &Source{base: base} }

// Dir uses dir as the artifact directory whatever the case ID.
func Dir(dir string) *Source { return &Source{dir: dir} }

func (s *Source) Name() string { return "local directory" }

// Attachments reports each entry directly under the root as present,
// files with their size: a plain directory has extracted nothing, and its
// entries are all it knows.
func (s *Source) Attachments(ctx context.Context, caseID string) ([]engine.Attachment, error) {
	root, err := s.Root(ctx, caseID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("case %s: %w", caseID, err)
	}
	out := make([]engine.Attachment, 0, len(entries))
	for _, e := range entries {
		a := engine.Attachment{Name: e.Name(), Size: -1, State: engine.AttachmentPresent}
		switch {
		case e.IsDir():
			a.Dir = e.Name()
		case e.Type().IsRegular():
			a.Path = e.Name()
			if info, err := e.Info(); err == nil {
				a.Size = info.Size()
			}
		default:
			continue // a link or device names nothing the case received
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Source) Root(ctx context.Context, caseID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := engine.CheckCaseID(caseID); err != nil {
		return "", err
	}
	path := s.dir
	if path == "" {
		path = filepath.Join(s.base, caseID)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("case %s: %w", caseID, err)
	}
	// Resolved here, once: the runner and primitives then confine
	// themselves to the real directory rather than a link into it.
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("case %s: %w: %s does not exist or is a broken link", caseID, engine.ErrNoArtifacts, abs)
	}
	if err != nil {
		return "", fmt.Errorf("case %s: cannot resolve artifact directory %s: %w", caseID, abs, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("case %s: artifact directory %s: %w", caseID, real, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("case %s: %w: %s is not a directory", caseID, engine.ErrNoArtifacts, real)
	}
	// Stat succeeds on a directory the engineer cannot read; opening it
	// finds that now rather than in the middle of collection.
	f, err := os.Open(real)
	if err != nil {
		return "", fmt.Errorf("case %s: cannot read artifact directory %s: %w; check its permissions or ACLs", caseID, real, err)
	}
	f.Close()
	return real, nil
}

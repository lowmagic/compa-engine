package collect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// StoreDir is the case store's directory. It sits in the case folder, which
// is the artifact root on the deployment host, so collection never reads it.
const StoreDir = ".compa"

var errExcluded = errors.New("is excluded from collection")

// errCredentials refuses a file that holds credentials and nothing a
// diagnosis needs.
var errCredentials = errors.New("holds only credentials and is never read")

// credentialsOnly reports a file the deployment names as holding
// credentials and nothing a diagnosis needs: a name matches a file of that
// base name, and a name with a directory, such as auth/kubeconfig, a file
// whose path ends with it, at any depth. A walk passes over them; naming
// one is refused. Files that hold secrets among what a diagnosis needs are
// read, and redaction on egress replaces their secrets.
func (a *artifactRoot) credentialsOnly(rel string) bool {
	// Gzipped, it is read decompressed, so it is the same file.
	rel = strings.TrimSuffix(rel, ".gz")
	for _, name := range a.credentials {
		if rel == name || strings.HasSuffix(rel, "/"+name) {
			return true
		}
	}
	return false
}

// artifactRoot gives read-only access to files beneath the investigation's
// artifact root. os.Root refuses any path, including through a symlink,
// that leaves the root.
type artifactRoot struct {
	root *os.Root
	// exclude names entries directly under the root that collection never
	// reads. Only the top level: every sandbox mechanism can deny a fixed
	// path, and none can match a name at any depth without a walk.
	exclude map[string]bool
	// credentials name the files that hold only credentials, at any depth.
	credentials []string
	// read records every file path read, relative to the root.
	read map[string]bool
}

func openArtifactRoot(dir string, exclude, credentials []string) (*artifactRoot, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	a := &artifactRoot{root: r, exclude: map[string]bool{}, credentials: credentials, read: map[string]bool{}}
	for _, name := range exclude {
		a.exclude[name] = true
	}
	return a, nil
}

// CheckExclude reports whether name can be excluded: one entry directly
// under the artifact root.
func CheckExclude(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("exclusion %q must name one entry directly under the artifact root", name)
	}
	return nil
}

func (a *artifactRoot) excludedTop(rel string) bool {
	top, _, _ := strings.Cut(rel, "/")
	return a.exclude[top]
}

// path cleans p and refuses it when it, or the path its symlinks resolve
// to, is excluded. A customer archive may carry a relative link into the
// store, which os.Root would follow since the store is inside the root.
func (a *artifactRoot) path(p string) (string, error) {
	c, err := clean(p)
	if err != nil {
		return "", err
	}
	real, err := a.resolve(c)
	if err != nil {
		return "", err
	}
	if a.excludedTop(c) || a.excludedTop(real) {
		return "", fmt.Errorf("%s %w", p, errExcluded)
	}
	if a.credentialsOnly(c) || a.credentialsOnly(real) {
		return "", fmt.Errorf("%s %w", p, errCredentials)
	}
	return c, nil
}

// maxLinks bounds symlink resolution, as the kernel's own limit does.
const maxLinks = 40

// resolve follows c's symlinks within the root, returning where it leads.
// Missing components are kept as written, for the open that follows to
// report.
func (a *artifactRoot) resolve(c string) (string, error) {
	var done []string
	todo := strings.Split(c, "/")
	for links := 0; len(todo) > 0; {
		next := todo[0]
		todo = todo[1:]
		switch next {
		case "", ".":
			continue
		case "..":
			if len(done) == 0 {
				return "", fmt.Errorf("%s leads out of the artifact root", c)
			}
			done = done[:len(done)-1]
			continue
		}
		cur := path.Join(append(done, next)...)
		info, err := a.root.Lstat(cur)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			done = append(done, next)
			continue
		}
		if links++; links > maxLinks {
			return "", fmt.Errorf("%s: too many levels of symbolic links", c)
		}
		target, err := a.root.Readlink(cur)
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			return "", fmt.Errorf("%s links outside the artifact root", cur)
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	if len(done) == 0 {
		return ".", nil
	}
	return path.Join(done...), nil
}

func (a *artifactRoot) Close() error { return a.root.Close() }

// clean validates a path parameter: relative, slash-separated, and inside
// the root. "" means the root itself.
func clean(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if path.IsAbs(p) || strings.Contains(p, "\\") {
		return "", fmt.Errorf("path %q must be relative to the artifact root", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q escapes the artifact root", p)
	}
	return c, nil
}

func (a *artifactRoot) stat(p string) (fs.FileInfo, error) {
	c, err := a.path(p)
	if err != nil {
		return nil, err
	}
	return a.root.Stat(c)
}

// open opens a regular file for reading and records that it was read. It
// opens without blocking, so a FIFO or a device among the artifacts is
// refused as not a regular file rather than holding the run until a
// writer appears, which no timeout could interrupt.
func (a *artifactRoot) open(p string) (*os.File, error) {
	c, err := a.path(p)
	if err != nil {
		return nil, err
	}
	f, err := a.root.OpenFile(c, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", c)
	}
	a.read[c] = true
	return f, nil
}

// readAll reads a whole file of at most limit bytes, stopping when ctx is
// done.
func (a *artifactRoot) readAll(ctx context.Context, p string, limit int64) ([]byte, error) {
	f, err := a.open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(ctxReader{ctx, io.LimitReader(f, limit+1)})
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return data, nil
}

// files lists regular files under dir whose base name matches glob, sorted.
// A dir that is itself a file is returned alone. Symlinks are not followed
// while walking.
func (a *artifactRoot) files(dir, glob string) ([]string, error) {
	c, err := a.path(dir)
	if err != nil {
		return nil, err
	}
	if glob == "" {
		glob = "*"
	}
	if _, err := path.Match(glob, ""); err != nil {
		return nil, fmt.Errorf("glob %q: %w", glob, err)
	}
	info, err := a.root.Stat(c)
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		return []string{c}, nil
	}
	var out []string
	err = fs.WalkDir(a.root.FS(), c, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if a.excludedTop(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !a.credentialsOnly(p) {
			if ok, _ := path.Match(glob, d.Name()); ok {
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

func (a *artifactRoot) paths() []string {
	out := make([]string, 0, len(a.read))
	for p := range a.read {
		out = append(out, p)
	}
	return out
}

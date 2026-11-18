package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/klauspost/compress/zstd"
)

// ErrTooLarge is returned for content over the store's blob limit.
var ErrTooLarge = errors.New("store: blob exceeds the limit")

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.dir, "blobs", "sha256", digest[:2], digest+".zst")
}

// PutBlob stores data and returns its digest, the hex SHA-256 of the
// content. Identical content is stored once however often it is put. The
// file is written whole and renamed into place, so a reader never sees a
// partial blob.
func (s *Store) PutBlob(data []byte) (string, error) {
	if int64(len(data)) > s.maxBlob {
		return "", fmt.Errorf("store: blob of %d bytes exceeds the limit of %d", len(data), s.maxBlob)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	path := s.blobPath(digest)
	if _, err := os.Stat(path); err == nil {
		return digest, s.index(digest, int64(len(data)), path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "put-*")
	if err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(s.enc.EncodeAll(data, nil)); err != nil {
		tmp.Close()
		return "", fmt.Errorf("store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	return digest, s.index(digest, int64(len(data)), path)
}

// PutBlobFrom stores what r yields, hashing and compressing it as it reads,
// so content of any size within the limit never sits in memory whole. It
// returns the digest and the content's size. Content over the limit is
// ErrTooLarge and leaves nothing behind.
func (s *Store) PutBlobFrom(r io.Reader) (string, int64, error) {
	tmpDir := filepath.Join(s.dir, "blobs", "sha256")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	defer tmp.Close()
	// One goroutine and the default window: the encoder's memory then
	// stays a few megabytes whatever the content's size.
	enc, err := zstd.NewWriter(tmp, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(enc, h), io.LimitReader(r, s.maxBlob+1))
	if err != nil {
		enc.Close()
		return "", 0, fmt.Errorf("store: %w", err)
	}
	if n > s.maxBlob {
		enc.Close()
		return "", 0, fmt.Errorf("%w of %d bytes", ErrTooLarge, s.maxBlob)
	}
	if err := enc.Close(); err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("store: %w", err)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	path := s.blobPath(digest)
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", 0, fmt.Errorf("store: %w", err)
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return "", 0, fmt.Errorf("store: %w", err)
		}
	}
	return digest, n, s.index(digest, n, path)
}

// OpenBlob streams the content stored under digest. The digest is checked
// when the reader reaches the end, which then returns an error rather than
// io.EOF if the blob is damaged; a reader closed early has checked nothing.
func (s *Store) OpenBlob(digest string) (io.ReadCloser, error) {
	if !digestPattern.MatchString(digest) {
		return nil, fmt.Errorf("store: %q is not a blob digest", digest)
	}
	f, err := os.Open(s.blobPath(digest))
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", digest, err)
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(s.maxBlob)))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("store: blob %s: %w", digest, err)
	}
	return &blobReader{digest: digest, f: f, dec: dec, h: sha256.New()}, nil
}

type blobReader struct {
	digest string
	f      *os.File
	dec    *zstd.Decoder
	h      hash.Hash
}

func (b *blobReader) Read(p []byte) (int, error) {
	n, err := b.dec.Read(p)
	b.h.Write(p[:n])
	if errors.Is(err, io.EOF) && !bytes.Equal(b.h.Sum(nil), mustHex(b.digest)) {
		return n, fmt.Errorf("store: blob %s is damaged: its content does not match its digest", b.digest)
	}
	return n, err
}

func (b *blobReader) Close() error {
	b.dec.Close()
	return b.f.Close()
}

func (s *Store) index(digest string, size int64, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	_, err = s.q.Exec(`INSERT INTO blobs (digest, size, stored_size, created_at) VALUES (?, ?, ?, ?) ON CONFLICT (digest) DO NOTHING`,
		digest, size, fi.Size(), s.stamp())
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Blob returns the content stored under digest. It decompresses no more
// than the store's blob limit, and checks the content against its digest,
// so a damaged blob is an error rather than wrong evidence.
func (s *Store) Blob(digest string) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, fmt.Errorf("store: %q is not a blob digest", digest)
	}
	compressed, err := os.ReadFile(s.blobPath(digest))
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", digest, err)
	}
	data, err := s.dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", digest, err)
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], mustHex(digest)) {
		return nil, fmt.Errorf("store: blob %s is damaged: its content does not match its digest", digest)
	}
	return data, nil
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

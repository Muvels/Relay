package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// BlobStore is a content-addressed store on disk: blobs/<aa>/<sha256>.
// Everything that moves between SDK, server, and agents is one of these:
// code bundles, pickled calls, or results.
type BlobStore struct {
	root string
}

var shaRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

func NewBlobStore(root string) (*BlobStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &BlobStore{root: root}, nil
}

func (b *BlobStore) path(sha string) (string, error) {
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("invalid blob id %q", sha)
	}
	return filepath.Join(b.root, sha[:2], sha), nil
}

func (b *BlobStore) Has(sha string) bool {
	p, err := b.path(sha)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func (b *BlobStore) Size(sha string) (int64, error) {
	p, err := b.path(sha)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (b *BlobStore) Open(sha string) (io.ReadCloser, error) {
	p, err := b.path(sha)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("blob %s: %w", sha[:12], ErrNotFound)
	}
	return f, err
}

// Write stores the stream and verifies it hashes to expected (when given).
// Returns the actual sha256.
func (b *BlobStore) Write(r io.Reader, expected string) (string, int64, error) {
	tmp, err := os.CreateTemp(b.root, ".upload-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	if expected != "" && !shaRe.MatchString(expected) {
		return "", 0, fmt.Errorf("invalid declared blob id %q", expected)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", 0, err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if expected != "" && sha != expected {
		return "", 0, fmt.Errorf(
			"blob hash mismatch: declared %s, received %s", expected[:12], sha[:12])
	}
	p, err := b.path(sha)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil && !errors.Is(err, os.ErrExist) {
		return "", 0, err
	}
	return sha, n, nil
}

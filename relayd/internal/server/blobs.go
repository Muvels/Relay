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
	"strings"
	"sync"
	"time"
)

// BlobStore is a content-addressed store on disk: blobs/<aa>/<sha256>.
// Everything that moves between SDK, server, and agents is one of these:
// code bundles, pickled calls, or results.
type BlobStore struct {
	root string

	mu sync.Mutex
	// leases records blobs a caller has just proven present or written, and
	// is therefore about to reference. Reachability alone cannot protect
	// them: every path that creates a reference (submit a run, deploy a
	// service, upsert a schedule, record a result) checks the blob FIRST and
	// writes the referencing row a moment later, so a sweep in between sees
	// a blob no row mentions. File mtime cannot stand in for this either,
	// because a content-addressed store deliberately does not rewrite a blob
	// whose content it already has, so a reused blob keeps its original,
	// arbitrarily old timestamp.
	leases map[string]time.Time
}

var shaRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

func NewBlobStore(root string) (*BlobStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &BlobStore{root: root, leases: map[string]time.Time{}}, nil
}

// lease marks a blob as about to be referenced. Cheap enough to call on
// every existence check, which is what makes it reliable: the check is the
// one step every reference-creating path has in common.
func (b *BlobStore) lease(sha string) {
	b.mu.Lock()
	b.leases[sha] = time.Now()
	b.mu.Unlock()
}

func (b *BlobStore) leasedSince(sha string, cutoff time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.leases[sha]
	return ok && at.After(cutoff)
}

// dropExpiredLeases keeps the map proportional to recent activity rather
// than to the lifetime of the process.
func (b *BlobStore) dropExpiredLeases(cutoff time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sha, at := range b.leases {
		if !at.After(cutoff) {
			delete(b.leases, sha)
		}
	}
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
	if _, err = os.Stat(p); err != nil {
		return false
	}
	b.lease(sha)
	return true
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
	// Link is an atomic create-if-absent on the same filesystem. Unlike Rename
	// on Unix it never replaces an identical blob another upload already put
	// in place, avoiding needless data and metadata writes during races.
	if err := os.Link(tmp.Name(), p); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", 0, err
		}
	}
	// Whoever wrote it is about to reference it, and on the already-exists
	// path there is no fresh mtime to show for it.
	b.lease(sha)
	return sha, n, nil
}

// Sweep removes blobs that no stored run or schedule references any more.
// keep must be COMPLETE: callers pass the store's full reference set, and a
// failed query must skip the sweep rather than hand over a partial one.
//
// cutoff spares blobs written or leased more recently than that, regardless
// of reachability. Both matter: the SDK uploads a bundle before submitting
// the run that names it, and when the store already has that content it is
// not rewritten at all, so only the lease marks it as spoken for.
func (b *BlobStore) Sweep(keep map[string]bool, cutoff time.Time) (int, error) {
	shards, err := os.ReadDir(b.root)
	if err != nil {
		return 0, err
	}
	defer b.dropExpiredLeases(cutoff)
	removed := 0
	for _, shard := range shards {
		if !shard.IsDir() {
			// Interrupted uploads leave .upload-* temp files behind.
			if strings.HasPrefix(shard.Name(), ".upload-") && olderThan(b.root, shard, cutoff) {
				if os.Remove(filepath.Join(b.root, shard.Name())) == nil {
					removed++
				}
			}
			continue
		}
		dir := filepath.Join(b.root, shard.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			// Anything that is not a well-formed blob id was not written by
			// path(), so it is not ours to delete.
			if entry.IsDir() || !shaRe.MatchString(name) || keep[name] {
				continue
			}
			if !olderThan(dir, entry, cutoff) || b.leasedSince(name, cutoff) {
				continue
			}
			if os.Remove(filepath.Join(dir, name)) == nil {
				removed++
			}
		}
		// The shard directory itself is deliberately left in place. Write()
		// does MkdirAll then Link, so removing an empty shard between those
		// two calls would fail a concurrent upload for the sake of 256
		// directory entries.
	}
	return removed, nil
}

func olderThan(dir string, entry os.DirEntry, cutoff time.Time) bool {
	info, err := entry.Info()
	if err != nil {
		return false
	}
	return info.ModTime().Before(cutoff)
}

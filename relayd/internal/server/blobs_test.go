package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestBlobWriteDoesNotReplaceExistingContent(t *testing.T) {
	store, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("same content")
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if _, _, err := store.Write(bytes.NewReader(data), want); err != nil {
		t.Fatal(err)
	}
	path, _ := store.path(want)
	old := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Write(bytes.NewReader(data), want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("duplicate blob replaced existing file: mtime=%s", info.ModTime())
	}
}

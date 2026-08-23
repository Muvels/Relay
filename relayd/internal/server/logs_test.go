package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogHubPersistsBatch(t *testing.T) {
	dir := t.TempDir()
	hub, err := NewLogHub(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub.Append("run_1", []string{"one", "two", "three"})
	hub.Close("run_1")
	got, err := os.ReadFile(filepath.Join(dir, "run_1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\ntwo\nthree\n" {
		t.Fatalf("unexpected log contents %q", got)
	}
}

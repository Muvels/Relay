package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupStaleRunDirsPreservesPersistentData(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "runs", "run_old", "workspace")
	volume := filepath.Join(root, "volumes", "models", "checkpoint.bin")
	venv := filepath.Join(root, "venvs", "abc", ".ready")
	for _, path := range []string{stale, filepath.Dir(volume), filepath.Dir(venv)} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(volume, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venv, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleRunDirs(root); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "runs")); err != nil || len(entries) != 0 {
		t.Fatalf("run scratch not empty: entries=%v err=%v", entries, err)
	}
	for _, path := range []string{volume, venv} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("persistent path %s was touched: %v", path, err)
		}
	}
}

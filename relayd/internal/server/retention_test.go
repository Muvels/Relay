package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRetentionFixture(t *testing.T) (*Store, *LogHub, *BlobStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ensureScheduleTable(); err != nil {
		t.Fatal(err)
	}
	logs, err := NewLogHub(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return store, logs, blobs
}

func putBlobForTest(t *testing.T, blobs *BlobStore, content string, age time.Duration) string {
	t.Helper()
	sha, _, err := blobs.Write(strings.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := blobs.path(sha)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	// Writing a blob leases it, so age the lease with the file: "this blob is
	// N old" has to mean nobody has touched it for N either.
	blobs.mu.Lock()
	blobs.leases[sha] = when
	blobs.mu.Unlock()
	return sha
}

func specWithBlobs(t *testing.T, bundle, args string) string {
	t.Helper()
	out, err := json.Marshal(RunSpecJSON{
		App: "app", Function: "fn", EntryFile: "m.py",
		BundleSha: bundle, ArgsSha: args,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func backdateRun(t *testing.T, s *Store, id string, age time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE runs SET updated_at=? WHERE id=?`,
		time.Now().Add(-age).Unix(), id); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionDropsExpiredRunsWithTheirLogsAndBlobs(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)
	bundle := putBlobForTest(t, blobs, "old bundle", 90*24*time.Hour)
	args := putBlobForTest(t, blobs, "old args", 90*24*time.Hour)

	run, err := store.CreateRun("app", "fn", "job", specWithBlobs(t, bundle, args))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionRun(run.ID, nil, "succeeded", "", "m1", "", 0); err != nil {
		t.Fatal(err)
	}
	logs.Append(run.ID, []string{"a line"})
	logs.Close(run.ID)
	backdateRun(t, store, run.ID, 60*24*time.Hour)

	deletedRuns, deletedBlobs := sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 30 * 24 * time.Hour}, time.Now())
	if deletedRuns != 1 {
		t.Fatalf("deleted %d runs, want 1", deletedRuns)
	}
	if deletedBlobs != 2 {
		t.Fatalf("deleted %d blobs, want 2", deletedBlobs)
	}
	if _, err := store.GetRun(run.ID); err == nil {
		t.Fatal("expired run row survived")
	}
	if _, err := os.Stat(logs.filePath(run.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired run's log file survived: %v", err)
	}
	if blobs.Has(bundle) {
		t.Fatal("blob survived after its last referencing run was deleted")
	}
}

// The TTL applies to settled runs only. A service that has been up for
// months is older than any retention window and must never be swept.
func TestRetentionNeverDeletesLiveRuns(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)
	bundle := putBlobForTest(t, blobs, "service bundle", 90*24*time.Hour)
	args := putBlobForTest(t, blobs, "service args", 90*24*time.Hour)

	run, err := store.CreateRun("app", "svc", "service", specWithBlobs(t, bundle, args))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionRun(run.ID, nil, "running", "", "m1", "", 0); err != nil {
		t.Fatal(err)
	}
	backdateRun(t, store, run.ID, 365*24*time.Hour)

	deletedRuns, deletedBlobs := sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 30 * 24 * time.Hour}, time.Now())
	if deletedRuns != 0 || deletedBlobs != 0 {
		t.Fatalf("swept a live run: runs=%d blobs=%d", deletedRuns, deletedBlobs)
	}
	if _, err := store.GetRun(run.ID); err != nil {
		t.Fatalf("live run row was deleted: %v", err)
	}
	if !blobs.Has(bundle) || !blobs.Has(args) {
		t.Fatal("live run's blobs were swept")
	}
}

// A schedule's bundle has no run pointing at it between firings, and the gap
// can be longer than any TTL. Reachability must include schedule specs.
func TestRetentionKeepsBlobsAScheduleStillNeeds(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)
	bundle := putBlobForTest(t, blobs, "cron bundle", 90*24*time.Hour)
	args := putBlobForTest(t, blobs, "cron args", 90*24*time.Hour)
	if _, err := store.UpsertSchedule("app", "nightly", "0 3 * * *",
		specWithBlobs(t, bundle, args)); err != nil {
		t.Fatal(err)
	}

	if _, removed := sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 30 * 24 * time.Hour}, time.Now()); removed != 0 {
		t.Fatalf("swept %d blobs a schedule still references", removed)
	}
	if !blobs.Has(bundle) || !blobs.Has(args) {
		t.Fatal("schedule's blobs were swept")
	}
}

// The SDK uploads a bundle and only then submits the run naming it. A sweep
// landing inside that window must not delete the upload.
func TestRetentionSparesFreshlyUploadedBlobs(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)
	justUploaded := putBlobForTest(t, blobs, "in flight", 0)

	if _, removed := sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 30 * 24 * time.Hour, BlobGrace: time.Hour},
		time.Now()); removed != 0 {
		t.Fatalf("swept %d blobs inside the upload grace window", removed)
	}
	if !blobs.Has(justUploaded) {
		t.Fatal("an upload still waiting for its run was deleted")
	}
}

func TestRetentionDisabledKeepsEverything(t *testing.T) {
	store, logs, blobs := newRetentionFixture(t)
	bundle := putBlobForTest(t, blobs, "ancient", 400*24*time.Hour)
	run, err := store.CreateRun("app", "fn", "job", specWithBlobs(t, bundle, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionRun(run.ID, nil, "succeeded", "", "m1", "", 0); err != nil {
		t.Fatal(err)
	}
	backdateRun(t, store, run.ID, 400*24*time.Hour)

	deletedRuns, deletedBlobs := sweepRetention(store, logs, blobs,
		RetentionConfig{RunTTL: 0}, time.Now())
	if deletedRuns != 0 || deletedBlobs != 0 {
		t.Fatalf("a zero TTL deleted data: runs=%d blobs=%d", deletedRuns, deletedBlobs)
	}
	if _, err := store.GetRun(run.ID); err != nil {
		t.Fatalf("run vanished with retention disabled: %v", err)
	}
	if !blobs.Has(bundle) {
		t.Fatal("blob vanished with retention disabled")
	}
}

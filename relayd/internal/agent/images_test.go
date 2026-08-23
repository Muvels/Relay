package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker serves the Engine API over a unix socket, which is the only
// transport dockerClient speaks. The socket lives under /tmp because the
// sun_path limit is ~100 bytes and the usual temp roots are longer.
func fakeDocker(t *testing.T, handler http.HandlerFunc) *dockerClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "relayd-dock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return newDockerClient(sock)
}

// staticInUse is the exemption set as a fixed answer.
func staticInUse(tags ...string) func() map[string]bool {
	set := map[string]bool{}
	for _, tag := range tags {
		set[tag] = true
	}
	return func() map[string]bool { return set }
}

type fakeImage struct {
	Id       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  int64    `json:"Created"`
	Size     int64    `json:"Size"`
}

// imageDaemon answers /images/json from the given set and records deletes.
func imageDaemon(t *testing.T, images []fakeImage) (*dockerClient, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var deleted []string
	client := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode(images)
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path,
				"/"+dockerAPIVersion+"/images/"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deleted...)
	}
}

func TestSweepEvictsColdImagesAndKeepsWarmOnes(t *testing.T) {
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	client, deletes := imageDaemon(t, []fakeImage{
		{Id: "sha256:a", RepoTags: []string{relayImagePrefix + "cold"}, Created: old, Size: 8 << 30},
		{Id: "sha256:b", RepoTags: []string{relayImagePrefix + "warm"}, Created: old, Size: 8 << 30},
		{Id: "sha256:c", RepoTags: []string{relayImagePrefix + "busy"}, Created: old, Size: 8 << 30},
	})
	cache := newImageCache(t.TempDir())
	// "warm" was built long ago but is used daily; age since build must not
	// be what decides its fate.
	cache.Touch(relayImagePrefix + "warm")

	inUse := staticInUse(relayImagePrefix + "busy")
	removed, freed := sweepImages(context.Background(), client, cache,
		14*24*time.Hour, inUse)

	if removed != 1 {
		t.Fatalf("removed %d images, want only the cold one", removed)
	}
	if freed != 8<<30 {
		t.Fatalf("freed %d bytes, want %d", freed, int64(8)<<30)
	}
	got := deletes()
	if len(got) != 1 || got[0] != relayImagePrefix+"cold" {
		t.Fatalf("deleted %v, want just the cold image", got)
	}
}

func TestSweepSkipsEverythingWhenRetentionIsGenerous(t *testing.T) {
	client, deletes := imageDaemon(t, []fakeImage{
		{Id: "sha256:a", RepoTags: []string{relayImagePrefix + "recent"},
			Created: time.Now().Add(-2 * 24 * time.Hour).Unix()},
	})
	cache := newImageCache(t.TempDir())
	if removed, _ := sweepImages(context.Background(), client, cache,
		14*24*time.Hour, staticInUse()); removed != 0 {
		t.Fatalf("removed %d images inside the retention window", removed)
	}
	if got := deletes(); len(got) != 0 {
		t.Fatalf("deleted %v inside the retention window", got)
	}
}

// Non-Relay images are not ours to delete, however old they are.
func TestSweepIgnoresImagesRelayDidNotBuild(t *testing.T) {
	old := time.Now().Add(-365 * 24 * time.Hour).Unix()
	client, deletes := imageDaemon(t, []fakeImage{
		{Id: "sha256:x", RepoTags: []string{"postgres:16"}, Created: old},
		{Id: "sha256:y", RepoTags: []string{"nvidia/cuda:12.4.0-base"}, Created: old},
	})
	cache := newImageCache(t.TempDir())
	if removed, _ := sweepImages(context.Background(), client, cache,
		time.Hour, staticInUse()); removed != 0 {
		t.Fatalf("removed %d foreign images", removed)
	}
	if got := deletes(); len(got) != 0 {
		t.Fatalf("deleted foreign images %v", got)
	}
}

func TestImageUsageSurvivesAgentRestart(t *testing.T) {
	dir := t.TempDir()
	cache := newImageCache(dir)
	cache.Touch(relayImagePrefix + "hot")

	restarted := newImageCache(dir)
	created := LocalImage{Tag: relayImagePrefix + "hot",
		CreatedAt: time.Now().Add(-90 * 24 * time.Hour)}
	if time.Since(restarted.lastUsed(created)) > time.Hour {
		t.Fatal("usage record lost across restart; a hot image would be evicted")
	}
}

func TestImageUsageWritesAreDebounced(t *testing.T) {
	dir := t.TempDir()
	cache := newImageCache(dir)
	cache.Touch(relayImagePrefix + "a")
	usageFile := filepath.Join(dir, "image-usage.json")
	if _, err := os.Stat(usageFile); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(usageFile, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for range 50 { // a burst of short runs on the same image
		cache.Touch(relayImagePrefix + "a")
	}
	after, err := os.Stat(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(stamp) {
		t.Fatal("a burst of runs rewrote the usage file instead of coalescing")
	}
}

func TestForgetDropsVanishedTags(t *testing.T) {
	cache := newImageCache(t.TempDir())
	cache.Touch(relayImagePrefix + "gone")
	cache.Touch(relayImagePrefix + "here")
	if !cache.forget(map[string]bool{relayImagePrefix + "here": true}) {
		t.Fatal("forget reported no change despite a vanished tag")
	}
	cache.mu.Lock()
	_, stale := cache.used[relayImagePrefix+"gone"]
	_, kept := cache.used[relayImagePrefix+"here"]
	cache.mu.Unlock()
	if stale {
		t.Fatal("usage record kept for an image that no longer exists")
	}
	if !kept {
		t.Fatal("forget dropped a record for an image that still exists")
	}
}

// The debounce must be measured from the last WRITE. Measured from the last
// use, an image running more often than the floor never crosses it, so it is
// written once and then never again: the file goes on claiming the original
// timestamp while the image is in fact the hottest one on the machine.
func TestUsageIsPersistedAgainOncePastTheFloor(t *testing.T) {
	dir := t.TempDir()
	cache := newImageCache(dir)
	cache.floor = time.Millisecond
	usageFile := filepath.Join(dir, "image-usage.json")

	cache.Touch(relayImagePrefix + "hot")
	first, err := os.Stat(usageFile)
	if err != nil {
		t.Fatal(err)
	}

	// Steady use, each touch closer together than the floor.
	for range 5 {
		time.Sleep(2 * time.Millisecond)
		cache.Touch(relayImagePrefix + "hot")
	}

	after, err := os.Stat(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	if after.ModTime().Equal(first.ModTime()) {
		t.Fatal("steady use never reached disk; a restart would call it cold")
	}
	restarted := newImageCache(dir)
	stale := LocalImage{Tag: relayImagePrefix + "hot",
		CreatedAt: time.Now().Add(-90 * 24 * time.Hour)}
	if time.Since(restarted.lastUsed(stale)) > time.Minute {
		t.Fatal("persisted usage is stale: the hot image would be evicted")
	}
}

// A run touch and a sweep can persist at the same time. A torn write is not
// harmless here: load() discards a corrupt file, which reseeds every image
// from its build date and evicts the warm ones.
func TestConcurrentPersistKeepsTheFileReadable(t *testing.T) {
	dir := t.TempDir()
	cache := newImageCache(dir)
	cache.floor = -time.Second // every touch persists

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Touch(relayImagePrefix + strconv.Itoa(i))
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(dir, "image-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]int64
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("usage file is not valid JSON after concurrent writes: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("usage file ended up empty")
	}
}

// Listing images is a round trip to the daemon, and a run that starts during
// it registers itself before the sweep gets to the delete. So the exemption
// set must be READ during the loop, not snapshotted before the listing --
// which is what a plain map argument would have been.
func TestSweepReadsTheExemptionSetAfterListing(t *testing.T) {
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	var mu sync.Mutex
	listed := false
	var deleted []string

	client := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/json"):
			mu.Lock()
			listed = true // the run starts while this call is in flight
			mu.Unlock()
			_ = json.NewEncoder(w).Encode([]fakeImage{{Id: "sha256:a",
				RepoTags: []string{relayImagePrefix + "claimed"}, Created: old}})
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	inUse := func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		if !listed {
			return map[string]bool{} // nothing had claimed it yet
		}
		return map[string]bool{relayImagePrefix + "claimed": true}
	}

	removed, _ := sweepImages(context.Background(), client,
		newImageCache(t.TempDir()), 14*24*time.Hour, inUse)
	if removed != 0 {
		t.Fatal("deleted an image a run claimed during the listing")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 0 {
		t.Fatalf("deleted %v despite a live claim", deleted)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// relayImagePrefix tags every image Relay builds. The tag is the content
// hash of the Dockerfile, so each distinct dependency set mints a new one
// and nothing ever overwrites an old one. On a machine that stays up, that
// is an unbounded pile of multi-gigabyte CUDA images unless something
// evicts them.
const relayImagePrefix = "relay-img:"

// DefaultImageTTL evicts an image only after it has gone unused this long.
// Age since BUILD would be the wrong metric: the base image you run every
// day was still built once, months ago.
const DefaultImageTTL = 14 * 24 * time.Hour

// imageUsagePersistFloor debounces the usage file: at most one small write
// per window however many runs happen, so a burst of a hundred short runs
// costs one write. It is measured from the last WRITE, not from the last
// use. Measuring from the last use would mean an image running every minute
// never crosses the floor and so never gets written again after the first
// time, leaving the file to claim weeks of idleness for the hottest image
// on the machine.
const imageUsagePersistFloor = 5 * time.Minute

// imageCache records when each Relay-built image was last used for a run.
// It lives beside the agent's other state so the record survives restarts;
// without that, every agent restart would look like "never used" and either
// evict hot images or (seeding from now) never evict anything.
type imageCache struct {
	path  string
	floor time.Duration

	mu          sync.Mutex
	used        map[string]time.Time
	persistedAt time.Time

	// writeMu serializes whole persist operations. The snapshot is taken
	// under it too, so a slow writer cannot publish a stale generation over
	// a newer one.
	writeMu sync.Mutex
}

func newImageCache(stateDir string) *imageCache {
	c := &imageCache{
		path:  filepath.Join(stateDir, "image-usage.json"),
		floor: imageUsagePersistFloor,
		used:  map[string]time.Time{},
	}
	c.load()
	return c
}

func (c *imageCache) load() {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var stored map[string]int64
	if json.Unmarshal(data, &stored) != nil {
		return // a corrupt cache reseeds from image creation times
	}
	for tag, unix := range stored {
		c.used[tag] = time.Unix(unix, 0)
	}
}

func (c *imageCache) persist() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	stored := make(map[string]int64, len(c.used))
	for tag, at := range c.used {
		stored[tag] = at.Unix()
	}
	c.persistedAt = time.Now()
	c.mu.Unlock()

	data, err := json.Marshal(stored)
	if err != nil {
		return
	}
	// Write-and-rename: a torn file would be discarded on load, which resets
	// every image to its build date and evicts the warm ones.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("could not persist image usage", "err", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		slog.Warn("could not publish image usage", "err", err)
		_ = os.Remove(tmp)
	}
}

// Touch records that a run just used tag.
func (c *imageCache) Touch(tag string) {
	if tag == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.used[tag] = now
	due := now.Sub(c.persistedAt) > c.floor
	c.mu.Unlock()
	if due {
		c.persist()
	}
}

// lastUsed falls back to the image's creation time so images that predate
// this cache (or a wiped state dir) still age out instead of living forever.
func (c *imageCache) lastUsed(img LocalImage) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.used[img.Tag]; ok {
		return at
	}
	c.used[img.Tag] = img.CreatedAt
	return img.CreatedAt
}

// forget drops bookkeeping for tags that no longer exist locally, so the
// usage file cannot outgrow the image set it describes.
func (c *imageCache) forget(existing map[string]bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for tag := range c.used {
		if !existing[tag] {
			delete(c.used, tag)
			changed = true
		}
	}
	return changed
}

// sweepImages removes Relay-built images unused for longer than ttl. inUse
// reports the images of runs live on this machine right now: those are
// exempt regardless of age, since a run still fetching its bundle has not
// created its container yet and Docker would not refuse the delete. It is a
// function rather than a snapshot so it can be re-read next to the delete.
func sweepImages(ctx context.Context, docker *dockerClient, cache *imageCache, ttl time.Duration, inUse func() map[string]bool) (removed int, freed int64) {
	images, err := docker.ListImages(ctx, relayImagePrefix)
	if err != nil {
		return 0, 0
	}
	existing := make(map[string]bool, len(images))
	for _, img := range images {
		existing[img.Tag] = true
	}
	cutoff := time.Now().Add(-ttl)
	for _, img := range images {
		// Read live, immediately before deleting, rather than from a snapshot
		// taken before the listing round trip: a run that started in the
		// meantime has already registered itself and touched the cache.
		if inUse()[img.Tag] || !cache.lastUsed(img).Before(cutoff) {
			continue
		}
		if err := docker.ImageRemove(ctx, img.Tag); err != nil {
			// Usually a container still referencing it. Retry next sweep.
			slog.Debug("image sweep skipped", "image", img.Tag, "err", err)
			continue
		}
		removed++
		freed += img.SizeBytes
		delete(existing, img.Tag)
		slog.Info("evicted unused image", "image", img.Tag,
			"unused_for", time.Since(cache.lastUsed(img)).Round(time.Hour))
	}
	if cache.forget(existing) || removed > 0 {
		cache.persist()
	}
	return removed, freed
}

// StartImageSweeper evicts cold images on an interval until ctx ends. ttl
// of zero disables eviction entirely.
func startImageSweeper(ctx context.Context, docker *dockerClient, cache *imageCache, ttl time.Duration, inUse func() map[string]bool) {
	if ttl <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Bounded per sweep. A daemon that accepts the socket but
				// never answers would otherwise pin this goroutine for the
				// life of the agent and stop all future eviction.
				sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				pingCtx, pingCancel := context.WithTimeout(sweepCtx, 5*time.Second)
				up := docker.Ping(pingCtx) == nil
				pingCancel()
				if up {
					if removed, freed := sweepImages(sweepCtx, docker, cache, ttl, inUse); removed > 0 {
						slog.Info("image sweep done", "removed", removed,
							"freed_mib", freed/(1<<20))
					}
				}
				cancel()
			}
		}
	}()
}

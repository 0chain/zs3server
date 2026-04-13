package zcn

import (
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// CacheManager provides unified cache policy for both NFS and S3 paths.
//
// Architecture:
//   NFS write → Ganesha → /nfs_export (tmpfs) → CacheManager tracks it
//   S3 write  → MinIO  → /mcache (tmpfs) → MinIO manages eviction
//
// The CacheManager handles:
//   1. Adaptive config: file-size-based tuning of batch/worker params
//   2. Eviction: remove committed files to bound cache usage
//   3. Cache routing: track which objects are in cache vs blobbers only
//
// Default eviction policy:
//   - After blobber commit: mark file eligible for eviction
//   - When cache > 80% full: evict oldest committed files (LRU)
//   - Never evict uncommitted files
//   - Files accessed in last 60s get a grace period (hot data)

// AdaptiveConfig returns optimal worker/batch settings based on file size.
// Called at startup and can be recalculated periodically.
type AdaptiveConfig struct {
	// S3 batch pipeline
	BatchSize     int // files per DoMultiOperation
	BatchWaitTime int // ms to wait for batch to fill
	BatchWorkers  int // goroutines processing batch channel

	// SDK settings
	UploadWorkers    int // concurrent chunk uploads per file
	LockedBlobbersCap int // concurrent WM locks per blobber

	// NFS sync
	NFSSyncWorkers int // inotify commit workers
	NFSBatchSize   int // files per NFS batch commit
}

// ConfigForFileSize returns optimal config for a given median file size.
func ConfigForFileSize(medianSizeBytes int64) AdaptiveConfig {
	switch {
	case medianSizeBytes <= 100*1024: // ≤100KB: small files, max obj/s
		return AdaptiveConfig{
			BatchSize:        25,
			BatchWaitTime:    10,
			BatchWorkers:     10,
			UploadWorkers:    4,
			LockedBlobbersCap: 10,
			NFSSyncWorkers:   8,
			NFSBatchSize:     25,
		}
	case medianSizeBytes <= 2*1024*1024: // 100KB-2MB: medium files
		return AdaptiveConfig{
			BatchSize:        10,
			BatchWaitTime:    50,
			BatchWorkers:     5,
			UploadWorkers:    8,
			LockedBlobbersCap: 5,
			NFSSyncWorkers:   4,
			NFSBatchSize:     10,
		}
	default: // >2MB: large files, max MB/s
		return AdaptiveConfig{
			BatchSize:        5,
			BatchWaitTime:    100,
			BatchWorkers:     3,
			UploadWorkers:    16,
			LockedBlobbersCap: 3,
			NFSSyncWorkers:   4,
			NFSBatchSize:     5,
		}
	}
}

// CacheEntry tracks a cached object's state.
type CacheEntry struct {
	Path       string
	Size       int64
	Committed  bool      // true after blobber confirms
	LastAccess time.Time // for LRU eviction
	CreatedAt  time.Time
}

// CacheTracker tracks objects in the NFS export cache.
// S3 uses MinIO's built-in cache management; this is for NFS only.
type CacheTracker struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry

	totalSize    atomic.Int64
	maxSize      int64 // max cache size in bytes (from tmpfs size)
	evictTarget  float64 // evict until this % full (default 0.6 = 60%)
	evictTrigger float64 // start eviction at this % (default 0.8 = 80%)

	// Stats
	TotalEvicted atomic.Int64
	TotalHits    atomic.Int64
	TotalMisses  atomic.Int64
}

// NewCacheTracker creates a tracker with the given max size.
func NewCacheTracker(maxSizeBytes int64) *CacheTracker {
	ct := &CacheTracker{
		entries:      make(map[string]*CacheEntry),
		maxSize:      maxSizeBytes,
		evictTarget:  0.6,
		evictTrigger: 0.8,
	}
	go ct.evictionLoop()
	return ct
}

// Track registers a new file in the cache.
func (ct *CacheTracker) Track(path string, size int64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if existing, ok := ct.entries[path]; ok {
		ct.totalSize.Add(-existing.Size)
	}
	ct.entries[path] = &CacheEntry{
		Path:       path,
		Size:       size,
		Committed:  false,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}
	ct.totalSize.Add(size)
}

// MarkCommitted marks a file as committed to blobbers (eligible for eviction).
func (ct *CacheTracker) MarkCommitted(path string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if e, ok := ct.entries[path]; ok {
		e.Committed = true
	}
}

// Touch updates last access time (for LRU).
func (ct *CacheTracker) Touch(path string) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if e, ok := ct.entries[path]; ok {
		e.LastAccess = time.Now()
		ct.TotalHits.Add(1)
	} else {
		ct.TotalMisses.Add(1)
	}
}

// Remove removes a file from tracking.
func (ct *CacheTracker) Remove(path string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if e, ok := ct.entries[path]; ok {
		ct.totalSize.Add(-e.Size)
		delete(ct.entries, path)
	}
}

// UsagePct returns current cache usage percentage.
func (ct *CacheTracker) UsagePct() float64 {
	if ct.maxSize == 0 {
		return 0
	}
	return float64(ct.totalSize.Load()) / float64(ct.maxSize) * 100
}

// evictionLoop periodically checks cache usage and evicts if needed.
func (ct *CacheTracker) evictionLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if ct.UsagePct() > ct.evictTrigger*100 {
			ct.evictOldest()
		}
	}
}

// evictOldest evicts the oldest committed files until usage drops below target.
func (ct *CacheTracker) evictOldest() {
	ct.mu.Lock()

	// Collect committed entries sorted by last access (oldest first)
	type candidate struct {
		path       string
		lastAccess time.Time
		size       int64
	}
	var candidates []candidate
	grace := time.Now().Add(-60 * time.Second)

	for path, e := range ct.entries {
		if e.Committed && e.LastAccess.Before(grace) {
			candidates = append(candidates, candidate{path, e.LastAccess, e.Size})
		}
	}
	ct.mu.Unlock()

	// Sort by last access (oldest first) — simple bubble sort, candidates are small
	for i := range candidates {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].lastAccess.Before(candidates[i].lastAccess) {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	// Evict until below target
	evicted := 0
	for _, c := range candidates {
		if ct.UsagePct() <= ct.evictTarget*100 {
			break
		}
		ct.Remove(c.path)
		ct.TotalEvicted.Add(1)
		evicted++
	}

	if evicted > 0 {
		log.Printf("[CacheManager] Evicted %d files (usage: %.0f%%)", evicted, ct.UsagePct())
	}
}

// --- File size tracking & adaptive config ---

const fileSizeRingCap = 1000

// FileSizeTracker keeps a ring buffer of recent file sizes for adaptive tuning.
type FileSizeTracker struct {
	mu    sync.Mutex
	buf   [fileSizeRingCap]int64
	pos   int
	count int
}

var fileSizeTracker FileSizeTracker

// trackFileSize records a file size into the ring buffer.
func trackFileSize(size int64) {
	if size <= 0 {
		return
	}
	fileSizeTracker.mu.Lock()
	fileSizeTracker.buf[fileSizeTracker.pos] = size
	fileSizeTracker.pos = (fileSizeTracker.pos + 1) % fileSizeRingCap
	if fileSizeTracker.count < fileSizeRingCap {
		fileSizeTracker.count++
	}
	fileSizeTracker.mu.Unlock()
}

// getMedianFileSize returns the median of tracked file sizes, or 0 if none.
func getMedianFileSize() int64 {
	fileSizeTracker.mu.Lock()
	n := fileSizeTracker.count
	if n == 0 {
		fileSizeTracker.mu.Unlock()
		return 0
	}
	tmp := make([]int64, n)
	copy(tmp, fileSizeTracker.buf[:n])
	fileSizeTracker.mu.Unlock()

	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	return tmp[n/2]
}

// activeConfig stores the current AdaptiveConfig (loaded via atomic.Value).
var activeConfig atomic.Value

// GetAdaptiveConfig returns the current adaptive config.
// Falls back to medium-file defaults if not yet initialized.
func GetAdaptiveConfig() AdaptiveConfig {
	if v := activeConfig.Load(); v != nil {
		return v.(AdaptiveConfig)
	}
	return ConfigForFileSize(512 * 1024) // default: medium
}

// StartAdaptiveLoop launches a goroutine that recalculates config every 30s
// based on the median of recently observed file sizes.
func StartAdaptiveLoop() {
	// Store initial config
	activeConfig.Store(ConfigForFileSize(512 * 1024))

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		lastCategory := sizeCategory(512 * 1024)
		for range ticker.C {
			median := getMedianFileSize()
			if median == 0 {
				continue
			}
			cat := sizeCategory(median)
			if cat != lastCategory {
				cfg := ConfigForFileSize(median)
				activeConfig.Store(cfg)
				lastCategory = cat
				log.Printf("[AdaptiveConfig] median=%dKB category=%s batch=%d workers=%d upload_workers=%d",
					median/1024, cat, cfg.BatchSize, cfg.BatchWorkers, cfg.UploadWorkers)
			}
		}
	}()
}

// sizeCategory returns a string label for the file size bucket.
func sizeCategory(size int64) string {
	switch {
	case size <= 100*1024:
		return "small"
	case size <= 2*1024*1024:
		return "medium"
	default:
		return "large"
	}
}

// DefaultCacheConfig returns recommended config as JSON documentation.
func DefaultCacheConfig() map[string]interface{} {
	return map[string]interface{}{
		"_comment":              "Cache configuration for NFS + S3 dual-access",
		"nfs_cache_mode":        "tmpfs",
		"nfs_ganesha_export_dir": "/nfs_export",
		"nfs_sync_workers":       8,
		"nfs_direct_threshold":   2097152,
		"nfs_cache_evict":        true,
		"nfs_spillover_dir":      "",
		"s3_direct_threshold":    0,
		"batch_workers":          5,
		"max_batch_size":         25,
		"batch_wait_time":        10,
		"upload_workers":         8,
		"download_workers":       64,
		"locked_blobbers_cap":    5,
		"enable_wal":             true,
	}
}

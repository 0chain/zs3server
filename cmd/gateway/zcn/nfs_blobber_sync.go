package zcn

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/fsnotify/fsnotify"
)

// BlobberSync watches the NFS export directory for changes and syncs them
// to blobbers via putFile(). This makes NFS-Ganesha eventually ACID:
//
//	NFS write → tmpfs (instant) → inotify → putFile → blobbers (async)
//	After blobber commit → delete from tmpfs (frees space)
//
// Spillover: when tmpfs is >80% full, new data spills to NVMe directory.
// Cache eviction: committed files are deleted from tmpfs to bound usage.
//
// Durability model (same as S3 writeback cache):
//   - Data in tmpfs/RAM survives process restart but NOT power loss
//   - Blobber commit makes it durable (erasure-coded across blobbers)
//   - Crash window: ~500ms between write and blobber commit start
type BlobberSync struct {
	exportDir   string // primary staging (tmpfs)
	spilloverDir string // secondary staging (NVMe), empty = disabled
	alloc       *sdk.Allocation
	watcher     *fsnotify.Watcher
	evictAfterCommit bool

	pendingMu sync.Mutex
	pending   map[string]time.Time

	committedMu sync.RWMutex
	committed   map[string]bool

	// stats
	totalCommitted atomic.Int64
	totalEvicted   atomic.Int64
	totalSpilled   atomic.Int64

	stopCh chan struct{}
}

// StartBlobberSync creates a BlobberSync watcher.
func StartBlobberSync(exportDir string, alloc *sdk.Allocation, workers int, spilloverDir string, evictAfterCommit bool) (*BlobberSync, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if spilloverDir != "" {
		if err := os.MkdirAll(spilloverDir, 0755); err != nil {
			return nil, fmt.Errorf("spillover dir: %w", err)
		}
	}

	bs := &BlobberSync{
		exportDir:        exportDir,
		spilloverDir:     spilloverDir,
		alloc:            alloc,
		watcher:          watcher,
		evictAfterCommit: evictAfterCommit,
		pending:          make(map[string]time.Time),
		committed:        make(map[string]bool),
		stopCh:           make(chan struct{}),
	}

	if err := bs.watchRecursive(exportDir); err != nil {
		watcher.Close()
		return nil, err
	}

	go bs.processEvents()

	if workers <= 0 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		go bs.commitWorker()
	}

	go bs.initialScan()

	if spilloverDir != "" {
		go bs.spilloverMonitor()
	}

	log.Printf("[NFS-Sync] Watching %s (spillover=%s, evict=%v, workers=%d)",
		exportDir, spilloverDir, evictAfterCommit, workers)
	return bs, nil
}

func (bs *BlobberSync) watchRecursive(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return bs.watcher.Add(path)
		}
		return nil
	})
}

func (bs *BlobberSync) processEvents() {
	for {
		select {
		case <-bs.stopCh:
			return
		case event, ok := <-bs.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			info, err := os.Stat(event.Name)
			if err != nil {
				continue
			}
			if info.IsDir() {
				bs.watcher.Add(event.Name)
				continue
			}
			relPath, err := filepath.Rel(bs.exportDir, event.Name)
			if err != nil {
				continue
			}
			if strings.HasPrefix(filepath.Base(relPath), ".") {
				continue
			}
			bs.pendingMu.Lock()
			bs.pending[relPath] = time.Now()
			bs.pendingMu.Unlock()

		case err, ok := <-bs.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[NFS-Sync] watcher error: %v", err)
		}
	}
}

func (bs *BlobberSync) commitWorker() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-bs.stopCh:
			return
		case <-ticker.C:
			bs.commitPending()
		}
	}
}

func (bs *BlobberSync) commitPending() {
	cutoff := time.Now().Add(-500 * time.Millisecond)
	bs.pendingMu.Lock()
	var ready []string
	for path, modTime := range bs.pending {
		if modTime.Before(cutoff) {
			ready = append(ready, path)
		}
	}
	for _, path := range ready {
		delete(bs.pending, path)
	}
	bs.pendingMu.Unlock()

	for _, relPath := range ready {
		bs.committedMu.RLock()
		done := bs.committed[relPath]
		bs.committedMu.RUnlock()
		if done {
			continue
		}
		if err := bs.commitFile(relPath); err != nil {
			log.Printf("[NFS-Sync] commit %s: %v", relPath, err)
			bs.pendingMu.Lock()
			bs.pending[relPath] = time.Now()
			bs.pendingMu.Unlock()
		} else {
			bs.committedMu.Lock()
			bs.committed[relPath] = true
			bs.committedMu.Unlock()
			bs.totalCommitted.Add(1)

			// Evict from tmpfs after successful blobber commit
			if bs.evictAfterCommit {
				fullPath := filepath.Join(bs.exportDir, relPath)
				os.Remove(fullPath)
				bs.totalEvicted.Add(1)
			}
		}
	}
}

func (bs *BlobberSync) commitFile(relPath string) error {
	fullPath := filepath.Join(bs.exportDir, relPath)
	remotePath := "/" + relPath

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	err = putFile(ctx, bs.alloc, remotePath, "application/octet-stream",
		bytes.NewReader(data), int64(len(data)), false, nil)
	if err != nil {
		return err
	}

	// WAL intent
	parts := strings.SplitN(strings.TrimPrefix(remotePath, "/"), "/", 2)
	if len(parts) == 2 && walWriter != nil {
		walWriter.RecordIntent(parts[0], parts[1], int64(len(data)))
	}
	return nil
}

// spilloverMonitor checks tmpfs usage and moves files to NVMe when >80% full.
func (bs *BlobberSync) spilloverMonitor() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-bs.stopCh:
			return
		case <-ticker.C:
			pct := bs.tmpfsUsagePct()
			if pct > 80 {
				bs.spillOldestFiles(pct)
			}
		}
	}
}

// tmpfsUsagePct returns the percentage of tmpfs used.
func (bs *BlobberSync) tmpfsUsagePct() float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(bs.exportDir, &stat); err != nil {
		return 0
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return 0
	}
	return float64(total-free) / float64(total) * 100
}

// spillOldestFiles moves the oldest committed files from tmpfs to spillover NVMe.
func (bs *BlobberSync) spillOldestFiles(currentPct float64) {
	if bs.spilloverDir == "" {
		return
	}

	// Walk export dir and find files to move
	var candidates []string
	filepath.Walk(bs.exportDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, _ := filepath.Rel(bs.exportDir, path)
		if relPath == "" || strings.HasPrefix(filepath.Base(relPath), ".") {
			return nil
		}
		// Prefer spilling committed files (already in blobbers)
		bs.committedMu.RLock()
		isCommitted := bs.committed[relPath]
		bs.committedMu.RUnlock()
		if isCommitted {
			candidates = append(candidates, relPath)
		}
		return nil
	})

	// Move files until usage drops below 70%
	moved := 0
	for _, relPath := range candidates {
		if bs.tmpfsUsagePct() < 70 {
			break
		}
		src := filepath.Join(bs.exportDir, relPath)
		dst := filepath.Join(bs.spilloverDir, relPath)
		os.MkdirAll(filepath.Dir(dst), 0755)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			continue
		}
		os.Remove(src)
		moved++
		bs.totalSpilled.Add(1)
	}

	if moved > 0 {
		log.Printf("[NFS-Sync] Spilled %d files to %s (tmpfs was %.0f%% full)", moved, bs.spilloverDir, currentPct)
	}
}

func (bs *BlobberSync) initialScan() {
	filepath.Walk(bs.exportDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(bs.exportDir, path)
		if err != nil || strings.HasPrefix(filepath.Base(relPath), ".") {
			return nil
		}
		bs.pendingMu.Lock()
		bs.pending[relPath] = time.Now()
		bs.pendingMu.Unlock()
		return nil
	})
}

func (bs *BlobberSync) Stop() {
	close(bs.stopCh)
	bs.watcher.Close()
}

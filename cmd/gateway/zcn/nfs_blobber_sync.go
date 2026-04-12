package zcn

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/0chain/gosdk/zboxcore/sdk"
)

// BlobberSync watches the NFS export directory for changes and syncs them
// to blobbers via putFile(). This makes NFS-Ganesha ACID:
//
//   NFS write → local NVMe (instant) → inotify → putFile → blobbers (async)
//   NFS read  → local NVMe (instant, or fetch from blobbers on cache miss)
//
// Crash recovery: on startup, scan export dir for files not yet committed
// to blobbers (compare with blobber refs) and re-sync.
type BlobberSync struct {
	exportDir string
	alloc     *sdk.Allocation
	watcher   *fsnotify.Watcher

	// pending tracks files that need blobber commit.
	// Key: relative path (e.g., "bucket/file.txt"), Value: mod time.
	pendingMu sync.Mutex
	pending   map[string]time.Time

	// committed tracks files already synced to avoid double-commit.
	committedMu sync.RWMutex
	committed   map[string]bool

	stopCh chan struct{}
}

// StartBlobberSync creates a BlobberSync watcher on the export directory.
// It watches for file creates/writes and syncs them to blobbers asynchronously.
func StartBlobberSync(exportDir string, alloc *sdk.Allocation, workers int) (*BlobberSync, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	bs := &BlobberSync{
		exportDir: exportDir,
		alloc:     alloc,
		watcher:   watcher,
		pending:   make(map[string]time.Time),
		committed: make(map[string]bool),
		stopCh:    make(chan struct{}),
	}

	// Watch the export dir and all subdirs (buckets)
	if err := bs.watchRecursive(exportDir); err != nil {
		watcher.Close()
		return nil, err
	}

	// Start event processor
	go bs.processEvents()

	// Start commit workers
	if workers <= 0 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		go bs.commitWorker()
	}

	// Initial scan: find files not yet committed
	go bs.initialScan()

	log.Printf("[NFS-Sync] Watching %s for blobber sync (%d workers)", exportDir, workers)
	return bs, nil
}

func (bs *BlobberSync) watchRecursive(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
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
			// Only care about creates and writes
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}

			info, err := os.Stat(event.Name)
			if err != nil {
				continue
			}

			if info.IsDir() {
				// New directory (bucket) — watch it
				bs.watcher.Add(event.Name)
				continue
			}

			// Regular file changed — mark for blobber commit
			relPath, err := filepath.Rel(bs.exportDir, event.Name)
			if err != nil {
				continue
			}

			// Skip hidden files and temp files
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

// commitWorker picks pending files and commits them to blobbers.
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
	// Grab files that have been stable for at least 500ms
	// (avoids committing partial writes)
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
		alreadyDone := bs.committed[relPath]
		bs.committedMu.RUnlock()
		if alreadyDone {
			continue
		}

		if err := bs.commitFile(relPath); err != nil {
			log.Printf("[NFS-Sync] commit %s failed: %v", relPath, err)
			// Re-queue for retry
			bs.pendingMu.Lock()
			bs.pending[relPath] = time.Now()
			bs.pendingMu.Unlock()
		} else {
			bs.committedMu.Lock()
			bs.committed[relPath] = true
			bs.committedMu.Unlock()
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

	// Record WAL intent
	parts := strings.SplitN(strings.TrimPrefix(remotePath, "/"), "/", 2)
	if len(parts) == 2 && walWriter != nil {
		walWriter.RecordIntent(parts[0], parts[1], int64(len(data)))
	}

	return nil
}

// initialScan finds all files in the export dir and queues them for commit.
func (bs *BlobberSync) initialScan() {
	filepath.Walk(bs.exportDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(bs.exportDir, path)
		if err != nil {
			return nil
		}
		if strings.HasPrefix(filepath.Base(relPath), ".") {
			return nil
		}
		bs.pendingMu.Lock()
		bs.pending[relPath] = time.Now()
		bs.pendingMu.Unlock()
		return nil
	})
}

// Stop gracefully shuts down the sync.
func (bs *BlobberSync) Stop() {
	close(bs.stopCh)
	bs.watcher.Close()
}

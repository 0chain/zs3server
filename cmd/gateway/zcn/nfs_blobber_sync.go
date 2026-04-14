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

	"github.com/0chain/gosdk/constants"
	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/fsnotify/fsnotify"
)

// BlobberSync watches the NFS-Ganesha export directory and commits changes
// to blobbers via DoMultiOperation batches.
//
// Architecture:
//   NFS write → Ganesha → /nfs_export (tmpfs/NVMe) → inotify
//   → collect files into batch (max_batch_size, up to batch_wait)
//   → DoMultiOperation(batch) → blobbers (one WM lock per batch)
//   → evict committed files from export dir
//
// This bypasses putFile/batchUploadChan entirely — no blocking on the
// S3 batch pipeline. The NFS sync has its own batching for maximum throughput.
type BlobberSync struct {
	exportDir        string
	spilloverDir     string
	alloc            *sdk.Allocation
	watcher          *fsnotify.Watcher
	evictAfterCommit bool
	directThreshold  int64

	// fileChan receives relative paths from inotify
	fileChan chan string

	committedMu sync.RWMutex
	committed   map[string]bool

	// Adaptive config
	batchSize     int
	batchWaitTime time.Duration
	maxConcurrent int

	// Stats
	totalCommitted atomic.Int64
	totalEvicted   atomic.Int64
	totalDirect    atomic.Int64
	totalFailed    atomic.Int64

	stopCh chan struct{}
}

func StartBlobberSync(exportDir string, alloc *sdk.Allocation, workers int, spilloverDir string, evictAfterCommit bool, directThreshold int64) (*BlobberSync, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if spilloverDir != "" {
		os.MkdirAll(spilloverDir, 0755)
	}

	if workers <= 0 {
		workers = 4
	}

	bs := &BlobberSync{
		exportDir:        exportDir,
		spilloverDir:     spilloverDir,
		alloc:            alloc,
		watcher:          watcher,
		evictAfterCommit: evictAfterCommit,
		directThreshold:  directThreshold,
		fileChan:         make(chan string, 10000),
		committed:        make(map[string]bool),
		batchSize:        25,
		batchWaitTime:    100 * time.Millisecond,
		maxConcurrent:    workers,
		stopCh:           make(chan struct{}),
	}

	if err := bs.watchRecursive(exportDir); err != nil {
		watcher.Close()
		return nil, err
	}

	// inotify event processor → fileChan
	go bs.processEvents()

	// Batch commit workers: drain fileChan, batch up, commit
	for i := 0; i < workers; i++ {
		go bs.batchCommitWorker(i)
	}

	// Spillover monitor
	if spilloverDir != "" {
		go bs.spilloverMonitor()
	}

	// Initial scan
	go bs.initialScan()

	log.Printf("[NFS-Sync] Watching %s (workers=%d, batch=%d, wait=%v, direct_threshold=%dKB, evict=%v)",
		exportDir, workers, bs.batchSize, bs.batchWaitTime, directThreshold/1024, evictAfterCommit)
	return bs, nil
}

func (bs *BlobberSync) watchRecursive(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		return bs.watcher.Add(path)
	})
}

func (bs *BlobberSync) processEvents() {
	// Debounce: track last event time per file, only send after 200ms quiet
	pending := make(map[string]time.Time)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

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
			trackFileSize(info.Size())
			relPath, err := filepath.Rel(bs.exportDir, event.Name)
			if err != nil || strings.HasPrefix(filepath.Base(relPath), ".") {
				continue
			}
			// Defense-in-depth: never upload stub files (xattr user.zus.stub set by /internal/list?stub=1)
			var stubbuf [2]byte
			if n, _ := syscall.Getxattr(event.Name, "user.zus.stub", stubbuf[:]); n > 0 {
				log.Printf("[NFS-Sync] skip stub (xattr): %s", relPath)
				continue
			}
			pending[relPath] = time.Now()

		case <-ticker.C:
			// Send files that have been quiet for 200ms
			cutoff := time.Now().Add(-200 * time.Millisecond)
			for path, t := range pending {
				if t.Before(cutoff) {
					bs.committedMu.RLock()
					done := bs.committed[path]
					bs.committedMu.RUnlock()
					if !done {
						select {
						case bs.fileChan <- path:
						default:
							// Channel full — drop (will be picked up on next scan)
						}
					}
					delete(pending, path)
				}
			}

		case err, ok := <-bs.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[NFS-Sync] watcher error: %v", err)
		}
	}
}

// batchCommitWorker drains fileChan, collects files into batches, commits.
func (bs *BlobberSync) batchCommitWorker(id int) {
	for {
		select {
		case <-bs.stopCh:
			return
		case firstFile := <-bs.fileChan:
			// Got a file — collect more for a batch
			batch := []string{firstFile}
			deadline := time.After(bs.batchWaitTime)

		collect:
			for len(batch) < bs.batchSize {
				select {
				case f := <-bs.fileChan:
					batch = append(batch, f)
				case <-deadline:
					break collect
				case <-bs.stopCh:
					return
				}
			}

			// Commit the batch
			if err := bs.commitBatch(batch); err != nil {
				log.Printf("[NFS-Sync] worker %d batch(%d) failed: %v", id, len(batch), err)
				bs.totalFailed.Add(int64(len(batch)))
				// Re-queue with backoff
				time.Sleep(2 * time.Second)
				for _, f := range batch {
					select {
					case bs.fileChan <- f:
					default:
					}
				}
			} else {
				bs.totalCommitted.Add(int64(len(batch)))
				if bs.evictAfterCommit {
					for _, f := range batch {
						os.Remove(filepath.Join(bs.exportDir, f))
						bs.totalEvicted.Add(1)
					}
				}
			}
		}
	}
}

// commitBatch commits a batch of files via a single DoMultiOperation.
// One WM lock acquisition for the entire batch = much less overhead.
func (bs *BlobberSync) commitBatch(files []string) error {
	var ops []sdk.OperationRequest

	for _, relPath := range files {
		fullPath := filepath.Join(bs.exportDir, relPath)
		remotePath := "/" + relPath

		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue // file may have been deleted
		}

		fileName := filepath.Base(remotePath)
		ops = append(ops, sdk.OperationRequest{
			OperationType: constants.FileOperationInsert,
			FileReader:    newMinioReader(bytes.NewReader(data)),
			Workdir:       workDir,
			RemotePath:    remotePath,
			FileMeta: sdk.FileMeta{
				RemotePath: remotePath,
				ActualSize: int64(len(data)),
				MimeType:   "application/octet-stream",
				RemoteName: fileName,
			},
			Opts: []sdk.ChunkedUploadOption{
				sdk.WithChunkNumber(120),
				sdk.WithEncrypt(encrypt),
			},
		})
	}

	if len(ops) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_ = ctx // DoMultiOperation uses its own context

	err := bs.alloc.DoMultiOperation(ops)
	if err != nil && isSameRootError(err) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("DoMultiOperation(%d files): %w", len(ops), err)
	}

	// Mark committed
	bs.committedMu.Lock()
	for _, f := range files {
		bs.committed[f] = true
	}
	bs.committedMu.Unlock()

	// WAL intents
	for _, f := range files {
		remotePath := "/" + f
		parts := strings.SplitN(strings.TrimPrefix(remotePath, "/"), "/", 2)
		if len(parts) == 2 && walWriter != nil {
			data, _ := os.ReadFile(filepath.Join(bs.exportDir, f))
			walWriter.RecordIntent(parts[0], parts[1], int64(len(data)))
		}
	}

	return nil
}

func (bs *BlobberSync) spilloverMonitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-bs.stopCh:
			return
		case <-ticker.C:
			if bs.tmpfsUsagePct() > 80 {
				bs.spillCommittedFiles()
			}
		}
	}
}

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

func (bs *BlobberSync) spillCommittedFiles() {
	if bs.spilloverDir == "" {
		return
	}
	bs.committedMu.RLock()
	var candidates []string
	for f, done := range bs.committed {
		if done {
			candidates = append(candidates, f)
		}
	}
	bs.committedMu.RUnlock()

	moved := 0
	for _, f := range candidates {
		if bs.tmpfsUsagePct() < 60 {
			break
		}
		src := filepath.Join(bs.exportDir, f)
		dst := filepath.Join(bs.spilloverDir, f)
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
	}
	if moved > 0 {
		log.Printf("[NFS-Sync] Spilled %d files to %s", moved, bs.spilloverDir)
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
		select {
		case bs.fileChan <- relPath:
		default:
		}
		return nil
	})
}

func (bs *BlobberSync) Stop() {
	close(bs.stopCh)
	bs.watcher.Close()
}

// MarkCommitted records relPath in the committed map so that the next inotify
// event for this path is suppressed (used by the prewarm endpoint after
// writing a fetched blob into the export dir).
func (bs *BlobberSync) MarkCommitted(relPath string) {
	bs.committedMu.Lock()
	bs.committed[relPath] = true
	bs.committedMu.Unlock()
}

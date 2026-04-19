package zcn

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	// fileChan receives relative paths from inotify for upload batching
	fileChan chan string
	// deleteChan receives relative paths for delete batching (Remove/Rename src events)
	deleteChan chan string

	committedMu sync.RWMutex
	committed   map[string]bool

	// skipRemoveMu guards one-shot suppression of inotify Remove events
	// for paths the S3 side just removed via mirrorS3Delete*ToExport.
	skipRemoveMu       sync.Mutex
	skipRemovePaths    map[string]bool // exact-path matches (consumed on first Remove)
	skipRemovePrefixes map[string]bool // prefix matches (for bucket/subtree removal)

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
		exportDir:          exportDir,
		spilloverDir:       spilloverDir,
		alloc:              alloc,
		watcher:            watcher,
		evictAfterCommit:   evictAfterCommit,
		directThreshold:    directThreshold,
		fileChan:           make(chan string, 10000),
		deleteChan:         make(chan string, 10000),
		committed:          make(map[string]bool),
		skipRemovePaths:    make(map[string]bool),
		skipRemovePrefixes: make(map[string]bool),
		batchSize:          25,
		batchWaitTime:      100 * time.Millisecond,
		maxConcurrent:      workers,
		stopCh:             make(chan struct{}),
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

	// Batch delete workers: drain deleteChan, batch up, issue FileOperationDelete
	for i := 0; i < workers; i++ {
		go bs.batchDeleteWorker(i)
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
			// Remove / Rename (src side) both indicate the old name is
			// gone. Enqueue a delete unless the S3 mirror asked us to
			// skip (subtree or exact-path suppression). For Rename within
			// the export dir, the destination fires a separate Create
			// which goes through the upload path; the net effect is
			// delete+upload. A future optimisation can pair Rename+Create
			// into a Zus FileOperationRename using inode tracking.
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				relPath, err := filepath.Rel(bs.exportDir, event.Name)
				if err != nil || strings.HasPrefix(filepath.Base(relPath), ".") || strings.HasSuffix(relPath, ".cacheback") || strings.HasSuffix(relPath, ".cachefetch") {
					continue
				}
				if bs.consumeRemoveSkip(relPath) {
					continue
				}
				select {
				case bs.deleteChan <- relPath:
				default:
				}
				continue
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
			if err != nil || strings.HasPrefix(filepath.Base(relPath), ".") || strings.HasSuffix(relPath, ".cacheback") || strings.HasSuffix(relPath, ".cachefetch") {
				continue
			}
			// Defense-in-depth: never upload stub files (xattr user.zus.stub set by /internal/list?stub=1)
			var stubbuf [2]byte
			if n, _ := syscall.Getxattr(event.Name, "user.zus.stub", stubbuf[:]); n > 0 {
				log.Printf("[NFS-Sync] skip stub (xattr): %s", relPath)
				continue
			}
			// Skip files already committed by the S3 PUT single-cache
			// path. Their bytes landed on blobbers via putFile; no inotify
			// re-upload needed. Guards against a late Write event firing
			// after putFile finishes.
			if n, _ := syscall.Getxattr(event.Name, "user.zus.committed", stubbuf[:]); n > 0 {
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
						// Blocking send: back-pressure fsnotify so rsync/tar/etc.
						// stall in Ganesha instead of silently losing uploads.
						bs.fileChan <- path
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

		// Defense-in-depth: never upload sparse stub placeholders. They
		// carry the real object's size but zero actual bytes; uploading
		// them would clobber the real content on Züs.
		var sbuf [2]byte
		if n, _ := syscall.Getxattr(fullPath, "user.zus.stub", sbuf[:]); n > 0 {
			continue
		}
		// Skip files already committed by the S3 PUT single-cache path.
		// Without this, zs3server restarts re-upload every file in
		// /nfs_export via initialScan → commitBatch, which at the blobber
		// level becomes a delete+insert cycle and corrupts ref state.
		if n, _ := syscall.Getxattr(fullPath, "user.zus.committed", sbuf[:]); n > 0 {
			continue
		}

		// Race E: serialise read of the local file against any
		// concurrent S3 PUT/cache-back/spill truncation for the same path.
		mu := acquirePathLock(relPath)
		data, rerr := os.ReadFile(fullPath)
		mu.Unlock()
		if rerr != nil {
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
	ticker := time.NewTicker(1 * time.Second)
	// Aggressive threshold: spill at 70% so prewarm ENOSPC is avoided.
	defer ticker.Stop()
	for {
		select {
		case <-bs.stopCh:
			return
		case <-ticker.C:
			if bs.tmpfsUsagePct() > 60 {
				bs.SpillNow()
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

// spilloverBytesUsed returns total size of files under spilloverDir.
func (bs *BlobberSync) spilloverBytesUsed() int64 {
	if bs.spilloverDir == "" {
		return 0
	}
	var total int64
	_ = filepath.Walk(bs.spilloverDir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// evictSpilloverOldest deletes oldest files from spilloverDir until
// `target` free bytes are available.
func (bs *BlobberSync) evictSpilloverOldest(need int64) {
	if bs.spilloverDir == "" || serverConfig.NFSSpilloverMaxBytes <= 0 {
		return
	}
	type entry struct {
		path string
		size int64
		mt   time.Time
	}
	var entries []entry
	_ = filepath.Walk(bs.spilloverDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			entries = append(entries, entry{p, info.Size(), info.ModTime()})
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].mt.Before(entries[j].mt) })
	used := bs.spilloverBytesUsed()
	cap := serverConfig.NFSSpilloverMaxBytes
	// Evict oldest until used + need <= cap. Skip files spilled <30s ago
	// so FSAL read2 can still find them just after a spill.
	for _, e := range entries {
		if used+need <= cap {
			break
		}
		if time.Since(e.mt) < 120*time.Second {
			continue
		}
		// Also respect atime (last read). With strictatime mount, every
		// NFS/S3 read bumps atime. Skip files read within the last 120s —
		// prevents deleting a spillover file that Ganesha is mid-serving.
		if info, sErr := os.Stat(e.path); sErr == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				at := time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec))
				if time.Since(at) < 120*time.Second {
					continue
				}
			}
		}
		_ = os.Remove(e.path)
		if rel, rerr := filepath.Rel(bs.spilloverDir, e.path); rerr == nil {
			MarkRecentlyEvicted(rel)
		}
		used -= e.size
	}
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

	// Race F: spill OLDEST files first (lowest mtime). Recently-prewarmed
	// files are likely still being read by Spark's multi-phase parquet
	// reader (footer, then row-groups); truncating them mid-read yields
	// [random-bytes] or [0,0,0,0] parquet magic errors. Oldest files are
	// least likely to be in an active reader's working set. Combined with
	// the per-path lock + re-scan under lock + 5s mtime grace below, this
	// serialises spill vs read reliably.
	sort.Slice(candidates, func(i, j int) bool {
		ia, _ := os.Lstat(filepath.Join(bs.exportDir, candidates[i]))
		ja, _ := os.Lstat(filepath.Join(bs.exportDir, candidates[j]))
		if ia == nil || ja == nil {
			return false
		}
		return ia.ModTime().Before(ja.ModTime())
	})

	// Build the set of inodes currently held open by any host process
	// (except this one). Files in this set have active readers — NFS
	// clients via ganesha.nfsd, S3 GET ranges via minio, fio, etc.
	// O_TRUNC'ing them would zero bytes beyond the truncate point that
	// those clients' pread() calls expect to see. Prior SF100 runs hit
	// this race as ChecksumException on store_sales/part-00025.parquet.
	activeInodes := openFdInodes()

	moved := 0
	for _, f := range candidates {
		if bs.tmpfsUsagePct() < 40 {
			break
		}
		src := filepath.Join(bs.exportDir, f)
		dst := filepath.Join(bs.spilloverDir, f)
		os.MkdirAll(filepath.Dir(dst), 0755)
		srcInfo, sErr := os.Lstat(src)
		if sErr != nil {
			continue
		}
		if st, ok := srcInfo.Sys().(*syscall.Stat_t); ok && activeInodes[st.Ino] {
			continue
		}
		// Race F: 5-second mtime grace. Freshly prewarmed files have mtime =
		// now and are actively being read by Spark's multi-open parquet
		// reader. Combined with the oldest-first sort above, this lets
		// spillover eat idle files first and avoid ambushing hot ones. The
		// 5s window is short enough that spillover still drains under
		// sustained prewarm load, and wide enough to cover the typical
		// open-close-open gap between Spark footer read and row-group read.
		// Use atime (last read) not mtime. With strictatime mount, every read
		// updates atime. An actively-read file has fresh atime even if mtime
		// is stale — prevents spill-during-Spark-range-read race.
		var atime time.Time
		if st, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
			atime = time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec))
		} else {
			atime = srcInfo.ModTime()
		}
		if time.Since(atime) < 120*time.Second {
			continue
		}
		// Race D: hold the per-path lock across the entire xattr-check +
		// Removexattr + O_TRUNC sequence. Without this, between
		// Removexattr("user.zus.committed") and O_TRUNC below, a parallel
		// cacheBackOnMiss or PutObject could Setxattr("committed") and
		// start overwriting — the O_TRUNC would then zero a partially
		// written authoritative file. Acquired BEFORE the xattr gate so
		// the check and the truncate see the same state.
		mu := acquirePathLock(f)
		// Defense-in-depth: only spill files tagged with the committed
		// xattr. Prevents racing an in-flight prewarm (which sets the
		// xattr only after the write is complete) and also prevents
		// reading sparse stubs (which were never prewarmed and whose
		// ReadFile returns 34MB of raw zero bytes — those would then be
		// saved to spillover as fully-allocated zero bytes, and a later
		// spillover-restore would copy those zeros into /nfs_export,
		// corrupting the file). Note: syscall.Getxattr returns a
		// NEGATIVE value (not 0) when the attribute is missing on Linux,
		// so the correct "missing" check is `n <= 0`, not `n == 0`.
		var cbuf [2]byte
		if n, _ := syscall.Getxattr(src, "user.zus.committed", cbuf[:]); n <= 0 {
			mu.Unlock()
			continue
		}
		// Race F: re-scan /proc fds UNDER the path lock,
		// immediately before O_TRUNC. The top-of-function activeInodes
		// map was built before we began iterating — stale by now. Any fd
		// opened since then (by ganesha.nfsd serving a live NFS read) is
		// missed. The path lock serialises us against PutObject /
		// cacheBackOnMiss but NOT against Ganesha's read path, so we need
		// this last-mile check. A single re-scan adds ~5ms per spill
		// cycle; we only run it for candidates that already passed the
		// first scan + mtime grace, so the list is short.
		var curIno uint64
		if st, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
			curIno = st.Ino
		}
		if curIno != 0 {
			if openFdInodes()[curIno] {
				mu.Unlock()
				continue
			}
		}
		origSize := srcInfo.Size()
		// Respect spillover cap: evict older entries if needed.
		if serverConfig.NFSSpilloverMaxBytes > 0 {
			bs.evictSpilloverOldest(origSize)
			// If still over cap (cap < single file size), skip spill.
			if bs.spilloverBytesUsed()+origSize > serverConfig.NFSSpilloverMaxBytes {
				mu.Unlock()
				continue
			}
		}
		data, err := os.ReadFile(src)
		if err != nil {
			mu.Unlock()
			continue
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			mu.Unlock()
			continue
		}
		// FSAL spillover-aware gate: tag spillover copy as committed so
		// FSAL_ZUS stub-check accepts it as authoritative content. Without
		// this xattr, zusfs_reopen2/read2/write2 skip spillover and fall
		// through to blobber fetch, defeating the spill-to-disk tier.
		if xerr := syscall.Setxattr(dst, "user.zus.committed", []byte{'1'}, 0); xerr != nil {
			log.Printf("[NFS-Sync] Setxattr(committed) on spillover %s failed: %v", dst, xerr)
		}
		// Race G: cross-process F_WRLCK on the tmpfs inode, paired with
		// F_RDLCK taken by FSAL_ZUS zusfs_read2 for the duration of each
		// sub-FSAL pread. The in-process sync.Mutex (mu) and openFdInodes
		// scan above only protect against other Go goroutines inside this
		// binary — they cannot block ganesha.nfsd (a separate PROCESS)
		// from reading past-EOF after our O_TRUNC. fcntl locks are held
		// against the kernel inode and are the only primitive visible
		// across both processes. F_SETLKW = blocking wait: readers finish,
		// then spill proceeds. Lock region covers Removexattr +
		// O_TRUNC + Truncate + Setxattr(stub); closing the fd releases
		// the lock.
		lockFd, lfErr := syscall.Open(src, syscall.O_WRONLY, 0)
		if lfErr != nil {
			mu.Unlock()
			continue
		}
		wlock := syscall.Flock_t{
			Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0,
		}
		if flErr := syscall.FcntlFlock(uintptr(lockFd), F_OFD_SETLKW, &wlock); flErr != nil {
			syscall.Close(lockFd)
			mu.Unlock()
			continue
		}
		// Critical: drop the committed xattr BEFORE truncating. This is
		// how the spillover-restore (prewarm) and concurrent spillover
		// cycles agree on "who owns this file right now". The xattr gate
		// in the spill candidate check (line ~470) is what prevents a
		// second spillover pass from truncating a file that prewarm is
		// restoring in-place; without this removal, the xattr would stay
		// set (O_TRUNC preserves xattrs) and spillover would race the
		// in-flight restore, zero-prefixing parquet files.
		_ = syscall.Removexattr(src, "user.zus.committed")
		// Re-stub in /nfs_export instead of deleting: keep the file visible
		// to Ganesha so NFS clients still see it; next read triggers
		// FSAL_ZUS prewarm which either fetches from blobbers or restores
		// from the spillover copy we just wrote above.
		if tf, oerr := os.OpenFile(src, os.O_WRONLY|os.O_TRUNC, 0644); oerr == nil {
			tf.Close()
		}
		if terr := os.Truncate(src, origSize); terr != nil {
			os.Remove(src)
			syscall.Close(lockFd)
			mu.Unlock()
			continue
		}
		if xerr := syscall.Setxattr(src, "user.zus.stub", []byte{'1'}, 0); xerr != nil {
			log.Printf("[NFS-Sync] spillover setxattr failed: %v", xerr)
		}
		var st syscall.Stat_t
		if serr := syscall.Lstat(src, &st); serr == nil {
			inodeRelSet(st.Ino, f)
		}
		moved++
		// Release the F_WRLCK by closing the fd (implicit unlock of all
		// locks held by this process on this fd). Paired close avoids
		// fd leaks in the fast path.
		syscall.Close(lockFd)
		mu.Unlock()
	}
	if moved > 0 {
		log.Printf("[NFS-Sync] Spilled %d files to %s", moved, bs.spilloverDir)
	}
}

// SpillNow is the synchronous public entry point for callers that need to
// free tmpfs space right now (e.g. prewarm on ENOSPC). Runs
// spillCommittedFiles once and returns when it completes.
func (bs *BlobberSync) SpillNow() {
	bs.spillCommittedFiles()
}

// SpillUntilFree keeps running spillCommittedFiles until at least `need`
// bytes are free in tmpfs or no more spill candidates are movable.
// Bounded retries — spillCommittedFiles itself stops at 60%, but with
// many active readers fewer files may be eligible per pass.
func (bs *BlobberSync) SpillUntilFree(need int64) {
	for i := 0; i < 5; i++ {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(bs.exportDir, &stat); err == nil {
			free := int64(stat.Bfree) * int64(stat.Bsize)
			if free >= need {
				return
			}
		}
		bs.spillCommittedFiles()
	}
}

// ensureFreeTmpfsMargin is the default safety margin applied on top of
// the incoming-file size when checking for tmpfs headroom. 64MiB is
// conservative: covers write amplification (WAL, inotify-induced metadata
// churn, block rounding) and small concurrent prewarms queued behind us.
const ensureFreeTmpfsMargin = int64(64 * 1024 * 1024)

// ShouldUseSpillover returns true when the incoming file is larger than
// the tmpfs total capacity (minus ensureFreeTmpfsMargin). In that case
// eviction cannot make room regardless of effort — the file will never
// fit. Callers should write directly to spilloverDir instead.
//
// Assumption: file_size <= spillover_max_bytes. If the file exceeds
// spillover too, this function still returns true and the subsequent
// write may exceed the spillover cap. True streaming-through (bypass
// cache entirely) is NOT implemented for NFS — FSAL_ZUS reads from
// on-disk files and has no streaming path. For S3, a future optimization
// could skip cache when file > tmpfs AND > spillover, serving directly
// from the blobber pipe to the S3 client.
func (bs *BlobberSync) ShouldUseSpillover(fileSize int64) bool {
	if bs == nil || bs.spilloverDir == "" {
		return false
	}
	if fileSize <= 0 {
		return false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(bs.exportDir, &st); err != nil {
		return false
	}
	tmpfsTotal := int64(st.Blocks) * int64(st.Bsize)
	return fileSize+ensureFreeTmpfsMargin > tmpfsTotal
}

// EnsureFreeTmpfs is the proactive 2-tier LRU entry point called by
// prewarm_router BEFORE writing a new file into /nfs_export. It ensures
// tmpfs has at least `needBytes + margin` free, by moving oldest
// committed files tmpfs→spillover one-at-a-time. For each such move,
// spillover is kept under its own cap by deleting oldest spillover
// entries first. This is synchronous — the prewarm write that follows
// sees the freed space. The 2s background spilloverMonitor remains as a
// legacy fallback (60% threshold) for cases where a writer bypasses this
// path (e.g. S3 PUT cache mirror).
//
// Returns an error only when tmpfs still has insufficient free space AND
// there are no more eligible eviction candidates. Callers may choose to
// proceed anyway — os.Create will ENOSPC if truly full.
func (bs *BlobberSync) EnsureFreeTmpfs(needBytes int64) error {
	if needBytes <= 0 {
		return nil
	}
	target := needBytes + ensureFreeTmpfsMargin
	var skip map[string]bool
	// Safety cap: never loop more than N times. With up to ~hundreds of
	// candidate files and one spill per iteration, 2048 is ample; the
	// usual case is 0–few iterations.
	for i := 0; i < 2048; i++ {
		var statfs syscall.Statfs_t
		if err := syscall.Statfs(bs.exportDir, &statfs); err != nil {
			return fmt.Errorf("statfs(%s): %w", bs.exportDir, err)
		}
		free := int64(statfs.Bavail) * int64(statfs.Bsize)
		if free >= target {
			return nil
		}
		oldest := bs.findOldestCommittedTmpfsFileExcept(skip)
		if oldest == "" {
			return fmt.Errorf("tmpfs needs %d bytes free (have %d); no evictable committed files", target, free)
		}
		if err := bs.spillOneFile(oldest); err != nil {
			// Could not spill THIS file (active reader / xattr gone /
			// etc.). Skip it and try the next-oldest candidate. We
			// exclude it from the next findOldestCommittedTmpfsFile
			// scan via the skip set, so we do not loop on the same
			// file.
			log.Printf("[NFS-Sync] EnsureFreeTmpfs: spillOneFile(%s) failed: %v; trying next candidate", oldest, err)
			if skip == nil {
				skip = make(map[string]bool)
			}
			skip[oldest] = true
			continue
		}
	}
	return fmt.Errorf("EnsureFreeTmpfs: exceeded iteration cap without reaching target")
}

// findOldestCommittedTmpfsFile walks exportDir and returns the relPath of
// the oldest (by mtime) file that is:
//   - committed (user.zus.committed xattr set)
//   - has real content (st_blocks > 0; excludes re-stubbed placeholders)
//   - not currently held open by any other process (checked via
//     openFdInodes to avoid mid-read truncation races)
//
// Returns "" when no eligible candidate exists; caller treats this as
// "nothing left to evict".
func (bs *BlobberSync) findOldestCommittedTmpfsFile() string {
	return bs.findOldestCommittedTmpfsFileExcept(nil)
}

func (bs *BlobberSync) findOldestCommittedTmpfsFileExcept(skip map[string]bool) string {
	activeInodes := openFdInodes()
	type cand struct {
		rel string
		mt  time.Time
	}
	var oldest *cand
	_ = filepath.Walk(bs.exportDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		// Skip hidden + cacheback artefacts
		rel, rerr := filepath.Rel(bs.exportDir, p)
		if rerr == nil && skip != nil && skip[rel] {
			return nil
		}
		if rerr != nil || strings.HasPrefix(filepath.Base(rel), ".") || strings.HasSuffix(rel, ".cacheback") || strings.HasSuffix(rel, ".cachefetch") {
			return nil
		}
		// Require committed xattr (spill candidate gate).
		var cbuf [2]byte
		if n, _ := syscall.Getxattr(p, "user.zus.committed", cbuf[:]); n <= 0 {
			return nil
		}
		// Require real content (not a stub); st_blocks > 0 on Linux
		// means the file has allocated data blocks.
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Blocks <= 0 {
			return nil
		}
		// Skip files held open by any other process.
		if activeInodes[st.Ino] {
			return nil
		}
		mt := info.ModTime()
		if oldest == nil || mt.Before(oldest.mt) {
			oldest = &cand{rel: rel, mt: mt}
		}
		return nil
	})
	if oldest == nil {
		return ""
	}
	return oldest.rel
}

// findOldestSpilloverFile returns the absolute path of the oldest
// committed file currently in spillover, for cascade eviction. Returns
// "" when spillover is empty or the directory is not configured.
func (bs *BlobberSync) findOldestSpilloverFile() string {
	if bs.spilloverDir == "" {
		return ""
	}
	var oldestPath string
	var oldestMt time.Time
	_ = filepath.Walk(bs.spilloverDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if oldestPath == "" || info.ModTime().Before(oldestMt) {
			oldestPath = p
			oldestMt = info.ModTime()
		}
		return nil
	})
	return oldestPath
}

// spillOneFile moves a single file from tmpfs → spillover using the same
// race-safe sequence as spillCommittedFiles (F_WRLCK on tmpfs fd, drop
// committed xattr, truncate, re-stub). Before the tmpfs→spillover copy,
// ensures spillover has room by deleting oldest spillover entries until
// the file fits; respects NFSSpilloverMaxBytes when set, otherwise falls
// back to actual free-space on the spillover filesystem.
//
// This is the "move one" primitive that EnsureFreeTmpfs calls in a loop.
func (bs *BlobberSync) spillOneFile(rel string) error {
	if bs.spilloverDir == "" {
		return fmt.Errorf("spillOneFile: spillover dir not configured")
	}
	src := filepath.Join(bs.exportDir, rel)
	dst := filepath.Join(bs.spilloverDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("mkdir spillover parent: %w", err)
	}

	mu := acquirePathLock(rel)
	defer mu.Unlock()

	srcInfo, sErr := os.Lstat(src)
	if sErr != nil {
		return fmt.Errorf("lstat src: %w", sErr)
	}
	// Re-check committed xattr under the lock (could have been
	// restored/evicted since findOldest picked it).
	var cbuf [2]byte
	if n, _ := syscall.Getxattr(src, "user.zus.committed", cbuf[:]); n <= 0 {
		return fmt.Errorf("not committed (xattr gone)")
	}
	// Re-check open fds under the lock (new NFS reads may have opened
	// this inode since findOldest scanned).
	if st, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
		if openFdInodes()[st.Ino] {
			return fmt.Errorf("has active fd")
		}
	}
	origSize := srcInfo.Size()

	// Ensure spillover has room: evict oldest spillover entries one at
	// a time until `origSize` fits, respecting the configured cap if
	// set, else statfs free-space on the spillover fs.
	for {
		if serverConfig.NFSSpilloverMaxBytes > 0 {
			used := bs.spilloverBytesUsed()
			if used+origSize <= serverConfig.NFSSpilloverMaxBytes {
				break
			}
		} else {
			var sstat syscall.Statfs_t
			if err := syscall.Statfs(bs.spilloverDir, &sstat); err != nil {
				break // fs unreadable — just try the write and let it fail
			}
			free := int64(sstat.Bavail) * int64(sstat.Bsize)
			if free >= origSize {
				break
			}
		}
		victim := bs.findOldestSpilloverFile()
		if victim == "" {
			return fmt.Errorf("spillover has no evictable entry to make room for %d bytes", origSize)
		}
		if err := os.Remove(victim); err != nil {
			return fmt.Errorf("os.Remove(spillover victim %s): %w", victim, err)
		}
	}

	// Copy tmpfs → spillover.
	data, rerr := os.ReadFile(src)
	if rerr != nil {
		return fmt.Errorf("readfile tmpfs: %w", rerr)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return fmt.Errorf("writefile spillover: %w", err)
	}
	if xerr := syscall.Setxattr(dst, "user.zus.committed", []byte{'1'}, 0); xerr != nil {
		log.Printf("[NFS-Sync] Setxattr(committed) on spillover %s failed: %v", dst, xerr)
	}

	// Race G: F_WRLCK on tmpfs inode across truncate+stub, paired with
	// F_RDLCK in FSAL_ZUS zusfs_read2. See spillCommittedFiles for the
	// full analysis; identical lock discipline here.
	lockFd, lfErr := syscall.Open(src, syscall.O_WRONLY, 0)
	if lfErr != nil {
		return fmt.Errorf("open tmpfs for lock: %w", lfErr)
	}
	defer syscall.Close(lockFd)
	wlock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if flErr := syscall.FcntlFlock(uintptr(lockFd), F_OFD_SETLKW, &wlock); flErr != nil {
		return fmt.Errorf("fcntl F_WRLCK: %w", flErr)
	}
	// Drop committed xattr BEFORE truncate so a racing spill pass can't
	// zero-prefix a file another goroutine is restoring. (See race
	// analysis in spillCommittedFiles.)
	_ = syscall.Removexattr(src, "user.zus.committed")
	if tf, oerr := os.OpenFile(src, os.O_WRONLY|os.O_TRUNC, 0644); oerr == nil {
		tf.Close()
	}
	if terr := os.Truncate(src, origSize); terr != nil {
		_ = os.Remove(src)
		return fmt.Errorf("truncate tmpfs: %w", terr)
	}
	if xerr := syscall.Setxattr(src, "user.zus.stub", []byte{'1'}, 0); xerr != nil {
		log.Printf("[NFS-Sync] spillOneFile setxattr(stub) failed: %v", xerr)
	}
	var st syscall.Stat_t
	if serr := syscall.Lstat(src, &st); serr == nil {
		inodeRelSet(st.Ino, rel)
	}
	bs.totalEvicted.Add(1)
	MarkRecentlyEvicted(rel)
	return nil
}

// ExportDir returns the path being watched. Used by prewarm_router to
// statfs this directory before writing.
func (bs *BlobberSync) ExportDir() string {
	return bs.exportDir
}

// openFdInodes returns the set of inodes currently held open by ANY
// process on the host, regardless of open mode (O_RDONLY, O_WRONLY,
// O_RDWR). Used by spillCommittedFiles to skip files with ANY active
// fd — readers AND writers — to avoid two spill-during-in-flight races:
//   - spill-during-active-read: SF100 v6 ChecksumException on
//     store_sales/part-00025.parquet from O_TRUNC under a live
//     ganesha.nfsd / S3-GET range fd.
//   - spill-during-active-write (Race A): S3 PUT putFile/mirror fds and
//     NFS Ganesha write fds being O_TRUNC'd mid-copy, silently losing
//     bytes already in-flight.
//
// Classes of concurrent fds to guard against:
//   - ganesha.nfsd: NFS read AND write fds
//   - minio (zs3server): S3 GET range reads AND S3 PUT write fds
//   - external tools: fio, cp, mc, rsync, tar
// syscall.Stat follows the /proc/PID/fd/N symlink regardless of the fd
// open mode, so write-only fds land in the result the same as reads.
// Scanning all PIDs catches any future reader/writer; cost is
// O(total_open_fds) per spill cycle.
//
// Inode-keyed (not path) because /proc/<pid>/fd/<n> readlink races with
// rename/truncate; the fd-path syscall.Stat returns the backing inode,
// which is stable under O_TRUNC (the re-stub preserves the inode).
func openFdInodes() map[uint64]bool {
	result := make(map[uint64]bool)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return result
	}
	self := os.Getpid()
	for _, e := range entries {
		name := e.Name()
		if len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		// Skip our own process — don't self-block, and /proc/self/fd
		// churns during this very scan (opening each /proc/PID/fd
		// directory allocates an fd in our process).
		if n, err := strconv.Atoi(name); err == nil && n == self {
			continue
		}
		fdDir := "/proc/" + name + "/fd"
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			var st syscall.Stat_t
			if syscall.Stat(fdDir+"/"+fd.Name(), &st) == nil {
				result[st.Ino] = true
			}
		}
	}
	return result
}

// pathMutexes provides a per-path keyed mutex serialising mutating ops
// on a single cache-file path across the NFS/S3 unified cache layer.
// Acquired by: spillCommittedFiles (check+remove+truncate window),
// cacheBackOnMiss (fetch+rename+setxattr), PutObject /
// mirrorS3PutToExport (local write+setxattr), commitBatch (NFS upload).
// Grows without bound in the worst case (one entry per distinct path
// ever seen); working set is bounded by live object count.
const (
	F_OFD_SETLK  = 37
	F_OFD_SETLKW = 38
)

var pathMutexes sync.Map

// acquirePathLock returns a locked per-relPath mutex. Callers must
// defer mu.Unlock().
func acquirePathLock(relPath string) *sync.Mutex {
	m, _ := pathMutexes.LoadOrStore(relPath, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu
}

func (bs *BlobberSync) initialScan() {
	filepath.Walk(bs.exportDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(bs.exportDir, p)
		if err != nil || strings.HasPrefix(filepath.Base(relPath), ".") || strings.HasSuffix(relPath, ".cacheback") || strings.HasSuffix(relPath, ".cachefetch") {
			return nil
		}
		// Never re-upload sparse stub placeholders created by the S3 mirror
		// or /internal/list?stub=1 — they would clobber real content on Züs.
		var buf [2]byte
		if n, _ := syscall.Getxattr(p, "user.zus.stub", buf[:]); n > 0 {
			return nil
		}
		// Skip files already committed to blobbers (tagged by PutObject's
		// single-cache path). Prevents restart from re-uploading every
		// file in /nfs_export, which at blobber level becomes a
		// delete+insert cycle that corrupts ref state.
		if n, _ := syscall.Getxattr(p, "user.zus.committed", buf[:]); n > 0 {
			return nil
		}
		bs.fileChan <- relPath // blocking; initialScan must not drop
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

// SkipNextRemove records a one-shot suppression for the next inotify Remove
// event on relPath. Used by the S3-side mirror so a delete initiated by
// DeleteObject does not re-issue a FileOperationDelete to Züs.
func (bs *BlobberSync) SkipNextRemove(relPath string) {
	bs.skipRemoveMu.Lock()
	bs.skipRemovePaths[relPath] = true
	bs.skipRemoveMu.Unlock()
}

// SkipRemoveSubtree suppresses all Remove events whose relPath has prefix
// equal to the given relPrefix (with a trailing slash). Used by the S3-side
// mirror's DeleteBucket path which os.RemoveAll's an entire subtree.
func (bs *BlobberSync) SkipRemoveSubtree(relPrefix string) {
	relPrefix = strings.TrimSuffix(relPrefix, "/")
	if relPrefix == "" {
		return
	}
	bs.skipRemoveMu.Lock()
	bs.skipRemovePrefixes[relPrefix] = true
	bs.skipRemoveMu.Unlock()
	// Expire subtree suppression after a few seconds so lingering entries
	// do not block future NFS-initiated deletes on a reused bucket name.
	go func(p string) {
		time.Sleep(5 * time.Second)
		bs.skipRemoveMu.Lock()
		delete(bs.skipRemovePrefixes, p)
		bs.skipRemoveMu.Unlock()
	}(relPrefix)
}

// consumeRemoveSkip returns true (and removes the exact-path skip) if the
// given relPath was marked via SkipNextRemove or falls under a subtree
// suppression set via SkipRemoveSubtree.
func (bs *BlobberSync) consumeRemoveSkip(relPath string) bool {
	bs.skipRemoveMu.Lock()
	defer bs.skipRemoveMu.Unlock()
	if bs.skipRemovePaths[relPath] {
		delete(bs.skipRemovePaths, relPath)
		return true
	}
	for p := range bs.skipRemovePrefixes {
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			return true
		}
	}
	return false
}

// batchDeleteWorker drains deleteChan, batches paths, issues a single
// DoMultiOperation with FileOperationDelete ops. Mirrors batchCommitWorker.
func (bs *BlobberSync) batchDeleteWorker(id int) {
	for {
		select {
		case <-bs.stopCh:
			return
		case first := <-bs.deleteChan:
			batch := []string{first}
			deadline := time.After(bs.batchWaitTime)
		collect:
			for len(batch) < bs.batchSize {
				select {
				case p := <-bs.deleteChan:
					batch = append(batch, p)
				case <-deadline:
					break collect
				case <-bs.stopCh:
					return
				}
			}
			if err := bs.commitDeleteBatch(batch); err != nil {
				log.Printf("[NFS-Sync] delete-worker %d batch(%d) failed: %v", id, len(batch), err)
			}
		}
	}
}

// commitDeleteBatch issues a single DoMultiOperation with FileOperationDelete
// for each relPath. Paths absent on Züs are treated as success (idempotent).
func (bs *BlobberSync) commitDeleteBatch(files []string) error {
	ops := make([]sdk.OperationRequest, 0, len(files))
	for _, relPath := range files {
		ops = append(ops, sdk.OperationRequest{
			OperationType: constants.FileOperationDelete,
			RemotePath:    "/" + relPath,
		})
	}
	if len(ops) == 0 {
		return nil
	}
	if err := bs.alloc.DoMultiOperation(ops); err != nil {
		if isSameRootError(err) || isPathNoExistError(err) {
			return nil
		}
		return fmt.Errorf("DoMultiOperation delete(%d): %w", len(ops), err)
	}
	for _, f := range files {
		if walWriter != nil {
			parts := strings.SplitN(f, "/", 2)
			if len(parts) == 2 {
				walWriter.Delete(parts[0], parts[1])
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admission predicate for cacheBackFullFetch
//
// Problem: on workloads where tmpfs (64 GiB) is small relative to the working
// set (e.g. 1 G dataset + 1 G cache over a larger corpus), unconditional
// cache-back after every range-read miss creates a cache-and-evict feedback
// loop: every GET copies a file into tmpfs, which evicts an older cached
// file, whose next GET misses again, which triggers another cache-back…
// Net effect: slower than NFSCacheDisabled because we pay the copy cost
// without the reuse benefit.
//
// This predicate gates the background cacheBackFullFetch goroutine with
// three admission checks:
//   1) file fits in free tmpfs (with margin) — no eviction needed
//   2) rolling hit-rate > 40 %  — cache is effective
//   3) not recently evicted — don't re-cache a file we just threw out
//
// ShouldCacheFile is the combined gate; RecordHit / MarkRecentlyEvicted
// are the signal updaters called from the S3 GET path and the spill/evict
// paths respectively.
// ---------------------------------------------------------------------------

// hitRateTracker: lock-protected circular buffer of the last 200 request
// outcomes (true = cache hit, false = miss). Approx rate = hits/total.
type hitRateTracker struct {
	mu     sync.Mutex
	recent [200]bool
	idx    int
	hits   int
	total  int
}

func (t *hitRateTracker) RecordHit(hit bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.total >= len(t.recent) {
		if t.recent[t.idx] {
			t.hits--
		}
	} else {
		t.total++
	}
	t.recent[t.idx] = hit
	if hit {
		t.hits++
	}
	t.idx = (t.idx + 1) % len(t.recent)
}

func (t *hitRateTracker) Rate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.total < 100 {
		return 1.0 // warmup period — let cache populate first
	}
	return float64(t.hits) / float64(t.total)
}

// cacheHitRate is the process-wide hit-rate window. Populated from the
// S3 GET path (gateway-zcn.go) on both hits (tmpfs/spillover fast path)
// and misses (fell through to getFileReader).
var cacheHitRate = &hitRateTracker{}

// recentEvicts holds bucket/object keys that were evicted from tmpfs or
// spillover within the last ~30 s. Re-caching such a key would likely
// just evict something else in turn. Entries older than 30 s are treated
// as absent (and opportunistically deleted on next RecentlyEvicted call).
var recentEvicts sync.Map // key: "bucket/object", val: time.Time

func MarkRecentlyEvicted(key string) {
	if key == "" {
		return
	}
	recentEvicts.Store(key, time.Now())
}

func RecentlyEvicted(key string) bool {
	if key == "" {
		return false
	}
	v, ok := recentEvicts.Load(key)
	if !ok {
		return false
	}
	if ts, ok := v.(time.Time); ok {
		if time.Since(ts) < 120*time.Second {
			return true
		}
		recentEvicts.Delete(key)
	}
	return false
}

// ShouldCacheFile returns true only when all three admission checks pass.
// Called from gateway-zcn.go just before spawning cacheBackFullFetch.
func (bs *BlobberSync) ShouldCacheFile(fileSize int64, bucket, object string) bool {
	if bs == nil {
		return false
	}
	if fileSize <= 0 {
		return false
	}
	// 1) Must fit in free tmpfs without forcing an eviction.
	var st syscall.Statfs_t
	if err := syscall.Statfs(bs.exportDir, &st); err == nil {
		free := int64(st.Bavail) * int64(st.Bsize)
		if fileSize+ensureFreeTmpfsMargin > free {
			return false
		}
	}
	// 2) Rolling hit-rate must exceed 40 % (cache is effective).
	if cacheHitRate.Rate() < 0.4 {
		return false
	}
	// 3) Don't re-cache a recently evicted key.
	if RecentlyEvicted(bucket + "/" + object) {
		return false
	}
	return true
}

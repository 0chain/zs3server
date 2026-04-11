package zcn

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
)

// WAL Intent Log — lightweight crash-recovery layer for writeback cache.
//
// Architecture:
//   MinIO writeback cache handles PUT data (write to /mcache, ~1ms) and GET (sendfile, ~0.1ms).
//   WAL records metadata-only "intent" entries (fdatasync, ~0.5ms amortized via group commit).
//   On crash recovery: WAL replay scans for uncommitted intents, matches against /mcache,
//   triggers MinIO's writeback retry for any objects that weren't committed to blobbers.
//
// ACID for all S3 operations:
//   PUT:    writeback cache write + WAL intent fsync → both on NVMe → survives crash
//   GET:    MinIO cache sendfile (fast path) or blobber fetch → always consistent
//   HEAD:   MinIO cache metadata → always consistent
//   LIST:   MinIO writeback includes uncommitted cached objects in listing
//   DELETE: MinIO removes from cache + sends delete to blobbers; WAL marks intent deleted
//   COPY:   Source from cache/blobber, dest goes through same PUT path

const (
	walFileName    = "zs3_wal_intent.log"
	walMaxEntries  = 100000
	directThreshold = 1 * 1024 * 1024 // >1MB: skip WAL intent (writeback cache handles it)
)

type walEntryStatus byte

const (
	walPending   walEntryStatus = 0
	walCommitted walEntryStatus = 1
	walDeleted   walEntryStatus = 2
)

// WALIntent is a lightweight metadata-only entry — no file data stored in WAL
type WALIntent struct {
	Status    walEntryStatus `json:"s"`
	Timestamp int64          `json:"t"`
	Bucket    string         `json:"b"`
	Key       string         `json:"k"`
	Size      int64          `json:"z"`
}

func (e *WALIntent) objectKey() string { return e.Bucket + "/" + e.Key }

type walWriteReq struct {
	intent *WALIntent
	data   []byte
	done   chan error
}

// WALWriter manages the intent log for crash recovery
type WALWriter struct {
	mu       sync.Mutex
	file     *os.File
	fileFd   int
	dir      string
	alloc    *sdk.Allocation
	writeCh  chan walWriteReq
	closed   atomic.Bool

	// Track pending intents for DELETE awareness
	index   map[string]*WALIntent
	indexMu sync.RWMutex
}

func NewWALWriter(dir string, alloc *sdk.Allocation, commitWorkers int) (*WALWriter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("wal: mkdir: %w", err)
	}

	walPath := filepath.Join(dir, walFileName)
	f, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("wal: open: %w", err)
	}

	w := &WALWriter{
		file:    f,
		fileFd:  int(f.Fd()),
		dir:     dir,
		alloc:   alloc,
		writeCh: make(chan walWriteReq, 10000),
		index:   make(map[string]*WALIntent),
	}

	go w.groupCommitWriter()

	replayed, _ := w.replay()
	log.Printf("WAL intent log: dir=%s replayed=%d", walPath, replayed)
	return w, nil
}

// ShouldUseWAL returns true if the file benefits from WAL intent tracking.
// Large files (>1MB) skip WAL — writeback cache handles them fine.
func (w *WALWriter) ShouldUseWAL(size int64) bool {
	return size > 0 && size <= directThreshold
}

// RecordIntent appends a metadata-only intent entry to the WAL.
// Called AFTER MinIO writeback cache writes the data to /mcache.
// The fdatasync ensures the intent survives a crash.
func (w *WALWriter) RecordIntent(bucket, key string, size int64) error {
	intent := &WALIntent{
		Status:    walPending,
		Timestamp: time.Now().UnixNano(),
		Bucket:    bucket,
		Key:       key,
		Size:      size,
	}

	data, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("wal: marshal: %w", err)
	}

	req := walWriteReq{intent: intent, data: data, done: make(chan error, 1)}
	w.writeCh <- req
	err = <-req.done

	if err == nil {
		w.indexMu.Lock()
		w.index[intent.objectKey()] = intent
		w.indexMu.Unlock()
	}
	return err
}

// MarkCommitted removes an intent from the index (object is safely on blobbers).
// Called by MinIO's writeback commit callback.
func (w *WALWriter) MarkCommitted(bucket, key string) {
	w.indexMu.Lock()
	if intent, ok := w.index[bucket+"/"+key]; ok {
		intent.Status = walCommitted
		delete(w.index, bucket+"/"+key)
	}
	w.indexMu.Unlock()
	w.maybeCompact()
}

// Delete marks an intent as deleted (object was deleted before blobber commit).
func (w *WALWriter) Delete(bucket, key string) bool {
	w.indexMu.Lock()
	intent, found := w.index[bucket+"/"+key]
	if found {
		intent.Status = walDeleted
		delete(w.index, bucket+"/"+key)
	}
	w.indexMu.Unlock()
	return found
}

// HasPending returns true if this object has an uncommitted WAL intent
func (w *WALWriter) HasPending(bucket, key string) bool {
	w.indexMu.RLock()
	intent, found := w.index[bucket+"/"+key]
	w.indexMu.RUnlock()
	return found && intent.Status == walPending
}

// ListPending returns all pending intent keys matching a prefix (for LIST merge if needed)
func (w *WALWriter) ListPending(bucket, prefix string) []string {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	var keys []string
	fullPrefix := bucket + "/" + prefix
	for key, intent := range w.index {
		if intent.Status == walPending && strings.HasPrefix(key, fullPrefix) {
			keys = append(keys, intent.Key)
		}
	}
	return keys
}

// --- WAL internals ---

func (w *WALWriter) groupCommitWriter() {
	batch := make([]walWriteReq, 0, 256)
	for {
		req, ok := <-w.writeCh
		if !ok {
			return
		}
		batch = append(batch, req)

		// Drain ready entries
	drain:
		for len(batch) < 256 {
			select {
			case r, ok := <-w.writeCh:
				if !ok {
					break drain
				}
				batch = append(batch, r)
			default:
				break drain
			}
		}

		// Batch write + single fdatasync
		var writeErr error
		w.mu.Lock()
		for _, r := range batch {
			lenBuf := make([]byte, 4)
			binary.LittleEndian.PutUint32(lenBuf, uint32(len(r.data)))
			if _, err := w.file.Write(lenBuf); err != nil {
				writeErr = err
				break
			}
			if _, err := w.file.Write(r.data); err != nil {
				writeErr = err
				break
			}
		}
		if writeErr == nil {
			writeErr = syscall.Fdatasync(w.fileFd)
		}
		w.mu.Unlock()

		for _, r := range batch {
			r.done <- writeErr
		}
		batch = batch[:0]
	}
}

func (w *WALWriter) maybeCompact() {
	w.indexMu.RLock()
	empty := len(w.index) == 0
	w.indexMu.RUnlock()
	if empty {
		w.mu.Lock()
		w.file.Truncate(0)
		w.file.Seek(0, 0)
		w.mu.Unlock()
	}
}

func (w *WALWriter) replay() (int, error) {
	walPath := filepath.Join(w.dir, walFileName)
	f, err := os.Open(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var replayed int
	for {
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(f, lenBuf); err != nil {
			break
		}
		entryLen := binary.LittleEndian.Uint32(lenBuf)
		if entryLen == 0 || entryLen > 1024*1024 {
			break
		}
		data := make([]byte, entryLen)
		if _, err := io.ReadFull(f, data); err != nil {
			break
		}
		var intent WALIntent
		if err := json.Unmarshal(data, &intent); err != nil {
			continue
		}
		if intent.Status == walPending {
			w.index[intent.objectKey()] = &intent
			replayed++
		}
	}
	if replayed > 0 {
		w.file.Truncate(0)
		w.file.Seek(0, 0)
		log.Printf("WAL: %d uncommitted intents found — writeback cache will retry them", replayed)
	}
	return replayed, nil
}

func (w *WALWriter) Close() error {
	w.closed.Store(true)
	close(w.writeCh)
	return w.file.Close()
}

func (w *WALWriter) PendingCount() int {
	w.indexMu.RLock()
	defer w.indexMu.RUnlock()
	return len(w.index)
}

package zcn

// LogCache: log-structured ACID cache for S3 objects.
//
// Data layout: single append-only file on NVMe.
// Each entry: [4-byte dataLen][headerLen][header JSON][raw data]
// In-memory index: map[bucket/key] → entry metadata + file offset
//
// PUT: append entry → group fdatasync → update index → return 200
// GET: index lookup → pread from cache file → write to HTTP response
// DELETE: mark deleted in index, data reclaimed at compaction
// LIST: scan index for prefix matches
//
// Background: drain entries to blobbers via DoMultiOperation
// Crash recovery: replay cache file, rebuild index

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
)

const (
	logCacheFileName  = "zs3_cache.dat"
	lcMaxGroupEntries = 512
	lcMaxGroupBytes   = 4 * 1024 * 1024 // 4MB max per group commit
	lcCommitBatch     = 25
	lcDirectThreshold = 1 * 1024 * 1024 // >1MB: skip cache, go direct to blobbers
)

type lcEntryStatus byte

const (
	lcPending   lcEntryStatus = 0
	lcCommitted lcEntryStatus = 1
	lcDeleted   lcEntryStatus = 2
)

// lcHeader is the metadata stored in the cache file before each data blob
type lcHeader struct {
	Bucket    string        `json:"b"`
	Key       string        `json:"k"`
	Size      int64         `json:"z"`
	MimeType  string        `json:"m"`
	Timestamp int64         `json:"t"`
	Status    lcEntryStatus `json:"s"`
}

// lcIndexEntry is the in-memory index entry pointing into the cache file
type lcIndexEntry struct {
	lcHeader
	DataOffset int64 // byte offset of data in cache file
	DataLen    int64 // actual data length
}

func (e *lcIndexEntry) objectKey() string { return e.Bucket + "/" + e.Key }

// lcWriteReq is a request to write an entry to the cache
type lcWriteReq struct {
	header []byte // serialized header
	data   []byte // raw object data
	entry  *lcIndexEntry
	done   chan error
}

// LogCache manages the log-structured cache
type LogCache struct {
	mu     sync.Mutex
	file   *os.File
	fileFd int
	fileOff int64 // current write offset
	dir    string
	alloc  *sdk.Allocation

	writeCh  chan lcWriteReq
	pending  chan *lcIndexEntry
	commitWG sync.WaitGroup
	closed   atomic.Bool

	index   map[string]*lcIndexEntry
	indexMu sync.RWMutex

	// Stats
	totalPut    atomic.Int64
	totalGet    atomic.Int64
	totalHit    atomic.Int64
	totalMiss   atomic.Int64
	totalCommit atomic.Int64
}

func NewLogCache(dir string, alloc *sdk.Allocation, commitWorkers int) (*LogCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("logcache: mkdir: %w", err)
	}

	cachePath := filepath.Join(dir, logCacheFileName)
	f, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("logcache: open: %w", err)
	}

	// Get current file size (for append offset)
	stat, _ := f.Stat()
	off := stat.Size()

	lc := &LogCache{
		file:    f,
		fileFd:  int(f.Fd()),
		fileOff: off,
		dir:     dir,
		alloc:   alloc,
		writeCh: make(chan lcWriteReq, 10000),
		pending: make(chan *lcIndexEntry, 100000),
		index:   make(map[string]*lcIndexEntry),
	}

	go lc.groupCommitWriter()

	replayed, _ := lc.replay()

	for i := 0; i < commitWorkers; i++ {
		lc.commitWG.Add(1)
		go lc.commitWorker()
	}

	log.Printf("LogCache: file=%s size=%dMB entries=%d workers=%d",
		cachePath, off/(1024*1024), replayed, commitWorkers)
	return lc, nil
}

// ShouldCache returns true if the file size benefits from caching
func (lc *LogCache) ShouldCache(size int64) bool {
	return size > 0 && size <= lcDirectThreshold
}

// --- PUT ---

func (lc *LogCache) Put(bucket, key string, data io.Reader, size int64, mimeType string) error {
	// Read all data into memory (only for small files ≤1MB)
	buf := make([]byte, size)
	n, err := io.ReadFull(data, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fmt.Errorf("logcache: read: %w", err)
	}
	buf = buf[:n]

	hdr := lcHeader{
		Bucket:    bucket,
		Key:       key,
		Size:      int64(n),
		MimeType:  mimeType,
		Timestamp: time.Now().UnixNano(),
		Status:    lcPending,
	}

	headerBytes, err := json.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("logcache: marshal: %w", err)
	}

	entry := &lcIndexEntry{lcHeader: hdr}

	req := lcWriteReq{
		header: headerBytes,
		data:   buf,
		entry:  entry,
		done:   make(chan error, 1),
	}
	lc.writeCh <- req
	err = <-req.done
	if err != nil {
		return err
	}

	lc.indexMu.Lock()
	lc.index[entry.objectKey()] = entry
	lc.indexMu.Unlock()

	lc.pending <- entry
	lc.totalPut.Add(1)
	return nil
}

// --- GET ---

// GetReader returns an io.ReadCloser for the cached object.
// Opens a new file descriptor positioned at the data offset — enables OS-level
// sendfile optimization when Go's io.Copy detects the underlying *os.File.
func (lc *LogCache) GetReader(bucket, key string) (io.ReadCloser, *lcIndexEntry, bool) {
	lc.totalGet.Add(1)

	lc.indexMu.RLock()
	entry, found := lc.index[bucket+"/"+key]
	lc.indexMu.RUnlock()

	if !found || entry.Status == lcDeleted {
		lc.totalMiss.Add(1)
		return nil, nil, false
	}

	// Open new fd for this GET — allows concurrent reads + sendfile
	cachePath := filepath.Join(lc.dir, logCacheFileName)
	f, err := os.Open(cachePath)
	if err != nil {
		lc.totalMiss.Add(1)
		return nil, nil, false
	}
	if _, err := f.Seek(entry.DataOffset, io.SeekStart); err != nil {
		f.Close()
		lc.totalMiss.Add(1)
		return nil, nil, false
	}

	lc.totalHit.Add(1)
	return &limitedFileReader{file: f, remaining: entry.DataLen}, entry, true
}

// limitedFileReader wraps an *os.File with a byte limit.
// Preserves the *os.File type for sendfile detection by Go's io.Copy.
type limitedFileReader struct {
	file      *os.File
	remaining int64
}

func (r *limitedFileReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.file.Read(p)
	r.remaining -= int64(n)
	if r.remaining <= 0 && err == nil {
		err = io.EOF
	}
	return n, err
}

func (r *limitedFileReader) Close() error { return r.file.Close() }

// WriteTo implements io.WriterTo for sendfile optimization.
// When Go's io.Copy sees this, it can use splice/sendfile on Linux.
func (r *limitedFileReader) WriteTo(w io.Writer) (int64, error) {
	// Use io.CopyN which preserves the *os.File → sendfile path
	n, err := io.CopyN(w, r.file, r.remaining)
	r.remaining -= n
	return n, err
}

// Get returns the object data as bytes from cache. Returns nil,false if not cached.
func (lc *LogCache) Get(bucket, key string) ([]byte, *lcIndexEntry, bool) {
	lc.totalGet.Add(1)

	lc.indexMu.RLock()
	entry, found := lc.index[bucket+"/"+key]
	lc.indexMu.RUnlock()

	if !found || entry.Status == lcDeleted {
		lc.totalMiss.Add(1)
		return nil, nil, false
	}

	buf := make([]byte, entry.DataLen)
	_, err := lc.file.ReadAt(buf, entry.DataOffset)
	if err != nil {
		lc.totalMiss.Add(1)
		return nil, nil, false
	}

	lc.totalHit.Add(1)
	return buf, entry, true
}

// --- HEAD ---

func (lc *LogCache) Head(bucket, key string) (*lcIndexEntry, bool) {
	lc.indexMu.RLock()
	entry, found := lc.index[bucket+"/"+key]
	lc.indexMu.RUnlock()
	if !found || entry.Status == lcDeleted {
		return nil, false
	}
	return entry, true
}

// --- LIST ---

func (lc *LogCache) List(bucket, prefix string) []*lcIndexEntry {
	lc.indexMu.RLock()
	defer lc.indexMu.RUnlock()

	var entries []*lcIndexEntry
	fullPrefix := bucket + "/" + prefix
	for key, entry := range lc.index {
		if entry.Status == lcDeleted {
			continue
		}
		if strings.HasPrefix(key, fullPrefix) {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries
}

// --- DELETE ---

func (lc *LogCache) Delete(bucket, key string) bool {
	lc.indexMu.Lock()
	entry, found := lc.index[bucket+"/"+key]
	if found {
		entry.Status = lcDeleted
		delete(lc.index, bucket+"/"+key)
	}
	lc.indexMu.Unlock()
	return found
}

// --- Group commit writer ---

func (lc *LogCache) groupCommitWriter() {
	batch := make([]lcWriteReq, 0, lcMaxGroupEntries)
	var batchBytes int

	for {
		req, ok := <-lc.writeCh
		if !ok {
			return
		}
		batch = append(batch, req)
		batchBytes += len(req.header) + len(req.data) + 8 // 8 for length prefixes

		// Drain ready entries, respecting volume limit
	drain:
		for batchBytes < lcMaxGroupBytes && len(batch) < lcMaxGroupEntries {
			select {
			case r, ok := <-lc.writeCh:
				if !ok {
					break drain
				}
				batch = append(batch, r)
				batchBytes += len(r.header) + len(r.data) + 8
			default:
				break drain
			}
		}

		// Write all entries + single fdatasync
		var writeErr error
		lc.mu.Lock()
		for _, r := range batch {
			// Record offset BEFORE writing
			r.entry.DataOffset = lc.fileOff + 4 + int64(len(r.header)) + 4
			r.entry.DataLen = int64(len(r.data))

			// Write: [4-byte headerLen][header][4-byte dataLen][data]
			hdrLen := make([]byte, 4)
			binary.LittleEndian.PutUint32(hdrLen, uint32(len(r.header)))
			if _, err := lc.file.Write(hdrLen); err != nil {
				writeErr = err
				break
			}
			if _, err := lc.file.Write(r.header); err != nil {
				writeErr = err
				break
			}
			dataLen := make([]byte, 4)
			binary.LittleEndian.PutUint32(dataLen, uint32(len(r.data)))
			if _, err := lc.file.Write(dataLen); err != nil {
				writeErr = err
				break
			}
			if _, err := lc.file.Write(r.data); err != nil {
				writeErr = err
				break
			}
			lc.fileOff += 4 + int64(len(r.header)) + 4 + int64(len(r.data))
		}
		if writeErr == nil {
			writeErr = syscall.Fdatasync(lc.fileFd)
		}
		lc.mu.Unlock()

		for _, r := range batch {
			r.done <- writeErr
		}
		batch = batch[:0]
		batchBytes = 0
	}
}

// --- Background commit to blobbers ---

func (lc *LogCache) commitWorker() {
	defer lc.commitWG.Done()

	batch := make([]*lcIndexEntry, 0, lcCommitBatch)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case entry, ok := <-lc.pending:
			if !ok {
				if len(batch) > 0 {
					lc.commitBatch(batch)
				}
				return
			}
			if entry.Status == lcDeleted {
				continue
			}
			batch = append(batch, entry)
			if len(batch) >= lcCommitBatch {
				lc.commitBatch(batch)
				batch = make([]*lcIndexEntry, 0, lcCommitBatch)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				lc.commitBatch(batch)
				batch = make([]*lcIndexEntry, 0, lcCommitBatch)
			}
		}
	}
}

func (lc *LogCache) commitBatch(entries []*lcIndexEntry) {
	ops := make([]sdk.OperationRequest, 0, len(entries))
	buffers := make([][]byte, 0, len(entries))
	valid := make([]*lcIndexEntry, 0, len(entries))

	for _, entry := range entries {
		if entry.Status != lcPending {
			continue
		}
		// Read data from cache file
		buf := make([]byte, entry.DataLen)
		if _, err := lc.file.ReadAt(buf, entry.DataOffset); err != nil {
			log.Printf("logcache: commit read error %s: %v", entry.Key, err)
			continue
		}
		remotePath := "/" + entry.Bucket + "/" + entry.Key
		ops = append(ops, sdk.OperationRequest{
			OperationType: "insert",
			RemotePath:    remotePath,
			FileReader:    newBytesReadCloser(buf),
			FileMeta: sdk.FileMeta{
				RemotePath: remotePath,
				RemoteName: filepath.Base(entry.Key),
				ActualSize: entry.Size,
				MimeType:   entry.MimeType,
			},
		})
		buffers = append(buffers, buf)
		valid = append(valid, entry)
	}

	if len(ops) == 0 {
		return
	}

	start := time.Now()
	err := lc.alloc.DoMultiOperation(ops)
	elapsed := time.Since(start)

	if err != nil {
		log.Printf("logcache: commit failed (%d ops, %dms): %v", len(ops), elapsed.Milliseconds(), err)
		for _, entry := range valid {
			if !lc.closed.Load() && entry.Status == lcPending {
				select {
				case lc.pending <- entry:
				default:
				}
			}
		}
		return
	}

	lc.totalCommit.Add(int64(len(valid)))
	log.Printf("logcache: committed %d ops in %dms", len(ops), elapsed.Milliseconds())

	// Mark as committed but KEEP in index (for continued GET serving)
	lc.indexMu.Lock()
	for _, entry := range valid {
		entry.Status = lcCommitted
		// Don't remove from index — keep serving GETs from cache
	}
	lc.indexMu.Unlock()
}

// --- Replay ---

func (lc *LogCache) replay() (int, error) {
	lc.file.Seek(0, 0)
	var offset int64
	var count int

	for {
		// Read header length
		hdrLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(lc.file, hdrLenBuf); err != nil {
			break
		}
		hdrLen := binary.LittleEndian.Uint32(hdrLenBuf)
		if hdrLen == 0 || hdrLen > 10*1024 {
			break
		}

		// Read header
		hdrBuf := make([]byte, hdrLen)
		if _, err := io.ReadFull(lc.file, hdrBuf); err != nil {
			break
		}

		// Read data length
		dataLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(lc.file, dataLenBuf); err != nil {
			break
		}
		dataLen := binary.LittleEndian.Uint32(dataLenBuf)

		var hdr lcHeader
		if err := json.Unmarshal(hdrBuf, &hdr); err != nil {
			// Skip corrupted entry
			lc.file.Seek(int64(dataLen), 1)
			offset += 4 + int64(hdrLen) + 4 + int64(dataLen)
			continue
		}

		dataOffset := offset + 4 + int64(hdrLen) + 4
		entry := &lcIndexEntry{
			lcHeader:   hdr,
			DataOffset: dataOffset,
			DataLen:    int64(dataLen),
		}

		// Skip data (we have the offset, no need to read it now)
		lc.file.Seek(int64(dataLen), 1)
		offset += 4 + int64(hdrLen) + 4 + int64(dataLen)

		if hdr.Status == lcPending {
			lc.index[entry.objectKey()] = entry
			lc.pending <- entry
			count++
		} else if hdr.Status == lcCommitted {
			// Keep committed entries in index for GET serving
			lc.index[entry.objectKey()] = entry
		}
	}

	lc.fileOff = offset
	lc.file.Seek(offset, 0) // Position for appending
	return count, nil
}

func (lc *LogCache) Close() error {
	lc.closed.Store(true)
	close(lc.writeCh)
	close(lc.pending)
	lc.commitWG.Wait()
	return lc.file.Close()
}

func (lc *LogCache) Stats() string {
	lc.indexMu.RLock()
	indexSize := len(lc.index)
	lc.indexMu.RUnlock()
	return fmt.Sprintf("put=%d get=%d hit=%d miss=%d commit=%d index=%d fileOff=%dMB",
		lc.totalPut.Load(), lc.totalGet.Load(), lc.totalHit.Load(),
		lc.totalMiss.Load(), lc.totalCommit.Load(), indexSize, lc.fileOff/(1024*1024))
}

// --- Helpers ---

type bytesReadCloser struct {
	*strings.Reader
}

func newBytesReadCloser(b []byte) io.ReadCloser {
	return &bytesReadCloser{strings.NewReader(string(b))}
}

func (b *bytesReadCloser) Close() error { return nil }

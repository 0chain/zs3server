// Copyright (c) 2015-2021 MinIO, Inc.
//
// Prewarm endpoint: fetch a Zus object into /nfs_export so the NFS-Ganesha
// VFS FSAL can serve it on the next read-miss retry. Called by the FSAL
// libcurl plugin when a NFS GET hits a missing file.
package zcn

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/logger"
	"golang.org/x/sync/singleflight"
)


// prewarmWriteSem limits the number of concurrent prewarm writes to
// tmpfs. Without this, N parallel NFS-initiated prewarms can each try
// to stream their blobber-fetched bytes into /nfs_export simultaneously;
// the 2s spilloverMonitor tick cannot drain fast enough → ENOSPC →
// partial-write stub → parquet [0,0,0,0] (Race G thundering herd).
// Serialising the write phase pairs naturally with the 60% spill
// threshold: each prewarm sees either headroom or waits its turn.
// singleflight dedups same-file prewarms before this mutex.
var prewarmWriteSem = make(chan struct{}, 3)
var prewarmGroup singleflight.Group

type prewarmReq struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	FileID uint64 `json:"fileid,omitempty"`
}

type prewarmResp struct {
	Path  string `json:"path,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func prewarmHandler(w http.ResponseWriter, r *http.Request) {
	var req prewarmReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, prewarmResp{Error: "invalid json"})
		return
	}
	if (req.Bucket == "" || req.Key == "") && req.FileID != 0 {
		rel := inodeRelGet(req.FileID)
		if rel == "" {
			writeJSON(w, http.StatusNotFound, prewarmResp{Error: "fileid not in inode map"})
			return
		}
		slash := strings.Index(rel, "/")
		if slash <= 0 || slash == len(rel)-1 {
			writeJSON(w, http.StatusInternalServerError, prewarmResp{Error: "invalid rel: " + rel})
			return
		}
		req.Bucket = rel[:slash]
		req.Key = rel[slash+1:]
	}
	if req.Bucket == "" || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, prewarmResp{Error: "need bucket+key or fileid"})
		return
	}

	exportDir := serverConfig.NFSGaneshaExportDir
	if exportDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, prewarmResp{Error: "nfs export dir not configured"})
		return
	}
	alloc := currentAlloc
	if alloc == nil {
		writeJSON(w, http.StatusServiceUnavailable, prewarmResp{Error: "allocation not initialized"})
		return
	}

	relPath := filepath.Join(req.Bucket, req.Key)
	finalPath := filepath.Join(exportDir, relPath)

	// Fast path: file already present AND not a stub placeholder.
	isStub := func(path string) bool {
		buf := make([]byte, 4)
		n, xerr := syscall.Getxattr(path, "user.zus.stub", buf)
		return xerr == nil && n > 0
	}
	if fi, err := os.Stat(finalPath); err == nil && !fi.IsDir() && !isStub(finalPath) {
		atomic.AddInt64(&nfsTmpfsHitCount, 1)
		writeJSON(w, http.StatusOK, prewarmResp{Path: finalPath, Size: fi.Size()})
		return
	}

	v, err, _ := prewarmGroup.Do(relPath, func() (interface{}, error) {
		// Re-check after singleflight wait.
		if fi, err := os.Stat(finalPath); err == nil && !fi.IsDir() && !isStub(finalPath) {
			atomic.AddInt64(&nfsTmpfsHitCount, 1)
			return prewarmResp{Path: finalPath, Size: fi.Size()}, nil
		}

		// Spillover-restore: an evicted file's bytes live in the spillover
		// dir. Copy them back into /nfs_export in-place (preserves inode
		// for cached Ganesha handles), with the same race-safe ordering as
		// the blobber-fetch path below: write bytes → set committed xattr
		// → drop stub xattr → MarkCommitted last. spillCommittedFiles
		// requires the xattr+map gate, so this restore can't be raced.
		if serverConfig.NFSSpilloverDir != "" {
			spillDir := serverConfig.NFSSpilloverDir
			spillPath := filepath.Join(spillDir, relPath)
			if spillInfo, sErr := os.Stat(spillPath); sErr == nil && !spillInfo.IsDir() {
				sf, ferr2 := os.Open(spillPath)
				if ferr2 == nil {
					f, oerr := os.OpenFile(finalPath, os.O_WRONLY|os.O_CREATE, 0o644)
					if oerr == nil {
						_, _ = f.Seek(0, 0)
						n, _ := io.Copy(f, sf)
						f.Sync()
						f.Close()
						sf.Close()
						if n == spillInfo.Size() {
							_ = syscall.Setxattr(finalPath, "user.zus.committed", []byte{'1'}, 0)
							_ = syscall.Removexattr(finalPath, "user.zus.stub")
							if currentBS != nil {
								currentBS.MarkCommitted(relPath)
							}
							atomic.AddInt64(&nfsSpilloverHitCount, 1)
							logger.Info("prewarm spillover: restored %s from %s size=%d", finalPath, spillPath, n)
							return prewarmResp{Path: finalPath, Size: n}, nil
						}
					}
					sf.Close()
				}
			}
		}

		remotePath := "/" + relPath
		// Retry blobber GET on transient errors (503, consensus_not_met,
		// "Blobber not registered") with escalating backoff: 200, 500,
		// 1000, 2000ms. These errors happen during heavy Spark fan-out
		// when a blobber is momentarily rate-limited or a sharder lags.
		// Inside singleflight, so only one retry chain per relPath.
		var reader io.Reader
		var oi *minio.ObjectInfo
		var closer func()
		var ferr error
		backoffs := []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, 1000 * time.Millisecond, 2000 * time.Millisecond}
		for attempt := 0; attempt <= len(backoffs); attempt++ {
			reader, oi, closer, _, ferr = getFileReader(r.Context(), alloc, req.Bucket, req.Key, remotePath, 0, -1)
			if ferr == nil {
				break
			}
			if !isBlobberTransientErr(ferr) {
				break
			}
			if attempt >= len(backoffs) {
				break
			}
			logger.Info("prewarm blobber transient: path=%s attempt=%d err=%v", remotePath, attempt, ferr)
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(backoffs[attempt]):
			}
		}
		// Fallback to upstream S3 when the file is not in zus allocation and
		// fallback_s3_enabled=true. Bytes are teed: streamed to the prewarm
		// writer AND asynchronously cached back to zus via teeReadCloser.
		if ferr != nil {
			if isPathNoExistError(ferr) && serverConfig.FallbackS3Enabled {
				fbReader, fbInfo, fbErr := fallbackFetchSingleflight(r.Context(), alloc, req.Bucket, req.Key)
				if fbErr == nil {
					reader = fbReader
					_ = fbInfo; oi = nil
					closer = nil  // teeReadCloser handles its own close
					ferr = nil
					logger.Info("prewarm fallback_s3: fetched %s/%s from upstream", req.Bucket, req.Key)
				}
			}
			if ferr != nil {
				return nil, ferr
			}
		}
		if closer != nil {
			defer closer()
		}
		// NFS-origin blobber (or fallback-S3) fetch succeeded — this is
		// the miss path that drives real network reads for NFS traffic.
		atomic.AddInt64(&nfsPrewarmFetchCount, 1)

		if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
			return nil, err
		}

		// In-place write that PRESERVES the inode. Required because Ganesha
		// caches inode-based file handles from earlier readdir/lookup; a
		// temp+rename here would leave Ganesha holding a stale handle to the
		// orphaned stub inode (sparse zeros) — causing parquet/EOF
		// corruption observed when Spark reads via NFS even though /nfs_export
		// shows correct bytes.
		//
		// Race protection vs spillCommittedFiles is provided by:
		//   1. MarkCommitted is called AFTER the write completes, so the
		//      file is NOT in bs.committed during the in-flight io.Copy.
		//   2. spillCommittedFiles also requires the user.zus.committed
		//      xattr (set after write here), so even a stale in-memory
		//      candidate can't trigger an eviction mid-write.
		// Race protection vs concurrent NFS readers is provided by FSAL_ZUS
		// open2 + zs3server's per-file singleflight: only one prewarm runs
		// at a time per relPath, and FSAL_ZUS waits synchronously before
		// delegating the read to the sub-FSAL.
		//
		// Open without O_TRUNC so the file size remains the original stub
		// size throughout — concurrent stat()s never see a 0-byte transient.
		var stubSt syscall.Stat_t
		_ = syscall.Lstat(finalPath, &stubSt)
		originalStubSize := stubSt.Size

		// Race G (prewarm-ENOSPC-during-stub-restore): pre-ensure tmpfs
		// headroom BEFORE io.Copy starts. The blobber reader is
		// forward-only, once io.Copy fails mid-stream there is no clean
		// way to restart it, so we trigger proactive 2-tier LRU FIRST.
		// EnsureFreeTmpfs moves oldest committed files tmpfs→spillover
		// one-at-a-time until free >= expectedSize + margin, cascading
		// oldest spillover → delete when spillover itself is full. The
		// 1s spilloverMonitor remains as a 60%-threshold safety net for
		// writers that bypass this path (e.g. S3 PUT mirror).
		//
		// Large-file fallback: when expectedSize exceeds tmpfs total
		// capacity (minus margin), no eviction can ever make room. In
		// that case we skip EnsureFreeTmpfs and write directly to the
		// spillover directory, setting the committed xattr so the file
		// is immediately a legitimate spillover entry (same invariants
		// as spillCommittedFiles output). Assumption: file_size <=
		// spillover_max_bytes; if not, the write may exceed spillover
		// cap but will still succeed on-disk.
		var writePath = finalPath
		var useSpilloverDirect bool
		{
			prewarmWriteSem <- struct{}{}
			defer func() { <-prewarmWriteSem }()
			var expectedSize int64 = originalStubSize
			if oi != nil && oi.Size > 0 {
				expectedSize = oi.Size
			}
			if expectedSize > 0 && currentBS != nil {
				if currentBS.ShouldUseSpillover(expectedSize) &&
					serverConfig.NFSSpilloverDir != "" {
					useSpilloverDirect = true
					writePath = filepath.Join(serverConfig.NFSSpilloverDir, relPath)
					if mkErr := os.MkdirAll(filepath.Dir(writePath), 0o755); mkErr != nil {
						return nil, mkErr
					}
					logger.Info("prewarm large-file: size=%d exceeds tmpfs — writing directly to spillover %s",
						expectedSize, writePath)
				} else if err := currentBS.EnsureFreeTmpfs(expectedSize); err != nil {
					// Log but continue — os.Create below will ENOSPC
					// cleanly if truly out of space, and concurrent
					// prewarms may have freed room in the interim.
					logger.Info("prewarm EnsureFreeTmpfs(%d) failed: %v — proceeding", expectedSize, err)
				}
			}
		}
		// When writing direct to spillover, use temp+rename (no Ganesha
		// handle concerns for spillover files). When writing to tmpfs,
		// keep the in-place open that preserves the stub inode.
		var f *os.File
		var openPath string
		var openErr error
		if useSpilloverDirect {
			openPath = writePath + ".prewarm"
			f, openErr = os.Create(openPath)
		} else {
			openPath = writePath
			f, openErr = os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE, 0o644)
		}
		if openErr != nil {
			return nil, openErr
		}
		if !useSpilloverDirect {
			if _, serr := f.Seek(0, 0); serr != nil {
				f.Close()
				return nil, serr
			}
		}
		n, cerr := io.Copy(f, reader)
		if cerr != nil {
			f.Close()
			if useSpilloverDirect {
				os.Remove(openPath)
			} else {
				// Tmpfs-direct path: restore the stub so readers do not see
				// a partial prewarm. Truncate to 0, then sparse-resize to
				// originalStubSize. stub xattr is already set; leave it.
				if tf, oerr := os.OpenFile(openPath, os.O_WRONLY|os.O_TRUNC, 0644); oerr == nil {
					tf.Close()
				}
				if originalStubSize > 0 {
					_ = os.Truncate(openPath, originalStubSize)
				}
			}
			return nil, cerr
		}
		if err := f.Sync(); err != nil {
			f.Close()
			if useSpilloverDirect {
				os.Remove(openPath)
			}
			return nil, err
		}
		if err := f.Close(); err != nil {
			if useSpilloverDirect {
				os.Remove(openPath)
			}
			return nil, err
		}

		// Validate: did we get a complete file?
		var expected int64 = originalStubSize
		if oi != nil && oi.Size > 0 {
			expected = oi.Size
		}
		if n == 0 {
			// Genuinely empty download — restore stub so next read retries.
			if useSpilloverDirect {
				os.Remove(openPath)
			} else {
				if tf, oerr := os.OpenFile(openPath, os.O_WRONLY|os.O_TRUNC, 0644); oerr == nil {
					tf.Close()
				}
				if originalStubSize > 0 {
					_ = os.Truncate(openPath, originalStubSize)
				}
			}
			logger.Info("prewarm empty-read: %s expected=%d — preserving stub",
				finalPath, expected)
			return nil, fmt.Errorf("prewarm empty-read: expected=%d", expected)
		}
		if expected > 0 && n != expected {
			// io.Copy succeeded (cerr==nil, n>0) but metadata disagrees with
			// actual bytes written — this happens when the best-effort fallback
			// ref in filerefsworker returns a ref with stale ActualFileSize.
			// Trust n (actual bytes on disk); do NOT restore the stub.
			logger.Info("prewarm size-mismatch (metadata): %s n=%d expected=%d — accepting actual bytes",
				finalPath, n, expected)
		}

		// Spillover-direct path: set xattr on temp BEFORE rename so it
		// travels with the inode atomically; then rename into place.
		// Note: we do NOT touch the stub at finalPath — FSAL_ZUS reopen2
		// knows to look up a matching spillover file when tmpfs shows a
		// stub, so the prewarm response returns the spillover path and
		// subsequent NFS reads will traverse spillover naturally.
		if useSpilloverDirect {
			if xerr := syscall.Setxattr(openPath, "user.zus.committed", []byte{'1'}, 0); xerr != nil {
				logger.LogIf(r.Context(), xerr)
			}
			if rerr := os.Rename(openPath, writePath); rerr != nil {
				os.Remove(openPath)
				return nil, rerr
			}
			_ = syscall.Setxattr(writePath, "user.zus.committed", []byte{'1'}, 0)
			logger.Info("prewarm large-file: wrote %s size=%d to spillover", writePath, n)
			size := n
			if oi != nil && oi.Size > 0 {
				size = oi.Size
			}
			return prewarmResp{Path: writePath, Size: size}, nil
		}

		// If the stub was sparse-extended beyond the real file size (can
		// happen when list_router fell back from ActualSize=0 to a larger
		// per-shard size), truncate the tail so readers never see zeros
		// where parquet footer magic should be.
		if originalStubSize > n {
			if terr := os.Truncate(finalPath, n); terr != nil {
				logger.LogIf(r.Context(), fmt.Errorf("prewarm tail-truncate %s to %d: %w", finalPath, n, terr))
			}
		}

		// Tag the inode as committed BEFORE removing the stub xattr and
		// BEFORE adding to bs.committed, so any racing observer
		// (spillCommittedFiles, processEvents, initialScan) sees a
		// consistent "committed, not stub" state.
		if xerr := syscall.Setxattr(finalPath, "user.zus.committed", []byte{'1'}, 0); xerr != nil {
			logger.LogIf(r.Context(), xerr)
		}
		if xerr := syscall.Removexattr(finalPath, "user.zus.stub"); xerr != nil && xerr != syscall.ENODATA {
			logger.LogIf(r.Context(), xerr)
		}
		// Now safe to admit to the in-memory candidate set; spillover may
		// evict this file at any future point.
		if currentBS != nil {
			currentBS.MarkCommitted(relPath)
		}

		size := n
		if oi != nil && oi.Size > 0 {
			size = oi.Size
		}
		return prewarmResp{Path: finalPath, Size: size}, nil
	})

	if err != nil {
		logger.LogIf(r.Context(), err)
		msg := err.Error()
		if isNotFoundErr(msg) {
			writeJSON(w, http.StatusNotFound, prewarmResp{Error: "not found"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, prewarmResp{Error: msg})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func isNotFoundErr(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "not_found") ||
		strings.Contains(low, "not found") ||
		strings.Contains(low, "file_does_not_exist") ||
		strings.Contains(low, "no_tokens") ||
		strings.Contains(low, "consensus_not_met")
}

// isBlobberTransientErr returns true for blobber errors that usually clear
// within a second or two: rate-limiting (503), consensus churn (sharder
// lag), or a blobber not yet registered with the allocation.
func isBlobberTransientErr(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "503") ||
		strings.Contains(low, "consensus_not_met") ||
		strings.Contains(low, "blobber not registered")
}

// RegisterPrewarmRouter wires POST /internal/prewarm onto the given router.
// Called from cmd gateway-main via the GatewayExtraRouters hook.
func RegisterPrewarmRouter(router *mux.Router) {
	router.Methods(http.MethodPost).Path("/internal/prewarm").HandlerFunc(prewarmHandler)
}

func init() {
	minio.GatewayExtraRouters = append(minio.GatewayExtraRouters, RegisterPrewarmRouter)
}

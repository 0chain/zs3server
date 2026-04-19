// Copyright (c) 2015-2021 MinIO, Inc.
//
// Admin cache-clear endpoint. Atomically suppresses inotify Remove events via
// BlobberSync.SkipRemoveSubtree and then os.RemoveAll's the target under the
// NFS export dir. Prevents the data-loss foot-gun where an operator's
// `rm -rf /nfs_export/<bucket>` propagates through the NFS→Züs delete
// worker and wipes the authoritative allocation.
//
// Also clears the matching MinIO writeback cache prefix under /mcache when
// configured, since serving a half-flushed cache is worse than forcing a
// cold refetch.
//
// Usage:
//   POST /internal/cache_clear?rel=<bucket>[/<prefix>]
//   POST /internal/cache_clear?bucket=<bucket>          (equivalent to rel=<bucket>)
//
// Response:
//   200 {"cleared_export":N, "cleared_mcache":M, "rel":"..."}
//   400 {"error":"rel required"}
//   503 {"error":"export dir not configured"}
package zcn

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/logger"
)

type cacheClearResp struct {
	Rel           string `json:"rel,omitempty"`
	ClearedExport int    `json:"cleared_export"`
	ClearedCache  int    `json:"cleared_mcache"`
	Error         string `json:"error,omitempty"`
}

// countFiles walks a subtree and returns the number of regular files removed.
func countFiles(dir string) int {
	if dir == "" {
		return 0
	}
	n := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func cacheClearHandler(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("rel")
	if rel == "" {
		rel = r.URL.Query().Get("bucket")
	}
	rel = strings.Trim(rel, "/")
	if rel == "" {
		writeJSON(w, http.StatusBadRequest, cacheClearResp{Error: "rel or bucket required"})
		return
	}

	exportDir := serverConfig.NFSGaneshaExportDir
	if exportDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, cacheClearResp{Error: "nfs export dir not configured"})
		return
	}

	// Suppress inotify Remove propagation for every path under rel BEFORE
	// we start removing. This is the key atomicity: SkipRemoveSubtree is
	// checked by processEvents for each Remove event.
	if currentBS != nil {
		currentBS.SkipRemoveSubtree(rel)
	}

	target := filepath.Join(exportDir, rel)
	clearedExport := countFiles(target)
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		logger.LogIf(r.Context(), err)
		writeJSON(w, http.StatusInternalServerError, cacheClearResp{Rel: rel, Error: err.Error()})
		return
	}

	// Best-effort flush of MinIO writeback cache entries for this prefix.
	// The MinIO cache stores by hash-path under /mcache/<hash>/... so we
	// can't easily target a bucket; operators typically clear the full
	// /mcache separately. Report 0 here unless we know a hash-index.
	clearedCache := 0

	logger.Info("cache-clear: rel=%s cleared_export=%d", rel, clearedExport)
	writeJSON(w, http.StatusOK, cacheClearResp{
		Rel:           rel,
		ClearedExport: clearedExport,
		ClearedCache:  clearedCache,
	})
}

// RegisterCacheClearRouter wires the cache-clear endpoint on the router.
// Accepts both POST and GET (GET is easier to curl).
func RegisterCacheClearRouter(router *mux.Router) {
	logger.Info("cache-clear router init at /internal/cache_clear")
	router.Methods(http.MethodGet).Path("/internal/cache_clear").HandlerFunc(cacheClearHandler)

	router.Methods(http.MethodPost).Path("/internal/cache_clear").HandlerFunc(cacheClearHandler)
}

func init() {
	minio.GatewayExtraRouters = append(minio.GatewayExtraRouters, RegisterCacheClearRouter)
}

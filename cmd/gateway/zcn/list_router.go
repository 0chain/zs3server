// Copyright (c) 2015-2021 MinIO, Inc.
//
// List endpoint: enumerate direct children of a Zus allocation path so the
// NFS-Ganesha FSAL_ZUS plugin can surface blobber-only files in readdir.
// When ?stub=1 is set we also materialize 0-byte placeholder files in
// /nfs_export for any child not already present and suppress the resulting
// inotify Create events via BlobberSync.MarkCommitted, so the next sub-FSAL
// readdir from Ganesha picks them up as real dirents. A later lookup/open
// on the placeholder triggers the existing prewarm path which overwrites
// it with real bytes.
package zcn

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"syscall"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/logger"
)

type listEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	Mtime int64  `json:"mtime"`
}

type listResp struct {
	Entries []listEntry `json:"entries"`
	Stubbed int         `json:"stubbed,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// server-side TTL cache to absorb readdir storms from multiple NFS clients.
// FSAL_ZUS also caches client-side with 60s TTL; this is a secondary guard.
const listTTL = 30 * time.Second

type listCacheEntry struct {
	resp listResp
	ts   time.Time
}

var (
	listCacheMu sync.Mutex
	listCache   = map[string]listCacheEntry{}
)

func listCacheGet(k string) (listResp, bool) {
	listCacheMu.Lock()
	defer listCacheMu.Unlock()
	e, ok := listCache[k]
	if !ok || time.Since(e.ts) > listTTL {
		return listResp{}, false
	}
	return e.resp, true
}

func listCachePut(k string, r listResp) {
	listCacheMu.Lock()
	defer listCacheMu.Unlock()
	listCache[k] = listCacheEntry{resp: r, ts: time.Now()}
}

func listHandler(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	prefix := r.URL.Query().Get("prefix")
	stub, _ := strconv.ParseBool(r.URL.Query().Get("stub"))

	if bucket == "" {
		writeJSON(w, http.StatusBadRequest, listResp{Error: "bucket required"})
		return
	}
	alloc := currentAlloc
	if alloc == nil {
		writeJSON(w, http.StatusServiceUnavailable, listResp{Error: "allocation not initialized"})
		return
	}

	// Normalize: bucket/prefix -> "/bucket/prefix" without trailing slash.
	remotePath := "/" + bucket
	if prefix != "" {
		remotePath = path.Join("/"+bucket, prefix)
	}
	remotePath = strings.TrimRight(remotePath, "/")
	if remotePath == "" {
		remotePath = "/"
	}

	cacheKey := remotePath + "|stub=" + strconv.FormatBool(stub)
	if cached, ok := listCacheGet(cacheKey); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	res, err := alloc.ListDir(remotePath)
	if err != nil {
		logger.LogIf(r.Context(), err)
		if isNotFoundErr(err.Error()) {
			writeJSON(w, http.StatusNotFound, listResp{Error: "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, listResp{Error: err.Error()})
		return
	}

	exportDir := serverConfig.NFSGaneshaExportDir
	localDir := ""
	if stub && exportDir != "" {
		// remotePath begins with '/', strip it when joining under exportDir.
		localDir = filepath.Join(exportDir, strings.TrimPrefix(remotePath, "/"))
	}

	resp := listResp{Entries: make([]listEntry, 0, len(res.Children))}
	for _, c := range res.Children {
		if c == nil || c.Name == "" {
			continue
		}
		size := c.ActualSize
		if size == 0 {
			size = c.Size
		}
		isDir := c.Type == "d"
		resp.Entries = append(resp.Entries, listEntry{
			Name:  c.Name,
			Size:  size,
			IsDir: isDir,
			Mtime: int64(c.UpdatedAt),
		})

		if !stub || localDir == "" {
			continue
		}
		// Materialize placeholder (if absent) and suppress inotify event.
		target := filepath.Join(localDir, c.Name)
		rel := strings.TrimPrefix(remotePath, "/")
		if rel != "" {
			rel = rel + "/" + c.Name
		} else {
			rel = c.Name
		}
		if _, statErr := os.Lstat(target); statErr == nil {
			continue // already present — real file or prior stub
		}
		if err := os.MkdirAll(localDir, 0o755); err != nil {
			logger.LogIf(r.Context(), err)
			continue
		}
		if currentBS != nil {
			currentBS.MarkCommitted(rel)
		}
		if isDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				logger.LogIf(r.Context(), err)
				continue
			}
		} else {
			f, cerr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if cerr != nil {
				if !os.IsExist(cerr) {
					logger.LogIf(r.Context(), cerr)
				}
				continue
			}
			f.Close()
			// Sparse-truncate to real size so stat() returns correct length
			// (no blocks allocated). Mark as stub so FSAL_ZUS prewarms on open.
			if size > 0 {
				if terr := os.Truncate(target, size); terr != nil {
					logger.LogIf(r.Context(), terr)
				}
			}
			if xerr := syscall.Setxattr(target, "user.zus.stub", []byte{'1'}, 0); xerr != nil {
				logger.LogIf(r.Context(), xerr)
			}
		}
		resp.Stubbed++
	}

	listCachePut(cacheKey, resp)
	writeJSON(w, http.StatusOK, resp)
}

// RegisterListRouter wires GET /internal/list onto the given router.
func RegisterListRouter(router *mux.Router) {
	router.Methods(http.MethodGet).Path("/internal/list").HandlerFunc(listHandler)
}

func init() {
	minio.GatewayExtraRouters = append(minio.GatewayExtraRouters, RegisterListRouter)
}

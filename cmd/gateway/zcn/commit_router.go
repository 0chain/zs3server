// Copyright (c) 2015-2021 MinIO, Inc.
//
// /internal/commit endpoint: synchronously commit a file sitting in
// /nfs_export to blobbers. Called by FSAL_ZUS close2 when an NFS client
// closes a file it wrote, so the NFS CLOSE op blocks until the data is
// durably on blobbers (same semantics as mc cp / S3 PutObject).
//
// Idempotent: if user.zus.committed xattr is already set, returns 200
// without re-uploading. FSAL_ZUS write2 clears the xattr on dirty writes.
package zcn

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"syscall"

	"github.com/gorilla/mux"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/logger"
)

type commitResp struct {
	Rel   string `json:"rel,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Skip  bool   `json:"skip,omitempty"`
	Error string `json:"error,omitempty"`
}

func commitHandler(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		writeJSON(w, http.StatusBadRequest, commitResp{Error: "bucket+key required"})
		return
	}
	exportDir := serverConfig.NFSGaneshaExportDir
	if exportDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, commitResp{Error: "nfs export dir not configured"})
		return
	}
	rel := path.Join(bucket, key)
	localPath := filepath.Join(exportDir, rel)

	// Fast path: already committed (read-only close, or duplicate).
	var cbuf [2]byte
	if n, _ := syscall.Getxattr(localPath, "user.zus.committed", cbuf[:]); n > 0 {
		writeJSON(w, http.StatusOK, commitResp{Rel: rel, Skip: true})
		return
	}
	// Dont try to upload a stub (sparse placeholder from /internal/list).
	if n, _ := syscall.Getxattr(localPath, "user.zus.stub", cbuf[:]); n > 0 {
		writeJSON(w, http.StatusOK, commitResp{Rel: rel, Skip: true})
		return
	}

	fi, err := os.Lstat(localPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, commitResp{Error: err.Error()})
		return
	}
	if fi.IsDir() {
		writeJSON(w, http.StatusOK, commitResp{Rel: rel, Skip: true})
		return
	}
	size := fi.Size()

	// Mark committed in-memory BEFORE the upload, so BlobberSyncs
	// processEvents skips any inotify event this path generates.
	if currentBS != nil {
		currentBS.MarkCommitted(rel)
	}

	f, err := os.Open(localPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, commitResp{Error: err.Error()})
		return
	}
	defer f.Close()

	remotePath := "/" + rel
	contentType := "application/octet-stream"
	// Reuse putFile — same code path as S3 PutObject takes.
	if putErr := putFile(r.Context(), currentAlloc, remotePath, contentType, f, size, false, nil); putErr != nil {
		writeJSON(w, http.StatusInternalServerError, commitResp{Error: putErr.Error()})
		return
	}
	// Success: tag as committed so:
	//   1. idempotent repeat calls short-circuit here
	//   2. BlobberSync.initialScan skips on restart
	//   3. spillCommittedFiles treats as eligible for eviction
	if xerr := syscall.Setxattr(localPath, "user.zus.committed", []byte{'1'}, 0); xerr != nil {
		logger.LogIf(r.Context(), xerr)
	}
	writeJSON(w, http.StatusOK, commitResp{Rel: rel, Size: size})
}

func RegisterCommitRouter(router *mux.Router) {
	router.Methods(http.MethodPost).Path("/internal/commit").HandlerFunc(commitHandler)
	router.Methods(http.MethodGet).Path("/internal/commit").HandlerFunc(commitHandler)
}

func init() {
	minio.GatewayExtraRouters = append(minio.GatewayExtraRouters, RegisterCommitRouter)
}

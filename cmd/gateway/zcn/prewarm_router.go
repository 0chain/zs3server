// Copyright (c) 2015-2021 MinIO, Inc.
//
// Prewarm endpoint: fetch a Zus object into /nfs_export so the NFS-Ganesha
// VFS FSAL can serve it on the next read-miss retry. Called by the FSAL
// libcurl plugin when a NFS GET hits a missing file.
package zcn

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/logger"
	"golang.org/x/sync/singleflight"
)

var prewarmGroup singleflight.Group

type prewarmReq struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Bucket == "" || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, prewarmResp{Error: "invalid request"})
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
		writeJSON(w, http.StatusOK, prewarmResp{Path: finalPath, Size: fi.Size()})
		return
	}

	v, err, _ := prewarmGroup.Do(relPath, func() (interface{}, error) {
		// Re-check after singleflight wait.
		if fi, err := os.Stat(finalPath); err == nil && !fi.IsDir() && !isStub(finalPath) {
			return prewarmResp{Path: finalPath, Size: fi.Size()}, nil
		}

		remotePath := "/" + relPath
		reader, oi, closer, _, ferr := getFileReader(r.Context(), alloc, req.Bucket, req.Key, remotePath, 0, -1)
		if ferr != nil {
			return nil, ferr
		}
		if closer != nil {
			defer closer()
		}

		if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
			return nil, err
		}
		tmpName := filepath.Join(filepath.Dir(finalPath), ".prewarm-"+uuid.New().String())
		f, err := os.Create(tmpName)
		if err != nil {
			return nil, err
		}
		n, cerr := io.Copy(f, reader)
		if cerr != nil {
			f.Close()
			os.Remove(tmpName)
			return nil, cerr
		}
		if err := f.Close(); err != nil {
			os.Remove(tmpName)
			return nil, err
		}

		// Pre-mark committed so the inotify Create event for the rename target is ignored.
		if currentBS != nil {
			currentBS.MarkCommitted(relPath)
		}
		if err := os.Rename(tmpName, finalPath); err != nil {
			os.Remove(tmpName)
			return nil, err
		}
		// Clear stub xattr if it was a stub placeholder; ignore ENODATA.
		if xerr := syscall.Removexattr(finalPath, "user.zus.stub"); xerr != nil && xerr != syscall.ENODATA {
			logger.LogIf(r.Context(), xerr)
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

// RegisterPrewarmRouter wires POST /internal/prewarm onto the given router.
// Called from cmd gateway-main via the GatewayExtraRouters hook.
func RegisterPrewarmRouter(router *mux.Router) {
	router.Methods(http.MethodPost).Path("/internal/prewarm").HandlerFunc(prewarmHandler)
}

func init() {
	minio.GatewayExtraRouters = append(minio.GatewayExtraRouters, RegisterPrewarmRouter)
}

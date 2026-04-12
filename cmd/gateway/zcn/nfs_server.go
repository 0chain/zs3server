package zcn

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/0chain/gosdk/zboxcore/sdk"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// StartNFSServer starts an NFSv3 server exposing the Züs allocation as a
// POSIX filesystem.
//
// Cache modes:
//
//	"disk"   (default) — writes go through MinIO CacheObjectLayer → /mcache NVMe.
//	                     ACID: crash-safe (data persists on NVMe + WAL).
//	"memory" — writes go to in-memory map, async commit to blobbers via putFile.
//	           Fastest (~0.1ms/write), but NO crash recovery.
func StartNFSServer(port int, alloc *sdk.Allocation, cacheDir, cacheMode string) error {
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "zs3-nfs-cache")
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("nfs: create cache dir: %w", err)
	}

	useMemoryMode := cacheMode == "memory"
	fs := NewZcnFS(alloc, cacheDir, useMemoryMode)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("nfs: listen on :%d: %w", port, err)
	}

	handler := nfshelper.NewNullAuthHandler(fs)
	cacheHandler := nfshelper.NewCachingHandler(handler, 8192)

	modeStr := "disk (ACID, in-process cache API)"
	if useMemoryMode {
		modeStr = "memory (fastest, no crash recovery)"
	}
	log.Printf("[NFS] Server listening on port %d (NFSv3, mode=%s, cache=%s)", port, modeStr, cacheDir)

	go func() {
		if err := nfs.Serve(listener, cacheHandler); err != nil {
			log.Printf("[NFS] Server error: %v", err)
		}
	}()

	return nil
}

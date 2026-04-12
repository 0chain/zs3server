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
// POSIX filesystem. Reads/writes go through MinIO's in-process ObjectLayer
// (writeback cache on /mcache) — same performance as S3, no HTTP overhead.
func StartNFSServer(port int, alloc *sdk.Allocation, cacheDir string) error {
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "zs3-nfs-cache")
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("nfs: create cache dir: %w", err)
	}

	fs := NewZcnFS(alloc, cacheDir)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("nfs: listen on :%d: %w", port, err)
	}

	handler := nfshelper.NewNullAuthHandler(fs)
	cacheHandler := nfshelper.NewCachingHandler(handler, 8192)

	log.Printf("[NFS] Server listening on port %d (NFSv3, in-process cache API, cache=%s)", port, cacheDir)

	go func() {
		if err := nfs.Serve(listener, cacheHandler); err != nil {
			log.Printf("[NFS] Server error: %v", err)
		}
	}()

	return nil
}

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
// POSIX filesystem. It shares the same blobber data as the S3 gateway —
// writes go through the WAL + writeback cache, reads come from cache or blobbers.
//
// Mount with: mount -t nfs -o vers=3,tcp,nolock <host>:<port>:/ /mnt/zs3
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
	cacheHandler := nfshelper.NewCachingHandler(handler, 4096)

	log.Printf("[NFS] Server listening on port %d (NFSv3, cache=%s)", port, cacheDir)

	go func() {
		if err := nfs.Serve(listener, cacheHandler); err != nil {
			log.Printf("[NFS] Server error: %v", err)
		}
	}()

	return nil
}

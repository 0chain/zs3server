// Copyright (c) 2015-2021 MinIO, Inc.
//
// In-memory inode → rel_path map, populated on stub creation by list_router.
// Used by prewarm_router to honor fileid-keyed prewarm requests from FSAL_ZUS
// when Ganesha re-hydrates handles without rel_path.
package zcn

import (
	"fmt"
	"sync"
)

var (
	inodeRelMu sync.RWMutex
	inodeRel   = make(map[uint64]string, 1<<16)
)

// inodeRelSet records/overwrites an inode → rel_path mapping.
// rel is "<bucket>/<key>" (no leading slash).
func inodeRelSet(inode uint64, rel string) {
	if inode == 0 || rel == "" {
		return
	}
	inodeRelMu.Lock()
	inodeRel[inode] = rel
	inodeRelMu.Unlock()
	fmt.Printf("[inodeRel] SET inode=%d rel=%s count=%d\n", inode, rel, len(inodeRel))
}

// inodeRelGet returns the rel_path for an inode, or empty string if unknown.
func inodeRelGet(inode uint64) string {
	inodeRelMu.RLock()
	rel := inodeRel[inode]
	inodeRelMu.RUnlock()
	return rel
}

// inodeRelDelete removes an inode from the map (e.g. on stub eviction).
func inodeRelDelete(inode uint64) {
	if inode == 0 {
		return
	}
	inodeRelMu.Lock()
	delete(inodeRel, inode)
	inodeRelMu.Unlock()
}

// inodeRelCount returns the current number of entries (for diagnostics).
func inodeRelCount() int {
	inodeRelMu.RLock()
	n := len(inodeRel)
	inodeRelMu.RUnlock()
	return n
}

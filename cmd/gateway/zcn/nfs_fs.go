package zcn

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/go-git/go-billy/v5"
)

// ZcnFS implements billy.Filesystem backed by Züs blobbers.
//
// Architecture:
//   NFS read  → download from blobbers (or writeback cache) to local temp → serve from temp
//   NFS write → buffer to local temp → on Close(), upload via putFile → WAL + blobbers
//   NFS list  → getRegularRefs from blobbers (same as S3 LIST)
//   NFS stat  → getSingleRegularRef from blobbers (same as S3 HEAD)
//
// This reuses the same allocation, WAL, and batch workers as the S3 gateway.
type ZcnFS struct {
	alloc    *sdk.Allocation
	cacheDir string // local cache for staging open files
	mu       sync.RWMutex
}

var _ billy.Filesystem = (*ZcnFS)(nil)

func NewZcnFS(alloc *sdk.Allocation, cacheDir string) *ZcnFS {
	return &ZcnFS{
		alloc:    alloc,
		cacheDir: cacheDir,
	}
}

// --- billy.Basic ---

func (fs *ZcnFS) Create(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
}

func (fs *ZcnFS) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

func (fs *ZcnFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	filename = cleanPath(filename)

	f := &ZcnFile{
		fs:   fs,
		name: filename,
		flag: flag,
		perm: perm,
	}

	isCreate := flag&os.O_CREATE != 0
	isTrunc := flag&os.O_TRUNC != 0
	isWrite := flag&(os.O_WRONLY|os.O_RDWR) != 0

	if isCreate && isTrunc {
		// New file — just create the temp staging file
		tmp, err := os.CreateTemp(fs.cacheDir, "nfs-write-*")
		if err != nil {
			return nil, fmt.Errorf("nfs: create temp: %w", err)
		}
		f.tmpFile = tmp
		f.dirty = true
		return f, nil
	}

	if !isCreate {
		// Existing file — check it exists
		ref, err := getSingleRegularRef(fs.alloc, filename)
		if err != nil || ref == nil {
			return nil, os.ErrNotExist
		}
		if ref.Type == "d" {
			return nil, fmt.Errorf("nfs: %s is a directory", filename)
		}
		f.size = ref.Size
	}

	if isWrite {
		// Read-write: download to temp first, then allow modifications
		tmp, err := os.CreateTemp(fs.cacheDir, "nfs-rw-*")
		if err != nil {
			return nil, fmt.Errorf("nfs: create temp: %w", err)
		}
		if f.size > 0 {
			if err := fs.downloadToFile(filename, tmp); err != nil {
				tmp.Close()
				os.Remove(tmp.Name())
				return nil, err
			}
			tmp.Seek(0, io.SeekStart)
		}
		f.tmpFile = tmp
		f.dirty = true
		return f, nil
	}

	// Read-only: download to temp for random access
	tmp, err := os.CreateTemp(fs.cacheDir, "nfs-read-*")
	if err != nil {
		return nil, fmt.Errorf("nfs: create temp: %w", err)
	}
	if f.size > 0 {
		if err := fs.downloadToFile(filename, tmp); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, err
		}
		tmp.Seek(0, io.SeekStart)
	}
	f.tmpFile = tmp
	return f, nil
}

func (fs *ZcnFS) Stat(filename string) (os.FileInfo, error) {
	filename = cleanPath(filename)

	if filename == "/" {
		return &zcnFileInfo{name: "/", isDir: true, modTime: time.Now()}, nil
	}

	ref, err := getSingleRegularRef(fs.alloc, filename)
	if err != nil || ref == nil {
		return nil, os.ErrNotExist
	}

	return refToFileInfo(ref), nil
}

func (fs *ZcnFS) Rename(oldpath, newpath string) error {
	oldpath = cleanPath(oldpath)
	newpath = cleanPath(newpath)

	// Use SDK multi-operation for rename (move + rename in blobber terms)
	ops := []sdk.OperationRequest{
		{
			OperationType: "move",
			RemotePath:    oldpath,
			DestPath:      path.Dir(newpath),
		},
	}
	// If the filename also changed, we need a rename op
	if path.Base(oldpath) != path.Base(newpath) {
		ops = append(ops, sdk.OperationRequest{
			OperationType: "rename",
			RemotePath:    path.Join(path.Dir(newpath), path.Base(oldpath)),
			DestName:      path.Base(newpath),
		})
	}

	return fs.alloc.DoMultiOperation(ops)
}

func (fs *ZcnFS) Remove(filename string) error {
	filename = cleanPath(filename)

	// Mark WAL as deleted if pending
	bucket, key := splitBucketObject(filename)
	if walWriter != nil {
		walWriter.Delete(bucket, key)
	}

	return fs.alloc.DeleteFile(filename)
}

func (fs *ZcnFS) Join(elem ...string) string {
	return path.Join(elem...)
}

// --- billy.Dir ---

func (fs *ZcnFS) ReadDir(dir string) ([]os.FileInfo, error) {
	dir = cleanPath(dir)

	result, err := getRegularRefs(fs.alloc, dir, "", "", 500)
	if err != nil {
		return nil, fmt.Errorf("nfs: readdir %s: %w", dir, err)
	}
	if result == nil {
		return nil, os.ErrNotExist
	}

	var entries []os.FileInfo
	for i := range result.Refs {
		ref := &result.Refs[i]
		// Skip the directory itself (blobber returns parent in refs)
		if ref.Path == dir {
			continue
		}
		entries = append(entries, refToFileInfo(ref))
	}
	return entries, nil
}

func (fs *ZcnFS) MkdirAll(dir string, perm os.FileMode) error {
	dir = cleanPath(dir)
	if dir == "/" {
		return nil
	}

	// CreateDir via multi-operation
	ops := []sdk.OperationRequest{
		{
			OperationType: "createdir",
			RemotePath:    dir,
		},
	}
	err := fs.alloc.DoMultiOperation(ops)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil // idempotent
	}
	return err
}

// --- billy.Symlink ---

func (fs *ZcnFS) Lstat(filename string) (os.FileInfo, error) {
	return fs.Stat(filename) // no symlink distinction
}

func (fs *ZcnFS) Symlink(target, link string) error {
	return fmt.Errorf("nfs: symlinks not supported")
}

func (fs *ZcnFS) Readlink(link string) (string, error) {
	return "", fmt.Errorf("nfs: symlinks not supported")
}

// --- billy.TempFile ---

func (fs *ZcnFS) TempFile(dir, prefix string) (billy.File, error) {
	name := path.Join(dir, fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()))
	return fs.Create(name)
}

// --- billy.Chroot (optional, for go-nfs compatibility) ---

func (fs *ZcnFS) Root() string {
	return "/"
}

func (fs *ZcnFS) Chroot(dir string) (billy.Filesystem, error) {
	return nil, fmt.Errorf("nfs: chroot not supported")
}

// --- internal helpers ---

// downloadToFile fetches a file from blobbers/cache and writes it to dst.
func (fs *ZcnFS) downloadToFile(remotePath string, dst *os.File) error {
	bucket, object := splitBucketObject(remotePath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reader, _, cleanup, _, err := getFileReader(ctx, fs.alloc, bucket, object, remotePath, 0, 0)
	if err != nil {
		return fmt.Errorf("nfs: download %s: %w", remotePath, err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	if _, err := io.Copy(dst, reader); err != nil {
		return fmt.Errorf("nfs: copy %s: %w", remotePath, err)
	}

	return nil
}

// uploadFromFile reads a local file and uploads it to blobbers via putFile.
func (fs *ZcnFS) uploadFromFile(remotePath string, src *os.File, size int64) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	contentType := "application/octet-stream"
	err := putFile(ctx, fs.alloc, remotePath, contentType, src, size, false, nil)
	if err != nil {
		return fmt.Errorf("nfs: upload %s: %w", remotePath, err)
	}

	// Record WAL intent for crash recovery
	if walWriter != nil && walWriter.ShouldUseWAL(size) {
		bucket, key := splitBucketObject(remotePath)
		if walErr := walWriter.RecordIntent(bucket, key, size); walErr != nil {
			log.Printf("[NFS] WAL intent failed for %s: %v", remotePath, walErr)
		}
	}

	return nil
}

// splitBucketObject splits "/bucket/path/file" into ("bucket", "path/file").
func splitBucketObject(remotePath string) (bucket, object string) {
	p := strings.TrimPrefix(remotePath, "/")
	parts := strings.SplitN(p, "/", 2)
	bucket = parts[0]
	if len(parts) > 1 {
		object = parts[1]
	}
	return
}

// cleanPath normalizes a path to always start with "/" and have no trailing slash.
func cleanPath(p string) string {
	if p == "" || p == "." {
		return "/"
	}
	p = path.Clean("/" + p)
	return p
}

// refToFileInfo converts a blobber ORef to os.FileInfo.
func refToFileInfo(ref *sdk.ORef) os.FileInfo {
	isDir := ref.Type == "d"
	mode := os.FileMode(0644)
	if isDir {
		mode = os.FileMode(0755) | os.ModeDir
	}
	return &zcnFileInfo{
		name:    ref.Name,
		size:    ref.Size,
		isDir:   isDir,
		modTime: time.Unix(int64(ref.UpdatedAt), 0),
		mode:    mode,
	}
}

// zcnFileInfo implements os.FileInfo for blobber refs.
type zcnFileInfo struct {
	name    string
	size    int64
	isDir   bool
	modTime time.Time
	mode    os.FileMode
}

func (fi *zcnFileInfo) Name() string      { return fi.name }
func (fi *zcnFileInfo) Size() int64        { return fi.size }
func (fi *zcnFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *zcnFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *zcnFileInfo) IsDir() bool        { return fi.isDir }
func (fi *zcnFileInfo) Sys() interface{}   { return nil }

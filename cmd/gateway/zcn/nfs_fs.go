package zcn

import (
	"bytes"
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
// NFS reads/writes go through MinIO's ObjectLayer API in-process (no HTTP).
type ZcnFS struct {
	alloc    *sdk.Allocation
	cacheDir string
	mu       sync.RWMutex

	// statCache: short-lived cache for file metadata after writes.
	statMu    sync.RWMutex
	statCache map[string]*zcnFileInfo
}

var _ billy.Filesystem = (*ZcnFS)(nil)

func NewZcnFS(alloc *sdk.Allocation, cacheDir string) *ZcnFS {
	return &ZcnFS{
		alloc:     alloc,
		cacheDir:  cacheDir,
		statCache: make(map[string]*zcnFileInfo),
	}
}

func (fs *ZcnFS) cacheFileStat(filename string, size int64) {
	fs.statMu.Lock()
	fs.statCache[filename] = &zcnFileInfo{
		name:    path.Base(filename),
		size:    size,
		isDir:   false,
		modTime: time.Now(),
		mode:    0644,
	}
	fs.statMu.Unlock()
	go func() {
		time.Sleep(30 * time.Second)
		fs.statMu.Lock()
		delete(fs.statCache, filename)
		fs.statMu.Unlock()
	}()
}

func (fs *ZcnFS) getCachedStat(filename string) *zcnFileInfo {
	fs.statMu.RLock()
	defer fs.statMu.RUnlock()
	return fs.statCache[filename]
}

func (fs *ZcnFS) Create(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
}

func (fs *ZcnFS) Open(filename string) (billy.File, error) {
	return fs.OpenFile(filename, os.O_RDONLY, 0)
}

func (fs *ZcnFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	filename = cleanPath(filename)

	f := &ZcnFile{fs: fs, name: filename, flag: flag, perm: perm}
	isCreate := flag&os.O_CREATE != 0
	isTrunc := flag&os.O_TRUNC != 0
	isWrite := flag&(os.O_WRONLY|os.O_RDWR) != 0

	if isCreate && isTrunc {
		f.buf = &bytes.Buffer{}
		f.dirty = true
		return f, nil
	}

	if !isCreate {
		if cached := fs.getCachedStat(filename); cached != nil {
			f.size = cached.size
		} else if nfsObjAPI.ready() {
			bucket, key := splitBucketObject(filename)
			if key != "" {
				ctx, cancel := nfsCtx()
				defer cancel()
				size, exists := nfsObjAPI.head(ctx, bucket, key)
				if !exists {
					return nil, os.ErrNotExist
				}
				f.size = size
			}
		} else {
			ref, err := getSingleRegularRef(fs.alloc, filename)
			if err != nil || ref == nil {
				return nil, os.ErrNotExist
			}
			if ref.Type == "d" {
				return nil, fmt.Errorf("nfs: %s is a directory", filename)
			}
			f.size = ref.Size
		}
	}

	if isWrite {
		if f.size <= smallFileThreshold {
			f.buf = &bytes.Buffer{}
			if f.size > 0 {
				if err := fs.downloadToBuffer(filename, f.buf); err != nil {
					return nil, err
				}
			}
			f.dirty = true
			return f, nil
		}
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

	// Read-only
	if f.size <= smallFileThreshold {
		f.buf = &bytes.Buffer{}
		if f.size > 0 {
			if err := fs.downloadToBuffer(filename, f.buf); err != nil {
				return nil, err
			}
		}
		return f, nil
	}
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

	if cached := fs.getCachedStat(filename); cached != nil {
		return cached, nil
	}

	bucket, key := splitBucketObject(filename)

	if nfsObjAPI.ready() {
		ctx, cancel := nfsCtx()
		defer cancel()

		if key != "" {
			size, exists := nfsObjAPI.head(ctx, bucket, key)
			if exists {
				return &zcnFileInfo{name: path.Base(filename), size: size, modTime: time.Now(), mode: 0644}, nil
			}
		} else if bucket != "" {
			if nfsObjAPI.bucketExists(ctx, bucket) {
				return &zcnFileInfo{name: bucket, isDir: true, modTime: time.Now(), mode: 0755 | os.ModeDir}, nil
			}
		}
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
	ops := []sdk.OperationRequest{{OperationType: "move", RemotePath: oldpath, DestPath: path.Dir(newpath)}}
	if path.Base(oldpath) != path.Base(newpath) {
		ops = append(ops, sdk.OperationRequest{OperationType: "rename", RemotePath: path.Join(path.Dir(newpath), path.Base(oldpath)), DestName: path.Base(newpath)})
	}
	return fs.alloc.DoMultiOperation(ops)
}

func (fs *ZcnFS) Remove(filename string) error {
	filename = cleanPath(filename)
	bucket, key := splitBucketObject(filename)

	if nfsObjAPI.ready() && key != "" {
		ctx, cancel := nfsCtx()
		defer cancel()
		nfsObjAPI.remove(ctx, bucket, key)
	}

	fs.statMu.Lock()
	delete(fs.statCache, filename)
	fs.statMu.Unlock()

	if walWriter != nil {
		walWriter.Delete(bucket, key)
	}
	return fs.alloc.DeleteFile(filename)
}

func (fs *ZcnFS) Join(elem ...string) string { return path.Join(elem...) }

func (fs *ZcnFS) ReadDir(dir string) ([]os.FileInfo, error) {
	dir = cleanPath(dir)

	if nfsObjAPI.ready() {
		ctx, cancel := nfsCtx()
		defer cancel()

		if dir == "/" {
			buckets, err := nfsObjAPI.listBuckets(ctx)
			if err == nil {
				var entries []os.FileInfo
				for _, b := range buckets {
					entries = append(entries, &zcnFileInfo{name: b.Name, isDir: true, modTime: b.Created, mode: 0755 | os.ModeDir})
				}
				return entries, nil
			}
		} else {
			bucket, prefix := splitBucketObject(dir)
			if prefix != "" {
				prefix += "/"
			}
			objects, err := nfsObjAPI.listDir(ctx, bucket, prefix)
			if err == nil {
				var entries []os.FileInfo
				for _, obj := range objects {
					name := strings.TrimPrefix(obj.Name, prefix)
					name = strings.TrimSuffix(name, "/")
					if name == "" {
						continue
					}
					isDir := obj.IsDir || strings.HasSuffix(obj.Name, "/")
					mode := os.FileMode(0644)
					if isDir {
						mode = 0755 | os.ModeDir
					}
					entries = append(entries, &zcnFileInfo{name: name, size: obj.Size, isDir: isDir, modTime: obj.ModTime, mode: mode})
				}
				if len(entries) > 0 {
					return entries, nil
				}
			}
		}
	}

	// Fallback: blobber refs
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
	bucket, key := splitBucketObject(dir)
	if nfsObjAPI.ready() && key == "" && bucket != "" {
		ctx, cancel := nfsCtx()
		defer cancel()
		err := nfsObjAPI.makeBucket(ctx, bucket)
		if err != nil && !strings.Contains(err.Error(), "already") {
			return err
		}
		return nil
	}
	ops := []sdk.OperationRequest{{OperationType: "createdir", RemotePath: dir}}
	err := fs.alloc.DoMultiOperation(ops)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (fs *ZcnFS) Lstat(filename string) (os.FileInfo, error) { return fs.Stat(filename) }
func (fs *ZcnFS) Symlink(target, link string) error          { return fmt.Errorf("nfs: symlinks not supported") }
func (fs *ZcnFS) Readlink(link string) (string, error)       { return "", fmt.Errorf("nfs: symlinks not supported") }

func (fs *ZcnFS) TempFile(dir, prefix string) (billy.File, error) {
	return fs.Create(path.Join(dir, fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())))
}

func (fs *ZcnFS) Root() string                                  { return "/" }
func (fs *ZcnFS) Chroot(dir string) (billy.Filesystem, error)   { return nil, fmt.Errorf("nfs: chroot not supported") }

// --- internal helpers ---

func (fs *ZcnFS) downloadToBuffer(remotePath string, buf *bytes.Buffer) error {
	bucket, object := splitBucketObject(remotePath)

	if nfsObjAPI.ready() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		body, size, err := nfsObjAPI.get(ctx, bucket, object)
		if err != nil {
			return fmt.Errorf("nfs: get %s: %w", remotePath, err)
		}
		defer body.Close()
		if size > 0 {
			buf.Grow(int(size))
		}
		_, err = io.Copy(buf, body)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reader, _, cleanup, _, err := getFileReader(ctx, fs.alloc, bucket, object, remotePath, 0, 0)
	if err != nil {
		return fmt.Errorf("nfs: download %s: %w", remotePath, err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	_, err = io.Copy(buf, reader)
	return err
}

func (fs *ZcnFS) downloadToFile(remotePath string, dst *os.File) error {
	bucket, object := splitBucketObject(remotePath)

	if nfsObjAPI.ready() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		body, _, err := nfsObjAPI.get(ctx, bucket, object)
		if err != nil {
			return fmt.Errorf("nfs: get %s: %w", remotePath, err)
		}
		defer body.Close()
		_, err = io.Copy(dst, body)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reader, _, cleanup, _, err := getFileReader(ctx, fs.alloc, bucket, object, remotePath, 0, 0)
	if err != nil {
		return fmt.Errorf("nfs: download %s: %w", remotePath, err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	_, err = io.Copy(dst, reader)
	return err
}

func (fs *ZcnFS) uploadFromReader(remotePath string, r io.Reader, size int64) error {
	bucket, key := splitBucketObject(remotePath)

	if nfsObjAPI.ready() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		// Read into bytes for the ObjectLayer API
		var data []byte
		if size > 0 && size <= smallFileThreshold {
			data = make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				return fmt.Errorf("nfs: read data: %w", err)
			}
		} else {
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, r); err != nil {
				return fmt.Errorf("nfs: read data: %w", err)
			}
			data = buf.Bytes()
		}
		return nfsObjAPI.put(ctx, bucket, key, data)
	}

	// Fallback
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := putFile(ctx, fs.alloc, remotePath, "application/octet-stream", r, size, false, nil)
	if err != nil {
		return fmt.Errorf("nfs: upload %s: %w", remotePath, err)
	}
	if walWriter != nil && walWriter.ShouldUseWAL(size) {
		if walErr := walWriter.RecordIntent(bucket, key, size); walErr != nil {
			log.Printf("[NFS] WAL intent failed for %s: %v", remotePath, walErr)
		}
	}
	return nil
}

func splitBucketObject(remotePath string) (bucket, object string) {
	p := strings.TrimPrefix(remotePath, "/")
	parts := strings.SplitN(p, "/", 2)
	bucket = parts[0]
	if len(parts) > 1 {
		object = parts[1]
	}
	return
}

func cleanPath(p string) string {
	if p == "" || p == "." {
		return "/"
	}
	return path.Clean("/" + p)
}

func refToFileInfo(ref *sdk.ORef) os.FileInfo {
	isDir := ref.Type == "d"
	mode := os.FileMode(0644)
	if isDir {
		mode = os.FileMode(0755) | os.ModeDir
	}
	return &zcnFileInfo{name: ref.Name, size: ref.Size, isDir: isDir, modTime: time.Unix(int64(ref.UpdatedAt), 0), mode: mode}
}

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

package zcn

import (
	"fmt"
	"io"
	"os"
)

// ZcnFile implements billy.File backed by a local temp file.
//
// Read path:  blobber/cache → temp file → NFS read/readat/seek
// Write path: NFS write → temp file → on Close() → putFile → blobbers
type ZcnFile struct {
	fs      *ZcnFS
	name    string   // remote path on blobbers
	flag    int      // os.O_RDONLY, O_WRONLY, O_RDWR, etc.
	perm    os.FileMode
	tmpFile *os.File // local staging file
	dirty   bool     // true if any Write/Truncate was called
	size    int64    // known remote size (0 if new file)
	closed  bool
}

func (f *ZcnFile) Name() string {
	return f.name
}

func (f *ZcnFile) Read(p []byte) (int, error) {
	if f.closed || f.tmpFile == nil {
		return 0, os.ErrClosed
	}
	return f.tmpFile.Read(p)
}

func (f *ZcnFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed || f.tmpFile == nil {
		return 0, os.ErrClosed
	}
	return f.tmpFile.ReadAt(p, off)
}

func (f *ZcnFile) Write(p []byte) (int, error) {
	if f.closed || f.tmpFile == nil {
		return 0, os.ErrClosed
	}
	f.dirty = true
	return f.tmpFile.Write(p)
}

func (f *ZcnFile) Seek(offset int64, whence int) (int64, error) {
	if f.closed || f.tmpFile == nil {
		return 0, os.ErrClosed
	}
	return f.tmpFile.Seek(offset, whence)
}

func (f *ZcnFile) Truncate(size int64) error {
	if f.closed || f.tmpFile == nil {
		return os.ErrClosed
	}
	f.dirty = true
	return f.tmpFile.Truncate(size)
}

// Close flushes dirty files to blobbers and cleans up the temp file.
func (f *ZcnFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true

	if f.tmpFile == nil {
		return nil
	}

	defer func() {
		tmpName := f.tmpFile.Name()
		f.tmpFile.Close()
		os.Remove(tmpName)
	}()

	if !f.dirty {
		return nil
	}

	// Get final size
	info, err := f.tmpFile.Stat()
	if err != nil {
		return fmt.Errorf("nfs: stat temp: %w", err)
	}
	size := info.Size()

	if size == 0 {
		// Empty file — still upload (creates the object on blobbers)
		if _, err := f.tmpFile.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}

	// Upload to blobbers via the same path as S3 PUT
	return f.fs.uploadFromFile(f.name, f.tmpFile, size)
}

// Lock implements billy.File advisory locking (no-op for now).
func (f *ZcnFile) Lock() error {
	return nil
}

// Unlock implements billy.File advisory unlocking (no-op for now).
func (f *ZcnFile) Unlock() error {
	return nil
}

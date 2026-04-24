package zcn

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

const smallFileThreshold = 1 << 20 // 1 MiB — files below this use memory buffer

// ZcnFile implements billy.File.
//
// Small files (≤1MB): buffer in memory, upload from bytes.Reader on Close().
// Large files (>1MB): use temp file on disk, upload from file on Close().
// Read path: S3 GET from cache/blobber → temp file or memory → NFS read.
type ZcnFile struct {
	fs     *ZcnFS
	name   string
	flag   int
	perm   os.FileMode
	dirty  bool
	closed bool
	size   int64 // known remote size (0 if new)

	// Small-file path: in-memory buffer
	buf    *bytes.Buffer
	bufOff int64 // read cursor for Read()

	// Large-file path: temp file on disk
	tmpFile *os.File
}

func (f *ZcnFile) Name() string { return f.name }

func (f *ZcnFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.buf != nil {
		data := f.buf.Bytes()
		if f.bufOff >= int64(len(data)) {
			return 0, io.EOF
		}
		n := copy(p, data[f.bufOff:])
		f.bufOff += int64(n)
		return n, nil
	}
	if f.tmpFile != nil {
		return f.tmpFile.Read(p)
	}
	return 0, os.ErrClosed
}

func (f *ZcnFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.buf != nil {
		data := f.buf.Bytes()
		if off >= int64(len(data)) {
			return 0, io.EOF
		}
		n := copy(p, data[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}
	if f.tmpFile != nil {
		return f.tmpFile.ReadAt(p, off)
	}
	return 0, os.ErrClosed
}

func (f *ZcnFile) Write(p []byte) (int, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	f.dirty = true

	if f.buf != nil {
		// Check if this write would push us past the small-file threshold
		if int64(f.buf.Len())+int64(len(p)) > smallFileThreshold {
			// Spill to temp file
			if err := f.spillToDisk(); err != nil {
				return 0, err
			}
			return f.tmpFile.Write(p)
		}
		return f.buf.Write(p)
	}
	if f.tmpFile != nil {
		return f.tmpFile.Write(p)
	}
	return 0, os.ErrClosed
}

func (f *ZcnFile) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.buf != nil {
		var abs int64
		switch whence {
		case io.SeekStart:
			abs = offset
		case io.SeekCurrent:
			abs = f.bufOff + offset
		case io.SeekEnd:
			abs = int64(f.buf.Len()) + offset
		}
		if abs < 0 {
			return 0, fmt.Errorf("nfs: negative seek")
		}
		f.bufOff = abs
		return abs, nil
	}
	if f.tmpFile != nil {
		return f.tmpFile.Seek(offset, whence)
	}
	return 0, os.ErrClosed
}

func (f *ZcnFile) Truncate(size int64) error {
	if f.closed {
		return os.ErrClosed
	}
	f.dirty = true
	if f.buf != nil {
		data := f.buf.Bytes()
		if int64(len(data)) > size {
			f.buf.Truncate(int(size))
		}
		return nil
	}
	if f.tmpFile != nil {
		return f.tmpFile.Truncate(size)
	}
	return os.ErrClosed
}

// Close flushes dirty data to S3 writeback cache.
func (f *ZcnFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true

	if !f.dirty {
		// Read-only — just clean up
		if f.tmpFile != nil {
			name := f.tmpFile.Name()
			f.tmpFile.Close()
			os.Remove(name)
		}
		return nil
	}

	// Upload dirty data
	if f.buf != nil {
		data := f.buf.Bytes()
		size := int64(len(data))
		err := f.fs.uploadFromReader(f.name, bytes.NewReader(data), size)
		if err != nil {
			return err
		}
		// Cache the stat so GETATTR after CLOSE is instant
		f.fs.cacheFileStat(f.name, size)
		return nil
	}

	if f.tmpFile != nil {
		info, err := f.tmpFile.Stat()
		if err != nil {
			return fmt.Errorf("nfs: stat temp: %w", err)
		}
		size := info.Size()
		if _, err := f.tmpFile.Seek(0, io.SeekStart); err != nil {
			return err
		}
		err = f.fs.uploadFromReader(f.name, f.tmpFile, size)
		name := f.tmpFile.Name()
		f.tmpFile.Close()
		os.Remove(name)
		if err != nil {
			return err
		}
		f.fs.cacheFileStat(f.name, size)
		return nil
	}

	return nil
}

func (f *ZcnFile) Lock() error   { return nil }
func (f *ZcnFile) Unlock() error { return nil }

// spillToDisk moves the in-memory buffer to a temp file (for files that grow past threshold).
func (f *ZcnFile) spillToDisk() error {
	tmp, err := os.CreateTemp(f.fs.cacheDir, "nfs-spill-*")
	if err != nil {
		return err
	}
	if f.buf.Len() > 0 {
		if _, err := tmp.Write(f.buf.Bytes()); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return err
		}
	}
	f.tmpFile = tmp
	f.buf = nil
	return nil
}

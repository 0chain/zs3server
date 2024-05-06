package zcn

import (
	"io"
	"io/fs"
)

type memFile struct {
	memFileDataChan chan memFileData
	errChan         chan error
}

type memFileData struct {
	buf []byte
	err error
}

func (mf *memFile) Read(p []byte) (int, error) {
	select {
	case err := <-mf.errChan:
		return 0, err
	case data, ok := <-mf.memFileDataChan:
		if !ok {
			return 0, io.EOF
		}
		if data.err != nil && data.err != io.EOF {
			return 0, data.err
		}
		if len(data.buf) > len(p) {
			return 0, io.ErrShortBuffer
		}
		n := copy(p, data.buf)
		return n, data.err
	}
}

type pipeFile struct {
	w *io.PipeWriter
}

func (pf *pipeFile) Write(p []byte) (int, error) {
	return pf.w.Write(p)
}

func (pf *pipeFile) Close() error {
	return nil
}

func (pf *pipeFile) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (pf *pipeFile) Stat() (fs.FileInfo, error) {
	return nil, nil
}

func (pf *pipeFile) Sync() error {
	return nil
}

func (pf *pipeFile) Seek(offset int64, whence int) (int64, error) {
	return 0, nil
}

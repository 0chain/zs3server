package zcn

import "io"

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

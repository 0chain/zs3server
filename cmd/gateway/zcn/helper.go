package zcn

import (
	"errors"
	"io"
	"strings"
	"time"

	"github.com/minio/pkg/wildcard"
)

func newMinioReader(source io.Reader) *MinioReader {
	return &MinioReader{source}
}

// MinioReader Reader that returns io.EOF for io.ErrUnexpectedEOF error
type MinioReader struct {
	io.Reader
}

func (r *MinioReader) Read(p []byte) (n int, err error) {
	if n, err = io.ReadAtLeast(r.Reader, p, len(p)); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return n, io.EOF
		}
	}
	return
}

func getTimeOut(size uint64) time.Duration {
	switch {
	case size >= oneGB:
		return time.Minute * 30
	case size >= 500*oneMB:
		return time.Minute * 10
	case size >= hundredMB:
		return time.Minute * 5
	default:
		return time.Minute * 2
	}
}

func hasStringSuffixInSlice(str string, list []string) bool {
	str = strings.ToLower(str)
	for _, v := range list {
		if strings.HasSuffix(str, strings.ToLower(v)) {
			return true
		}
	}
	return false
}

// Returns true if any of the given wildcard patterns match the matchStr.
func hasPattern(patterns []string, matchStr string) bool {
	for _, pattern := range patterns {
		if ok := wildcard.MatchSimple(pattern, matchStr); ok {
			return true
		}
	}
	return false
}

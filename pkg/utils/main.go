package utils

import (
	"errors"
	"io"
)

// Like LimitedReader but returning a non-EOF error on reaching the limit
type LimitedReadCloser struct {
	io.ReadCloser
	N int64
}

// Returns a LimitedReadCloser instance.
// LimitedReadCloser acts as a LimitedReader but returns a non-EOF error on reaching the limit.
func NewLimitedReadCloser(rc io.ReadCloser, l int64) *LimitedReadCloser {
	return &LimitedReadCloser{rc, l}
}

func (l *LimitedReadCloser) Read(p []byte) (n int, err error) {
	if l.N <= 0 {
		return 0, errors.New("input stream too large")
	}
	if int64(len(p)) > l.N {
		p = p[0:l.N]
	}
	n, err = l.ReadCloser.Read(p)
	l.N -= int64(n)
	return
}

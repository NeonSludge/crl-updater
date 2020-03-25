package utils

import (
	"errors"
	"io"
)

// LimitedStrictReader acts as an io.LimitedReader but returns a non-EOF error on reaching the limit.
type LimitedStrictReader struct {
	R io.Reader // Underlying io.Reader
	N int64     // Bytes remaining
}

// Wraps an io.Reader in a LimitedStrictReader.
func LimitStrictReader(r io.Reader, n int64) io.Reader {
	return &LimitedStrictReader{r, n}
}

// Read method implementation for LimitedStrictReader
func (l *LimitedStrictReader) Read(p []byte) (n int, err error) {
	if l.N <= 0 {
		return 0, errors.New("input stream too large")
	}
	if int64(len(p)) > l.N {
		p = p[0:l.N]
	}
	n, err = l.R.Read(p)
	l.N -= int64(n)
	return
}

// LimitedStrictReadCloser acts as an io.LimitedReader but returns a non-EOF error on reaching the limit and its underlying implementation is an io.ReadCloser.
type LimitedStrictReadCloser struct {
	R   io.ReadCloser
	N   int64
	Err error
}

// Wraps an io.ReadCloser in a LimitedStrictReadCloser
func LimitStrictReadCloser(r io.ReadCloser, n int64) io.ReadCloser {
	return &LimitedStrictReadCloser{R: r, N: n}
}

// Read method implementation for LimitedStrictReadCloser
func (l *LimitedStrictReadCloser) Read(p []byte) (n int, err error) {
	if l.Err != nil {
		return 0, l.Err
	}

	if len(p) == 0 {
		return 0, nil
	}

	if int64(len(p)) > l.N+1 {
		p = p[:l.N+1]
	}

	n, err = l.R.Read(p)

	if int64(n) <= l.N {
		l.N -= int64(n)
		l.Err = err
		return n, err
	}

	n = int(l.N)
	l.N = 0

	l.Err = errors.New("input stream too large")
	return n, l.Err
}

// Close method implementation for LimitedStrictReadCloser
func (l *LimitedStrictReadCloser) Close() error {
	return l.R.Close()
}

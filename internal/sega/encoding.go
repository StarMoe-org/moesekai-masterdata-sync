package sega

import (
	"compress/gzip"
	"compress/zlib"
	"io"
)

func newGzipReader(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }

// HTTP 的 deflate 实际是 zlib 格式。
func newDeflateReader(r io.Reader) io.Reader {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return errReader{err}
	}
	return zr
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

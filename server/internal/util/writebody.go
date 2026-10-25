package util

import (
	"bytes"
	"io"
)

// WriteBody copies an already-built response body to w and reports the number
// of bytes written, like w.Write. Callers set the Content-Type and status
// before calling it.
func WriteBody(w io.Writer, body []byte) (int, error) {
	n, err := io.Copy(w, bytes.NewReader(body))
	return int(n), err
}

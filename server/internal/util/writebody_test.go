package util

import (
	"net/http/httptest"
	"testing"
)

func TestWriteBody(t *testing.T) {
	rec := httptest.NewRecorder()
	n, err := WriteBody(rec, []byte(`{"ok":true}`))
	if err != nil || n != 11 {
		t.Fatalf("WriteBody = %d, %v; want 11, nil", n, err)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteBodyEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	if n, err := WriteBody(rec, nil); err != nil || n != 0 {
		t.Fatalf("WriteBody(nil) = %d, %v; want 0, nil", n, err)
	}
}

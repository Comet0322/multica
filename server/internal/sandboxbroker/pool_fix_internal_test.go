package sandboxbroker

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// L-2: the workspaces_changed hint has a slot of its own, so wake hints filling
// the send buffer cannot crowd it out, and repeated hints coalesce.
func TestWorkspacesChangedIsNotCrowdedOutByWakes(t *testing.T) {
	t.Parallel()
	p, err := NewPool(PoolConfig{MulticaURL: "http://127.0.0.1:1", Token: "x", WakeInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wc := &wakeConn{send: make(chan []byte, wakeSendBuffer), changed: make(chan struct{}, 1)}
	for range wakeSendBuffer {
		wc.send <- wakeFrame // the buffer is full of wakes already
	}
	p.wakers["10.0.0.1"] = wc

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			p.notifyWorkspacesChanged()
			p.wakePod("10.0.0.1")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full wake buffer blocked the hint")
	}
	if len(wc.changed) != 1 {
		t.Fatalf("five hints must coalesce into one pending signal, got %d", len(wc.changed))
	}
	if len(wc.send) != wakeSendBuffer {
		t.Fatalf("the wake buffer is untouched by the hint: %d", len(wc.send))
	}
	for range wakeSendBuffer {
		if got := <-wc.send; !bytes.Equal(got, wakeFrame) {
			t.Fatalf("the send buffer holds only wakes, got %s", got)
		}
	}
}

// A socket made without the signal still gets the hint, through the wake
// buffer.
func TestWorkspacesChangedReachesASocketWithoutTheSignal(t *testing.T) {
	t.Parallel()
	p, err := NewPool(PoolConfig{MulticaURL: "http://127.0.0.1:1", Token: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wc := &wakeConn{send: make(chan []byte, wakeSendBuffer)}
	p.wakers["10.0.0.1"] = wc
	p.notifyWorkspacesChanged()
	select {
	case got := <-wc.send:
		if !bytes.Equal(got, workspacesChangedFrame) {
			t.Fatalf("got %s", got)
		}
	default:
		t.Fatal("no hint was queued")
	}
}

// H-3: only "runtime not found" from Multica's own 404 means the runtime is
// gone; the error text never carries the body.
func TestRuntimeNotFoundIsRecognisedFromTheBodyOfA404(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"the server's own message", http.StatusNotFound, `{"error":"runtime not found"}`, true},
		{"a different 404", http.StatusNotFound, `{"error":"not found"}`, false},
		{"an empty 404", http.StatusNotFound, ``, false},
		{"the marker with another status", http.StatusInternalServerError, `{"error":"runtime not found"}`, false},
		{"the marker with 403", http.StatusForbidden, `{"error":"runtime not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.status)
			}))
			defer srv.Close()
			err := newUpstream(srv.URL, "tok", func() map[string]string { return nil }).call(context.Background(), http.MethodPost, "/api/daemon/heartbeat", map[string]any{}, nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := runtimeNotFound(err); got != tc.want {
				t.Fatalf("runtimeNotFound = %v, want %v (%v)", got, tc.want, err)
			}
			if strings.Contains(err.Error(), "runtime not found") && tc.body != "" {
				t.Fatalf("the error text must not carry the response body: %v", err)
			}
		})
	}
	if runtimeNotFound(context.Canceled) || runtimeNotFound(nil) {
		t.Fatal("only an upstream error can say the runtime is gone")
	}
}

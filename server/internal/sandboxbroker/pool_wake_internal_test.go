package sandboxbroker

import (
	"bytes"
	"testing"
	"time"
)

func TestWakeSendsNeverBlockWhenASocketIsBacklogged(t *testing.T) {
	t.Parallel()
	p, err := NewPool(PoolConfig{MulticaURL: "http://127.0.0.1:1", Token: "x", WakeInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A socket whose writer is stuck: nothing drains send.
	wc := &wakeConn{send: make(chan []byte, wakeSendBuffer)}
	p.wakers["10.0.0.1"] = wc

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 * wakeSendBuffer {
			p.notifyWorkspacesChanged()
			p.wakePod("10.0.0.1")
		}
		p.wakePod("10.0.0.9") // no socket at all
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a full send buffer blocked the sender")
	}
	if len(wc.send) != wakeSendBuffer {
		t.Fatalf("want a full buffer of %d, got %d", wakeSendBuffer, len(wc.send))
	}
	// The first hints stay queued, the overflow is dropped.
	if got := <-wc.send; !bytes.Equal(got, workspacesChangedFrame) {
		t.Fatalf("first queued frame = %s", got)
	}
}

func TestFramesAreDistinct(t *testing.T) {
	t.Parallel()
	if bytes.Equal(wakeFrame, workspacesChangedFrame) {
		t.Fatal("the two hints must differ")
	}
	if !bytes.Contains(workspacesChangedFrame, []byte("daemon:workspaces_changed")) || !bytes.Contains(wakeFrame, []byte("daemon:task_available")) {
		t.Fatalf("unexpected frames: %s / %s", wakeFrame, workspacesChangedFrame)
	}
}

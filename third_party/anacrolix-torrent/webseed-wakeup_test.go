package torrent

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWebseedDemandWakesAndCoalesces(t *testing.T) {
	cl := &Client{}
	cl._mu.client = cl
	completed := make(chan struct{}, 2)
	var calls atomic.Int32
	cl.webseedRequestTimer = time.AfterFunc(time.Hour, func() {
		calls.Add(1)
		cl.updateWebseedRequestsTimerFunc()
		completed <- struct{}{}
	})
	defer cl.webseedRequestTimer.Stop()
	peer := &webseedPeer{peer: Peer{cl: cl}}
	cl.lock()
	for range 1024 {
		peer.onNeedUpdateRequests("reader demand")
		peer.onNeedUpdateRequests("piece incomplete")
	}
	cl.unlock()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("reader/completion demand did not wake the global scheduler")
	}
	cl.lock()
	defer cl.unlock()
	if calls.Load() != 1 {
		t.Fatalf("coalesced callbacks = %d, want 1", calls.Load())
	}
	if cl.webseedUpdateReason != "" {
		t.Fatalf("update left a reason pending: %q", cl.webseedUpdateReason)
	}
	if !cl.webseedRequestTimer.Stop() {
		t.Fatal("safety poll was not rearmed")
	}
}

func TestWebseedDemandDoesNotRestartClosedClient(t *testing.T) {
	cl := &Client{}
	cl._mu.client = cl
	cl.webseedRequestTimer = time.NewTimer(time.Hour)
	defer cl.webseedRequestTimer.Stop()
	peer := &webseedPeer{peer: Peer{cl: cl}}
	cl.closed.Set()
	cl.lock()
	peer.onNeedUpdateRequests("reader demand after close")
	if cl.webseedUpdateReason != "" {
		t.Fatal("closed client accepted a scheduling reason")
	}
	cl.unlock()
	if !cl.webseedRequestTimer.Stop() {
		t.Fatal("closed client ran an immediate update")
	}
	cl.updateWebseedRequestsTimerFunc()
	if cl.webseedRequestTimer.Stop() {
		t.Fatal("closed timer callback restarted the safety poll")
	}
}

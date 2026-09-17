package session

import (
	"testing"
	"time"
)

// probeWait bounds every probe assertion so a missing event fails the test
// instead of hanging it.
const probeWait = 5 * time.Second

// TestSetReadProbeRestoreClearsFirstInstall is the regression test for a
// restore that must be a no-op for the previous nil probe: after the first
// install and its restore, later reads must not report events at all. A probe
// left installed would deliver events into an abandoned channel and could
// silently break later tests in the same binary.
func TestSetReadProbeRestoreClearsFirstInstall(t *testing.T) {
	ambient := readProbe.Swap(nil)
	defer readProbe.Store(ambient)

	ch := make(chan readProbeEvent, 1)
	restore := SetReadProbe(ch)

	emitReadProbe(readProbeEvent{Kind: "reader-started"})
	select {
	case got := <-ch:
		if got.Kind != "reader-started" {
			t.Fatalf("installed probe saw %q, want reader-started", got.Kind)
		}
	default:
		t.Fatal("installed probe received no event")
	}

	restore()
	if probe := readProbe.Load(); probe != nil {
		t.Fatal("restore from a first install left the probe installed")
	}
	emitReadProbe(readProbeEvent{Kind: "reader-started"})
	select {
	case got := <-ch:
		t.Fatalf("probe received %q after restore", got.Kind)
	default:
	}
}

// TestSetReadProbeRestoresPreviousState covers the general contract: the probe
// in place after a restore is the one that was installed before, including
// when probes are nested, and the ambient state is never left modified.
func TestSetReadProbeRestoresPreviousState(t *testing.T) {
	ambient := readProbe.Load()
	defer readProbe.Store(ambient)

	outer := make(chan readProbeEvent, 2)
	restoreOuter := SetReadProbe(outer)

	inner := make(chan readProbeEvent, 2)
	restoreInner := SetReadProbe(inner)

	emitReadProbe(readProbeEvent{Kind: "admission-attempt"})
	if got := probeEventKind(t, inner); got != "admission-attempt" {
		t.Fatalf("nested probe saw %q, want admission-attempt", got)
	}
	select {
	case got := <-outer:
		t.Fatalf("outer probe saw %q while the inner probe was installed", got.Kind)
	default:
	}

	restoreInner()
	emitReadProbe(readProbeEvent{Kind: "admission-attempt"})
	if got := probeEventKind(t, outer); got != "admission-attempt" {
		t.Fatalf("after the inner restore the outer probe saw %q, want admission-attempt", got)
	}

	restoreOuter()
	if got := readProbe.Load(); got != ambient {
		t.Fatalf("readProbe after restore = %v, want the ambient probe %v", got, ambient)
	}
}

// TestEmitReadProbeNeverBlocks checks the non-blocking send contract: a probe
// with no room must drop the event rather than stall the calling reader.
func TestEmitReadProbeNeverBlocks(t *testing.T) {
	ambient := readProbe.Swap(nil)
	defer readProbe.Store(ambient)

	ch := make(chan readProbeEvent, 1)
	restore := SetReadProbe(ch)
	defer restore()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 8; i++ {
			emitReadProbe(readProbeEvent{Kind: "reader-started", Offset: int64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(probeWait):
		t.Fatal("emitReadProbe blocked on a full probe channel")
	}
	if got := probeEventKind(t, ch); got != "reader-started" {
		t.Fatalf("probe saw %q, want reader-started", got)
	}
}

// TestSetReadProbeNilClears installs a probe and then clears it with a nil
// channel, matching the documented restore semantics.
func TestSetReadProbeNilClears(t *testing.T) {
	ambient := readProbe.Swap(nil)
	defer readProbe.Store(ambient)

	ch := make(chan readProbeEvent, 1)
	restore := SetReadProbe(ch)
	restore()

	clear := SetReadProbe(nil)
	defer clear()
	if probe := readProbe.Load(); probe != nil {
		t.Fatal("SetReadProbe(nil) left a probe installed")
	}
}

func probeEventKind(t *testing.T, ch <-chan readProbeEvent) string {
	t.Helper()
	select {
	case event := <-ch:
		return event.Kind
	case <-time.After(probeWait):
		t.Fatal("probe received no event")
		return ""
	}
}

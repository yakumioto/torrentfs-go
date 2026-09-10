package session

import (
	"sync/atomic"
	"testing"
)

// TestSetReadGateRestoreClearsFirstInstall is the regression test for a
// restore that used to be a no-op when the previous gate was nil: after the
// first install and its restore, later reads must not run the hook at all.
// A gate left installed would block every subsequent read in the test binary.
func TestSetReadGateRestoreClearsFirstInstall(t *testing.T) {
	// Start from a known nil gate without dropping an ambient one.
	ambient := readGate.Swap(nil)
	defer readGate.Store(ambient)

	var calls atomic.Int64
	restore := SetReadGate(func() { calls.Add(1) })

	enterReadGate()
	if got := calls.Load(); got != 1 {
		t.Fatalf("installed hook ran %d times, want 1", got)
	}

	restore()
	if gate := readGate.Load(); gate != nil {
		t.Fatal("restore from a first install left the gate installed")
	}
	enterReadGate()
	if got := calls.Load(); got != 1 {
		t.Fatalf("hook ran %d times after restore, want it to stay at 1", got)
	}
}

// TestSetReadGateRestoresPreviousState covers the general contract: the gate in
// place after a restore is the one that was installed before, including when
// gates are nested, and the ambient state is never left modified.
func TestSetReadGateRestoresPreviousState(t *testing.T) {
	ambient := readGate.Load()
	defer readGate.Store(ambient)

	var outerCalls atomic.Int64
	restoreOuter := SetReadGate(func() { outerCalls.Add(1) })

	var innerCalls atomic.Int64
	restoreInner := SetReadGate(func() { innerCalls.Add(1) })

	enterReadGate()
	if innerCalls.Load() != 1 || outerCalls.Load() != 0 {
		t.Fatalf("nested gate ran outer=%d inner=%d, want outer=0 inner=1", outerCalls.Load(), innerCalls.Load())
	}

	restoreInner()
	enterReadGate()
	if outerCalls.Load() != 1 {
		t.Fatalf("after the inner restore the outer hook ran %d times, want 1", outerCalls.Load())
	}

	restoreOuter()
	if got := readGate.Load(); got != ambient {
		t.Fatalf("readGate after restore = %v, want the ambient gate %v", got, ambient)
	}
}

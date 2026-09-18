package cache

import (
	"bytes"
	"sync"
	"testing"
)

func TestCacheHitCountAndCopyIsolation(t *testing.T) {
	c := New(8)
	key := Key{Torrent: "a", Piece: 1}
	value := []byte("data")
	c.Put(key, value)
	value[0] = 'X'

	got, ok := c.Get(key)
	if !ok || !bytes.Equal(got, []byte("data")) {
		t.Fatalf("Get = %q, %v; want data, true", got, ok)
	}
	got[0] = 'X'
	again, ok := c.Get(key)
	if !ok || !bytes.Equal(again, []byte("data")) {
		t.Fatalf("second Get = %q, %v; want data, true", again, ok)
	}
	if got := c.HitCount(); got != 2 {
		t.Fatalf("HitCount = %d, want 2", got)
	}
}

func TestCacheEvictsLeastRecentlyUsedByBytes(t *testing.T) {
	// Capacity 8: eviction starts above the high-water mark (7) and reclaims to
	// the low-water mark (6).
	c := New(8)
	first := Key{Torrent: "a", Piece: 1}
	second := Key{Torrent: "a", Piece: 2}
	third := Key{Torrent: "a", Piece: 3}
	c.Put(first, []byte("111"))
	c.Put(second, []byte("222"))
	if _, ok := c.Get(first); !ok {
		t.Fatal("first entry should hit before eviction")
	}
	c.Put(third, []byte("333"))
	if _, ok := c.Get(second); ok {
		t.Fatal("least-recently-used entry should be evicted")
	}
	if _, ok := c.Get(first); !ok {
		t.Fatal("recently-used entry should remain")
	}
	if _, ok := c.Get(third); !ok {
		t.Fatal("new entry should remain")
	}
	if got := c.Size(); got != 6 {
		t.Fatalf("Size = %d, want 6 (low-water mark)", got)
	}
}

func TestCacheEvictsOnlyWhenHighWaterIsCrossed(t *testing.T) {
	// Capacity 8: 7 is the high-water trigger, 6 the low-water target. Filling
	// exactly to the high-water mark must not evict anything.
	c := New(8)
	for index := 0; index < 3; index++ {
		c.Put(Key{Torrent: "a", Piece: index}, []byte("12"))
	}
	if got := c.Size(); got != 6 {
		t.Fatalf("Size = %d, want 6", got)
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3: no eviction below the high-water mark", got)
	}

	// One more 2-byte insert crosses 7 and reclaims down to 6 in one pass.
	c.Put(Key{Torrent: "a", Piece: 3}, []byte("12"))
	if got := c.Size(); got != 6 {
		t.Fatalf("Size after crossing = %d, want 6", got)
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len after crossing = %d, want 3", got)
	}
}

func TestCachePinProtectsFromEviction(t *testing.T) {
	c := New(8)
	first := Key{Torrent: "a", Piece: 1}
	second := Key{Torrent: "a", Piece: 2}
	third := Key{Torrent: "a", Piece: 3}
	fourth := Key{Torrent: "a", Piece: 4}
	c.Put(first, []byte("11"))
	c.Put(second, []byte("22"))
	c.Put(third, []byte("33"))
	if !c.Pin(first) {
		t.Fatal("Pin of a resident piece should succeed")
	}
	c.Put(fourth, []byte("44"))
	if _, ok := c.Get(first); !ok {
		t.Fatal("pinned entry was evicted")
	}
	if _, ok := c.Get(second); ok {
		t.Fatal("unpinned least-recently-used entry should be evicted")
	}
	if !c.IsPinned(first) {
		t.Fatal("IsPinned(first) = false after pin")
	}
	c.Unpin(first)
	if c.IsPinned(first) {
		t.Fatal("IsPinned(first) = true after unpin")
	}
}

func TestCachePinBudgetRejectsOversizedPin(t *testing.T) {
	// Capacity 8 pins at most capacity-lowWater = 2 bytes.
	c := New(8)
	key := Key{Torrent: "a", Piece: 1}
	c.Put(key, []byte("123"))
	if c.Pin(key) {
		t.Fatal("Pin succeeded past the pin budget")
	}
	if c.IsPinned(key) {
		t.Fatal("rejected Pin still marked the key pinned")
	}
}

func TestCacheRollsBackInsertItCannotFit(t *testing.T) {
	// Two keys pinned while still absent cost nothing against the pin budget,
	// so both may be pinned. When their data later arrives the pinned total can
	// exceed the capacity; the insert that cannot fit must be rolled back
	// rather than held, and must not corrupt the eviction list.
	c := New(8)
	first := Key{Torrent: "a", Piece: 1}
	second := Key{Torrent: "a", Piece: 2}
	third := Key{Torrent: "a", Piece: 3}
	if !c.Pin(first) || !c.Pin(second) {
		t.Fatal("pinning absent keys failed within the budget")
	}
	c.Put(first, []byte("123456"))
	c.Put(second, []byte("123456"))
	if got := c.Size(); got > 8 {
		t.Fatalf("Size = %d, want at most the capacity 8", got)
	}
	// A third insert while both pinned entries hold six bytes each cannot fit.
	c.Put(third, []byte("123456"))
	if got := c.Size(); got > 8 {
		t.Fatalf("Size after a rolled-back insert = %d, want at most 8", got)
	}
	if c.Has(third) {
		t.Fatal("an insert that cannot fit was retained")
	}
	if !c.IsPinned(first) || !c.IsPinned(second) {
		t.Fatal("rollback released a pin")
	}
	// The cache must still be usable after the rollback.
	c.UnpinTorrent("a")
	c.Put(third, []byte("123456"))
	if !c.Has(third) {
		t.Fatal("cache did not accept a value after a rolled-back insert")
	}
}

func TestWatermarksStayOrderedForSmallCapacities(t *testing.T) {
	for capacity := int64(1); capacity <= 64; capacity++ {
		high := HighWater(capacity)
		low := LowWater(capacity)
		if low <= 0 || high <= 0 {
			t.Fatalf("capacity %d: watermarks = high %d, low %d; want both positive", capacity, high, low)
		}
		if low > high {
			t.Fatalf("capacity %d: low %d > high %d", capacity, low, high)
		}
		if high > capacity {
			t.Fatalf("capacity %d: high %d exceeds the capacity", capacity, high)
		}
	}
	if got := HighWater(0); got != 0 {
		t.Fatalf("HighWater(0) = %d, want 0", got)
	}
	if got := LowWater(0); got != 0 {
		t.Fatalf("LowWater(0) = %d, want 0", got)
	}
}

// TestCacheRetainsAFittingEntryAtSmallCapacity covers capacities whose
// fractional watermarks round to zero. Both marks are clamped to one byte, and
// an eviction pass never reclaims the entry the insertion is placing, so a
// piece that fits the capacity stays resident instead of being evicted by the
// very insert that accepted it.
func TestCacheRetainsAFittingEntryAtSmallCapacity(t *testing.T) {
	for _, capacity := range []int64{1, 2, 3, 4} {
		c := New(capacity)
		key := Key{Torrent: "a", Piece: 1}
		c.Put(key, bytes.Repeat([]byte("x"), int(capacity)))
		if !c.Has(key) {
			t.Fatalf("capacity %d: a fitting entry was not retained", capacity)
		}
		if got := c.Size(); got != capacity {
			t.Fatalf("capacity %d: Size = %d, want %d", capacity, got, capacity)
		}
	}
}

// TestCacheRetainsItsOnlyEntryAtCapacityOne pins the smallest valid capacity.
// Both watermarks rounded down to zero there, so any eviction pass -- including
// the one Unpin runs after releasing a read window -- reclaimed the only entry,
// and the cache could never hold the single piece that fits it.
func TestCacheRetainsItsOnlyEntryAtCapacityOne(t *testing.T) {
	c := New(1)
	key := Key{Torrent: "a", Piece: 1}
	c.Put(key, []byte("x"))
	if !c.Has(key) || c.Size() != 1 {
		t.Fatalf("after Put: Has = %v, Size = %d; want the fitting entry retained", c.Has(key), c.Size())
	}
	c.Unpin(key)
	if !c.Has(key) {
		t.Fatal("Unpin evicted the only resident entry")
	}
	c.Put(key, []byte("x"))
	if !c.Has(key) || c.Size() != 1 {
		t.Fatalf("after re-insert: Has = %v, Size = %d", c.Has(key), c.Size())
	}
}

// TestCacheKeepsNewEntryWhenItAloneExceedsLowWater pins the same rule for a
// larger piece: with capacity 10 the low-water mark is 7, so a 9-byte piece
// cannot be reclaimed to the mark and must not be evicted for trying.
func TestCacheKeepsNewEntryWhenItAloneExceedsLowWater(t *testing.T) {
	c := New(10)
	key := Key{Torrent: "a", Piece: 1}
	c.Put(key, bytes.Repeat([]byte("y"), 9))
	if !c.Has(key) {
		t.Fatal("a 9-byte entry was evicted from a 10-byte cache")
	}
	if got := c.Size(); got != 9 {
		t.Fatalf("Size = %d, want 9", got)
	}
	// Eviction still reclaims the least-recently-used entry to make room: the
	// protection only stops an insert from evicting itself.
	newer := Key{Torrent: "a", Piece: 2}
	c.Put(newer, []byte("z"))
	if !c.Has(newer) {
		t.Fatal("the newest entry was evicted by its own insertion")
	}
	if c.Has(key) {
		t.Fatal("the least-recently-used entry was not reclaimed")
	}
	if got := c.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
}

func TestCacheSizeOfAndSnapshot(t *testing.T) {
	c := New(1024)
	a0 := Key{Torrent: "a", Piece: 0}
	a1 := Key{Torrent: "a", Piece: 1}
	b0 := Key{Torrent: "b", Piece: 0}
	c.Put(a0, []byte("aaaa"))
	c.Put(a1, []byte("aa"))
	c.Put(b0, []byte("bbbbbb"))

	if got := c.SizeOf("a"); got != 6 {
		t.Fatalf("SizeOf(a) = %d, want 6", got)
	}
	if got := c.SizeOf("b"); got != 6 {
		t.Fatalf("SizeOf(b) = %d, want 6", got)
	}
	if got := c.SizeOf("missing"); got != 0 {
		t.Fatalf("SizeOf(missing) = %d, want 0", got)
	}

	if !c.Pin(a1) {
		t.Fatal("Pin(a1) failed")
	}
	cached, pinned := c.Snapshot("a")
	if len(cached) != 2 || cached[0] != 4 || cached[1] != 2 {
		t.Fatalf("Snapshot(a) cached = %+v, want {0:4, 1:2}", cached)
	}
	if len(pinned) != 1 || !pinned[1] {
		t.Fatalf("Snapshot(a) pinned = %+v, want {1:true}", pinned)
	}
	other, otherPinned := c.Snapshot("b")
	if len(other) != 1 || other[0] != 6 || len(otherPinned) != 0 {
		t.Fatalf("Snapshot(b) = %+v/%+v, want one 6-byte entry and no pins", other, otherPinned)
	}

	c.Remove(a0)
	if got := c.SizeOf("a"); got != 2 {
		t.Fatalf("SizeOf(a) after Remove = %d, want 2", got)
	}
	// InvalidateTorrent drops entries and pins together.
	c.InvalidateTorrent("a")
	if got := c.SizeOf("a"); got != 0 {
		t.Fatalf("SizeOf(a) after InvalidateTorrent = %d, want 0", got)
	}
	if c.IsPinned(a1) {
		t.Fatal("InvalidateTorrent left a pin behind")
	}
	if got := c.PinnedBytes(); got != 0 {
		t.Fatalf("PinnedBytes after InvalidateTorrent = %d, want 0", got)
	}
}

func TestCacheUnpinTorrentReleasesEveryPin(t *testing.T) {
	c := New(1024)
	a0 := Key{Torrent: "a", Piece: 0}
	a1 := Key{Torrent: "a", Piece: 1}
	b0 := Key{Torrent: "b", Piece: 0}
	c.Put(a0, []byte("aa"))
	c.Put(a1, []byte("aa"))
	c.Put(b0, []byte("aa"))
	if !c.Pin(a0) || !c.Pin(a1) || !c.Pin(b0) {
		t.Fatal("Pin failed within budget")
	}
	c.UnpinTorrent("a")
	if c.IsPinned(a0) || c.IsPinned(a1) {
		t.Fatal("UnpinTorrent left a pin behind")
	}
	if !c.IsPinned(b0) {
		t.Fatal("UnpinTorrent released another torrent's pin")
	}
	if got := c.PinnedBytes(); got != 2 {
		t.Fatalf("PinnedBytes = %d, want 2", got)
	}
}

func TestCacheSkipsOversizedEntryAndInvalidatesTorrent(t *testing.T) {
	c := New(4)
	oversized := Key{Torrent: "a", Piece: 1}
	c.Put(oversized, []byte("12345"))
	if _, ok := c.Get(oversized); ok {
		t.Fatal("oversized entry should not be cached")
	}

	c.Put(Key{Torrent: "a", Piece: 2}, []byte("12"))
	c.Put(Key{Torrent: "b", Piece: 2}, []byte("34"))
	c.InvalidateTorrent("a")
	if _, ok := c.Get(Key{Torrent: "a", Piece: 2}); ok {
		t.Fatal("torrent a entry should be invalidated")
	}
	if _, ok := c.Get(Key{Torrent: "b", Piece: 2}); !ok {
		t.Fatal("torrent b entry should remain")
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := New(1024)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := Key{Torrent: "a", Piece: i}
			for j := 0; j < 100; j++ {
				c.Put(key, []byte("value"))
				_, _ = c.Get(key)
			}
		}(i)
	}
	wg.Wait()
	if c.HitCount() == 0 {
		t.Fatal("concurrent access produced no cache hits")
	}
}

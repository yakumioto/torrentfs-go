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
	c := New(5)
	first := Key{Torrent: "a", Piece: 1}
	second := Key{Torrent: "a", Piece: 2}
	third := Key{Torrent: "a", Piece: 3}
	c.Put(first, []byte("aa"))
	c.Put(second, []byte("bb"))
	if _, ok := c.Get(first); !ok {
		t.Fatal("first entry should hit before eviction")
	}
	c.Put(third, []byte("ccc"))
	if _, ok := c.Get(second); ok {
		t.Fatal("least-recently-used entry should be evicted")
	}
	if _, ok := c.Get(first); !ok {
		t.Fatal("recently-used entry should remain")
	}
	if _, ok := c.Get(third); !ok {
		t.Fatal("new entry should remain")
	}
	if got := c.Size(); got != 5 {
		t.Fatalf("Size = %d, want 5", got)
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

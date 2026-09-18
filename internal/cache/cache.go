// Package cache provides the session's in-memory piece cache.
package cache

import (
	"container/list"
	"sync"
)

// Key identifies one piece in one torrent.
type Key struct {
	Torrent string
	Piece   int
}

type entry struct {
	key   Key
	value []byte
}

// Eviction watermarks as fractions of the cache capacity. A high-water mark
// triggers eviction and a low-water mark ends it, so a burst of inserts is
// reclaimed in one batch instead of churning at the capacity boundary. The
// headroom between the high-water mark and the hard capacity absorbs a read
// window without blocking on eviction.
const (
	highWaterNumerator   int64 = 7
	highWaterDenominator int64 = 8
	lowWaterNumerator    int64 = 3
	lowWaterDenominator  int64 = 4
)

// HighWater returns the byte size that triggers eviction for a capacity.
func HighWater(capacity int64) int64 {
	return capacity * highWaterNumerator / highWaterDenominator
}

// LowWater returns the byte size that ends an eviction pass for a capacity.
func LowWater(capacity int64) int64 {
	return capacity * lowWaterNumerator / lowWaterDenominator
}

// pinBytesCap returns how many bytes may be pinned at once. It leaves the
// low-water mark evictable at all times, so an eviction always has a victim and
// a pinned read window can never deadlock against the cache.
func (c *Cache) pinBytesCap() int64 {
	return c.capacity - LowWater(c.capacity)
}

// Cache is a byte-capacity, least-recently-used cache. Insertion is bounded by
// the capacity; eviction starts at the high-water mark and stops at the low.
type Cache struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	hits     uint64
	items    map[Key]*list.Element
	lru      *list.List

	// pinned holds the keys that eviction must skip. A key may be pinned
	// before it is resident, so that data arriving later is protected.
	pinned      map[Key]struct{}
	pinnedBytes int64

	// usedByTorrent tracks resident bytes per torrent so the management API can
	// report the cache's actual occupancy without walking the LRU.
	usedByTorrent map[string]int64
}

// New returns an empty cache with the given byte capacity.
func New(capacity int64) *Cache {
	return &Cache{
		capacity:      capacity,
		items:         make(map[Key]*list.Element),
		lru:           list.New(),
		pinned:        make(map[Key]struct{}),
		usedByTorrent: make(map[string]int64),
	}
}

// Has reports whether key is cached without changing LRU order or hit count.
func (c *Cache) Has(key Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[key]
	return ok
}

// Get returns a copy of the cached value and promotes it to most recently used.
func (c *Cache) Get(key Key) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(elem)
	c.hits++
	value := elem.Value.(*entry).value
	return append([]byte(nil), value...), true
}

// Put stores a copy of value. Inserting may push the cache past the high-water
// mark, which triggers an eviction pass down to the low-water mark. A value
// larger than the capacity is never retained, and an insertion that cannot be
// made to fit under the hard capacity is rolled back.
func (c *Cache) Put(key Key, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.remove(elem)
	}
	if c.capacity <= 0 || int64(len(value)) > c.capacity {
		return
	}

	stored := append([]byte(nil), value...)
	elem := c.lru.PushFront(&entry{key: key, value: stored})
	c.items[key] = elem
	c.used += int64(len(stored))
	c.usedByTorrent[key.Torrent] += int64(len(stored))
	if _, ok := c.pinned[key]; ok {
		c.pinnedBytes += int64(len(stored))
	}
	c.evictLocked()
	if c.used > c.capacity {
		// Reclaiming everything evictable was not enough (pinned entries
		// remain), so the insert cannot be honoured. Roll it back so the hard
		// cap holds; the caller's next read falls back to the network. The
		// element may already be gone if the eviction pass reached it, so look
		// it up rather than reusing the pointer.
		if current, ok := c.items[key]; ok {
			c.remove(current)
		}
	}
}

// Remove drops key from the cache if it is present, without touching pins.
func (c *Cache) Remove(key Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.remove(elem)
	}
}

// Pin protects key from eviction. Pinning a key that is not resident yet is
// allowed: the protection applies if the piece arrives later. Pin fails when it
// would push the pinned total past the pin budget, in which case the caller
// must proceed without protection.
func (c *Cache) Pin(key Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pinned[key]; ok {
		return true
	}
	size := c.sizeOfLocked(key)
	if c.pinnedBytes+size > c.pinBytesCap() {
		return false
	}
	c.pinned[key] = struct{}{}
	c.pinnedBytes += size
	return true
}

// Unpin releases one pinned key.
func (c *Cache) Unpin(key Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unpinLocked(key)
	if c.used > HighWater(c.capacity) {
		c.evictLocked()
	}
}

// UnpinTorrent releases every pinned key belonging to torrent.
func (c *Cache) UnpinTorrent(torrent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.pinned {
		if key.Torrent == torrent {
			c.unpinLocked(key)
		}
	}
}

// IsPinned reports whether key is protected from eviction.
func (c *Cache) IsPinned(key Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.pinned[key]
	return ok
}

// PinnedBytes returns the number of resident bytes currently pinned.
func (c *Cache) PinnedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinnedBytes
}

// SizeOf returns the number of cached bytes belonging to torrent.
func (c *Cache) SizeOf(torrent string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedByTorrent[torrent]
}

// Snapshot returns the resident byte size and the pinned flag of every piece of
// torrent that the cache knows about, in one lock acquisition. Pieces absent
// from the maps are neither cached nor pinned.
func (c *Cache) Snapshot(torrent string) (cached map[int]int64, pinned map[int]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached = make(map[int]int64)
	pinned = make(map[int]bool)
	for key, elem := range c.items {
		if key.Torrent == torrent {
			cached[key.Piece] = int64(len(elem.Value.(*entry).value))
		}
	}
	for key := range c.pinned {
		if key.Torrent == torrent {
			pinned[key.Piece] = true
		}
	}
	return cached, pinned
}

// HitCount returns the number of successful Get calls.
func (c *Cache) HitCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// InvalidateTorrent removes every entry and pin belonging to torrent.
func (c *Cache) InvalidateTorrent(torrent string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for elem := c.lru.Back(); elem != nil; {
		previous := elem.Prev()
		if elem.Value.(*entry).key.Torrent == torrent {
			c.remove(elem)
		}
		elem = previous
	}
	for key := range c.pinned {
		if key.Torrent == torrent {
			delete(c.pinned, key)
		}
	}
	c.pinnedBytes = c.recountPinnedLocked()
}

// Capacity returns the cache's byte capacity.
func (c *Cache) Capacity() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}

// Len returns the number of cached pieces.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Size returns the number of cached bytes.
func (c *Cache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// evictLocked reclaims from the least-recently-used end until the cache is back
// under the low-water mark, skipping pinned entries. It gives up when no
// unpinned victim is left.
func (c *Cache) evictLocked() {
	if c.used <= HighWater(c.capacity) {
		return
	}
	for c.used > LowWater(c.capacity) {
		victim := c.victimLocked()
		if victim == nil {
			return
		}
		c.remove(victim)
	}
}

// victimLocked returns the least-recently-used unpinned entry.
func (c *Cache) victimLocked() *list.Element {
	for elem := c.lru.Back(); elem != nil; elem = elem.Prev() {
		if _, ok := c.pinned[elem.Value.(*entry).key]; !ok {
			return elem
		}
	}
	return nil
}

func (c *Cache) sizeOfLocked(key Key) int64 {
	if elem, ok := c.items[key]; ok {
		return int64(len(elem.Value.(*entry).value))
	}
	return 0
}

func (c *Cache) unpinLocked(key Key) {
	if _, ok := c.pinned[key]; !ok {
		return
	}
	delete(c.pinned, key)
	c.pinnedBytes -= c.sizeOfLocked(key)
}

func (c *Cache) recountPinnedLocked() int64 {
	var total int64
	for key := range c.pinned {
		total += c.sizeOfLocked(key)
	}
	return total
}

func (c *Cache) remove(elem *list.Element) {
	if elem == nil {
		return
	}
	item := elem.Value.(*entry)
	delete(c.items, item.key)
	c.lru.Remove(elem)
	n := int64(len(item.value))
	c.used -= n
	if c.usedByTorrent[item.key.Torrent] -= n; c.usedByTorrent[item.key.Torrent] <= 0 {
		delete(c.usedByTorrent, item.key.Torrent)
	}
	if _, ok := c.pinned[item.key]; ok {
		c.pinnedBytes -= n
	}
}

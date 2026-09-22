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

// Stats is a point-in-time accounting snapshot for cache behaviour. It does
// not change eviction or pin semantics.
type Stats struct {
	Hits                     uint64
	Misses                   uint64
	Evictions                uint64
	EvictedBytes             int64
	PutRejections            uint64
	PinReservationRejections uint64
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

// HighWater returns the byte size that triggers eviction for a capacity. For a
// positive capacity it is never zero: a capacity of 1 or 2 bytes would otherwise
// round both marks down to zero and evict every insertion immediately, so the
// cache would accept a piece that fits and then refuse to keep it.
func HighWater(capacity int64) int64 {
	if capacity <= 0 {
		return 0
	}
	if mark := capacity * highWaterNumerator / highWaterDenominator; mark > 0 {
		return mark
	}
	return 1
}

// LowWater returns the byte size that ends an eviction pass for a capacity. Like
// HighWater it is clamped to at least one byte, and clamped marks keep the
// ordering low <= high <= capacity for every positive capacity because
// max(1, cap*3/4) <= max(1, cap*7/8).
func LowWater(capacity int64) int64 {
	if capacity <= 0 {
		return 0
	}
	if mark := capacity * lowWaterNumerator / lowWaterDenominator; mark > 0 {
		return mark
	}
	return 1
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
	mu                       sync.Mutex
	capacity                 int64
	used                     int64
	hits                     uint64
	misses                   uint64
	evictions                uint64
	evictedBytes             int64
	putRejections            uint64
	pinReservationRejections uint64
	items                    map[Key]*list.Element
	lru                      *list.List

	// pinned holds the keys that eviction must skip. A key may be pinned
	// before it is resident, so that data arriving later is protected.
	pinned map[Key]struct{}
	// pinnedSizes tracks the bytes reserved by each pin. For legacy Pin calls,
	// an absent resident reserves zero until data arrives; PinSize can reserve a
	// known piece size before insertion.
	pinnedSizes map[Key]int64
	pinnedSized map[Key]bool
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
		pinnedSizes:   make(map[Key]int64),
		pinnedSized:   make(map[Key]bool),
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
		c.misses++
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

	if c.capacity <= 0 || int64(len(value)) > c.capacity {
		c.putRejections++
		return
	}
	if c.pinnedSized[key] {
		oldReservation := c.pinnedSizes[key]
		if c.pinnedBytes-oldReservation+int64(len(value)) > c.pinBytesCap() {
			c.putRejections++
			return
		}
	}
	if elem, ok := c.items[key]; ok {
		c.remove(elem)
	}

	stored := append([]byte(nil), value...)
	elem := c.lru.PushFront(&entry{key: key, value: stored})
	c.items[key] = elem
	c.used += int64(len(stored))
	c.usedByTorrent[key.Torrent] += int64(len(stored))
	if _, ok := c.pinned[key]; ok {
		oldReservation := c.pinnedSizes[key]
		newReservation := int64(len(stored))
		c.pinnedBytes += newReservation - oldReservation
		c.pinnedSizes[key] = newReservation
	}
	c.evictLocked(elem)
	if c.used > c.capacity {
		c.putRejections++
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
// allowed and retains the legacy zero-cost behavior; callers that know the
// eventual piece size should use PinSize so future pins cannot consume the
// entire cache without spending budget.
func (c *Cache) Pin(key Key) bool {
	return c.pin(key, 0, false)
}

// PinSize protects key from eviction and reserves size bytes even when the key
// is not resident yet. This prevents a read-ahead window from pinning an
// unbounded number of zero-cost future pieces and starving the requested piece.
func (c *Cache) PinSize(key Key, size int64) bool {
	if size < 0 {
		return false
	}
	return c.pin(key, size, true)
}

func (c *Cache) pin(key Key, requested int64, reserve bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	actual := c.sizeOfLocked(key)
	if requested > actual {
		actual = requested
	}
	if _, ok := c.pinned[key]; ok {
		if reserve && !c.pinnedSized[key] && c.pinnedBytes > c.pinBytesCap() {
			c.pinReservationRejections++
			return false
		}
		if !reserve || actual <= c.pinnedSizes[key] {
			if reserve {
				c.pinnedSized[key] = true
			}
			return true
		}
		if c.pinnedBytes+(actual-c.pinnedSizes[key]) > c.pinBytesCap() {
			c.pinReservationRejections++
			return false
		}
		c.pinnedBytes += actual - c.pinnedSizes[key]
		c.pinnedSizes[key] = actual
		c.pinnedSized[key] = true
		return true
	}
	reservation := actual
	if !reserve && c.sizeOfLocked(key) == 0 {
		reservation = 0
	}
	if c.pinnedBytes+reservation > c.pinBytesCap() {
		c.pinReservationRejections++
		return false
	}
	c.pinned[key] = struct{}{}
	c.pinnedSizes[key] = reservation
	c.pinnedSized[key] = reserve
	c.pinnedBytes += reservation
	return true
}

// Unpin releases one pinned key.
func (c *Cache) Unpin(key Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unpinLocked(key)
	if c.used > HighWater(c.capacity) {
		c.evictLocked(nil)
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

// ContiguousRun reports how many bytes of torrent are resident in one
// uninterrupted run beginning at byte start. Each piece occupies step bytes in
// the caller's geometry, the walk stops at the first missing piece or once
// limit bytes past start have been counted, and a resident piece shorter than
// its nominal extent ends the run at its real length. It allocates nothing, so
// a reader can ask for its playable buffer while holding its own lock.
func (c *Cache) ContiguousRun(torrent string, start, step, limit int64) int64 {
	if step <= 0 || limit <= 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	cursor := start
	for total < limit {
		index := cursor / step
		elem, ok := c.items[Key{Torrent: torrent, Piece: int(index)}]
		if !ok {
			break
		}
		pieceEnd := (index + 1) * step
		available := pieceEnd - cursor
		if resident := int64(len(elem.Value.(*entry).value)); resident < available {
			available = resident
		}
		if available <= 0 {
			break
		}
		if total+available > limit {
			available = limit - total
		}
		total += available
		cursor = pieceEnd
	}
	return total
}

// Stats returns a point-in-time accounting snapshot. It is reporting only.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits:                     c.hits,
		Misses:                   c.misses,
		Evictions:                c.evictions,
		EvictedBytes:             c.evictedBytes,
		PutRejections:            c.putRejections,
		PinReservationRejections: c.pinReservationRejections,
	}
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
			delete(c.pinnedSizes, key)
			delete(c.pinnedSized, key)
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
//
// keep, when non-nil, is the entry an insertion is trying to place: the pass
// must never evict it. A piece that fits the capacity has to stay resident, and
// when it is the only entry the low-water target is below its size, so without
// this the cache would evict the very piece it just accepted.
func (c *Cache) evictLocked(keep *list.Element) {
	if c.used <= HighWater(c.capacity) {
		return
	}
	for c.used > LowWater(c.capacity) {
		victim := c.victimLocked(keep)
		if victim == nil {
			return
		}
		c.evictions++
		c.evictedBytes += int64(len(victim.Value.(*entry).value))
		c.remove(victim)
	}
}

// victimLocked returns the least-recently-used unpinned entry, never keep.
func (c *Cache) victimLocked(keep *list.Element) *list.Element {
	for elem := c.lru.Back(); elem != nil; elem = elem.Prev() {
		if elem == keep {
			continue
		}
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
	c.pinnedBytes -= c.pinnedSizes[key]
	delete(c.pinnedSizes, key)
	delete(c.pinnedSized, key)
}

func (c *Cache) recountPinnedLocked() int64 {
	var total int64
	for key := range c.pinned {
		total += c.pinnedSizes[key]
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
	// A sized reservation belongs to the pin, not to this resident entry.
	// Keep it until Unpin so Remove/rollback cannot turn a protected key back
	// into a zero-cost pin.
}

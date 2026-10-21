package filesystem

import (
	"sync"

	"github.com/anacrolix/torrent/metainfo"
)

// snapshotCache owns only the current value for a hash. An observation fences
// publication against concurrent replacement/removal without a generation
// counter. Backend calls and expensive construction happen outside mu.
type snapshotCache[T any] struct {
	mu       sync.Mutex
	slots    map[metainfo.Hash]*snapshotSlot[T]
	listings map[*snapshotListing[T]]struct{}
}

type snapshotSlot[T any] struct{ current *T }

type snapshotObservation[T any] struct {
	slot    *snapshotSlot[T]
	value   *T
	listing *snapshotListing[T]
}

type snapshotListing[T any] struct {
	observed    map[metainfo.Hash]snapshotObservation[T]
	invalidated map[metainfo.Hash]bool
	// Completed concurrent listings fence even hashes that had no slot when
	// this read began. This set exists only for the lifetime of the read.
	allowed    map[metainfo.Hash]bool
	restricted bool
}

func (c *snapshotCache[T]) observe(hash metainfo.Hash) snapshotObservation[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slots == nil {
		c.slots = make(map[metainfo.Hash]*snapshotSlot[T])
	}
	slot := c.slots[hash]
	if slot == nil {
		slot = &snapshotSlot[T]{}
		c.slots[hash] = slot
	}
	return snapshotObservation[T]{slot: slot, value: slot.current}
}

// finish drops a transient slot when a read did not produce a cacheable value.
func (c *snapshotCache[T]) finish(hash metainfo.Hash, observed snapshotObservation[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot := c.slots[hash]; slot == observed.slot && slot != nil && slot.current == nil {
		delete(c.slots, hash)
	}
}

func (c *snapshotCache[T]) observeListing() *snapshotListing[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	listing := &snapshotListing[T]{
		observed:    make(map[metainfo.Hash]snapshotObservation[T], len(c.slots)),
		invalidated: make(map[metainfo.Hash]bool),
	}
	for hash, slot := range c.slots {
		listing.observed[hash] = snapshotObservation[T]{slot: slot, value: slot.current, listing: listing}
	}
	if c.listings == nil {
		c.listings = make(map[*snapshotListing[T]]struct{})
	}
	c.listings[listing] = struct{}{}
	return listing
}

func (l *snapshotListing[T]) observation(hash metainfo.Hash) snapshotObservation[T] {
	observed := l.observed[hash]
	observed.listing = l
	return observed
}

func (c *snapshotCache[T]) finishListing(listing *snapshotListing[T], live map[metainfo.Hash]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for hash, slot := range c.slots {
		if live[hash] {
			continue
		}
		observed := listing.observed[hash]
		if slot.current == nil {
			// Fence the current placeholder even if the original slot was
			// removed and recreated. Published values keep the old identity check.
			observed = snapshotObservation[T]{slot: slot}
		}
		c.forgetLocked(hash, observed)
	}
	delete(c.listings, listing)
	for pending := range c.listings {
		if !pending.restricted {
			pending.allowed = make(map[metainfo.Hash]bool, len(live))
			for hash := range live {
				pending.allowed[hash] = true
			}
			pending.restricted = true
		} else {
			for hash := range pending.allowed {
				if !live[hash] {
					delete(pending.allowed, hash)
				}
			}
		}
	}
}

func (c *snapshotCache[T]) currentLocked(hash metainfo.Hash) *T {
	if slot := c.slots[hash]; slot != nil {
		return slot.current
	}
	return nil
}

func (c *snapshotCache[T]) unchangedLocked(hash metainfo.Hash, observed snapshotObservation[T]) bool {
	if listing := observed.listing; listing != nil && (listing.invalidated[hash] || (listing.restricted && !listing.allowed[hash])) {
		return false
	}
	slot := c.slots[hash]
	return slot == observed.slot && (slot == nil || slot.current == observed.value)
}

func (c *snapshotCache[T]) publishLocked(hash metainfo.Hash, observed snapshotObservation[T], value *T) bool {
	if !c.unchangedLocked(hash, observed) {
		return false
	}
	if c.slots == nil {
		c.slots = make(map[metainfo.Hash]*snapshotSlot[T])
	}
	slot := c.slots[hash]
	if slot == nil {
		slot = &snapshotSlot[T]{}
		c.slots[hash] = slot
	}
	slot.current = value
	return true
}

func (c *snapshotCache[T]) forget(hash metainfo.Hash, observed snapshotObservation[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetLocked(hash, observed)
}

func (c *snapshotCache[T]) forgetLocked(hash metainfo.Hash, observed snapshotObservation[T]) {
	if !c.unchangedLocked(hash, observed) {
		return
	}
	delete(c.slots, hash)
	// A library read can have started before the first per-hash cache entry
	// existed. Remember removals only for those in-flight reads, so their late
	// results cannot recreate a deleted entry. No tombstones survive the read.
	for listing := range c.listings {
		listing.invalidated[hash] = true
	}
}

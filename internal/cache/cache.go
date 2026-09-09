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

// Cache is a byte-capacity, least-recently-used cache.
type Cache struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	hits     uint64
	items    map[Key]*list.Element
	lru      *list.List
}

// New returns an empty cache with the given byte capacity.
func New(capacity int64) *Cache {
	return &Cache{
		capacity: capacity,
		items:    make(map[Key]*list.Element),
		lru:      list.New(),
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

// Put stores a copy of value, evicting least-recently-used entries as needed.
// A value larger than the cache capacity is not retained.
func (c *Cache) Put(key Key, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.remove(elem)
	}
	if int64(len(value)) > c.capacity || c.capacity <= 0 {
		return
	}

	stored := append([]byte(nil), value...)
	elem := c.lru.PushFront(&entry{key: key, value: stored})
	c.items[key] = elem
	c.used += int64(len(stored))
	for c.used > c.capacity {
		c.remove(c.lru.Back())
	}
}

// HitCount returns the number of successful Get calls.
func (c *Cache) HitCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// InvalidateTorrent removes every entry belonging to torrent.
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

func (c *Cache) remove(elem *list.Element) {
	if elem == nil {
		return
	}
	item := elem.Value.(*entry)
	delete(c.items, item.key)
	c.lru.Remove(elem)
	c.used -= int64(len(item.value))
}

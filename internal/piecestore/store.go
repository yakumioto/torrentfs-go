// Package piecestore adapts the session's in-memory piece cache to anacrolix's
// storage interfaces.
//
// Nothing is persisted. A piece exists only as long as it is resident in the
// LRU: anacrolix writes incoming chunks into a per-piece staging buffer, and a
// verified piece is promoted into the cache. A read that misses both the cache
// and the staging buffer reports the piece as missing, which lets anacrolix's
// own retry path mark the piece incomplete and download it again.
package piecestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

// Store implements storage.ClientImplCloser on top of an in-memory LRU cache.
type Store struct {
	capacity int64
	cache    *cache.Cache
	logger   *slog.Logger

	mu             sync.Mutex
	staging        map[cache.Key]*stagingBuffer
	lastRead       map[cache.Key]uint64
	epochs         map[cache.Key]uint64
	verifiedEpoch  map[cache.Key]uint64
	nextGeneration uint64
	closed         bool
}

// byteRange is a half-open [start, end) span of a piece that has been received.
type byteRange struct {
	start int64
	end   int64
}

// stagingBuffer accumulates the chunks of one piece that a peer has delivered.
// received tracks which byte ranges actually arrived, so a read can never be
// served the zero fill of a region that is still in flight.
type stagingBuffer struct {
	generation uint64
	data       []byte
	received   []byteRange
}

// write copies a received chunk and records the range it covers.
func (b *stagingBuffer) write(off int64, src []byte) {
	n := copy(b.data[off:], src)
	if n > 0 {
		b.cover(off, off+int64(n))
	}
}

// cover merges [start, end) into the received ranges, which stay sorted and
// disjoint.
func (b *stagingBuffer) cover(start, end int64) {
	if start >= end {
		return
	}
	merged := make([]byteRange, 0, len(b.received)+1)
	placed := false
	for _, span := range b.received {
		switch {
		case span.end < start: // strictly before the new range
			merged = append(merged, span)
		case end < span.start: // strictly after it
			if !placed {
				merged = append(merged, byteRange{start: start, end: end})
				placed = true
			}
			merged = append(merged, span)
		default: // overlapping or adjacent: absorb into the new range
			if span.start < start {
				start = span.start
			}
			if span.end > end {
				end = span.end
			}
		}
	}
	if !placed {
		merged = append(merged, byteRange{start: start, end: end})
	}
	b.received = merged
}

// covers reports whether every byte of [start, end) has been received.
func (b *stagingBuffer) covers(start, end int64) bool {
	for _, span := range b.received {
		if span.start <= start && end <= span.end {
			return true
		}
	}
	return false
}

var _ storage.ClientImplCloser = (*Store)(nil)

// New returns a store backed by c. Every piece lives in c until it is evicted.
func New(c *cache.Cache, logger *slog.Logger) *Store {
	if logger == nil {
		logger = logging.Discard()
	}
	return &Store{
		capacity:      c.Capacity(),
		cache:         c,
		logger:        logger,
		staging:       make(map[cache.Key]*stagingBuffer),
		lastRead:      make(map[cache.Key]uint64),
		epochs:        make(map[cache.Key]uint64),
		verifiedEpoch: make(map[cache.Key]uint64),
	}
}

// OpenTorrent binds the store to one torrent. A torrent whose piece length does
// not fit the cache can never serve a read, so it is rejected outright instead
// of looping through download/evict. A piece that fits but exceeds the low-water
// mark is admitted with a warning: it will be reclaimed by the next eviction
// pass, falling back to the network on every read.
func (s *Store) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	key := infoHash.HexString()
	if info.PieceLength <= 0 {
		return storage.TorrentImpl{}, fmt.Errorf("piecestore: %s: invalid piece length %d", key, info.PieceLength)
	}
	if info.PieceLength > s.capacity {
		return storage.TorrentImpl{}, fmt.Errorf(
			"piecestore: %s: piece length %d exceeds cache capacity %d",
			key, info.PieceLength, s.capacity,
		)
	}
	if low := cache.LowWater(s.capacity); info.PieceLength > low {
		s.logger.Warn("piece length exceeds cache low-water mark",
			"hash", key,
			"piece_length", info.PieceLength,
			"cache_capacity_bytes", s.capacity,
		)
	}
	return storage.TorrentImpl{
		PieceWithHash: func(p metainfo.Piece, _ g.Option[[]byte]) storage.PieceImpl {
			pieceKey := cache.Key{Torrent: key, Piece: p.Index()}
			return &piece{store: s, key: pieceKey, length: p.Length(), generation: s.epoch(pieceKey)}
		},
		Close: func() error { return nil },
	}, nil
}

func (s *Store) epoch(key cache.Key) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epochs[key]
}

// markVerified records the token produced by the actual hash WriteTo operation.
// Ordinary ReaderAt calls never update it.
func (s *Store) markVerified(key cache.Key, generation uint64, length int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || s.epochs[key] != generation {
		return fmt.Errorf("piecestore: hash observed stale epoch %d for current %d", generation, s.epochs[key])
	}
	if buf := s.staging[key]; buf != nil {
		if buf.generation != generation || !buf.covers(0, length) || s.lastRead[key] != generation {
			return io.ErrUnexpectedEOF
		}
	} else if !s.cache.Has(key) {
		return io.EOF
	}
	s.verifiedEpoch[key] = generation
	return nil
}

// Close drops every staging buffer. Pieces already promoted to the cache are
// owned by the cache and are not touched here.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.staging = make(map[cache.Key]*stagingBuffer)
	s.lastRead = make(map[cache.Key]uint64)
	s.epochs = make(map[cache.Key]uint64)
	s.verifiedEpoch = make(map[cache.Key]uint64)
	return nil
}

// read copies one window under the store lock. The lock serializes choosing a
// resident value with starting a new staging generation, so a hash cannot finish
// reading the old resident value after a replacement download has begun.
//
// A window that is not fully received reports the piece as missing instead of
// returning the zero fill of a region a peer has not delivered yet. A successful
// staging read records its generation for MarkComplete's binding check.
func (s *Store) read(key cache.Key, dst []byte, off int64) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if buf, ok := s.staging[key]; ok {
		if off < 0 || !buf.covers(off, off+int64(len(dst))) {
			return 0, false, nil
		}
		n := copy(dst, buf.data[off:])
		s.lastRead[key] = buf.generation
		return n, true, nil
	}
	data, ok := s.cache.Get(key)
	if !ok {
		return 0, false, nil
	}
	n, err := copyResident(dst, data, off)
	return n, true, err
}

// piece is the PieceImpl for one piece of one torrent.
type piece struct {
	store      *Store
	key        cache.Key
	length     int64
	generation uint64
}

var _ storage.PieceImpl = (*piece)(nil)
var _ io.WriterTo = (*piece)(nil)

// WriteTo is the hash path used by anacrolix's Piece.WriteTo wrapper. It is the
// only operation that issues a verified token; ordinary streaming ReaderAt calls
// can read bytes but cannot authorize MarkComplete.
func (p *piece) WriteTo(w io.Writer) (int64, error) {
	n, err := io.Copy(w, io.NewSectionReader(p, 0, p.length))
	if err != nil {
		return n, err
	}
	if n != p.length {
		return n, io.ErrUnexpectedEOF
	}
	if err := p.store.markVerified(p.key, p.generation, p.length); err != nil {
		return n, err
	}
	return n, nil
}

// ReadAt serves the piece from the LRU, then from the staging buffer of an
// in-progress download. A resident piece is always served in full: a short read
// reports an error rather than a clean end of the piece, so anacrolix's wrapper
// never mistakes live data for lost data and calls MarkNotComplete on it. Only
// a piece that is in neither place returns io.EOF, which is the truthful answer.
func (p *piece) ReadAt(b []byte, off int64) (int, error) {
	// io.ReaderAt requires a non-nil error whenever fewer than len(b) bytes are
	// returned, so the caller's original request length is what the result is
	// measured against -- not the piece-clamped window served below.
	requested := len(b)
	if off < 0 {
		return 0, errors.New("piecestore: negative read offset")
	}
	if off >= p.length {
		return 0, io.EOF
	}
	if available := p.length - off; int64(len(b)) > available {
		b = b[:available]
	}
	if len(b) == 0 {
		return 0, nil
	}

	if n, served, err := p.store.read(p.key, b, off); served {
		return shortRead(n, requested, err)
	}
	return 0, io.EOF
}

// shortRead reports the end of a piece as io.EOF whenever the caller asked for
// more bytes than the piece provides. Without it a read whose buffer crosses
// the piece end would come back short with a nil error, which io.ReaderAt
// forbids and which lets a caller mistake a truncated piece-boundary read for a
// successful one.
func shortRead(n, requested int, err error) (int, error) {
	if err != nil {
		return n, err
	}
	if n < requested {
		return n, io.EOF
	}
	return n, nil
}

// copyResident copies the requested window out of resident data. A buffer that
// is shorter than the piece cannot satisfy the request, so it is reported as an
// unexpected EOF instead of a clean end of the piece.
func copyResident(b, data []byte, off int64) (int, error) {
	if off > int64(len(data)) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(b, data[off:])
	if n < len(b) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// WriteAt fills the piece's staging buffer. anacrolix calls this once per
// received chunk, so the buffer is allocated on first write and reused.
func (p *piece) WriteAt(b []byte, off int64) (int, error) {
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("piecestore: store is closed")
	}
	buf := s.staging[p.key]
	if buf == nil {
		s.nextGeneration++
		s.epochs[p.key] = s.nextGeneration
		buf = &stagingBuffer{generation: s.nextGeneration, data: make([]byte, p.length)}
		// A new staging generation invalidates any previously verified resident
		// value. A hash must never verify the old value while MarkComplete later
		// promotes this new generation.
		s.cache.Remove(p.key)
		delete(s.lastRead, p.key)
		s.staging[p.key] = buf
	}
	p.generation = buf.generation
	if off < 0 || off+int64(len(b)) > int64(len(buf.data)) {
		return 0, fmt.Errorf("piecestore: write [%d,%d) outside piece length %d", off, off+int64(len(b)), p.length)
	}
	buf.write(off, b)
	return len(b), nil
}

// MarkComplete promotes a verified piece into the cache and drops its staging
// buffer. Completion is bound to the staging generation that was actually read:
// an older PieceImpl cannot promote a newer download over the value it hashed,
// and a generation that was never read cannot be promoted. An incomplete
// staging buffer is also rejected rather than cached.
func (p *piece) MarkComplete() error {
	s := p.store
	s.mu.Lock()
	buf := s.staging[p.key]
	if buf == nil {
		s.mu.Unlock()
		return nil
	}
	if p.generation == 0 || buf.generation != p.generation {
		s.mu.Unlock()
		return fmt.Errorf("piecestore: stale staging generation %d for %s (current %d)", p.generation, p.key.Torrent, buf.generation)
	}
	if s.verifiedEpoch[p.key] != buf.generation {
		s.mu.Unlock()
		return fmt.Errorf("piecestore: staging generation %d was not verified by its hash", buf.generation)
	}
	if !buf.covers(0, p.length) {
		s.mu.Unlock()
		return io.ErrUnexpectedEOF
	}
	delete(s.staging, p.key)
	delete(s.lastRead, p.key)
	delete(s.verifiedEpoch, p.key)
	s.mu.Unlock()
	s.cache.Put(p.key, buf.data)
	return nil
}

// MarkNotComplete drops the piece from both the staging buffer and the cache, so
// the next read reports the piece as missing and anacrolix downloads it again.
// A stale PieceImpl must not discard a newer staging generation.
func (p *piece) MarkNotComplete() error {
	s := p.store
	s.mu.Lock()
	if buf := s.staging[p.key]; buf != nil && p.generation != 0 && buf.generation == p.generation {
		delete(s.staging, p.key)
		delete(s.lastRead, p.key)
	}
	s.mu.Unlock()
	s.cache.Remove(p.key)
	return nil
}

// Completion reports the piece as known, and complete exactly when it is
// resident. Ok is always true and Err is always nil: an unknown completion
// would make anacrolix hash every piece at startup, and an error would make it
// refuse to download at all. Neither would survive the "no piece recovery"
// requirement.
func (p *piece) Completion() storage.Completion {
	return storage.Completion{Ok: true, Complete: p.store.cache.Has(p.key)}
}

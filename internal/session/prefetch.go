package session

import (
	"context"
	"io"
	"sync"

	"github.com/anacrolix/missinggo/v2/pubsub"
	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

const (
	defaultPrefetchLow    int64 = 32 << 20
	defaultPrefetchHigh   int64 = 128 << 20
	defaultPrefetchPieces       = 4
)

type prefetchState uint8

const (
	prefetchIdle prefetchState = iota
	prefetchFilling
	prefetchPaused
	prefetchBudgetBlocked
)

func (s prefetchState) String() string {
	switch s {
	case prefetchFilling:
		return "filling"
	case prefetchPaused:
		return "paused"
	case prefetchBudgetBlocked:
		return "budget-blocked"
	default:
		return "idle"
	}
}

// prefetchBudget is shared by every torrent in a session. It deliberately uses
// a count rather than a channel so tests can compare limits without replacing a
// live coordinator or waking a polling loop.
type prefetchBudget struct {
	mu    sync.Mutex
	limit int
	used  int
}

func newPrefetchBudget(limit int) *prefetchBudget {
	if limit < 1 {
		limit = 1
	}
	return &prefetchBudget{limit: limit}
}

func (b *prefetchBudget) tryAcquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limit {
		return false
	}
	b.used++
	return true
}

func (b *prefetchBudget) release() {
	b.mu.Lock()
	if b.used > 0 {
		b.used--
	}
	b.mu.Unlock()
}

func (b *prefetchBudget) setLimit(limit int) (restore func()) {
	if limit < 1 {
		limit = 1
	}
	b.mu.Lock()
	previous := b.limit
	b.limit = limit
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.limit = previous
		b.mu.Unlock()
	}
}

func (b *prefetchBudget) snapshot() (limit, used int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit, b.used
}

type prefetchAnchor struct {
	file        *raFile
	fileStart   int64
	fileSize    int64
	pieceLength int64
	torrentSize int64
	cursor      int64
}

type foregroundTicket struct {
	coordinator *prefetchCoordinator
	id          uint64
	generation  uint64
	file        *raFile
	keys        []int
	once        sync.Once
}

func (t *foregroundTicket) finish(off int64, n int, err error) {
	if t == nil || t.coordinator == nil {
		return
	}
	t.once.Do(func() {
		t.coordinator.finishForeground(t, off, n, err)
	})
}

type prefetchCoordinator struct {
	torrent    *torrent.Torrent
	cache      *cache.Cache
	budget     *prefetchBudget
	torrentKey string

	ctx    context.Context
	cancel context.CancelFunc
	sub    *pubsub.Subscription[torrent.PieceStateChange]
	wake   chan struct{}
	done   chan struct{}

	mu           sync.Mutex
	closed       bool
	anchor       prefetchAnchor
	hasAnchor    bool
	generation   uint64
	nextTicket   uint64
	anchorTicket uint64

	foreground map[uint64][]int
	refs       map[int]int
	windowPins map[int]struct{}
	active     map[int]struct{}

	state           prefetchState
	bufferedBytes   int64
	effectiveLow    int64
	effectiveHigh   int64
	maxActive       int
	priorityAdds    uint64
	priorityCancels uint64
	dedupe          uint64
	budgetBlocks    uint64
	cancelled       uint64
}

// prefetchSnapshot is intentionally package-private. export_test.go exposes a
// stable test-only copy without adding a runtime API.
type prefetchSnapshot struct {
	Generation        uint64
	State             string
	Cursor            int64
	BufferedBytes     int64
	EffectiveLow      int64
	EffectiveHigh     int64
	ActivePieces      int
	ActiveBytes       int64
	MaxActivePieces   int
	PriorityAdds      uint64
	PriorityCancels   uint64
	DedupeCount       uint64
	BudgetBlocks      uint64
	CancelledPieces   uint64
	ForegroundTickets int
	PinnedPieces      int
}

func newPrefetchCoordinator(s *Session, tor *torrent.Torrent, c *cache.Cache) *prefetchCoordinator {
	ctx := context.Background()
	if s != nil && s.bgCtx != nil {
		ctx = s.bgCtx
	}
	ctx, cancel := context.WithCancel(ctx)
	// The session owns one budget for its whole lifetime. A nil session only
	// happens for a coordinator built directly by a test.
	budget := newPrefetchBudget(defaultPrefetchPieces)
	if s != nil && s.prefetchBudget != nil {
		budget = s.prefetchBudget
	}
	coordinator := &prefetchCoordinator{
		torrent:    tor,
		cache:      c,
		budget:     budget,
		torrentKey: tor.InfoHash().HexString(),
		ctx:        ctx,
		cancel:     cancel,
		sub:        tor.SubscribePieceStateChanges(),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		foreground: make(map[uint64][]int),
		refs:       make(map[int]int),
		windowPins: make(map[int]struct{}),
		active:     make(map[int]struct{}),
	}
	if s != nil {
		s.bgMu.Lock()
		s.bgWg.Add(1)
		s.bgMu.Unlock()
		go func() {
			defer s.bgWg.Done()
			coordinator.run()
		}()
	} else {
		go coordinator.run()
	}
	return coordinator
}

func (c *prefetchCoordinator) run() {
	defer close(c.done)
	defer c.sub.Close()
	for {
		c.reconcile()
		select {
		case <-c.ctx.Done():
			c.cleanup()
			return
		case <-c.wake:
		case _, ok := <-c.sub.Values:
			if !ok {
				c.cleanup()
				return
			}
		}
	}
}

func (c *prefetchCoordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *prefetchCoordinator) beginForeground(f *raFile, req cache.ReadRequest, plan cache.ReadPlan) *foregroundTicket {
	keys := uniquePieceIndexes(plan.Wanted)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.nextTicket++
	ticketID := c.nextTicket
	if c.shouldAdvanceGenerationLocked(f, req.FileOffset) {
		c.generation++
		c.anchor = prefetchAnchor{
			file:        f,
			fileStart:   req.FileStart,
			fileSize:    req.FileSize,
			pieceLength: req.PieceLength,
			torrentSize: req.TorrentLength,
			cursor:      clampCursor(req.FileOffset, req.FileSize),
		}
		c.hasAnchor = true
		c.cancelStaleLocked(keys)
	}
	if c.hasAnchor {
		c.anchorTicket = ticketID
	}
	ticket := &foregroundTicket{coordinator: c, id: ticketID, generation: c.generation, file: f}
	for _, index := range keys {
		if c.retainLocked(index) {
			ticket.keys = append(ticket.keys, index)
		}
	}
	c.foreground[ticket.id] = append([]int(nil), ticket.keys...)
	c.mu.Unlock()
	c.signal()
	return ticket
}

func (c *prefetchCoordinator) finishForeground(ticket *foregroundTicket, off int64, n int, err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if n > 0 && (err == nil || err == io.EOF) && ticket.generation == c.generation && ticket.id == c.anchorTicket && c.hasAnchor && c.anchor.file == ticket.file {
		c.anchor.cursor = clampCursor(off+int64(n), c.anchor.fileSize)
	}
	for _, index := range c.foreground[ticket.id] {
		c.releaseLocked(index)
	}
	delete(c.foreground, ticket.id)
	c.mu.Unlock()
	c.signal()
}

func (c *prefetchCoordinator) shouldAdvanceGenerationLocked(f *raFile, cursor int64) bool {
	if !c.hasAnchor {
		return true
	}
	if c.anchor.file != f || c.anchor.fileStart != f.fileOffset || c.anchor.fileSize != f.fileSize {
		return true
	}
	if c.anchor.pieceLength <= 0 {
		return true
	}
	oldGlobal := c.anchor.fileStart + c.anchor.cursor
	newGlobal := c.anchor.fileStart + cursor
	if oldGlobal == newGlobal {
		return false
	}
	if newGlobal < oldGlobal {
		return true
	}
	return newGlobal-oldGlobal > defaultPrefetchHigh/2
}

func (c *prefetchCoordinator) cancelStaleLocked(target []int) {
	keep := make(map[int]struct{}, len(target))
	for _, index := range target {
		keep[index] = struct{}{}
	}
	for index := range c.active {
		if _, ok := keep[index]; ok {
			continue
		}
		c.cancelLeaseLocked(index)
	}
	for index := range c.windowPins {
		if _, ok := keep[index]; ok {
			continue
		}
		delete(c.windowPins, index)
		c.releaseLocked(index)
	}
}

func (c *prefetchCoordinator) reconcile() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.hasAnchor {
		return
	}
	c.updateBufferedLocked()
	desired := c.desiredPiecesLocked()
	wanted := make(map[int]struct{}, len(desired))
	for _, index := range desired {
		wanted[index] = struct{}{}
	}
	for index := range c.windowPins {
		if _, ok := wanted[index]; !ok {
			delete(c.windowPins, index)
			c.releaseLocked(index)
		}
	}
	// A forward move within the window can push earlier pieces out of it without
	// changing the generation, so the active set is pruned on every pass rather
	// than only when the window is full.
	c.cancelUnneededActiveLocked(wanted)

	reservedPrefix := int64(0)
	reservationBlocked := false
	for _, index := range desired {
		if _, ok := c.windowPins[index]; !ok {
			if !c.retainLocked(index) {
				reservationBlocked = true
				break
			}
			c.windowPins[index] = struct{}{}
		}
		reservedPrefix += c.pieceOverlapLocked(index)
	}
	if reservationBlocked {
		c.budgetBlocks++
	}

	c.computeWatermarksLocked(reservedPrefix, reservationBlocked)

	// Consumption keeps the byte count a little below the high water, so the
	// window is also "paused" when every piece it wants is already resident:
	// there is nothing left to prefetch even though the count is short of the
	// mark. Without this the state would report filling forever after the first
	// read of a fully buffered window.
	work := false
	for _, index := range desired {
		if !c.cache.Has(c.key(index)) {
			work = true
			break
		}
	}
	if c.effectiveHigh == 0 || c.bufferedBytes >= c.effectiveHigh || !work {
		c.state = prefetchPaused
		c.completeActiveLocked()
		return
	}
	if c.bufferedBytes < c.effectiveLow || c.state == prefetchIdle || c.state == prefetchPaused || c.state == prefetchBudgetBlocked {
		c.state = prefetchFilling
	}
	if c.state != prefetchFilling || reservationBlocked {
		if reservationBlocked {
			c.state = prefetchBudgetBlocked
		}
		return
	}

	for _, index := range desired {
		if c.bufferedBytes >= c.effectiveHigh || len(c.active) >= c.currentLimit() {
			break
		}
		if c.cache.Has(c.key(index)) {
			continue
		}
		if _, ok := c.active[index]; ok {
			c.dedupe++
			continue
		}
		if !c.budget.tryAcquire() {
			c.budgetBlocks++
			c.state = prefetchBudgetBlocked
			break
		}
		c.torrent.Piece(index).UpdateCompletion()
		c.torrent.Piece(index).SetPriority(torrent.PiecePriorityNormal)
		c.active[index] = struct{}{}
		c.priorityAdds++
		if len(c.active) > c.maxActive {
			c.maxActive = len(c.active)
		}
	}
	c.completeActiveLocked()
}

// computeWatermarksLocked derives the effective low/high marks from the
// remaining file bytes and how much of the window the pin budget actually
// admitted. The 1:4 low/high ratio is preserved when the window shrinks, so a
// small file or a tight cache still produces hysteresis instead of churn.
func (c *prefetchCoordinator) computeWatermarksLocked(reservedPrefix int64, reservationBlocked bool) {
	high := min64(defaultPrefetchHigh, c.anchor.fileSize-c.anchor.cursor)
	if high < 0 {
		high = 0
	}
	if reservationBlocked && reservedPrefix < high {
		high = reservedPrefix
	}
	c.effectiveHigh = high
	c.effectiveLow = min64(defaultPrefetchLow, high/4)
}

func (c *prefetchCoordinator) currentLimit() int {
	limit, _ := c.budget.snapshot()
	return limit
}

func (c *prefetchCoordinator) completeActiveLocked() {
	for index := range c.active {
		if !c.cache.Has(c.key(index)) {
			continue
		}
		c.torrent.Piece(index).UpdateCompletion()
		c.torrent.CancelPieces(index, index+1)
		c.priorityCancels++
		delete(c.active, index)
		c.budget.release()
	}
}

func (c *prefetchCoordinator) cancelUnneededActiveLocked(wanted map[int]struct{}) {
	for index := range c.active {
		if _, ok := wanted[index]; ok {
			continue
		}
		c.cancelLeaseLocked(index)
	}
}

func (c *prefetchCoordinator) cancelLeaseLocked(index int) {
	if _, ok := c.active[index]; !ok {
		return
	}
	c.torrent.CancelPieces(index, index+1)
	c.priorityCancels++
	c.cancelled++
	delete(c.active, index)
	c.budget.release()
}

func (c *prefetchCoordinator) desiredPiecesLocked() []int {
	if !c.hasAnchor || c.anchor.pieceLength <= 0 || c.anchor.fileSize <= c.anchor.cursor {
		return nil
	}
	start := c.anchor.fileStart + c.anchor.cursor
	end := start + defaultPrefetchHigh
	fileEnd := c.anchor.fileStart + c.anchor.fileSize
	if end > fileEnd {
		end = fileEnd
	}
	if c.anchor.torrentSize > 0 && end > c.anchor.torrentSize {
		end = c.anchor.torrentSize
	}
	if end <= start {
		return nil
	}
	first := int(start / c.anchor.pieceLength)
	last := int((end-1)/c.anchor.pieceLength) + 1
	out := make([]int, 0, last-first)
	for index := first; index < last; index++ {
		out = append(out, index)
	}
	return out
}

// updateBufferedLocked recomputes the contiguous verified buffer ahead of the
// cursor. It runs on the coordinator goroutine: the read path only moves the
// cursor and signals, so a foreground read never pays for the walk.
func (c *prefetchCoordinator) updateBufferedLocked() {
	if !c.hasAnchor || c.anchor.pieceLength <= 0 {
		c.bufferedBytes = 0
		return
	}
	end := c.anchor.fileStart + c.anchor.fileSize
	if c.anchor.torrentSize > 0 && end > c.anchor.torrentSize {
		end = c.anchor.torrentSize
	}
	limit := defaultPrefetchHigh
	if remaining := end - (c.anchor.fileStart + c.anchor.cursor); remaining < limit {
		limit = remaining
	}
	if limit <= 0 {
		c.bufferedBytes = 0
		return
	}
	c.bufferedBytes = c.cache.ContiguousRun(c.torrentKey, c.anchor.fileStart+c.anchor.cursor, c.anchor.pieceLength, limit)
}

func (c *prefetchCoordinator) pieceOverlapLocked(index int) int64 {
	if !c.hasAnchor || c.anchor.pieceLength <= 0 {
		return 0
	}
	start := int64(index) * c.anchor.pieceLength
	end := start + c.anchor.pieceLength
	fileStart := c.anchor.fileStart + c.anchor.cursor
	fileEnd := c.anchor.fileStart + c.anchor.fileSize
	if start < fileStart {
		start = fileStart
	}
	if end > fileEnd {
		end = fileEnd
	}
	if c.anchor.torrentSize > 0 && end > c.anchor.torrentSize {
		end = c.anchor.torrentSize
	}
	if end <= start {
		return 0
	}
	return end - start
}

func (c *prefetchCoordinator) retainLocked(index int) bool {
	if c.refs[index] == 0 {
		if !c.cache.PinSize(c.key(index), c.pieceSizeLocked(index)) {
			return false
		}
	}
	c.refs[index]++
	return true
}

func (c *prefetchCoordinator) releaseLocked(index int) {
	if c.refs[index] <= 0 {
		return
	}
	c.refs[index]--
	if c.refs[index] == 0 {
		delete(c.refs, index)
		c.cache.Unpin(c.key(index))
	}
}

func (c *prefetchCoordinator) pieceSizeLocked(index int) int64 {
	if c.anchor.pieceLength <= 0 {
		return 0
	}
	start := int64(index) * c.anchor.pieceLength
	remaining := c.anchor.torrentSize - start
	if remaining < c.anchor.pieceLength {
		return remaining
	}
	return c.anchor.pieceLength
}

func (c *prefetchCoordinator) key(index int) cache.Key {
	return cache.Key{Torrent: c.torrentKey, Piece: index}
}

func (c *prefetchCoordinator) releaseFile(f *raFile) {
	c.mu.Lock()
	if !c.closed && c.hasAnchor && c.anchor.file == f {
		c.generation++
		for index := range c.active {
			c.cancelLeaseLocked(index)
		}
		for index := range c.windowPins {
			delete(c.windowPins, index)
			c.releaseLocked(index)
		}
		c.hasAnchor = false
		c.state = prefetchIdle
		c.bufferedBytes = 0
	}
	c.mu.Unlock()
	c.signal()
}

func (c *prefetchCoordinator) snapshot() prefetchSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cursor int64
	if c.hasAnchor {
		cursor = c.anchor.cursor
	}
	var activeBytes int64
	for index := range c.active {
		activeBytes += c.pieceSizeLocked(index)
	}
	return prefetchSnapshot{
		Generation:        c.generation,
		State:             c.state.String(),
		Cursor:            cursor,
		BufferedBytes:     c.bufferedBytes,
		EffectiveLow:      c.effectiveLow,
		EffectiveHigh:     c.effectiveHigh,
		ActivePieces:      len(c.active),
		ActiveBytes:       activeBytes,
		MaxActivePieces:   c.maxActive,
		PriorityAdds:      c.priorityAdds,
		PriorityCancels:   c.priorityCancels,
		DedupeCount:       c.dedupe,
		BudgetBlocks:      c.budgetBlocks,
		CancelledPieces:   c.cancelled,
		ForegroundTickets: len(c.foreground),
		PinnedPieces:      len(c.refs),
	}
}

func (c *prefetchCoordinator) close() {
	c.cancel()
	<-c.done
}

func (c *prefetchCoordinator) cleanup() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for index := range c.active {
		c.torrent.CancelPieces(index, index+1)
		c.priorityCancels++
		c.budget.release()
	}
	c.active = make(map[int]struct{})
	for index := range c.refs {
		c.cache.Unpin(c.key(index))
	}
	c.refs = make(map[int]int)
	c.windowPins = make(map[int]struct{})
	c.foreground = make(map[uint64][]int)
	c.mu.Unlock()
}

func uniquePieceIndexes(indexes []int) []int {
	if len(indexes) < 2 {
		return append([]int(nil), indexes...)
	}
	seen := make(map[int]struct{}, len(indexes))
	out := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		out = append(out, index)
	}
	return out
}

func clampCursor(cursor, size int64) int64 {
	if cursor < 0 {
		return 0
	}
	if cursor > size {
		return size
	}
	return cursor
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

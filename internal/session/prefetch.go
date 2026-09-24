package session

import (
	"context"
	"io"
	"sort"
	"sync"

	"github.com/anacrolix/missinggo/v2/pubsub"
	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

const (
	defaultPrefetchLow           int64 = 16 << 20
	defaultPlaybackWindow        int64 = 32 << 20
	defaultPrefetchHigh                = defaultPlaybackWindow
	defaultSeekClearThreshold    int64 = 64 << 20
	defaultSeekCandidateLocality int64 = 8 << 20
	defaultSeekConfirmationReads       = 2
	defaultPrefetchPieces              = 4
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

type foregroundKind uint8

const (
	foregroundCurrentWindow foregroundKind = iota
	foregroundOnly
	foregroundCandidate
)

type byteSpan struct {
	start int64
	end   int64
}

type seekCandidate struct {
	id                 uint64
	generation         uint64
	file               *raFile
	start              int64
	spans              []byteSpan
	successfulTickets  map[uint64]struct{}
	uniqueBytes        int64
	successfulReadings int
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
	file            *raFile
	fileStart       int64
	fileSize        int64
	pieceLength     int64
	torrentSize     int64
	cursor          int64
	committedCursor int64
}

type foregroundTicket struct {
	coordinator  *prefetchCoordinator
	id           uint64
	generation   uint64
	file         *raFile
	kind         foregroundKind
	candidateID  uint64
	requestStart int64
	requestEnd   int64
	wanted       []int
	pinned       []int
	active       map[int]int
	ctx          context.Context
	cancel       context.CancelFunc
	once         sync.Once
}

func (t *foregroundTicket) finish(off int64, n int, err error) {
	if t == nil || t.coordinator == nil {
		if t != nil && t.cancel != nil {
			t.cancel()
		}
		return
	}
	t.once.Do(func() {
		t.coordinator.finishForeground(t, off, n, err)
	})
}

func (t *foregroundTicket) recordStaleSpan() {
	if t == nil || t.coordinator == nil {
		return
	}
	t.coordinator.mu.Lock()
	t.coordinator.staleSpanRejects++
	t.coordinator.mu.Unlock()
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

	mu              sync.Mutex
	closed          bool
	anchor          prefetchAnchor
	hasAnchor       bool
	anchorConfirmed bool
	generation      uint64
	nextTicket      uint64
	anchorTicket    uint64
	consumed        []byteSpan
	candidate       *seekCandidate
	nextCandidate   uint64

	foreground       map[uint64]*foregroundTicket
	foregroundPieces map[int]int
	refs             map[int]int
	windowPins       map[int]struct{}
	active           map[int]struct{}

	state             prefetchState
	bufferedBytes     int64
	effectiveLow      int64
	effectiveHigh     int64
	maxActive         int
	priorityAdds      uint64
	priorityCancels   uint64
	dedupe            uint64
	budgetBlocks      uint64
	cancelled         uint64
	foregroundCancels uint64
	staleSpanRejects  uint64
}

// prefetchSnapshot is intentionally package-private. export_test.go exposes a
// stable test-only copy without adding a runtime API.
type prefetchSnapshot struct {
	Generation           uint64
	State                string
	Cursor               int64
	PlaybackAnchor       int64
	ConfirmedAnchor      int64
	AnchorConfirmed      bool
	WindowStart          int64
	WindowEnd            int64
	BufferedBytes        int64
	EffectiveLow         int64
	EffectiveHigh        int64
	ActivePieces         int
	ActivePieceIndexes   []int
	ActiveBytes          int64
	MaxActivePieces      int
	PriorityAdds         uint64
	PriorityCancels      uint64
	DedupeCount          uint64
	BudgetBlocks         uint64
	CancelledPieces      uint64
	ForegroundTickets    int
	ForegroundPieces     int
	ForegroundIndexes    []int
	PinnedPieces         int
	ForegroundCancels    uint64
	StaleSpanRejects     uint64
	CandidatePresent     bool
	CandidateID          uint64
	CandidateStart       int64
	CandidateReads       int
	CandidateUniqueBytes int64
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
		torrent:          tor,
		cache:            c,
		budget:           budget,
		torrentKey:       tor.InfoHash().HexString(),
		ctx:              ctx,
		cancel:           cancel,
		sub:              tor.SubscribePieceStateChanges(),
		wake:             make(chan struct{}, 1),
		done:             make(chan struct{}),
		foreground:       make(map[uint64]*foregroundTicket),
		foregroundPieces: make(map[int]int),
		refs:             make(map[int]int),
		windowPins:       make(map[int]struct{}),
		active:           make(map[int]struct{}),
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

func (c *prefetchCoordinator) beginForeground(f *raFile, req cache.ReadRequest, plan cache.ReadPlan, requestCtx context.Context) *foregroundTicket {
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	requestStart, requestEnd := foregroundRequestRange(req, plan)
	if requestStart >= requestEnd {
		return nil
	}
	keys := uniquePieceIndexes(plan.Wanted)
	ticketCtx, ticketCancel := context.WithCancel(requestCtx)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		ticketCancel()
		return nil
	}
	if c.foreground == nil {
		c.foreground = make(map[uint64]*foregroundTicket)
	}
	if c.foregroundPieces == nil {
		c.foregroundPieces = make(map[int]int)
	}

	kind := foregroundOnly
	candidateID := uint64(0)
	if !c.hasAnchor {
		c.generation++
		c.anchor = prefetchAnchor{
			file:            f,
			fileStart:       req.FileStart,
			fileSize:        req.FileSize,
			pieceLength:     req.PieceLength,
			torrentSize:     req.TorrentLength,
			cursor:          clampCursor(requestStart, req.FileSize),
			committedCursor: clampCursor(requestStart, req.FileSize),
		}
		c.hasAnchor = true
		c.anchorConfirmed = false
		c.consumed = nil
		c.candidate = nil
		kind = foregroundCurrentWindow
	} else if c.currentWindowRequestLocked(f, requestStart) {
		kind = foregroundCurrentWindow
	} else if c.candidate != nil && c.candidateMatchesLocked(f, requestStart) {
		kind = foregroundCandidate
		candidateID = c.candidate.id
	} else {
		c.nextCandidate++
		c.candidate = &seekCandidate{
			id:                c.nextCandidate,
			generation:        c.generation,
			file:              f,
			start:             clampCursor(requestStart, req.FileSize),
			successfulTickets: make(map[uint64]struct{}),
		}
		candidateID = c.candidate.id
	}

	c.nextTicket++
	ticket := &foregroundTicket{
		coordinator:  c,
		id:           c.nextTicket,
		generation:   c.generation,
		file:         f,
		kind:         kind,
		candidateID:  candidateID,
		requestStart: requestStart,
		requestEnd:   requestEnd,
		wanted:       append([]int(nil), keys...),
		active:       make(map[int]int),
		ctx:          ticketCtx,
		cancel:       ticketCancel,
	}
	for _, index := range keys {
		if c.retainLocked(index) {
			ticket.pinned = append(ticket.pinned, index)
		}
	}
	c.foreground[ticket.id] = ticket
	c.anchorTicket = ticket.id

	for _, index := range keys {
		if _, ok := c.active[index]; ok {
			c.cancelLeaseLocked(index)
		}
	}
	c.mu.Unlock()

	c.signal()
	return ticket
}

func (c *prefetchCoordinator) finishForeground(ticket *foregroundTicket, off int64, n int, err error) {
	if ticket == nil {
		return
	}
	var cancels []context.CancelFunc
	c.mu.Lock()
	if current, ok := c.foreground[ticket.id]; ok && current == ticket {
		sameGeneration := ticket.generation == c.generation && c.hasAnchor
		successful := n > 0 && (err == nil || err == io.EOF) && sameGeneration
		if successful {
			switch ticket.kind {
			case foregroundCurrentWindow:
				if c.anchor.file == ticket.file {
					c.recordConsumedLocked(off, n)
				}
			case foregroundOnly, foregroundCandidate:
				if c.recordCandidateLocked(ticket, off, n) {
					cancels = append(cancels, c.confirmCandidateLocked(ticket)...)
				}
			}
		}
		if cancel := c.removeForegroundLocked(ticket); cancel != nil {
			cancels = append(cancels, cancel)
		}
		if ticket.kind == foregroundCurrentWindow && !c.anchorConfirmed {
			c.dropUnconfirmedAnchorLocked()
		}
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	c.signal()
}

func (c *prefetchCoordinator) recomputeCursorLocked(f *raFile) {
	if !c.hasAnchor || c.anchor.file != f {
		return
	}
	c.advanceAnchorLocked()
}

func (c *prefetchCoordinator) shouldAdvanceGenerationLocked(f *raFile, requestStart int64) bool {
	if !c.hasAnchor {
		return true
	}
	if c.anchor.file != f || c.anchor.fileStart != f.fileOffset || c.anchor.fileSize != f.fileSize {
		return true
	}
	if c.foregroundPieceForRequestLocked(f, requestStart) {
		return false
	}
	return !c.currentWindowRequestLocked(f, requestStart)
}

func (c *prefetchCoordinator) classificationFloorLocked(f *raFile) int64 {
	if !c.hasAnchor || c.anchor.file != f {
		return 0
	}
	floor := c.anchor.cursor - defaultSeekCandidateLocality
	if floor < 0 {
		return 0
	}
	return floor
}

func (c *prefetchCoordinator) currentWindowRequestLocked(f *raFile, requestStart int64) bool {
	if !c.hasAnchor || c.anchor.file != f || c.anchor.fileStart != f.fileOffset || c.anchor.fileSize != f.fileSize {
		return false
	}
	lower := classificationFloorLocked(c.anchor.cursor)
	upper := playbackWindowEnd(c.anchor.cursor, c.anchor.fileSize)
	return requestStart >= lower && requestStart < upper
}

func classificationFloorLocked(cursor int64) int64 {
	floor := cursor - defaultSeekCandidateLocality
	if floor < 0 {
		return 0
	}
	return floor
}

func (c *prefetchCoordinator) candidateMatchesLocked(f *raFile, requestStart int64) bool {
	candidate := c.candidate
	if candidate == nil || candidate.file != f || candidate.generation != c.generation {
		return false
	}
	return absInt64(requestStart-candidate.start) <= defaultSeekCandidateLocality
}

func absInt64(value int64) int64 {
	if value < 0 {
		if value == -1<<63 {
			return 1<<63 - 1
		}
		return -value
	}
	return value
}

func clampReadSpan(off int64, n int, size int64) (int64, int64) {
	if n <= 0 || size <= 0 {
		return 0, 0
	}
	start := clampCursor(off, size)
	end := off + int64(n)
	if end < off {
		end = size
	}
	end = clampCursor(end, size)
	if end <= start {
		return 0, 0
	}
	return start, end
}

func byteSpanBytes(spans []byteSpan) int64 {
	var total int64
	for _, span := range spans {
		if span.end > span.start {
			total += span.end - span.start
		}
	}
	return total
}

func mergeByteSpan(spans []byteSpan, added byteSpan) ([]byteSpan, int64) {
	if added.end <= added.start {
		return spans, 0
	}
	before := byteSpanBytes(spans)
	all := make([]byteSpan, 0, len(spans)+1)
	all = append(all, spans...)
	all = append(all, added)
	sort.Slice(all, func(i, j int) bool {
		if all[i].start == all[j].start {
			return all[i].end < all[j].end
		}
		return all[i].start < all[j].start
	})
	merged := make([]byteSpan, 0, len(all))
	for _, span := range all {
		if span.end <= span.start {
			continue
		}
		if len(merged) == 0 || span.start > merged[len(merged)-1].end {
			merged = append(merged, span)
			continue
		}
		if span.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = span.end
		}
	}
	return merged, byteSpanBytes(merged) - before
}

func (c *prefetchCoordinator) recordConsumedLocked(off int64, n int) {
	if !c.hasAnchor {
		return
	}
	start, end := clampReadSpan(off, n, c.anchor.fileSize)
	if end <= start {
		return
	}
	previous := c.anchor.cursor
	c.consumed, _ = mergeByteSpan(c.consumed, byteSpan{start: start, end: end})
	c.advanceAnchorLocked()
	if c.anchor.cursor > previous {
		c.anchorConfirmed = true
	}
}

func (c *prefetchCoordinator) advanceAnchorLocked() {
	if !c.hasAnchor {
		return
	}
	cursor := c.anchor.cursor
	remaining := make([]byteSpan, 0, len(c.consumed))
	for _, span := range c.consumed {
		if span.end <= cursor {
			continue
		}
		if span.start <= cursor {
			cursor = span.end
			continue
		}
		remaining = append(remaining, span)
	}
	c.consumed = remaining
	c.anchor.cursor = clampCursor(cursor, c.anchor.fileSize)
	c.anchor.committedCursor = c.anchor.cursor
	if c.candidate != nil && absInt64(c.anchor.cursor-c.candidate.start) > defaultSeekClearThreshold {
		c.candidate = nil
	}
}

func (c *prefetchCoordinator) recordCandidateLocked(ticket *foregroundTicket, off int64, n int) bool {
	candidate := c.candidate
	if candidate == nil || ticket.candidateID != candidate.id || ticket.generation != candidate.generation || ticket.file != candidate.file {
		return false
	}
	start, end := clampReadSpan(off, n, candidate.file.fileSize)
	if end <= start {
		return false
	}
	if candidate.successfulTickets == nil {
		candidate.successfulTickets = make(map[uint64]struct{})
	}
	if _, seen := candidate.successfulTickets[ticket.id]; seen {
		return false
	}
	var added int64
	candidate.spans, added = mergeByteSpan(candidate.spans, byteSpan{start: start, end: end})
	if added <= 0 {
		return false
	}
	candidate.successfulTickets[ticket.id] = struct{}{}
	candidate.uniqueBytes += added
	candidate.successfulReadings++
	return candidate.successfulReadings >= defaultSeekConfirmationReads
}

func (c *prefetchCoordinator) confirmCandidateLocked(ticket *foregroundTicket) []context.CancelFunc {
	candidate := c.candidate
	if candidate == nil || ticket.candidateID != candidate.id || ticket.file != candidate.file {
		return nil
	}
	file := candidate.file
	c.generation++
	c.anchor = prefetchAnchor{
		file:            file,
		fileStart:       file.fileOffset,
		fileSize:        file.fileSize,
		pieceLength:     file.pieceLength,
		torrentSize:     file.torrentSize,
		cursor:          clampCursor(candidate.start, file.fileSize),
		committedCursor: clampCursor(candidate.start, file.fileSize),
	}
	c.hasAnchor = true
	c.anchorConfirmed = true
	c.consumed = append([]byteSpan(nil), candidate.spans...)
	c.candidate = nil
	c.advanceAnchorLocked()
	c.state = prefetchIdle
	c.bufferedBytes = 0

	var cancels []context.CancelFunc
	for id, old := range c.foreground {
		if id == ticket.id {
			continue
		}
		if cancel := c.removeForegroundLocked(old); cancel != nil {
			cancels = append(cancels, cancel)
			c.foregroundCancels++
		}
	}
	for index := range c.windowPins {
		delete(c.windowPins, index)
		c.releaseLocked(index)
	}
	for index := range c.active {
		c.cancelLeaseLocked(index)
	}
	ticket.generation = c.generation
	ticket.kind = foregroundCurrentWindow
	ticket.candidateID = 0
	c.anchorTicket = ticket.id
	return cancels
}

func (c *prefetchCoordinator) foregroundPieceForRequestLocked(f *raFile, requestStart int64) bool {
	if c.anchor.pieceLength <= 0 || requestStart < 0 {
		return false
	}
	global := f.fileOffset + requestStart
	if global < f.fileOffset || global < 0 {
		return false
	}
	_, ok := c.foregroundPieces[int(global/c.anchor.pieceLength)]
	return ok
}

func (t *foregroundTicket) startPiece(index int) {
	if t == nil || t.coordinator == nil {
		return
	}
	coordinator := t.coordinator
	coordinator.mu.Lock()
	if current := coordinator.foreground[t.id]; current == t {
		if _, ok := coordinator.active[index]; ok {
			coordinator.cancelLeaseLocked(index)
		}
		if t.active == nil {
			t.active = make(map[int]int)
		}
		t.active[index]++
		coordinator.foregroundPieces[index]++
	}
	coordinator.mu.Unlock()
	coordinator.signal()
}

func (t *foregroundTicket) finishPiece(index int) {
	if t == nil || t.coordinator == nil {
		return
	}
	coordinator := t.coordinator
	coordinator.mu.Lock()
	if current := coordinator.foreground[t.id]; current == t {
		coordinator.releaseForegroundPieceLocked(t, index)
	}
	coordinator.mu.Unlock()
	coordinator.signal()
}

func (c *prefetchCoordinator) releaseForegroundPieceLocked(ticket *foregroundTicket, index int) {
	if ticket.active[index] <= 0 {
		return
	}
	if ticket.active[index] == 1 {
		delete(ticket.active, index)
	} else {
		ticket.active[index]--
	}
	if count := c.foregroundPieces[index]; count > 1 {
		c.foregroundPieces[index] = count - 1
	} else {
		delete(c.foregroundPieces, index)
	}
}

func (c *prefetchCoordinator) removeForegroundLocked(ticket *foregroundTicket) context.CancelFunc {
	current, ok := c.foreground[ticket.id]
	if !ok || current != ticket {
		return nil
	}
	delete(c.foreground, ticket.id)
	for index, count := range ticket.active {
		for i := 0; i < count; i++ {
			c.releaseForegroundPieceLocked(ticket, index)
		}
	}
	for _, index := range ticket.pinned {
		c.releaseLocked(index)
	}
	return ticket.cancel
}

func foregroundLeaseWithin(ticket *foregroundTicket, desired map[int]struct{}) bool {
	for _, index := range ticket.wanted {
		if _, ok := desired[index]; !ok {
			return false
		}
	}
	return true
}

func (c *prefetchCoordinator) dropUnconfirmedAnchorLocked() {
	for _, ticket := range c.foreground {
		if ticket.generation == c.generation && ticket.kind == foregroundCurrentWindow {
			return
		}
	}
	for index := range c.active {
		c.cancelLeaseLocked(index)
	}
	for index := range c.windowPins {
		delete(c.windowPins, index)
		c.releaseLocked(index)
	}
	c.hasAnchor = false
	c.anchorConfirmed = false
	c.consumed = nil
	c.candidate = nil
	c.state = prefetchIdle
	c.bufferedBytes = 0
}

func foregroundRequestRange(req cache.ReadRequest, plan cache.ReadPlan) (int64, int64) {
	if len(plan.Spans) == 0 {
		return 0, 0
	}
	start := clampCursor(req.FileOffset, req.FileSize)
	end := start
	for _, span := range plan.Spans {
		end += span.Length
	}
	if end < start || end > req.FileSize {
		end = req.FileSize
	}
	return start, end
}

func playbackWindowEnd(start, fileSize int64) int64 {
	if start < 0 {
		start = 0
	}
	if start >= fileSize {
		return fileSize
	}
	end := start + defaultPlaybackWindow
	if end < start || end > fileSize {
		return fileSize
	}
	return end
}

func makePieceSet(indexes []int) map[int]struct{} {
	set := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		set[index] = struct{}{}
	}
	return set
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
		if c.cache.Has(c.key(index)) || c.foregroundPieces[index] > 0 {
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
// admitted. The 1:2 low/high ratio is preserved when the window shrinks, so a
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
	c.effectiveLow = min64(defaultPrefetchLow, high/2)
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
		if c.torrent != nil {
			c.torrent.Piece(index).UpdateCompletion()
			c.torrent.CancelPieces(index, index+1)
		}
		c.priorityCancels++
		delete(c.active, index)
		c.budget.release()
	}
}

func (c *prefetchCoordinator) cancelUnneededActiveLocked(wanted map[int]struct{}) {
	for index := range c.active {
		if _, ok := wanted[index]; ok && c.foregroundPieces[index] == 0 {
			continue
		}
		c.cancelLeaseLocked(index)
	}
}

func (c *prefetchCoordinator) cancelLeaseLocked(index int) {
	if _, ok := c.active[index]; !ok {
		return
	}
	if c.torrent != nil {
		c.torrent.CancelPieces(index, index+1)
	}
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
	var cancels []context.CancelFunc
	c.mu.Lock()
	if !c.closed {
		for _, ticket := range c.foreground {
			if ticket.file != f {
				continue
			}
			if cancel := c.removeForegroundLocked(ticket); cancel != nil {
				cancels = append(cancels, cancel)
			}
		}
		if c.candidate != nil && c.candidate.file == f {
			c.candidate = nil
		}
		if c.hasAnchor && c.anchor.file == f {
			c.generation++
			for index := range c.active {
				c.cancelLeaseLocked(index)
			}
			for index := range c.windowPins {
				delete(c.windowPins, index)
				c.releaseLocked(index)
			}
			c.hasAnchor = false
			c.anchorConfirmed = false
			c.consumed = nil
			c.state = prefetchIdle
			c.bufferedBytes = 0
			c.candidate = nil
		}
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	c.signal()
}

func (c *prefetchCoordinator) snapshot() prefetchSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cursor, windowStart, windowEnd int64
	if c.hasAnchor {
		cursor = c.anchor.cursor
		windowStart, windowEnd = c.windowBoundsLocked()
	}
	confirmedAnchor := int64(0)
	if c.anchorConfirmed {
		confirmedAnchor = cursor
	}
	activeIndexes := make([]int, 0, len(c.active))
	var activeBytes int64
	for index := range c.active {
		activeIndexes = append(activeIndexes, index)
		activeBytes += c.pieceSizeLocked(index)
	}
	foregroundIndexes := make([]int, 0, len(c.foregroundPieces))
	for index := range c.foregroundPieces {
		foregroundIndexes = append(foregroundIndexes, index)
	}
	sort.Ints(activeIndexes)
	sort.Ints(foregroundIndexes)
	snapshot := prefetchSnapshot{
		Generation:         c.generation,
		State:              c.state.String(),
		Cursor:             cursor,
		PlaybackAnchor:     confirmedAnchor,
		ConfirmedAnchor:    confirmedAnchor,
		AnchorConfirmed:    c.anchorConfirmed,
		WindowStart:        windowStart,
		WindowEnd:          windowEnd,
		BufferedBytes:      c.bufferedBytes,
		EffectiveLow:       c.effectiveLow,
		EffectiveHigh:      c.effectiveHigh,
		ActivePieces:       len(c.active),
		ActivePieceIndexes: activeIndexes,
		ActiveBytes:        activeBytes,
		MaxActivePieces:    c.maxActive,
		PriorityAdds:       c.priorityAdds,
		PriorityCancels:    c.priorityCancels,
		DedupeCount:        c.dedupe,
		BudgetBlocks:       c.budgetBlocks,
		CancelledPieces:    c.cancelled,
		ForegroundTickets:  len(c.foreground),
		ForegroundPieces:   len(c.foregroundPieces),
		ForegroundIndexes:  foregroundIndexes,
		PinnedPieces:       len(c.refs),
		ForegroundCancels:  c.foregroundCancels,
		StaleSpanRejects:   c.staleSpanRejects,
	}
	if c.candidate != nil {
		snapshot.CandidatePresent = true
		snapshot.CandidateID = c.candidate.id
		snapshot.CandidateStart = c.candidate.start
		snapshot.CandidateReads = c.candidate.successfulReadings
		snapshot.CandidateUniqueBytes = c.candidate.uniqueBytes
	}
	return snapshot
}

func (c *prefetchCoordinator) windowBoundsLocked() (int64, int64) {
	if !c.hasAnchor {
		return 0, 0
	}
	start := clampCursor(c.anchor.cursor, c.anchor.fileSize)
	end := playbackWindowEnd(start, c.anchor.fileSize)
	if c.anchor.pieceLength <= 0 {
		return start, end
	}
	globalStart := c.anchor.fileStart + start
	alignedStart := globalStart - globalStart%c.anchor.pieceLength
	start = clampCursor(alignedStart-c.anchor.fileStart, c.anchor.fileSize)
	globalEnd := c.anchor.fileStart + end
	if remainder := globalEnd % c.anchor.pieceLength; remainder != 0 {
		globalEnd += c.anchor.pieceLength - remainder
	}
	end = clampCursor(globalEnd-c.anchor.fileStart, c.anchor.fileSize)
	return start, end
}

func (c *prefetchCoordinator) close() {
	c.cancel()
	<-c.done
}

func (c *prefetchCoordinator) cleanup() {
	var cancels []context.CancelFunc
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, ticket := range c.foreground {
		if cancel := c.removeForegroundLocked(ticket); cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	for index := range c.active {
		if c.torrent != nil {
			c.torrent.CancelPieces(index, index+1)
		}
		c.priorityCancels++
		c.budget.release()
	}
	c.active = make(map[int]struct{})
	for index := range c.refs {
		c.cache.Unpin(c.key(index))
	}
	c.refs = make(map[int]int)
	c.windowPins = make(map[int]struct{})
	c.foreground = make(map[uint64]*foregroundTicket)
	c.foregroundPieces = make(map[int]int)
	c.consumed = nil
	c.candidate = nil
	c.hasAnchor = false
	c.anchorConfirmed = false
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
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

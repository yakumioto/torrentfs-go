package session

import (
	"context"
	"io"
	"sort"
	"sync"
	"time"

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

// prefetchAnchor is the degraded demand-only compatibility window.
type prefetchAnchor struct {
	file            *raFile
	fileStart       int64
	fileSize        int64
	pieceLength     int64
	torrentSize     int64
	cursor          int64
	committedCursor int64
}

type playbackStreamState struct {
	id                    string
	path                  string
	file                  *raFile
	fileStart             int64
	fileSize              int64
	pieceLength           int64
	torrentSize           int64
	cursor                int64
	generation            uint64
	lastSequence          uint64
	lastEvent             string
	lastPosition          int64
	lastUpdate            time.Time
	expiresAt             time.Time
	playbackConsumedBytes int64
	usefulDownloadBase    int64
	bufferedBytes         int64
	targetLow             int64
	targetHigh            int64
	effectiveLow          int64
	effectiveHigh         int64
	state                 prefetchState
	owners                map[int]struct{}
	pins                  map[int]struct{}
	onExpire              func()
}

type demandState struct {
	id            uint64
	file          *raFile
	fileStart     int64
	fileSize      int64
	pieceLength   int64
	torrentSize   int64
	cursor        int64
	hasCursor     bool
	generation    uint64
	confirmed     bool
	spans         []byteSpan
	candidate     *seekCandidate
	nextCandidate uint64
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
	demandID     uint64
	demand       *demandState
	legacy       bool
	wanted       []int
	pinned       []int
	activePinned map[int]int
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

	mu               sync.Mutex
	closed           bool
	demandOnly       bool
	anchor           prefetchAnchor
	hasAnchor        bool
	anchorConfirmed  bool
	generation       uint64
	streams          map[string]*playbackStreamState
	normalOwners     map[int]map[string]struct{}
	demands          map[uint64]*demandState
	demandByFile     map[*raFile]*demandState
	demandCursors    map[uint64]int64
	expiredCallbacks []func()
	now              func() time.Time
	nextTicket       uint64
	anchorTicket     uint64
	demandSpans      []byteSpan
	candidate        *seekCandidate
	nextCandidate    uint64

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
	DemandCursor         int64
	PlaybackCursor       int64
	PlaybackStreams      []PlaybackStreamSnapshot
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
		streams:          make(map[string]*playbackStreamState),
		normalOwners:     make(map[int]map[string]struct{}),
		demands:          make(map[uint64]*demandState),
		demandByFile:     make(map[*raFile]*demandState),
		demandCursors:    make(map[uint64]int64),
		now:              time.Now,
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		c.reconcile()
		select {
		case <-c.ctx.Done():
			c.cleanup()
			return
		case <-c.wake:
		case <-ticker.C:
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

func (c *prefetchCoordinator) coordinatorNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *prefetchCoordinator) ensurePlaybackMapsLocked() {
	if c.streams == nil {
		c.streams = make(map[string]*playbackStreamState)
	}
	if c.normalOwners == nil {
		c.normalOwners = make(map[int]map[string]struct{})
	}
}

func (c *prefetchCoordinator) startPlaybackStream(id, path string, f *raFile, position int64, onExpire ...func()) (PlaybackStreamSnapshot, error) {
	if f == nil || position < 0 || position > f.fileSize {
		return PlaybackStreamSnapshot{}, ErrPlaybackPositionInvalid
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return PlaybackStreamSnapshot{}, ErrPlaybackStreamNotFound
	}
	c.ensurePlaybackMapsLocked()
	if _, exists := c.streams[id]; exists {
		c.mu.Unlock()
		return PlaybackStreamSnapshot{}, ErrPlaybackSequenceConflict
	}
	if len(c.streams) == 0 {
		c.clearLegacyLocked()
	}
	var usefulDownloadBase int64
	if c.torrent != nil {
		stats := c.torrent.Stats()
		usefulDownloadBase = stats.ConnStats.BytesReadUsefulData.Int64()
	}
	now := c.coordinatorNow()
	stream := &playbackStreamState{
		id:                 id,
		path:               path,
		file:               f,
		fileStart:          f.fileOffset,
		fileSize:           f.fileSize,
		pieceLength:        f.pieceLength,
		torrentSize:        f.torrentSize,
		cursor:             clampCursor(position, f.fileSize),
		generation:         1,
		lastPosition:       clampCursor(position, f.fileSize),
		lastUpdate:         now,
		expiresAt:          now.Add(playbackLeaseDuration),
		targetLow:          defaultPrefetchLow,
		targetHigh:         defaultPrefetchHigh,
		usefulDownloadBase: usefulDownloadBase,
		state:              prefetchFilling,
		owners:             make(map[int]struct{}),
		pins:               make(map[int]struct{}),
	}
	stream.lastEvent = "start"
	if len(onExpire) > 0 {
		stream.onExpire = onExpire[0]
	}
	c.streams[id] = stream
	c.reconcilePlaybackStreamLocked(stream)
	snapshot := c.playbackSnapshotLocked(stream)
	c.mu.Unlock()
	c.signal()
	return snapshot, nil
}

func (c *prefetchCoordinator) updatePlaybackStream(id string, request PlaybackStreamUpdate) (PlaybackStreamSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stream := c.streams[id]
	if stream == nil {
		return PlaybackStreamSnapshot{}, ErrPlaybackStreamNotFound
	}
	if request.Sequence == 0 {
		return PlaybackStreamSnapshot{}, ErrPlaybackSequenceConflict
	}
	if request.Event != PlaybackEventProgress && request.Event != PlaybackEventSeek {
		return PlaybackStreamSnapshot{}, ErrPlaybackEventInvalid
	}
	if request.PositionBytes < 0 || request.PositionBytes > stream.fileSize {
		return PlaybackStreamSnapshot{}, ErrPlaybackPositionInvalid
	}
	if request.Sequence < stream.lastSequence {
		return PlaybackStreamSnapshot{}, ErrPlaybackSequenceConflict
	}
	if request.Sequence == stream.lastSequence {
		if stream.lastEvent == request.Event && stream.lastPosition == request.PositionBytes {
			return c.playbackSnapshotLocked(stream), nil
		}
		return PlaybackStreamSnapshot{}, ErrPlaybackSequenceConflict
	}
	if request.Event == PlaybackEventProgress && request.PositionBytes < stream.cursor {
		return PlaybackStreamSnapshot{}, ErrPlaybackPositionInvalid
	}
	if request.Event == PlaybackEventSeek {
		c.releasePlaybackResourcesLocked(stream)
		stream.generation++
		stream.cursor = request.PositionBytes
		stream.bufferedBytes = 0
		stream.effectiveHigh = 0
		stream.effectiveLow = 0
		stream.state = prefetchFilling
	} else {
		stream.playbackConsumedBytes += request.PositionBytes - stream.cursor
		stream.cursor = request.PositionBytes
	}
	now := c.coordinatorNow()
	stream.lastSequence = request.Sequence
	stream.lastEvent = request.Event
	stream.lastPosition = request.PositionBytes
	stream.lastUpdate = now
	stream.expiresAt = now.Add(playbackLeaseDuration)
	c.reconcilePlaybackStreamLocked(stream)
	c.signal()
	return c.playbackSnapshotLocked(stream), nil
}

func (c *prefetchCoordinator) stopPlaybackStream(id string) error {
	c.mu.Lock()
	stream := c.streams[id]
	if stream == nil {
		c.mu.Unlock()
		return ErrPlaybackStreamNotFound
	}
	c.releasePlaybackResourcesLocked(stream)
	delete(c.streams, id)
	c.mu.Unlock()
	c.signal()
	return nil
}

func (c *prefetchCoordinator) playbackSnapshots() []PlaybackStreamSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PlaybackStreamSnapshot, 0, len(c.streams))
	for _, stream := range c.streams {
		out = append(out, c.playbackSnapshotLocked(stream))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *prefetchCoordinator) playbackSnapshotLocked(stream *playbackStreamState) PlaybackStreamSnapshot {
	background := make([]int, 0, len(stream.owners))
	for index := range stream.owners {
		background = append(background, index)
	}
	pinned := make([]int, 0, len(stream.pins))
	for index := range stream.pins {
		pinned = append(pinned, index)
	}
	foregroundSet := make(map[int]struct{})
	for _, ticket := range c.foreground {
		if ticket.file != stream.file {
			continue
		}
		for index := range ticket.active {
			foregroundSet[index] = struct{}{}
		}
	}
	foreground := make([]int, 0, len(foregroundSet))
	for index := range foregroundSet {
		foreground = append(foreground, index)
	}
	sort.Ints(background)
	sort.Ints(pinned)
	sort.Ints(foreground)
	torrentID := ""
	var usefulDownload int64
	if c.torrent != nil {
		torrentID = c.torrent.InfoHash().HexString()
		stats := c.torrent.Stats()
		usefulDownload = stats.ConnStats.BytesReadUsefulData.Int64() - stream.usefulDownloadBase
		if usefulDownload < 0 {
			usefulDownload = 0
		}
	}
	cacheResident := int64(0)
	if c.cache != nil {
		cacheResident = c.cache.SizeOf(c.torrentKey)
	}
	return PlaybackStreamSnapshot{
		ID:                    stream.id,
		TorrentID:             torrentID,
		Path:                  stream.path,
		Generation:            stream.generation,
		PlaybackCursor:        stream.cursor,
		PlaybackConsumedBytes: stream.playbackConsumedBytes,
		UsefulDownloadBytes:   usefulDownload,
		CacheResidentBytes:    cacheResident,
		BufferedBytes:         stream.bufferedBytes,
		TargetLow:             stream.targetLow,
		TargetHigh:            stream.targetHigh,
		EffectiveLow:          stream.effectiveLow,
		EffectiveHigh:         stream.effectiveHigh,
		State:                 stream.state.String(),
		LastSequence:          stream.lastSequence,
		LastUpdate:            stream.lastUpdate,
		ExpiresAt:             stream.expiresAt,
		ForegroundPieces:      foreground,
		BackgroundPieces:      background,
		PinnedPieces:          pinned,
	}
}

func (c *prefetchCoordinator) expirePlaybackStreamsLocked(now time.Time) {
	for id, stream := range c.streams {
		if now.Before(stream.expiresAt) {
			continue
		}
		c.releasePlaybackResourcesLocked(stream)
		delete(c.streams, id)
		if stream.onExpire != nil {
			c.expiredCallbacks = append(c.expiredCallbacks, stream.onExpire)
		}
	}
}

func (c *prefetchCoordinator) releasePlaybackResourcesLocked(stream *playbackStreamState) {
	for index := range stream.owners {
		c.removeNormalOwnerLocked(index, stream.id)
	}
	for index := range stream.pins {
		delete(stream.pins, index)
		c.releaseLocked(index)
	}
	stream.owners = make(map[int]struct{})
	stream.pins = make(map[int]struct{})
}

func (c *prefetchCoordinator) removeNormalOwnerLocked(index int, streamID string) {
	owners := c.normalOwners[index]
	if len(owners) == 0 {
		return
	}
	delete(owners, streamID)
	if len(owners) == 0 {
		delete(c.normalOwners, index)
	}
	if len(owners) == 0 && c.foregroundPieces[index] == 0 {
		c.cancelLeaseLocked(index)
	}
}

func (c *prefetchCoordinator) clearLegacyLocked() {
	for index := range c.active {
		c.cancelLeaseLocked(index)
	}
	for index := range c.windowPins {
		delete(c.windowPins, index)
		c.releaseLocked(index)
	}
	c.hasAnchor = false
	c.anchorConfirmed = false
	c.candidate = nil
	c.demandSpans = nil
	c.state = prefetchIdle
	c.bufferedBytes = 0
}

func (c *prefetchCoordinator) demandForLocked(f *raFile, id uint64) *demandState {
	if c.demands == nil {
		c.demands = make(map[uint64]*demandState)
	}
	if c.demandByFile == nil {
		c.demandByFile = make(map[*raFile]*demandState)
	}
	if id == 0 {
		if state := c.demandByFile[f]; state != nil {
			return state
		}
	} else if state := c.demands[id]; state != nil {
		return state
	}
	state := &demandState{id: id, file: f, generation: 1}
	if f != nil {
		state.fileStart = f.fileOffset
		state.fileSize = f.fileSize
		state.pieceLength = f.pieceLength
		state.torrentSize = f.torrentSize
	}
	if id == 0 {
		c.demandByFile[f] = state
	} else {
		c.demands[id] = state
	}
	return state
}

func demandCurrentWindowLocked(state *demandState, requestStart int64) bool {
	if state == nil || !state.hasCursor || requestStart < 0 {
		return false
	}
	lower := state.cursor - defaultSeekCandidateLocality
	if lower < 0 {
		lower = 0
	}
	return requestStart >= lower && requestStart < playbackWindowEnd(state.cursor, state.fileSize)
}

func (c *prefetchCoordinator) demandCandidateMatchesLocked(state *demandState, f *raFile, requestStart int64) bool {
	if state == nil || state.candidate == nil || state.file != f {
		return false
	}
	return absInt64(requestStart-state.candidate.start) <= defaultSeekCandidateLocality
}

func recordDemandForStateLocked(state *demandState, off int64, n int) {
	if state == nil {
		return
	}
	start, end := clampReadSpan(off, n, state.fileSize)
	if end <= start {
		return
	}
	state.spans, _ = mergeByteSpan(state.spans, byteSpan{start: start, end: end})
	cursor := state.cursor
	remaining := make([]byteSpan, 0, len(state.spans))
	for _, span := range state.spans {
		if span.end <= cursor {
			continue
		}
		if span.start <= cursor {
			cursor = span.end
			continue
		}
		remaining = append(remaining, span)
	}
	state.spans = remaining
	state.cursor = clampCursor(cursor, state.fileSize)
	state.hasCursor = true
	state.confirmed = true
}

func (c *prefetchCoordinator) recordDemandCandidateLocked(state *demandState, ticket *foregroundTicket, off int64, n int) bool {
	candidate := state.candidate
	if candidate == nil || ticket.demand != state || ticket.candidateID != candidate.id || ticket.generation != state.generation {
		return false
	}
	start, end := clampReadSpan(off, n, state.fileSize)
	if end <= start {
		return false
	}
	if candidate.successfulTickets == nil {
		candidate.successfulTickets = make(map[uint64]struct{})
	}
	if _, seen := candidate.successfulTickets[ticket.id]; seen {
		return false
	}
	spans, added := mergeByteSpan(candidate.spans, byteSpan{start: start, end: end})
	if added <= 0 {
		return false
	}
	candidate.spans = spans
	candidate.successfulTickets[ticket.id] = struct{}{}
	candidate.uniqueBytes += added
	candidate.successfulReadings++
	return candidate.successfulReadings >= defaultSeekConfirmationReads
}

func (c *prefetchCoordinator) confirmDemandCandidateLocked(state *demandState, ticket *foregroundTicket) []context.CancelFunc {
	candidate := state.candidate
	if candidate == nil || ticket.demand != state || ticket.candidateID != candidate.id {
		return nil
	}
	state.generation++
	state.cursor = clampCursor(candidate.start, state.fileSize)
	state.hasCursor = true
	state.confirmed = true
	state.spans = append([]byteSpan(nil), candidate.spans...)
	state.candidate = nil
	var cancels []context.CancelFunc
	for id, old := range c.foreground {
		if id == ticket.id || old.demand != state {
			continue
		}
		if cancel := c.removeForegroundLocked(old); cancel != nil {
			cancels = append(cancels, cancel)
			c.foregroundCancels++
		}
	}
	ticket.generation = state.generation
	ticket.kind = foregroundCurrentWindow
	ticket.candidateID = 0
	return cancels
}

func (c *prefetchCoordinator) dropDemandLocked(state *demandState) {
	if state == nil {
		return
	}
	for _, ticket := range c.foreground {
		if ticket.demand == state && ticket.kind == foregroundCurrentWindow {
			return
		}
	}
	state.candidate = nil
	state.spans = nil
	state.hasCursor = false
	state.confirmed = false
}

func (c *prefetchCoordinator) beginForeground(f *raFile, req cache.ReadRequest, plan cache.ReadPlan, requestCtx context.Context, demandIDs ...uint64) *foregroundTicket {
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
	if c.demandCursors == nil {
		c.demandCursors = make(map[uint64]int64)
	}

	var demandID uint64
	if len(demandIDs) > 0 {
		demandID = demandIDs[0]
	}
	legacy := !c.demandOnly && c.torrent == nil && len(c.streams) == 0
	kind := foregroundOnly
	candidateID := uint64(0)
	var demand *demandState
	if legacy {
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
			c.demandSpans = nil
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
	} else {
		demand = c.demandForLocked(f, demandID)
		if demandID != 0 {
			c.demandCursors[demandID] = requestStart
		}
		if !demand.hasCursor {
			demand.cursor = clampCursor(requestStart, req.FileSize)
			demand.hasCursor = true
			kind = foregroundCurrentWindow
		} else if demandCurrentWindowLocked(demand, requestStart) {
			kind = foregroundCurrentWindow
		} else if c.demandCandidateMatchesLocked(demand, f, requestStart) {
			kind = foregroundCandidate
			candidateID = demand.candidate.id
		} else {
			demand.nextCandidate++
			demand.candidate = &seekCandidate{
				id:                demand.nextCandidate,
				generation:        demand.generation,
				file:              f,
				start:             clampCursor(requestStart, req.FileSize),
				successfulTickets: make(map[uint64]struct{}),
			}
			candidateID = demand.candidate.id
		}
	}

	c.nextTicket++
	generation := c.generation
	if demand != nil {
		generation = demand.generation
	}
	ticket := &foregroundTicket{
		coordinator:  c,
		id:           c.nextTicket,
		generation:   generation,
		file:         f,
		demandID:     demandID,
		demand:       demand,
		legacy:       legacy,
		kind:         kind,
		candidateID:  candidateID,
		requestStart: requestStart,
		requestEnd:   requestEnd,
		wanted:       append([]int(nil), keys...),
		activePinned: make(map[int]int),
		active:       make(map[int]int),
		ctx:          ticketCtx,
		cancel:       ticketCancel,
	}
	if legacy {
		for _, index := range keys {
			if c.retainLocked(index) {
				ticket.pinned = append(ticket.pinned, index)
			}
		}
	}
	c.foreground[ticket.id] = ticket
	if legacy {
		c.anchorTicket = ticket.id
		for _, index := range keys {
			if _, ok := c.active[index]; ok {
				c.cancelLeaseLocked(index)
			}
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
		legacy := ticket.legacy || (ticket.demand == nil && c.torrent == nil)
		sameGeneration := false
		if legacy {
			sameGeneration = ticket.generation == c.generation && c.hasAnchor
		} else if ticket.demand != nil {
			sameGeneration = ticket.generation == ticket.demand.generation
		}
		successful := n > 0 && (err == nil || err == io.EOF) && sameGeneration
		if successful {
			if legacy {
				switch ticket.kind {
				case foregroundCurrentWindow:
					if c.anchor.file == ticket.file {
						c.recordDemandLocked(off, n)
					}
				case foregroundOnly, foregroundCandidate:
					if c.recordCandidateLocked(ticket, off, n) {
						cancels = append(cancels, c.confirmCandidateLocked(ticket)...)
					}
				}
			} else if ticket.kind == foregroundCurrentWindow {
				recordDemandForStateLocked(ticket.demand, off, n)
			} else if c.recordDemandCandidateLocked(ticket.demand, ticket, off, n) {
				cancels = append(cancels, c.confirmDemandCandidateLocked(ticket.demand, ticket)...)
			}
			if ticket.demandID != 0 {
				c.demandCursors[ticket.demandID] = ticket.demand.cursor
			}
		}
		if cancel := c.removeForegroundLocked(ticket); cancel != nil {
			cancels = append(cancels, cancel)
		}
		if legacy {
			if ticket.kind == foregroundCurrentWindow && !c.anchorConfirmed {
				c.dropUnconfirmedAnchorLocked()
			}
		} else if ticket.kind == foregroundCurrentWindow && ticket.demand != nil && !ticket.demand.confirmed {
			c.dropDemandLocked(ticket.demand)
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

func (c *prefetchCoordinator) recordDemandLocked(off int64, n int) {
	if !c.hasAnchor {
		return
	}
	start, end := clampReadSpan(off, n, c.anchor.fileSize)
	if end <= start {
		return
	}
	previous := c.anchor.cursor
	c.demandSpans, _ = mergeByteSpan(c.demandSpans, byteSpan{start: start, end: end})
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
	remaining := make([]byteSpan, 0, len(c.demandSpans))
	for _, span := range c.demandSpans {
		if span.end <= cursor {
			continue
		}
		if span.start <= cursor {
			cursor = span.end
			continue
		}
		remaining = append(remaining, span)
	}
	c.demandSpans = remaining
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
	c.demandSpans = append([]byteSpan(nil), candidate.spans...)
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
		if t.legacy || coordinator.torrent == nil {
			if _, ok := coordinator.active[index]; ok {
				coordinator.cancelLeaseLocked(index)
			}
		}
		if t.active == nil {
			t.active = make(map[int]int)
		}
		t.active[index]++
		if !t.legacy {
			if coordinator.retainForFileLocked(t.file, index) {
				if t.activePinned == nil {
					t.activePinned = make(map[int]int)
				}
				t.activePinned[index]++
			}
		}
		if coordinator.torrent != nil {
			coordinator.torrent.Piece(index).SetPriority(torrent.PiecePriorityNow)
		}
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

func (c *prefetchCoordinator) restorePiecePriorityLocked(index int) {
	if c.torrent == nil || c.foregroundPieces[index] > 0 {
		return
	}
	if owners := c.normalOwners[index]; len(owners) > 0 {
		c.torrent.Piece(index).SetPriority(torrent.PiecePriorityNormal)
		return
	}
	c.torrent.Piece(index).SetPriority(torrent.PiecePriorityNone)
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
	if ticket.activePinned[index] > 0 {
		if ticket.activePinned[index] == 1 {
			delete(ticket.activePinned, index)
		} else {
			ticket.activePinned[index]--
		}
		c.releaseLocked(index)
	}
	if count := c.foregroundPieces[index]; count > 1 {
		c.foregroundPieces[index] = count - 1
	} else {
		delete(c.foregroundPieces, index)
	}
	c.restorePiecePriorityLocked(index)
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
	c.demandSpans = nil
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

func (c *prefetchCoordinator) reconcilePlaybackStreamLocked(stream *playbackStreamState) {
	if stream.fileSize <= stream.cursor || stream.pieceLength <= 0 {
		stream.bufferedBytes = 0
		stream.effectiveHigh = 0
		stream.effectiveLow = 0
		stream.state = prefetchPaused
		c.releasePlaybackResourcesLocked(stream)
		return
	}
	c.updatePlaybackBufferedLocked(stream)
	desired := c.desiredPlaybackPiecesLocked(stream)
	wanted := make(map[int]struct{}, len(desired))
	for _, index := range desired {
		wanted[index] = struct{}{}
	}
	for index := range stream.pins {
		if _, ok := wanted[index]; ok {
			continue
		}
		delete(stream.pins, index)
		c.releaseLocked(index)
	}
	reservedPrefix := int64(0)
	reservationBlocked := false
	for _, index := range desired {
		if _, ok := stream.pins[index]; !ok {
			if !c.retainForFileLocked(stream.file, index) {
				reservationBlocked = true
				break
			}
			stream.pins[index] = struct{}{}
		}
		reservedPrefix += c.playbackPieceOverlapLocked(stream, index)
	}
	stream.effectiveHigh = min64(stream.targetHigh, stream.fileSize-stream.cursor)
	if reservationBlocked && reservedPrefix < stream.effectiveHigh {
		stream.effectiveHigh = reservedPrefix
	}
	if stream.effectiveHigh < 0 {
		stream.effectiveHigh = 0
	}
	stream.effectiveLow = min64(stream.targetLow, stream.effectiveHigh/2)
	if reservationBlocked && reservedPrefix == 0 {
		stream.state = prefetchBudgetBlocked
		c.releasePlaybackOwnersLocked(stream)
		return
	}
	work := false
	for _, index := range desired {
		if !c.cache.Has(c.key(index)) {
			work = true
			break
		}
	}
	if !work || stream.bufferedBytes >= stream.effectiveHigh {
		stream.state = prefetchPaused
		c.releasePlaybackOwnersLocked(stream)
		return
	}
	if stream.state == prefetchPaused && stream.bufferedBytes > stream.effectiveLow {
		c.releasePlaybackOwnersLocked(stream)
		return
	}
	stream.state = prefetchFilling
	for _, index := range desired {
		if stream.bufferedBytes >= stream.effectiveHigh || len(c.active) >= c.currentLimit() {
			break
		}
		if c.cache.Has(c.key(index)) || c.foregroundPieces[index] > 0 {
			continue
		}
		owners := c.normalOwners[index]
		if owners == nil {
			owners = make(map[string]struct{})
			c.normalOwners[index] = owners
		}
		if _, ok := owners[stream.id]; !ok {
			owners[stream.id] = struct{}{}
			stream.owners[index] = struct{}{}
		}
		if _, ok := c.active[index]; ok {
			c.dedupe++
			continue
		}
		if !c.budget.tryAcquire() {
			c.budgetBlocks++
			stream.state = prefetchBudgetBlocked
			break
		}
		if c.torrent != nil {
			c.torrent.Piece(index).UpdateCompletion()
			c.torrent.Piece(index).SetPriority(torrent.PiecePriorityNormal)
		}
		c.active[index] = struct{}{}
		c.priorityAdds++
		if len(c.active) > c.maxActive {
			c.maxActive = len(c.active)
		}
	}
	c.completeActiveLocked()
}

func (c *prefetchCoordinator) releasePlaybackOwnersLocked(stream *playbackStreamState) {
	for index := range stream.owners {
		c.removeNormalOwnerLocked(index, stream.id)
	}
	stream.owners = make(map[int]struct{})
}

func (c *prefetchCoordinator) desiredPlaybackPiecesLocked(stream *playbackStreamState) []int {
	start := stream.fileStart + stream.cursor
	end := start + stream.targetHigh
	fileEnd := stream.fileStart + stream.fileSize
	if end > fileEnd {
		end = fileEnd
	}
	if stream.torrentSize > 0 && end > stream.torrentSize {
		end = stream.torrentSize
	}
	if end <= start {
		return nil
	}
	first := int(start / stream.pieceLength)
	last := int((end-1)/stream.pieceLength) + 1
	out := make([]int, 0, last-first)
	for index := first; index < last; index++ {
		out = append(out, index)
	}
	return out
}

func (c *prefetchCoordinator) updatePlaybackBufferedLocked(stream *playbackStreamState) {
	end := stream.fileStart + stream.fileSize
	if stream.torrentSize > 0 && end > stream.torrentSize {
		end = stream.torrentSize
	}
	limit := stream.targetHigh
	if remaining := end - (stream.fileStart + stream.cursor); remaining < limit {
		limit = remaining
	}
	if limit <= 0 {
		stream.bufferedBytes = 0
		return
	}
	stream.bufferedBytes = c.cache.ContiguousRun(c.torrentKey, stream.fileStart+stream.cursor, stream.pieceLength, limit)
}

func (c *prefetchCoordinator) playbackPieceOverlapLocked(stream *playbackStreamState, index int) int64 {
	start := int64(index) * stream.pieceLength
	end := start + stream.pieceLength
	fileStart := stream.fileStart + stream.cursor
	fileEnd := stream.fileStart + stream.fileSize
	if start < fileStart {
		start = fileStart
	}
	if end > fileEnd {
		end = fileEnd
	}
	if stream.torrentSize > 0 && end > stream.torrentSize {
		end = stream.torrentSize
	}
	if end <= start {
		return 0
	}
	return end - start
}

func (c *prefetchCoordinator) retainForFileLocked(f *raFile, index int) bool {
	if f == nil || f.pieceLength <= 0 {
		return false
	}
	if c.refs[index] == 0 && !c.cache.PinSize(cache.Key{Torrent: c.torrentKey, Piece: index}, pieceSizeForFile(f, index)) {
		return false
	}
	c.refs[index]++
	return true
}

func pieceSizeForFile(f *raFile, index int) int64 {
	start := int64(index) * f.pieceLength
	remaining := f.torrentSize - start
	if remaining < f.pieceLength {
		return remaining
	}
	return f.pieceLength
}

func (c *prefetchCoordinator) reconcile() {
	c.mu.Lock()
	if len(c.streams) == 0 {
		c.mu.Unlock()
		return
	}
	c.expirePlaybackStreamsLocked(c.coordinatorNow())
	for _, stream := range c.streams {
		c.reconcilePlaybackStreamLocked(stream)
	}
	callbacks := c.expiredCallbacks
	c.expiredCallbacks = nil
	c.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
	return

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
		c.restorePiecePriorityLocked(index)
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
	c.restorePiecePriorityLocked(index)
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

func (c *prefetchCoordinator) releaseDemandLocked(id uint64) []context.CancelFunc {
	if id == 0 {
		return nil
	}
	state := c.demands[id]
	var cancels []context.CancelFunc
	for ticketID, ticket := range c.foreground {
		if ticket.demandID != id && (state == nil || ticket.demand != state) {
			continue
		}
		if cancel := c.removeForegroundLocked(ticket); cancel != nil {
			cancels = append(cancels, cancel)
		}
		delete(c.foreground, ticketID)
	}
	delete(c.demands, id)
	for file, candidate := range c.demandByFile {
		if candidate == state {
			delete(c.demandByFile, file)
		}
	}
	delete(c.demandCursors, id)
	return cancels
}

func (c *prefetchCoordinator) releaseDemand(id uint64) {
	c.mu.Lock()
	cancels := c.releaseDemandLocked(id)
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	c.signal()
}

func (c *prefetchCoordinator) releaseFile(f *raFile) {
	var cancels []context.CancelFunc
	c.mu.Lock()
	if !c.closed {
		for id, state := range c.demands {
			if state.file == f {
				cancels = append(cancels, c.releaseDemandLocked(id)...)
			}
		}
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
			c.demandSpans = nil
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
		pieceSize := c.pieceSizeLocked(index)
		if pieceSize == 0 {
			for _, stream := range c.streams {
				pieceSize = pieceSizeForFile(stream.file, index)
				break
			}
		}
		activeBytes += pieceSize
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
		DemandCursor:       cursor,
		PlaybackCursor:     0,
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
	if len(c.streams) > 0 {
		snapshot.PlaybackStreams = make([]PlaybackStreamSnapshot, 0, len(c.streams))
		for _, stream := range c.streams {
			snapshot.PlaybackStreams = append(snapshot.PlaybackStreams, c.playbackSnapshotLocked(stream))
		}
		sort.Slice(snapshot.PlaybackStreams, func(i, j int) bool {
			return snapshot.PlaybackStreams[i].ID < snapshot.PlaybackStreams[j].ID
		})
		first := snapshot.PlaybackStreams[0]
		snapshot.Generation = first.Generation
		snapshot.State = first.State
		snapshot.Cursor = first.PlaybackCursor
		snapshot.DemandCursor = 0
		snapshot.PlaybackCursor = first.PlaybackCursor
		snapshot.BufferedBytes = first.BufferedBytes
		snapshot.EffectiveLow = first.EffectiveLow
		snapshot.EffectiveHigh = first.EffectiveHigh
		snapshot.PlaybackAnchor = first.PlaybackCursor
		snapshot.ConfirmedAnchor = first.PlaybackCursor
		snapshot.AnchorConfirmed = true
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
	for _, stream := range c.streams {
		c.releasePlaybackResourcesLocked(stream)
	}
	c.streams = make(map[string]*playbackStreamState)
	c.normalOwners = make(map[int]map[string]struct{})
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
	c.demands = make(map[uint64]*demandState)
	c.demandByFile = make(map[*raFile]*demandState)
	c.demandCursors = make(map[uint64]int64)
	c.expiredCallbacks = nil
	c.demandSpans = nil
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

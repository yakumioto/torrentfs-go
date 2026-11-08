package session

import (
	"sync"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const (
	// probeConns is the allowance for a cached torrent that nobody is reading
	// or uploading. One slot lets a peer connect and ask for pieces; without it
	// an upload could never be discovered, because the client refuses a new
	// connection before it can see a piece request.
	probeConns = 1
	// defaultUploadGrace is how long after the last observed upload a cached
	// torrent keeps its uploading allowance.
	defaultUploadGrace = 5 * time.Minute
	// defaultProbeRotate is how long a probe slot stays with one peer before it
	// is closed and offered to another. The quiet period starts when the slot
	// is established, so a fresh slot always gets the full interval.
	defaultProbeRotate = 10 * time.Minute
	// defaultProbePause is how long the probe slot stays closed during a
	// rotation, so the peer occupying it actually disconnects.
	defaultProbePause = 30 * time.Second
	// metadataConns is the allowance while a magnet is still fetching its
	// metadata: enough peers to find one that has it, bounded enough that a
	// batch of magnets cannot flood the client.
	metadataConns = 10
	// metadataSlowConns is the allowance once the fast metadata window has
	// passed without metadata. One slot retries slowly instead of letting a
	// dead magnet hold ten connections forever.
	metadataSlowConns = 1
	// metadataFastWindow is how long a magnet keeps the wider metadata
	// allowance before it degrades to the slow retry.
	metadataFastWindow = 5 * time.Minute
	// metadataPause is how long the metadata allowance stays closed during a
	// rotation, long enough for the connections to actually go away.
	metadataPause = 2 * time.Second
	// defaultConnAllocInterval is how often the allocator re-evaluates every
	// torrent. Read activity wakes it sooner, so this is only the idle rate.
	defaultConnAllocInterval = time.Second
)

// metadataRotationIntervals is the progressive schedule for re-offering a
// magnet's metadata connections. Early rotations happen quickly because the
// first batch of peers may all be leechers that cannot serve metadata yet; once
// the magnet is clearly slow, the interval backs off so it does not churn.
var metadataRotationIntervals = [...]time.Duration{
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// metadataRotationInterval returns the wait before the next metadata rotation
// after the given number of rotations.
func metadataRotationInterval(rotations int) time.Duration {
	if rotations <= 0 {
		return metadataRotationIntervals[0]
	}
	if rotations >= len(metadataRotationIntervals) {
		return metadataRotationIntervals[len(metadataRotationIntervals)-1]
	}
	return metadataRotationIntervals[rotations]
}

// connectionState is the allocator's classification of a torrent's use.
type connectionState int

const (
	connectionStateIdle connectionState = iota
	connectionStateProbe
	connectionStateUploading
	connectionStateReading
	connectionStateMetadata
)

func (s connectionState) String() string {
	switch s {
	case connectionStateIdle:
		return "idle"
	case connectionStateProbe:
		return "probe"
	case connectionStateUploading:
		return "uploading"
	case connectionStateReading:
		return "reading"
	case connectionStateMetadata:
		return "metadata"
	default:
		return "unknown"
	}
}

// connectionAllocatorTiming holds the windows of the allocator. Production uses
// the compiled-in defaults.
type connectionAllocatorTiming struct {
	interval    time.Duration
	uploadGrace time.Duration
	probeRotate time.Duration
	probePause  time.Duration
}

// connectionAllocator wakes on a timer or on read activity and keeps every
// torrent's peer connection allowance in line with its use. There is no
// session-wide budget: every torrent answers for its own allowance.
//
// The mutex guards the clock and the timing windows so a test can replace the
// clock while the allocator goroutine is running; the decision state itself
// lives in atomics on the torrent.
type connectionAllocator struct {
	mu     sync.RWMutex
	kick   chan struct{}
	now    func() time.Time
	timing connectionAllocatorTiming
}

func newConnectionAllocator() connectionAllocator {
	return connectionAllocator{
		kick: make(chan struct{}, 1),
		now:  time.Now,
		timing: connectionAllocatorTiming{
			interval:    defaultConnAllocInterval,
			uploadGrace: defaultUploadGrace,
			probeRotate: defaultProbeRotate,
			probePause:  defaultProbePause,
		},
	}
}

// clock returns the allocator's current time under the configuration lock.
func (a *connectionAllocator) clock() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.now()
}

// windows returns a consistent copy of the timing windows.
func (a *connectionAllocator) windows() connectionAllocatorTiming {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.timing
}

// connectionActivity is the per-torrent snapshot one allocation decision reads.
// It is a plain value so the decision itself is a pure function.
type connectionActivity struct {
	HasInfo          bool
	AddedAt          time.Time
	ReadingActivity  bool
	Complete         bool
	LastUpload       time.Time
	CachedBytes      int64
	PreviousState    connectionState
	ProbeSince       time.Time
	ProbePausedUntil time.Time

	MetadataRotatedAt   time.Time
	MetadataPausedUntil time.Time
	MetadataRotations   int
}

// connectionPlan is the allowance decision plus the bookkeeping the next
// decision needs. The allocator stores the bookkeeping back on the torrent.
type connectionPlan struct {
	State     connectionState
	Allowance int

	ProbeSince       time.Time
	ProbePausedUntil time.Time

	MetadataRotatedAt   time.Time
	MetadataPausedUntil time.Time
	MetadataRotations   int
}

// connectionPolicy decides one torrent's peer connection allowance. The rules,
// in order of precedence:
//
//   - metadata stage: a magnet without metadata keeps the metadata allowance,
//     rotating its peers on a progressive schedule and degrading to a slow
//     retry after metadataFastWindow;
//   - reading: a handle is open, or one was open within ReadGrace, and the
//     content is not fully cached: the reading allowance;
//   - uploading: cached and a real upload was observed within the upload
//     grace: the uploading allowance;
//   - probe: cached but quiet: one probe connection, rotated periodically;
//   - idle: otherwise, no connections.
func connectionPolicy(now time.Time, activity connectionActivity, conn config.Connections, timing connectionAllocatorTiming) connectionPlan {
	// A torrent without metadata can be neither read nor cached; its only job
	// is to reach a peer that can serve the metadata.
	if !activity.HasInfo {
		return metadataConnectionPlan(now, activity)
	}
	if plan, ok := readingAllowance(activity.ReadingActivity, activity.Complete, conn); ok {
		return plan
	}
	switch {
	case activity.CachedBytes > 0 && !activity.LastUpload.IsZero() && now.Sub(activity.LastUpload) < timing.uploadGrace:
		return connectionPlan{State: connectionStateUploading, Allowance: conn.UploadingConns}
	case activity.CachedBytes > 0:
		return probeConnectionPlan(now, activity, timing)
	default:
		return connectionPlan{State: connectionStateIdle}
	}
}

// readingAllowance returns the reading plan when a file handle is open (or was
// open within the grace window) and the content is not fully cached. A fully
// cached torrent reads from memory, so it does not need peers for the read.
func readingAllowance(readingActivity, complete bool, conn config.Connections) (connectionPlan, bool) {
	if !readingActivity || complete {
		return connectionPlan{}, false
	}
	return connectionPlan{State: connectionStateReading, Allowance: conn.ReadingConns}, true
}

// metadataConnectionPlan keeps a magnet's connection allowance while it fetches
// metadata. The rotation closes the connection briefly so the client redials a
// different peer; metadata pieces that were already received are kept on the
// torrent, so a rotation never loses progress.
func metadataConnectionPlan(now time.Time, activity connectionActivity) connectionPlan {
	plan := connectionPlan{
		State:               connectionStateMetadata,
		MetadataRotatedAt:   activity.MetadataRotatedAt,
		MetadataPausedUntil: activity.MetadataPausedUntil,
		MetadataRotations:   activity.MetadataRotations,
	}
	if !activity.MetadataPausedUntil.IsZero() && now.Before(activity.MetadataPausedUntil) {
		return plan
	}
	plan.Allowance = metadataConns
	if !activity.AddedAt.IsZero() && now.Sub(activity.AddedAt) >= metadataFastWindow {
		plan.Allowance = metadataSlowConns
	}
	if activity.AddedAt.IsZero() {
		return plan
	}
	next := activity.AddedAt.Add(metadataRotationInterval(0))
	if !activity.MetadataRotatedAt.IsZero() {
		next = activity.MetadataRotatedAt.Add(metadataRotationInterval(activity.MetadataRotations))
	}
	if !now.Before(next) {
		plan.Allowance = 0
		plan.MetadataRotatedAt = now
		plan.MetadataPausedUntil = now.Add(metadataPause)
		plan.MetadataRotations = activity.MetadataRotations + 1
	}
	return plan
}

// probeConnectionPlan keeps one connection on a cached but quiet torrent and
// rotates it after a full quiet period. The quiet period starts when the slot
// is established (the torrent left reading or uploading), not when the last
// activity happened, so a freshly opened slot gets the whole interval.
func probeConnectionPlan(now time.Time, activity connectionActivity, timing connectionAllocatorTiming) connectionPlan {
	since := activity.ProbeSince
	if activity.PreviousState != connectionStateProbe || since.IsZero() {
		since = now
	}
	plan := connectionPlan{
		State:            connectionStateProbe,
		ProbeSince:       since,
		ProbePausedUntil: activity.ProbePausedUntil,
	}
	if !activity.ProbePausedUntil.IsZero() && now.Before(activity.ProbePausedUntil) {
		return plan
	}
	if now.Sub(since) >= timing.probeRotate {
		plan.ProbeSince = now
		plan.ProbePausedUntil = now.Add(timing.probePause)
		return plan
	}
	plan.Allowance = probeConns
	return plan
}

// startConnectionAllocator starts the goroutine that keeps peer connection
// allowances in line with activity. It joins the session background lifecycle,
// so Close cancels it and waits before the client is torn down.
func (s *Session) startConnectionAllocator() {
	s.bgMu.Lock()
	s.bgWg.Add(1)
	s.bgMu.Unlock()
	go s.runConnectionAllocator()
}

func (s *Session) runConnectionAllocator() {
	defer s.bgWg.Done()
	ticker := time.NewTicker(s.connAlloc.windows().interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.bgCtx.Done():
			return
		case <-ticker.C:
		case <-s.connAlloc.kick:
		}
		s.allocateConnections()
	}
}

// kickConnectionAllocator asks the allocator to run now. It never blocks: the
// wake-up is coalesced, and the next tick would pick the change up anyway.
func (s *Session) kickConnectionAllocator() {
	if s == nil {
		return
	}
	select {
	case s.connAlloc.kick <- struct{}{}:
	default:
	}
}

// allocatorNow returns the time the allocator's policy should observe.
func (s *Session) allocatorNow() time.Time {
	if s == nil {
		return time.Now()
	}
	return s.connAlloc.clock()
}

// allocateConnections re-evaluates every torrent and applies the allowance its
// activity calls for. Lowering the allowance through SetMaxEstablishedConns
// drops the connections that no longer fit, which is how excess inbound
// connections are reclaimed.
func (s *Session) allocateConnections() {
	now := s.connAlloc.clock()
	timing := s.connAlloc.windows()
	conn := s.cfg.Connections
	s.mu.RLock()
	torrents := make([]*Torrent, 0, len(s.torrents))
	for _, st := range s.torrents {
		torrents = append(torrents, st)
	}
	s.mu.RUnlock()
	for _, st := range torrents {
		st.observeUpload(now)
		plan := connectionPolicy(now, st.connectionActivity(now, conn), conn, timing)
		st.applyConnectionPlan(now, plan, conn)
	}
}

// connectionActivity snapshots the state the allowance policy reads for one
// torrent. The counters are atomics because readers update them without the
// torrent lock.
func (t *Torrent) connectionActivity(now time.Time, conn config.Connections) connectionActivity {
	return connectionActivity{
		HasInfo:             t.Info() != nil,
		AddedAt:             t.addedAt,
		ReadingActivity:     t.readingActivity(now, conn),
		Complete:            t.fullyCached(),
		LastUpload:          timeFromUnixNano(t.lastUploadAt.Load()),
		CachedBytes:         t.CachedBytes(),
		PreviousState:       connectionState(t.connState.Load()),
		ProbeSince:          timeFromUnixNano(t.probeSince.Load()),
		ProbePausedUntil:    timeFromUnixNano(t.probePausedUntil.Load()),
		MetadataRotatedAt:   timeFromUnixNano(t.metadataRotatedAt.Load()),
		MetadataPausedUntil: timeFromUnixNano(t.metadataPausedUntil.Load()),
		MetadataRotations:   int(t.metadataRotations.Load()),
	}
}

// observeUpload records a real upload since the previous pass, which keeps the
// torrent in the uploading state (and out of the probe rotation) while peers
// are fetching pieces from it.
func (t *Torrent) observeUpload(now time.Time) {
	stats := t.tor.Stats()
	uploaded := stats.ConnStats.BytesWrittenData.Int64()
	if previous := t.uploadedSeen.Swap(uploaded); uploaded > previous {
		t.lastUploadAt.Store(now.UnixNano())
	}
}

// applyConnectionPlan stores the decision's bookkeeping and pushes the
// allowance to the client. A drop to zero re-checks reading first, so a read
// that started after the snapshot is never refused by a stale decision.
func (t *Torrent) applyConnectionPlan(now time.Time, plan connectionPlan, conn config.Connections) {
	if plan.Allowance == 0 {
		if reading, ok := readingAllowance(t.readingActivity(now, conn), t.fullyCached(), conn); ok {
			plan = reading
		}
	}
	t.connState.Store(int32(plan.State))
	t.probeSince.Store(unixNanoOrZero(plan.ProbeSince))
	t.probePausedUntil.Store(unixNanoOrZero(plan.ProbePausedUntil))
	t.metadataRotatedAt.Store(unixNanoOrZero(plan.MetadataRotatedAt))
	t.metadataPausedUntil.Store(unixNanoOrZero(plan.MetadataPausedUntil))
	t.metadataRotations.Store(int32(plan.MetadataRotations))
	if int(t.connAllowance.Swap(int32(plan.Allowance))) == plan.Allowance {
		return
	}
	t.tor.SetMaxEstablishedConns(plan.Allowance)
}

// readingActivity reports whether a file handle is open or the torrent was read
// within the configured grace window.
func (t *Torrent) readingActivity(now time.Time, conn config.Connections) bool {
	if t.readersOpen.Load() > 0 {
		return true
	}
	lastRead := timeFromUnixNano(t.lastReadAt.Load())
	return !lastRead.IsZero() && now.Sub(lastRead) < time.Duration(conn.ReadGrace)
}

// fullyCached reports whether every byte of the torrent is in the piece cache.
// Reads are served from memory then, so a read does not need peers.
func (t *Torrent) fullyCached() bool {
	length := t.Length()
	return length > 0 && t.CachedBytes() >= length
}

// noteReadActivity records when the torrent was last read and wakes the
// allocator so the allowance follows the activity without waiting for the next
// tick.
func (t *Torrent) noteReadActivity() {
	t.lastReadAt.Store(t.session.allocatorNow().UnixNano())
	t.session.kickConnectionAllocator()
}

func timeFromUnixNano(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func unixNanoOrZero(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixNano()
}

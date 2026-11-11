package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

var (
	// ErrTorrentNotReady reports that a torrent's metadata is not yet
	// available, or that it is in the error state, so it cannot serve a piece
	// download.
	ErrTorrentNotReady = errors.New("session: torrent is not ready")
	// ErrInvalidPieceDownloadRequest reports a piece download request that
	// violates the range contract: the wrapped cause names the problem.
	ErrInvalidPieceDownloadRequest = errors.New("session: invalid piece download request")
	// ErrNoPieceDownload reports that a torrent exists but no piece download
	// has ever been started on it in this process.
	ErrNoPieceDownload = errors.New("session: no piece download for torrent")
)

// pieceDownloadConcurrency bounds how many pieces, summed across every
// torrent's piece download, may be fetched at once. It is a constant, not a
// configuration field: 8 is a starting point with no measurement behind it
// yet.
const pieceDownloadConcurrency = 8

// pieceDownloadBudget is a session-wide, blocking semaphore shared by every
// torrent's piece download. Unlike prefetchBudget (which drops speculative
// work when it is full), a piece download must eventually fetch every piece
// it was asked for, so acquire blocks until a slot is free instead of
// failing fast.
type pieceDownloadBudget struct {
	sem chan struct{}
}

func newPieceDownloadBudget() *pieceDownloadBudget {
	return &pieceDownloadBudget{sem: make(chan struct{}, pieceDownloadConcurrency)}
}

// acquire blocks until a slot is free or ctx ends, whichever comes first. A
// caller that gets a nil error must call release exactly once; one that gets
// ctx's error acquired nothing and must not call release.
func (b *pieceDownloadBudget) acquire(ctx context.Context) error {
	select {
	case b.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *pieceDownloadBudget) release() {
	<-b.sem
}

// PieceRange is a half-open range of piece indexes [Start, End).
type PieceRange struct {
	Start int
	End   int
}

// PieceDownloadState is the lifecycle state of one piece download task.
type PieceDownloadState string

const (
	// PieceDownloadRunning is a piece download still fetching pieces.
	PieceDownloadRunning PieceDownloadState = "running"
	// PieceDownloadCompleted is a piece download whose every requested piece
	// has been fetched or was already resident in the cache.
	PieceDownloadCompleted PieceDownloadState = "completed"
	// PieceDownloadCancelled is a piece download CancelPieceDownload stopped,
	// or that a later StartPieceDownload replaced, before it completed.
	PieceDownloadCancelled PieceDownloadState = "cancelled"
	// PieceDownloadExpired is a piece download its own timeout cut off
	// before it completed. Pieces already fetched remain in the cache.
	PieceDownloadExpired PieceDownloadState = "expired"
	// PieceDownloadFailed is a piece download a genuine, non-cancellation
	// fetch error stopped. Error names the cause; the remaining requested
	// pieces were never attempted.
	PieceDownloadFailed PieceDownloadState = "failed"
)

// PieceDownloadStatus is the management API's snapshot of one torrent's most
// recent piece download.
type PieceDownloadStatus struct {
	State PieceDownloadState
	// RequestedPieces is the deduplicated count of pieces the request named.
	RequestedPieces int
	// DonePieces is how many of those pieces have been fetched or were
	// already resident in the cache when the download started.
	DonePieces int
	StartedAt  time.Time
	// FinishedAt is zero while the download is running.
	FinishedAt time.Time
	Error      string
}

// pieceDownload is the mutable state of one torrent's piece download task. It
// lives on the Torrent for the lifetime of the process, is never persisted,
// and is replaced wholesale by the next start.
type pieceDownload struct {
	mu         sync.Mutex
	state      PieceDownloadState
	requested  int
	done       int
	startedAt  time.Time
	finishedAt time.Time
	err        string
	// cancel stops this task's background fetch loop. It is nil for a task
	// that completed synchronously at start (every piece was already
	// cached), set once before the goroutine starts otherwise, and never
	// reassigned, so reading it without the mutex is safe.
	cancel context.CancelFunc
	// timer fires stop(PieceDownloadExpired) when the requested timeout
	// elapses. It is nil when no timeout was requested or the task
	// completed synchronously, set once before the goroutine starts
	// otherwise, and never reassigned, so reading it without the mutex is
	// safe. finish stops it on every terminal transition, whichever one
	// gets there first, so it never fires uselessly after the task is
	// already done.
	timer *time.Timer
}

func (p *pieceDownload) snapshot() PieceDownloadStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PieceDownloadStatus{
		State:           p.state,
		RequestedPieces: p.requested,
		DonePieces:      p.done,
		StartedAt:       p.startedAt,
		FinishedAt:      p.finishedAt,
		Error:           p.err,
	}
}

func (p *pieceDownload) incDone() {
	p.mu.Lock()
	p.done++
	p.mu.Unlock()
}

// finish moves the task to a terminal state if it is still running, and
// reports whether it did. A task already in a terminal state is left
// untouched: whichever of a natural completion and an explicit cancel gets
// there first wins, and the other becomes a no-op instead of overwriting it.
func (p *pieceDownload) finish(state PieceDownloadState, errMsg string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != PieceDownloadRunning {
		return false
	}
	p.state = state
	p.err = errMsg
	p.finishedAt = time.Now().UTC()
	if p.timer != nil {
		p.timer.Stop()
	}
	return true
}

// stop cancels a running download's background fetch loop if finish still
// found it running, moving it to state. A download with no background loop
// (cancel is nil, because every requested piece was already cached at start)
// only needed the state change.
//
// stop moves a running download to a terminal state and unblocks its fetch
// loop. It reports whether it won the transition; a download already in a
// terminal state is left untouched.
func (p *pieceDownload) stop(state PieceDownloadState) bool {
	if !p.finish(state, "") {
		return false
	}
	if p.cancel != nil {
		p.cancel()
	}
	return true
}

// StartPieceDownload fetches the pieces named by ranges into the piece cache
// without opening a file reader. A piece already resident in the cache counts
// as done immediately and is never refetched; the rest are fetched in the
// background, concurrently, under a session-wide budget shared with every
// other piece download (see pieceDownloadConcurrency). The returned status reflects the
// state at the moment of the call: a request fully satisfied by the cache
// completes synchronously.
//
// Starting a new piece download replaces any previous one this torrent had;
// see CancelPieceDownload for stopping one without replacing it.
//
// ctx bounds only this call: once the request is accepted the fetch runs on
// the session's background context, so a caller that disconnects does not
// abort it. Use CancelPieceDownload or timeout to stop a download.
//
// timeout bounds how long the download may run before it is cut off and
// moved to the expired state; pieces already fetched by then stay cached.
// timeout <= 0 means no deadline. This method accepts any precision, so a
// test can use a millisecond-scale timeout instead of waiting out a real
// one; translating an external, second-granularity contract (with its own
// default and bounds) into this duration is a caller concern.
func (s *Session) StartPieceDownload(ctx context.Context, id string, ranges []PieceRange, timeout time.Duration) (PieceDownloadStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PieceDownloadStatus{}, err
	}
	hash, entry, st, err := s.lookupPieceDownloadTarget(id)
	if err != nil {
		return PieceDownloadStatus{}, err
	}
	if entry.State != StateReady || st == nil || st.Info() == nil {
		return PieceDownloadStatus{}, ErrTorrentNotReady
	}

	info := st.Info()
	pieceCount := info.NumPieces()
	ordered, err := normalizePieceRanges(ranges, pieceCount)
	if err != nil {
		return PieceDownloadStatus{}, err
	}

	torrentKey := hash.HexString()
	doneNow := 0
	remaining := make([]int, 0, len(ordered))
	for _, index := range ordered {
		if s.pieceCache.Has(cache.Key{Torrent: torrentKey, Piece: index}) {
			doneNow++
			continue
		}
		remaining = append(remaining, index)
	}

	task := &pieceDownload{
		state:     PieceDownloadRunning,
		requested: len(ordered),
		done:      doneNow,
		startedAt: time.Now().UTC(),
	}
	if len(remaining) == 0 {
		task.state = PieceDownloadCompleted
		task.finishedAt = task.startedAt
	}

	var taskCtx context.Context
	var taskCancel context.CancelFunc
	if len(remaining) > 0 {
		taskCtx, taskCancel = context.WithCancel(s.bgCtx)
		task.cancel = taskCancel
		if timeout > 0 {
			task.timer = time.AfterFunc(timeout, func() {
				task.stop(PieceDownloadExpired)
				snapshot := task.snapshot()
				s.logger.Warn("piece download expired", "hash", hash.HexString(),
					"timeout", timeout, "requested_pieces", snapshot.RequestedPieces, "done_pieces", snapshot.DonePieces)
			})
		}
	}

	st.mu.Lock()
	previous := st.pieceDownload
	st.pieceDownload = task
	loader := st.ensurePieceLoaderLocked()
	st.mu.Unlock()
	// Replacing a running download stops it: only one piece download per
	// torrent exists at a time, and the pointer swap above already made this
	// one the one GET and a future replace will see.
	if previous != nil && previous.stop(PieceDownloadCancelled) {
		s.logPieceDownloadEnd("piece download cancelled", torrentKey, "replaced", previous)
	}
	s.logger.Info("piece download started", "hash", torrentKey,
		"requested_pieces", task.requested, "cached_pieces", doneNow, "timeout", timeout)

	if len(remaining) == 0 {
		s.logPieceDownloadEnd("piece download completed", torrentKey, "", task)
		return task.snapshot(), nil
	}

	pieceLength := info.PieceLength
	torrentLength := st.Length()
	st.pieceDownloadActive.Add(1)
	st.noteReadActivity()
	s.bgMu.Lock()
	s.bgWg.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.bgWg.Done()
		defer taskCancel()
		defer func() {
			st.pieceDownloadActive.Add(-1)
			st.noteReadActivity()
		}()
		// Pieces are fetched concurrently, bounded by the session-wide
		// budget: dispatch blocks on acquire before starting the next
		// piece's worker, so at most pieceDownloadConcurrency fetches are
		// ever in flight across every torrent's piece download at once.
		var wg sync.WaitGroup
		completed := true
		for _, index := range remaining {
			if taskCtx.Err() != nil {
				completed = false
				break
			}
			size := pieceSizeAt(index, pieceLength, torrentLength)
			if size <= 0 {
				task.incDone()
				continue
			}
			if err := s.pieceDownloadBudget.acquire(taskCtx); err != nil {
				// The task was cancelled or expired while this piece waited
				// for a slot: nothing was fetched for it, and there is no
				// slot to release.
				completed = false
				break
			}
			index := index
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer s.pieceDownloadBudget.release()
				buf := make([]byte, size)
				if _, err := loader.ReadAtContext(taskCtx, buf, int64(index)*pieceLength, 0); err != nil {
					// A context error here means a cancel or an expiry
					// already claimed the terminal state (and cancelled
					// taskCtx to unblock this read); this is not a fetch
					// failure. Anything else is a genuine fetch error: claim
					// the failed state and cancel taskCtx ourselves so every
					// other piece this download is waiting on, in flight or
					// still queued for a budget slot, stops too.
					if taskCtx.Err() == nil && task.finish(PieceDownloadFailed, err.Error()) {
						s.logger.Error("piece download failed", "hash", hash.HexString(), "piece", index, "err", err)
						taskCancel()
					}
					return
				}
				task.incDone()
			}()
		}
		wg.Wait()
		// Only an uninterrupted run may claim completion. A cancel, expiry
		// or session teardown that cut the dispatch short already claimed its
		// own terminal state, or (for teardown) leaves the record to
		// stopPieceDownload on the way out.
		if completed && task.finish(PieceDownloadCompleted, "") {
			s.logPieceDownloadEnd("piece download completed", torrentKey, "", task)
		}
	}()

	return task.snapshot(), nil
}

// CancelPieceDownload stops one torrent's in-flight piece download. It is
// idempotent: a torrent with no piece download, or whose download already
// reached a terminal state, is left untouched and reports no error. Pieces
// already fetched before the cancel remain in the cache.
func (s *Session) CancelPieceDownload(id string) error {
	hash, _, st, err := s.lookupPieceDownloadTarget(id)
	if err != nil {
		if errors.Is(err, ErrDeleting) {
			// A cancel racing a deletion has nothing left to stop; the
			// deletion owns the teardown.
			return nil
		}
		return err
	}
	if st == nil {
		return nil
	}
	task, stopped := st.stopPieceDownload(PieceDownloadCancelled)
	if !stopped {
		return nil
	}
	s.logPieceDownloadEnd("piece download cancelled", hash.HexString(), "", task)
	return nil
}

// PieceDownloadStatus returns the current or most recent piece download of
// one torrent. It reports ErrNoPieceDownload when the torrent exists but no
// piece download has ever been started on it in this process.
func (s *Session) PieceDownloadStatus(id string) (PieceDownloadStatus, error) {
	_, _, st, err := s.lookupPieceDownloadTarget(id)
	if err != nil {
		return PieceDownloadStatus{}, err
	}
	if st == nil {
		return PieceDownloadStatus{}, ErrNoPieceDownload
	}
	st.mu.Lock()
	task := st.pieceDownload
	st.mu.Unlock()
	if task == nil {
		return PieceDownloadStatus{}, ErrNoPieceDownload
	}
	return task.snapshot(), nil
}

// lookupPieceDownloadTarget resolves a piece download id to the torrent's
// registry entry and live torrent under one lock, applying the checks every
// piece download operation shares: the session must be active, the torrent
// must be known, and a torrent held by a deletion reports ErrDeleting rather
// than "never started".
func (s *Session) lookupPieceDownloadTarget(id string) (metainfo.Hash, *registryEntry, *Torrent, error) {
	hash, err := parseInfoHash(id)
	if err != nil {
		return metainfo.Hash{}, nil, nil, ErrUnknownTorrent
	}
	s.mu.RLock()
	activeErr := s.ensureActiveLocked()
	entry := s.states[hash]
	st := s.torrents[hash]
	s.mu.RUnlock()
	if activeErr != nil {
		return metainfo.Hash{}, nil, nil, activeErr
	}
	if entry == nil {
		return metainfo.Hash{}, nil, nil, ErrUnknownTorrent
	}
	if entry.State == StateDeleting || entry.State == StateDeleteFailed {
		return metainfo.Hash{}, nil, nil, ErrDeleting
	}
	return hash, entry, st, nil
}

// logPieceDownloadEnd records a terminal transition with the seed hash and
// piece counts required by the API contract. reason distinguishes a replace
// or a teardown from a plain cancel and is omitted when empty.
func (s *Session) logPieceDownloadEnd(msg, torrentKey, reason string, task *pieceDownload) {
	status := task.snapshot()
	attrs := []any{"hash", torrentKey,
		"requested_pieces", status.RequestedPieces, "done_pieces", status.DonePieces}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	s.logger.Info(msg, attrs...)
}

// normalizePieceRanges validates ranges against pieceCount and returns the
// requested piece indexes in the order the ranges were given, with repeated
// and overlapping indexes deduplicated to their first occurrence.
func normalizePieceRanges(ranges []PieceRange, pieceCount int) ([]int, error) {
	if len(ranges) == 0 {
		return nil, fmt.Errorf("%w: ranges must not be empty", ErrInvalidPieceDownloadRequest)
	}
	seen := make(map[int]bool, pieceCount)
	ordered := make([]int, 0, pieceCount)
	for _, r := range ranges {
		if r.Start < 0 || r.End <= r.Start || r.End > pieceCount {
			return nil, fmt.Errorf("%w: range [%d,%d) is out of bounds for %d pieces", ErrInvalidPieceDownloadRequest, r.Start, r.End, pieceCount)
		}
		for index := r.Start; index < r.End; index++ {
			if !seen[index] {
				seen[index] = true
				ordered = append(ordered, index)
			}
		}
	}
	return ordered, nil
}

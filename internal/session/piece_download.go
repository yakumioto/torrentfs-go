package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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

// stop cancels task's background fetch loop if finish still found it running,
// moving it to state. A task with no background loop (cancel is nil, because
// every requested piece was already cached at start) only needed the state
// change.
func (p *pieceDownload) stop(state PieceDownloadState) {
	if !p.finish(state, "") {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
}

// StartPieceDownload fetches the pieces named by ranges into the piece cache
// without opening a file reader. A piece already resident in the cache counts
// as done immediately and is never refetched; the rest are fetched in the
// background in the order ranges were given. The returned status reflects the
// state at the moment of the call: a request fully satisfied by the cache
// completes synchronously.
//
// Starting a new piece download replaces any previous one this torrent had;
// see CancelPieceDownload for stopping one without replacing it.
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
	hash, err := parseInfoHash(id)
	if err != nil {
		return PieceDownloadStatus{}, ErrUnknownTorrent
	}

	s.mu.RLock()
	activeErr := s.ensureActiveLocked()
	entry := s.states[hash]
	st := s.torrents[hash]
	s.mu.RUnlock()
	if activeErr != nil {
		return PieceDownloadStatus{}, activeErr
	}
	if entry == nil {
		return PieceDownloadStatus{}, ErrUnknownTorrent
	}
	if entry.State == StateDeleting || entry.State == StateDeleteFailed {
		return PieceDownloadStatus{}, ErrDeleting
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
	previous := st.pieceDL
	st.pieceDL = task
	loader := st.ensurePieceLoaderLocked()
	st.mu.Unlock()
	// Replacing a running download stops it: only one piece download per
	// torrent exists at a time, and the pointer swap above already made this
	// one the one GET and a future replace will see.
	if previous != nil {
		previous.stop(PieceDownloadCancelled)
	}

	if len(remaining) == 0 {
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
		for _, index := range remaining {
			if err := taskCtx.Err(); err != nil {
				return
			}
			size := pieceByteSize(index, pieceLength, torrentLength)
			if size <= 0 {
				task.incDone()
				continue
			}
			buf := make([]byte, size)
			if _, err := loader.ReadAtContext(taskCtx, buf, int64(index)*pieceLength, 0); err != nil {
				// A context error here means a cancel or an expiry already
				// claimed the terminal state (and cancelled taskCtx to
				// unblock this read); this is not a fetch failure. Anything
				// else is a genuine fetch error: stop here and do not try
				// the remaining pieces anyway.
				if taskCtx.Err() == nil && task.finish(PieceDownloadFailed, err.Error()) {
					s.logger.Error("piece download failed", "hash", hash.HexString(), "piece", index, "err", err)
				}
				return
			}
			task.incDone()
		}
		task.finish(PieceDownloadCompleted, "")
	}()

	return task.snapshot(), nil
}

// CancelPieceDownload stops one torrent's in-flight piece download. It is
// idempotent: a torrent with no piece download, or whose download already
// reached a terminal state, is left untouched and reports no error. Pieces
// already fetched before the cancel remain in the cache.
func (s *Session) CancelPieceDownload(id string) error {
	hash, err := parseInfoHash(id)
	if err != nil {
		return ErrUnknownTorrent
	}

	s.mu.RLock()
	activeErr := s.ensureActiveLocked()
	entry := s.states[hash]
	st := s.torrents[hash]
	s.mu.RUnlock()
	if activeErr != nil {
		return activeErr
	}
	if entry == nil {
		return ErrUnknownTorrent
	}
	if st == nil {
		return nil
	}
	st.mu.Lock()
	task := st.pieceDL
	st.mu.Unlock()
	if task == nil {
		return nil
	}
	task.stop(PieceDownloadCancelled)
	return nil
}

// PieceDownloadStatus returns the current or most recent piece download of
// one torrent. It reports ErrNoPieceDownload when the torrent exists but no
// piece download has ever been started on it in this process.
func (s *Session) PieceDownloadStatus(id string) (PieceDownloadStatus, error) {
	hash, err := parseInfoHash(id)
	if err != nil {
		return PieceDownloadStatus{}, ErrUnknownTorrent
	}

	s.mu.RLock()
	activeErr := s.ensureActiveLocked()
	entry := s.states[hash]
	st := s.torrents[hash]
	s.mu.RUnlock()
	if activeErr != nil {
		return PieceDownloadStatus{}, activeErr
	}
	if entry == nil {
		return PieceDownloadStatus{}, ErrUnknownTorrent
	}
	if st == nil {
		return PieceDownloadStatus{}, ErrNoPieceDownload
	}
	st.mu.Lock()
	task := st.pieceDL
	st.mu.Unlock()
	if task == nil {
		return PieceDownloadStatus{}, ErrNoPieceDownload
	}
	return task.snapshot(), nil
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

// pieceByteSize returns the byte size of piece index, which is shorter than
// pieceLength only for the torrent's final piece.
func pieceByteSize(index int, pieceLength, torrentLength int64) int64 {
	start := int64(index) * pieceLength
	remaining := torrentLength - start
	if remaining < pieceLength {
		return remaining
	}
	return pieceLength
}

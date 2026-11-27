package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/handoff"
)

var (
	// ErrHandoffUnconfigured means no handoff target has an endpoint.
	ErrHandoffUnconfigured = errors.New("handoff target is not configured")
	// ErrHandoffInProgress means the torrent already has a running handoff.
	ErrHandoffInProgress = errors.New("torrent handoff is in progress")
)

// Operation states of a handoff. A handoff ends in one of three ways: the
// torrent is gone (completed), the handoff failed and the torrent is seeding
// again (restored), or the handoff succeeded but the local deletion failed
// (failed).
const (
	StateRunning   TorrentState = "running"
	StateCompleted TorrentState = "completed"
	StateRestored  TorrentState = "restored"
	StateFailed    TorrentState = "failed"
)

// Stable machine-readable reasons a handoff did not reach completion.
const (
	HandoffCodeUnreachable = "handoff_unreachable"
	HandoffCodeAuthFailed  = "handoff_auth_failed"
	HandoffCodeRejected    = "handoff_rejected"
)

// handoffTimeout bounds one delivery to the target. It is fixed rather than
// configurable: the target is a local or LAN service, and a longer wait would
// only extend the pause window of the torrent.
const handoffTimeout = handoff.DefaultTimeout

// HandoffTargetView is the management API's view of the configured target.
type HandoffTargetView struct {
	// Configured is false when no endpoint is set; every handoff entry point
	// is disabled then.
	Configured bool
	// Target is the configured endpoint URL. The dialog shows it so an
	// operator can tell which downloader will receive the torrent; it is
	// empty while no endpoint is configured.
	Target string
	// Version is the version the target reported, when the view came from a
	// connection test.
	Version string
}

// HandoffTarget reports whether a handoff target is configured.
func (s *Session) HandoffTarget() HandoffTargetView {
	target := s.Config().Handoff.Transmission
	return HandoffTargetView{Configured: target.Configured(), Target: target.URL}
}

// TestHandoffTarget connects to the configured target and reports its version.
// It is what the configuration page asks after a save, so an operator can tell
// a wrong endpoint or password from a target that is merely slow.
func (s *Session) TestHandoffTarget(ctx context.Context) (HandoffTargetView, error) {
	target := s.Config().Handoff.Transmission
	view := HandoffTargetView{Configured: target.Configured(), Target: target.URL}
	if !view.Configured {
		return view, ErrHandoffUnconfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, handoffTimeout)
	defer cancel()
	version, err := newHandoffTarget(target).Version(ctx)
	if err != nil {
		return view, fmt.Errorf("session: test handoff target: %w", err)
	}
	view.Version = version
	return view, nil
}

// HandoffTorrent hands one ready torrent off to the configured target and, on
// success, deletes the local torrent. The delivery and the deletion run in the
// background; the returned operation observes both.
func (s *Session) HandoffTorrent(ctx context.Context, id string) (*Operation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hash, err := parseInfoHash(id)
	if err != nil {
		s.logger.Error("torrent handoff failed", "op", "parse-id", "err", ErrUnknownTorrent)
		return nil, ErrUnknownTorrent
	}
	target := s.Config().Handoff.Transmission
	if !target.Configured() {
		return nil, ErrHandoffUnconfigured
	}
	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if opID, ok := s.activeOps[hash]; ok {
		if op, ok := s.operations[opID]; ok && op.State == StateRunning {
			s.mu.Unlock()
			return nil, ErrHandoffInProgress
		}
	}
	entry, ok := s.states[hash]
	if !ok {
		s.mu.Unlock()
		return nil, ErrUnknownTorrent
	}
	switch entry.State {
	case StateReady:
	case StateDeleting, StateDeleteFailed:
		s.mu.Unlock()
		return nil, ErrDeleting
	default:
		s.mu.Unlock()
		return nil, ErrTorrentNotReady
	}
	st, ok := s.torrents[hash]
	if !ok || st == nil {
		s.mu.Unlock()
		return nil, ErrTorrentNotReady
	}
	opID, err := newOperationID()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	now := time.Now().UTC()
	op := &Operation{ID: opID, TorrentID: hash.HexString(), State: StateRunning, CreatedAt: now, UpdatedAt: now}
	s.operations[opID] = op
	s.activeOps[hash] = opID
	s.lastOps[hash] = opID

	s.bgMu.Lock()
	s.bgWg.Add(1)
	s.bgMu.Unlock()
	out := op.clone()
	s.mu.Unlock()

	s.logger.Info("torrent handoff started", "hash", hash.HexString(), "operation_id", opID)
	go func() {
		defer s.bgWg.Done()
		s.runHandoff(hash, st, opID, target)
	}()
	return &out, nil
}

// runHandoff pauses the torrent, delivers its metainfo, and deletes the local
// torrent once the target accepted it. A failed delivery restores the torrent
// instead: the connection allowance is handed back, and the next allocator
// pass re-establishes peers. A deletion failure keeps the restore: the target
// already serves the torrent, so rolling back would leave two half-seeded
// copies.
func (s *Session) runHandoff(hash metainfo.Hash, st *Torrent, opID string, target config.HandoffTransmission) {
	st.pauseForHandoff()
	meta, err := s.handoffMetainfo(hash, st)
	if err == nil {
		err = s.deliverHandoff(meta, target)
	}
	if err != nil {
		st.resumeAfterHandoff()
		s.kickConnectionAllocator()
		s.finishHandoff(hash, opID, StateRestored, err, HandoffErrorCode(err))
		s.logger.Warn("torrent handoff restored",
			"hash", hash.HexString(),
			"operation_id", opID,
			"err", err)
		return
	}
	deleting, ok := s.beginHandoffDeletion(hash, opID)
	if !ok {
		s.finishHandoff(hash, opID, StateCompleted, nil, "")
		return
	}
	s.runDelete(hash, deleting, opID)
	s.finishHandoffDeletion(opID)
}

// handoffMetainfo returns the torrent's metainfo file bytes: the disk copy the
// torrent was added from, or the in-memory metainfo when the registries
// disagree.
func (s *Session) handoffMetainfo(hash metainfo.Hash, st *Torrent) ([]byte, error) {
	if data, err := os.ReadFile(s.finalMetainfoPath(hash)); err == nil {
		return data, nil
	}
	if st != nil && st.Info() != nil {
		var buffer bytes.Buffer
		meta := st.tor.Metainfo()
		if err := meta.Write(&buffer); err == nil {
			return buffer.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("%w: metainfo is unavailable", handoff.ErrRejected)
}

// deliverHandoff sends the metainfo to the target, bounded by the handoff
// timeout and by the session's background context so a shutdown cannot leave
// a delivery in flight.
func (s *Session) deliverHandoff(metainfo []byte, target config.HandoffTransmission) error {
	ctx, cancel := context.WithTimeout(s.bgCtx, handoffTimeout)
	defer cancel()
	return newHandoffTarget(target).Add(ctx, metainfo)
}

func newHandoffTarget(target config.HandoffTransmission) *handoff.Target {
	return handoff.New(handoff.Config{
		URL:      target.URL,
		Username: target.Username,
		Password: target.Password,
	})
}

// beginHandoffDeletion flips the durable entry to deleting and detaches the
// torrent from the session, so a crash during the deletion is resumed as an
// ordinary deletion. The returned torrent is the one the caller must delete;
// false means the entry vanished meanwhile and there is nothing left to do.
func (s *Session) beginHandoffDeletion(hash metainfo.Hash, opID string) (*Torrent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.states[hash]
	if !ok {
		return nil, false
	}
	entry = cloneRegistryEntry(entry)
	entry.State = StateDeleting
	entry.Error = ""
	entry.OperationID = opID
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		// The deletion still runs: the write only makes a crash resumable,
		// and removeRegistryEntryLocked deletes the file at the end.
		s.logger.Error("persist handoff deletion", "hash", hash.HexString(), "err", err)
	}
	s.states[hash] = entry
	st := s.torrents[hash]
	delete(s.torrents, hash)
	return st, true
}

// finishHandoffDeletion maps the deletion outcome recorded by runDelete onto
// the handoff operation. A completed deletion left no torrent behind; a failed
// one leaves the torrent in delete_failed while the handoff itself succeeded.
func (s *Session) finishHandoffDeletion(opID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[opID]
	if op == nil {
		return
	}
	switch op.State {
	case StateDeleted:
		op.State = StateCompleted
		op.Error = ""
		op.ErrorCode = ""
	case StateDeleteFailed:
		op.State = StateFailed
	}
	op.UpdatedAt = time.Now().UTC()
}

func (s *Session) finishHandoff(hash metainfo.Hash, opID string, state TorrentState, cause error, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[opID]
	if op == nil {
		return
	}
	op.State = state
	op.ErrorCode = code
	if cause != nil {
		op.Error = cause.Error()
	} else {
		op.Error = ""
	}
	op.UpdatedAt = time.Now().UTC()
	delete(s.activeOps, hash)
}

// HandoffErrorCode maps a delivery failure onto the stable code the API and
// the interface show.
func HandoffErrorCode(err error) string {
	switch {
	case errors.Is(err, handoff.ErrUnreachable):
		return HandoffCodeUnreachable
	case errors.Is(err, handoff.ErrAuthFailed):
		return HandoffCodeAuthFailed
	case errors.Is(err, handoff.ErrRejected):
		return HandoffCodeRejected
	default:
		return HandoffCodeRejected
	}
}

// pauseForHandoff stops every peer connection of the torrent and pins its
// allowance at zero. It sends no announce: a successful handoff is followed by
// the deletion pipeline, which reports stopped itself (ADR 0008), and a
// restored torrent must not be missing from the swarm for a whole announce
// interval.
func (t *Torrent) pauseForHandoff() {
	t.handoffPaused.Store(true)
	t.applyHandoffPause()
}

// resumeAfterHandoff releases the pause. The next allocator pass grants the
// allowance the torrent's activity calls for.
func (t *Torrent) resumeAfterHandoff() {
	t.handoffPaused.Store(false)
}

// applyHandoffPause closes the peer connections once and records the zero
// allowance so the allocator's bookkeeping matches the client.
func (t *Torrent) applyHandoffPause() {
	if t.connAllowance.Swap(0) == 0 {
		return
	}
	t.connState.Store(int32(connectionStateIdle))
	t.tor.SetMaxEstablishedConns(0)
}

// handoffConfigKeys lists the configuration keys the session applies to the
// running handoff target.
var handoffConfigKeys = []string{
	"handoff.transmission.url",
	"handoff.transmission.username",
	"handoff.transmission.password",
}

// HandoffConfigKeys returns the configuration keys the session can apply
// without a restart.
func HandoffConfigKeys() []string {
	keys := make([]string, len(handoffConfigKeys))
	copy(keys, handoffConfigKeys)
	return keys
}

// ApplyHandoffConfig replaces the handoff target values in the session's
// current configuration snapshot. Every handoff call builds its target from
// the snapshot, so no client object needs rebuilding.
func (s *Session) ApplyHandoffConfig(cfg config.Config) error {
	current := s.Config()
	if err := config.MergeKeys(&current, cfg, handoffConfigKeys); err != nil {
		return err
	}
	s.SetConfig(current)
	return nil
}

// HandoffApplier applies the handoff target configuration keys to a running
// session.
type HandoffApplier struct {
	session *Session
}

// NewHandoffApplier returns the applier of the handoff keys to session.
func NewHandoffApplier(session *Session) *HandoffApplier {
	return &HandoffApplier{session: session}
}

// Keys returns the configuration keys the applier owns.
func (a *HandoffApplier) Keys() []string { return HandoffConfigKeys() }

// Apply applies the handoff values of cfg to the running session.
func (a *HandoffApplier) Apply(cfg config.Config) error {
	return a.session.ApplyHandoffConfig(cfg)
}

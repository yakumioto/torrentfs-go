package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	PlaybackEventProgress = "progress"
	PlaybackEventSeek     = "seek"
)

var playbackLeaseDuration = 30 * time.Second

var (
	ErrPlaybackStreamNotFound   = errors.New("session: playback stream not found")
	ErrPlaybackSequenceConflict = errors.New("session: playback sequence conflict")
	ErrPlaybackPositionInvalid  = errors.New("session: invalid playback position")
	ErrPlaybackEventInvalid     = errors.New("session: invalid playback event")
)

// PlaybackStreamStart creates a stream bound to one torrent file.
type PlaybackStreamStart struct {
	Path          string
	PositionBytes int64
}

// PlaybackStreamUpdate advances or seeks a playback stream.
type PlaybackStreamUpdate struct {
	Sequence      uint64
	Event         string
	PositionBytes int64
}

// PlaybackStartRequest is an alias for the stream creation request.
type PlaybackStartRequest = PlaybackStreamStart

// PlaybackUpdateRequest is an alias for the stream update request.
type PlaybackUpdateRequest = PlaybackStreamUpdate

// PlaybackStreamSnapshot is an immutable view of one playback stream.
type PlaybackStreamSnapshot struct {
	ID                    string
	TorrentID             string
	Path                  string
	Generation            uint64
	PlaybackCursor        int64
	PlaybackConsumedBytes int64
	UsefulDownloadBytes   int64
	CacheResidentBytes    int64
	BufferedBytes         int64
	TargetLow             int64
	TargetHigh            int64
	EffectiveLow          int64
	EffectiveHigh         int64
	State                 string
	LastSequence          uint64
	LastUpdate            time.Time
	ExpiresAt             time.Time
	ForegroundPieces      []int
	BackgroundPieces      []int
	PinnedPieces          []int
}

type playbackSessionStream struct {
	id      string
	torrent *Torrent
	file    *openedFile
}

func newPlaybackStreamID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("session: generate playback stream id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// StartPlaybackStream creates an authenticated-control-plane playback stream.
func (s *Session) StartPlaybackStream(ctx context.Context, torrentID string, request PlaybackStreamStart) (PlaybackStreamSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PlaybackStreamSnapshot{}, err
	}
	if request.PositionBytes < 0 {
		return PlaybackStreamSnapshot{}, ErrPlaybackPositionInvalid
	}
	hash, err := parseInfoHash(torrentID)
	if err != nil {
		return PlaybackStreamSnapshot{}, ErrUnknownTorrent
	}

	s.mu.RLock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.RUnlock()
		return PlaybackStreamSnapshot{}, err
	}
	t, ok := s.torrents[hash]
	entry := s.states[hash]
	ready := ok && entry != nil && entry.State == StateReady
	s.mu.RUnlock()
	if !ready {
		return PlaybackStreamSnapshot{}, ErrUnknownTorrent
	}

	id, err := newPlaybackStreamID()
	if err != nil {
		return PlaybackStreamSnapshot{}, err
	}
	stream, snapshot, err := t.startPlaybackStream(id, request)
	if err != nil {
		return PlaybackStreamSnapshot{}, err
	}

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		_ = t.coordinator.stopPlaybackStream(id)
		_ = stream.file.Close()
		return PlaybackStreamSnapshot{}, err
	}
	if s.playbackStreams == nil {
		s.playbackStreams = make(map[string]*playbackSessionStream)
	}
	s.playbackStreams[id] = stream
	s.mu.Unlock()
	return snapshot, nil
}

// UpdatePlaybackStream applies one ordered progress or seek event.
func (s *Session) UpdatePlaybackStream(ctx context.Context, id string, request PlaybackStreamUpdate) (PlaybackStreamSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PlaybackStreamSnapshot{}, err
	}
	s.mu.RLock()
	stream := s.playbackStreams[id]
	s.mu.RUnlock()
	if stream == nil {
		return PlaybackStreamSnapshot{}, ErrPlaybackStreamNotFound
	}
	snapshot, err := stream.torrent.coordinator.updatePlaybackStream(id, request)
	if errors.Is(err, ErrPlaybackStreamNotFound) {
		s.removeExpiredPlaybackStream(id, stream)
	}
	return snapshot, err
}

// StopPlaybackStream releases one stream's pins, background ownership, and file handle.
func (s *Session) StopPlaybackStream(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	stream := s.playbackStreams[id]
	if stream != nil {
		delete(s.playbackStreams, id)
	}
	s.mu.Unlock()
	if stream == nil {
		return ErrPlaybackStreamNotFound
	}

	err := stream.torrent.coordinator.stopPlaybackStream(id)
	if closeErr := stream.file.Close(); err == nil {
		err = closeErr
	} else if closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return err
}

func (s *Session) removeExpiredPlaybackStream(id string, stream *playbackSessionStream) {
	s.mu.Lock()
	if current := s.playbackStreams[id]; current == stream {
		delete(s.playbackStreams, id)
	}
	s.mu.Unlock()
	_ = stream.file.Close()
}

func (s *Session) playbackSnapshotsFor(t *Torrent) []PlaybackStreamSnapshot {
	if t == nil || t.coordinator == nil {
		return nil
	}
	return t.coordinator.playbackSnapshots()
}

func (s *Session) detachPlaybackStreamsForTorrent(t *Torrent) []*playbackSessionStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	var detached []*playbackSessionStream
	for id, stream := range s.playbackStreams {
		if stream.torrent != t {
			continue
		}
		detached = append(detached, stream)
		delete(s.playbackStreams, id)
	}
	return detached
}

func (t *Torrent) startPlaybackStream(id string, request PlaybackStreamStart) (*playbackSessionStream, PlaybackStreamSnapshot, error) {
	opened, err := t.readerFor(request.Path)
	if err != nil {
		return nil, PlaybackStreamSnapshot{}, err
	}
	stream := &playbackSessionStream{id: id, torrent: t, file: opened}
	onExpire := func() {
		if t.session != nil {
			t.session.removeExpiredPlaybackStream(id, stream)
		}
	}
	snapshot, err := t.coordinator.startPlaybackStream(id, request.Path, opened.file, request.PositionBytes, onExpire)
	if err != nil {
		_ = opened.Close()
		return nil, PlaybackStreamSnapshot{}, err
	}
	return stream, snapshot, nil
}

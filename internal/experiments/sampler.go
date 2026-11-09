package experiments

import (
	"context"
	"time"
)

// PeerSnapshot is one torrent's live peer gauges and real payload counters at a
// sampling instant.
type PeerSnapshot struct {
	InfoHash         string
	UploadedBytes    int64
	DownloadedBytes  int64
	TotalPeers       int
	PendingPeers     int
	ActivePeers      int
	ConnectedSeeders int
}

// PeerSampler periodically records peer activity and real payload counters into
// the announce log. It is the peer-connection history that a tracker-side
// detector can be compared against: an announce whose uploaded delta cannot be
// explained by any real transfer is the anomaly under test.
type PeerSampler struct {
	recorder *Recorder
	interval time.Duration
	snapshot func() []PeerSnapshot
	now      func() time.Time
}

// NewPeerSampler builds a sampler. A non-positive interval falls back to the
// default sampling period. recorder and snapshot may be nil, in which case Run
// returns immediately.
func NewPeerSampler(recorder *Recorder, interval time.Duration, snapshot func() []PeerSnapshot) *PeerSampler {
	if interval <= 0 {
		interval = defaultPeerSampleInterval
	}
	return &PeerSampler{
		recorder: recorder,
		interval: interval,
		snapshot: snapshot,
		now:      time.Now,
	}
}

// Run samples once immediately and then on every interval until ctx is done.
func (s *PeerSampler) Run(ctx context.Context) {
	if s == nil || s.recorder == nil || s.snapshot == nil || s.interval <= 0 {
		return
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.sample()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sample()
		}
	}
}

// sample records one snapshot of every torrent.
func (s *PeerSampler) sample() {
	now := s.now()
	for _, snapshot := range s.snapshot() {
		s.recorder.WritePeerSample(PeerSampleRecord{
			Time:             now,
			InfoHash:         snapshot.InfoHash,
			UploadedBytes:    snapshot.UploadedBytes,
			DownloadedBytes:  snapshot.DownloadedBytes,
			TotalPeers:       snapshot.TotalPeers,
			PendingPeers:     snapshot.PendingPeers,
			ActivePeers:      snapshot.ActivePeers,
			ConnectedSeeders: snapshot.ConnectedSeeders,
		})
	}
}

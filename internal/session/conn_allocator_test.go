package session

import (
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// testConnectionPolicy uses the production defaults, so a default change shows
// up here instead of silently drifting away from a hand-copied value.
func testConnectionPolicy() (config.Connections, connectionAllocatorTiming) {
	return config.Default().Connections, newConnectionAllocator().timing
}

func TestMetadataRotationInterval(t *testing.T) {
	for _, tt := range []struct {
		rotations int
		want      time.Duration
	}{
		{rotations: 0, want: 10 * time.Second},
		{rotations: 1, want: 20 * time.Second},
		{rotations: 2, want: 30 * time.Second},
		{rotations: 3, want: 60 * time.Second},
		{rotations: 9, want: 60 * time.Second},
	} {
		if got := metadataRotationInterval(tt.rotations); got != tt.want {
			t.Errorf("metadataRotationInterval(%d) = %v, want %v", tt.rotations, got, tt.want)
		}
	}
}

func TestConnectionPolicyStates(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	conn, timing := testConnectionPolicy()
	const length = int64(1 << 30)
	const cached = int64(1 << 20)

	tests := []struct {
		name     string
		activity connectionActivity
		want     connectionState
		allow    int
	}{
		{
			name:     "a fresh magnet gets the metadata allowance",
			activity: connectionActivity{HasInfo: false, AddedAt: now.Add(-time.Second)},
			want:     connectionStateMetadata,
			allow:    metadataConns,
		},
		{
			name: "a slow magnet keeps one metadata connection",
			activity: connectionActivity{
				HasInfo:           false,
				AddedAt:           now.Add(-metadataFastWindow - time.Minute),
				MetadataRotatedAt: now.Add(-time.Second),
				MetadataRotations: 4,
			},
			want:  connectionStateMetadata,
			allow: metadataSlowConns,
		},
		{
			name:     "a magnet rotates its metadata peers when the interval elapses",
			activity: connectionActivity{HasInfo: false, AddedAt: now.Add(-11 * time.Second)},
			want:     connectionStateMetadata,
			allow:    0,
		},
		{
			name: "the metadata allowance stays closed during a rotation",
			activity: connectionActivity{
				HasInfo:             false,
				AddedAt:             now.Add(-time.Minute),
				MetadataPausedUntil: now.Add(time.Second),
			},
			want:  connectionStateMetadata,
			allow: 0,
		},
		{
			name: "metadata connections reopen after a rotation",
			activity: connectionActivity{
				HasInfo:           false,
				AddedAt:           now.Add(-time.Minute),
				MetadataRotatedAt: now.Add(-time.Second),
				MetadataRotations: 1,
			},
			want:  connectionStateMetadata,
			allow: metadataConns,
		},
		{
			name:     "a read torrent gets the reading allowance",
			activity: connectionActivity{HasInfo: true, ReadingActivity: true, CachedBytes: cached},
			want:     connectionStateReading,
			allow:    conn.ReadingConns,
		},
		{
			name:     "a fully cached torrent does not use the reading allowance",
			activity: connectionActivity{HasInfo: true, ReadingActivity: true, Complete: true, CachedBytes: length},
			want:     connectionStateProbe,
			allow:    probeConns,
		},
		{
			name:     "a recent upload gets the uploading allowance",
			activity: connectionActivity{HasInfo: true, LastUpload: now.Add(-time.Minute), CachedBytes: cached},
			want:     connectionStateUploading,
			allow:    conn.UploadingConns,
		},
		{
			name: "an expired upload falls back to the probe",
			activity: connectionActivity{
				HasInfo:       true,
				LastUpload:    now.Add(-timing.uploadGrace - time.Minute),
				CachedBytes:   cached,
				PreviousState: connectionStateUploading,
			},
			want:  connectionStateProbe,
			allow: probeConns,
		},
		{
			name: "a cached quiet torrent opens a fresh probe slot",
			activity: connectionActivity{
				HasInfo:       true,
				CachedBytes:   cached,
				PreviousState: connectionStateReading,
			},
			want:  connectionStateProbe,
			allow: probeConns,
		},
		{
			name: "a quiet probe slot is rotated",
			activity: connectionActivity{
				HasInfo:       true,
				CachedBytes:   cached,
				PreviousState: connectionStateProbe,
				ProbeSince:    now.Add(-timing.probeRotate - time.Second),
			},
			want:  connectionStateProbe,
			allow: 0,
		},
		{
			name: "the probe slot stays closed during a rotation",
			activity: connectionActivity{
				HasInfo:          true,
				CachedBytes:      cached,
				PreviousState:    connectionStateProbe,
				ProbeSince:       now.Add(-time.Minute),
				ProbePausedUntil: now.Add(10 * time.Second),
			},
			want:  connectionStateProbe,
			allow: 0,
		},
		{
			name: "the probe slot reopens after a rotation",
			activity: connectionActivity{
				HasInfo:          true,
				CachedBytes:      cached,
				PreviousState:    connectionStateProbe,
				ProbeSince:       now.Add(-timing.probePause - time.Second),
				ProbePausedUntil: now.Add(-time.Second),
			},
			want:  connectionStateProbe,
			allow: probeConns,
		},
		{
			name:     "a torrent with no cache and no read is idle",
			activity: connectionActivity{HasInfo: true},
			want:     connectionStateIdle,
			allow:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := connectionPolicy(now, tt.activity, conn, timing)
			if plan.State != tt.want {
				t.Fatalf("state = %s, want %s", plan.State, tt.want)
			}
			if plan.Allowance != tt.allow {
				t.Fatalf("allowance = %d, want %d", plan.Allowance, tt.allow)
			}
		})
	}
}

// TestProbeRotationStartsWhenTheSlotOpens checks the rotation timer starts at
// the probe entry, not at the last read or upload: a slot opened after a long
// reading session must still get the full quiet interval.
func TestProbeRotationStartsWhenTheSlotOpens(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	conn, timing := testConnectionPolicy()
	const length = int64(1 << 30)

	plan := connectionPolicy(now, connectionActivity{
		HasInfo:         true,
		CachedBytes:     1 << 20,
		PreviousState:   connectionStateReading,
		LastUpload:      now.Add(-time.Hour),
		ReadingActivity: false,
	}, conn, timing)
	if plan.State != connectionStateProbe || plan.Allowance != probeConns {
		t.Fatalf("fresh probe = (%s, %d), want (probe, %d)", plan.State, plan.Allowance, probeConns)
	}
	if !plan.ProbeSince.Equal(now) {
		t.Fatalf("probe since = %v, want the probe entry time %v", plan.ProbeSince, now)
	}

	// A full interval later the slot rotates, exactly from the entry time.
	rotation := connectionPolicy(now.Add(timing.probeRotate+time.Second), connectionActivity{
		HasInfo:       true,
		CachedBytes:   1 << 20,
		PreviousState: connectionStateProbe,
		ProbeSince:    plan.ProbeSince,
	}, conn, timing)
	if rotation.Allowance != 0 || !rotation.ProbePausedUntil.Equal(now.Add(timing.probeRotate+time.Second+timing.probePause)) {
		t.Fatalf("rotation = (%d, pause until %v), want (0, %v)",
			rotation.Allowance, rotation.ProbePausedUntil, now.Add(timing.probeRotate+time.Second+timing.probePause))
	}
}

func TestReadingAllowanceExcludesCompleteContent(t *testing.T) {
	conn, _ := testConnectionPolicy()
	if _, ok := readingAllowance(true, true, conn); ok {
		t.Fatal("fully cached content got a reading allowance")
	}
	if plan, ok := readingAllowance(true, false, conn); !ok || plan.State != connectionStateReading || plan.Allowance != conn.ReadingConns {
		t.Fatalf("reading plan = (%+v, %t), want a reading plan", plan, ok)
	}
	if _, ok := readingAllowance(false, false, conn); ok {
		t.Fatal("idle content got a reading allowance")
	}
}

// TestConnectionPolicyDowngradeTimeline walks the path a torrent takes after
// playback stops: the reading allowance holds through ReadGrace, then the
// uploading allowance or the probe applies, and a quiet probe is rotated.
func TestConnectionPolicyDowngradeTimeline(t *testing.T) {
	start := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	conn, timing := testConnectionPolicy()
	const length = int64(1 << 30)
	const cached = int64(1 << 20)

	activity := connectionActivity{HasInfo: true, ReadingActivity: true, CachedBytes: cached}
	plan := connectionPolicy(start, activity, conn, timing)
	if plan.State != connectionStateReading || plan.Allowance != conn.ReadingConns {
		t.Fatalf("while reading = (%s, %d), want (reading, %d)", plan.State, plan.Allowance, conn.ReadingConns)
	}

	// The handle closes at start+10m; within ReadGrace the allowance is
	// unchanged, so a pause or a seek does not tear the connections down.
	closedAt := start.Add(10 * time.Minute)
	activity = connectionActivity{HasInfo: true, ReadingActivity: true, CachedBytes: cached, PreviousState: connectionStateReading}
	if got := connectionPolicy(closedAt.Add(time.Minute), activity, conn, timing); got.Allowance != conn.ReadingConns {
		t.Fatalf("within the read grace allowance = %d, want %d", got.Allowance, conn.ReadingConns)
	}

	// ReadGrace expires without any upload: the torrent drops to one probe slot
	// and the slot's quiet period starts now.
	stoppedAt := closedAt.Add(time.Duration(conn.ReadGrace) + time.Second)
	activity = connectionActivity{HasInfo: true, CachedBytes: cached, PreviousState: connectionStateReading}
	plan = connectionPolicy(stoppedAt, activity, conn, timing)
	if plan.State != connectionStateProbe || plan.Allowance != probeConns || !plan.ProbeSince.Equal(stoppedAt) {
		t.Fatalf("after the read grace = (%s, %d, since %v), want (probe, %d, since %v)",
			plan.State, plan.Allowance, plan.ProbeSince, probeConns, stoppedAt)
	}
	activity.PreviousState, activity.ProbeSince = plan.State, plan.ProbeSince

	// An upload observed while the probe is open promotes the torrent to the
	// uploading allowance.
	uploadAt := stoppedAt.Add(time.Minute)
	activity.LastUpload = uploadAt
	activity.PreviousState = connectionStateProbe
	plan = connectionPolicy(uploadAt, activity, conn, timing)
	if plan.State != connectionStateUploading || plan.Allowance != conn.UploadingConns {
		t.Fatalf("while uploading = (%s, %d), want (uploading, %d)", plan.State, plan.Allowance, conn.UploadingConns)
	}

	// Once the upload grace expires the torrent falls back to a fresh probe.
	idleAt := uploadAt.Add(timing.uploadGrace + time.Second)
	activity.PreviousState = plan.State
	plan = connectionPolicy(idleAt, activity, conn, timing)
	if plan.State != connectionStateProbe || plan.Allowance != probeConns || !plan.ProbeSince.Equal(idleAt) {
		t.Fatalf("after the upload grace = (%s, %d, since %v), want (probe, %d, since %v)",
			plan.State, plan.Allowance, plan.ProbeSince, probeConns, idleAt)
	}
	activity.PreviousState, activity.ProbeSince = plan.State, plan.ProbeSince

	// A full quiet interval later the slot rotates, then reopens.
	rotateAt := idleAt.Add(timing.probeRotate + time.Second)
	plan = connectionPolicy(rotateAt, activity, conn, timing)
	if plan.State != connectionStateProbe || plan.Allowance != 0 || !plan.ProbePausedUntil.Equal(rotateAt.Add(timing.probePause)) {
		t.Fatalf("rotation = (%s, %d, pause until %v), want (probe, 0, %v)",
			plan.State, plan.Allowance, plan.ProbePausedUntil, rotateAt.Add(timing.probePause))
	}
	activity.PreviousState, activity.ProbeSince, activity.ProbePausedUntil = plan.State, plan.ProbeSince, plan.ProbePausedUntil
	reopened := connectionPolicy(rotateAt.Add(timing.probePause+time.Second), activity, conn, timing)
	if reopened.State != connectionStateProbe || reopened.Allowance != probeConns {
		t.Fatalf("after the rotation = (%s, %d), want (probe, %d)", reopened.State, reopened.Allowance, probeConns)
	}
}

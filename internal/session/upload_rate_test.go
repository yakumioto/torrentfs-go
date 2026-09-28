package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

const testUploadRate = 1 << 20

func scheduledUploadPolicy(t *testing.T, start, end string) uploadRatePolicy {
	t.Helper()
	policy, err := newUploadRatePolicy(config.Upload{
		RateLimitBytesPerSecond: testUploadRate,
		Schedule:                config.UploadSchedule{Start: start, End: end},
	})
	if err != nil {
		t.Fatalf("newUploadRatePolicy: %v", err)
	}
	return policy
}

// TestUploadRatePolicyBoundaries pins the half-open window: the start minute is
// limited and the end minute is not, in the location of the observed time.
func TestUploadRatePolicyBoundaries(t *testing.T) {
	policy := scheduledUploadPolicy(t, "08:00", "22:00")
	loc := time.FixedZone("test", 8*60*60)
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 3, 1, hour, minute, 0, 0, loc)
	}

	tests := []struct {
		name    string
		when    time.Time
		limited bool
	}{
		{name: "midnight", when: at(0, 0), limited: false},
		{name: "one minute before start", when: at(7, 59), limited: false},
		{name: "start minute", when: at(8, 0), limited: true},
		{name: "just after start", when: at(8, 1), limited: true},
		{name: "one minute before end", when: at(21, 59), limited: true},
		{name: "end minute", when: at(22, 0), limited: false},
		{name: "late evening", when: at(23, 59), limited: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy.limitedAt(tt.when); got != tt.limited {
				t.Fatalf("limitedAt(%s) = %t, want %t", tt.when.Format("15:04"), got, tt.limited)
			}
			want := rate.Inf
			if tt.limited {
				want = rate.Limit(testUploadRate)
			}
			if got := policy.effectiveLimit(tt.when); got != want {
				t.Fatalf("effectiveLimit(%s) = %v, want %v", tt.when.Format("15:04"), got, want)
			}
		})
	}

	transitions := []struct {
		name string
		from time.Time
		want time.Time
	}{
		{name: "before start targets today's start", from: at(7, 59), want: at(8, 0)},
		{name: "inside targets today's end", from: at(12, 0), want: at(22, 0)},
		{name: "after end targets tomorrow's start", from: at(22, 30), want: time.Date(2026, 3, 2, 8, 0, 0, 0, loc)},
	}
	for _, tt := range transitions {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy.nextTransition(tt.from); !got.Equal(tt.want) {
				t.Fatalf("nextTransition(%s) = %s, want %s", tt.from.Format(time.RFC3339), got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
		})
	}
}

// requireZone loads a tzdata zone, skipping when the host carries no tzdata so
// the DST cases never turn an environment gap into a failure.
func requireZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("zone %s is unavailable on this host: %v", name, err)
	}
	return loc
}

// TestUploadRatePolicySpringForward covers the DST gap: on the transition day
// the 02:00 boundary never exists, so the window opens at the first minute the
// clock shows (03:00) instead of resolving the missing 02:00 to an instant
// before the window and waiting for the next day's boundary.
func TestUploadRatePolicySpringForward(t *testing.T) {
	loc := requireZone(t, "America/New_York")
	// US DST starts 2026-03-08: 02:00 EST jumps straight to 03:00 EDT.
	before := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	boundary := time.Date(2026, 3, 8, 3, 0, 0, 0, loc)
	if offsetOf(t, before) == offsetOf(t, boundary) {
		t.Skip("zone has no spring-forward transition at the expected instant")
	}

	policy := scheduledUploadPolicy(t, "02:00", "22:00")
	if policy.limitedAt(before) {
		t.Fatal("policy is limited before the window opens")
	}
	got := policy.nextTransition(before)
	if !got.Equal(boundary) {
		t.Fatalf("nextTransition(%s) = %s, want the first existing window minute %s", before.Format(time.RFC3339), got.Format(time.RFC3339), boundary.Format(time.RFC3339))
	}
	if !policy.limitedAt(got) {
		t.Fatalf("policy is not limited at the resolved boundary %s", got.Format(time.RFC3339))
	}
	// No state change may hide between the two instants.
	for probe := before.Add(time.Minute); probe.Before(got); probe = probe.Add(time.Minute) {
		if policy.limitedAt(probe) {
			t.Fatalf("policy is limited at %s, before the resolved boundary", probe.Format(time.RFC3339))
		}
	}
	// The window still closes on the ordinary end boundary that day.
	if end := policy.nextTransition(got); !end.Equal(time.Date(2026, 3, 8, 22, 0, 0, 0, loc)) {
		t.Fatalf("nextTransition(%s) = %s, want the same-day end boundary", got.Format(time.RFC3339), end.Format(time.RFC3339))
	}
}

// TestUploadRatePolicyFallBack covers the repeated hour: when the clock falls
// back to 01:00 it leaves a window that opened at 01:30, so the state flips
// there rather than at the day's 02:30 that a date-based reconstruction picks.
func TestUploadRatePolicyFallBack(t *testing.T) {
	loc := requireZone(t, "America/New_York")
	// US DST ends 2026-11-01: 02:00 EDT falls back to 01:00 EST.
	early := time.Date(2026, 11, 1, 1, 0, 0, 0, loc) // 01:00 EDT, still -04:00
	late := time.Date(2026, 11, 1, 3, 0, 0, 0, loc)  // 03:00 EST, already -05:00
	if offsetOf(t, early) == offsetOf(t, late) {
		t.Skip("zone has no fall-back transition at the expected instant")
	}

	policy := scheduledUploadPolicy(t, "01:30", "02:30")
	// The repeated 01:00-01:59 hour is ambiguous in local terms, so the instants
	// under test are built from UTC and viewed in the zone.
	beforeWindow := time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC).In(loc)
	open := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC).In(loc)   // 01:30 EDT, first occurrence
	inside := time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC).In(loc) // 01:45 EDT
	flip := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC).In(loc)    // fall-back instant, back to 01:00 EST
	last := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC).In(loc)   // 02:30 EST, the day's end

	if got := policy.nextTransition(beforeWindow); !got.Equal(open) {
		t.Fatalf("nextTransition(%s) = %s, want the first occurrence %s", beforeWindow.Format(time.RFC3339), got.Format(time.RFC3339), open.Format(time.RFC3339))
	}
	if !policy.limitedAt(flip.Add(-time.Minute)) {
		t.Fatalf("policy should be limited at %s, just before the fall-back instant", flip.Add(-time.Minute).Format(time.RFC3339))
	}
	if policy.limitedAt(flip) {
		t.Fatalf("policy should be unlimited at the fall-back instant %s", flip.Format(time.RFC3339))
	}
	if got := policy.nextTransition(inside); !got.Equal(flip) {
		t.Fatalf("nextTransition(%s) = %s, want the fall-back instant %s", inside.Format(time.RFC3339), got.Format(time.RFC3339), flip.Format(time.RFC3339))
	}
	// When the repeated hour lands back inside the window there is no change at
	// the fall-back instant; the boundary stays the ordinary window end.
	spansRepeat := scheduledUploadPolicy(t, "00:30", "02:30")
	if !spansRepeat.limitedAt(flip) {
		t.Fatalf("a window spanning the repeat should still be limited at %s", flip.Format(time.RFC3339))
	}
	if got := spansRepeat.nextTransition(inside); !got.Equal(last) {
		t.Fatalf("nextTransition for a window spanning the repeat = %s, want %s", got.Format(time.RFC3339), last.Format(time.RFC3339))
	}
}

func offsetOf(t *testing.T, at time.Time) int {
	t.Helper()
	_, offset := at.Zone()
	return offset
}

func TestUploadRatePolicyWithoutSchedule(t *testing.T) {
	policy, err := newUploadRatePolicy(config.Upload{RateLimitBytesPerSecond: testUploadRate})
	if err != nil {
		t.Fatalf("newUploadRatePolicy: %v", err)
	}
	if !policy.enabled() || policy.scheduled {
		t.Fatalf("policy = %+v, want an enabled all-day limit", policy)
	}
	when := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	if !policy.limitedAt(when) || policy.effectiveLimit(when) != rate.Limit(testUploadRate) {
		t.Fatalf("all-day policy is not limited at %s", when)
	}
	if next := policy.nextTransition(when); !next.IsZero() {
		t.Fatalf("nextTransition = %s, want zero without a window", next)
	}

	disabled, err := newUploadRatePolicy(config.Upload{})
	if err != nil {
		t.Fatalf("newUploadRatePolicy: %v", err)
	}
	if disabled.enabled() {
		t.Fatalf("policy = %+v, want upload limiting disabled", disabled)
	}
	if got := disabled.effectiveLimit(when); got != rate.Inf {
		t.Fatalf("disabled effectiveLimit = %v, want rate.Inf", got)
	}
}

// TestUploadRateControllerSwitchesAtBoundaries drives the boundary loop with a
// pinned clock so each transition is observed without sleeping.
func TestUploadRateControllerSwitchesAtBoundaries(t *testing.T) {
	policy := scheduledUploadPolicy(t, "08:00", "22:00")
	controller := newUploadRateController(policy, logging.Discard())

	loc := time.FixedZone("test", 8*60*60)
	current := time.Date(2026, 3, 1, 7, 0, 0, 0, loc)
	waits := make(chan time.Duration, 4)
	release := make(chan struct{})
	controller.now = func() time.Time { return current }
	controller.wait = func(ctx context.Context, d time.Duration) bool {
		waits <- d
		select {
		case <-ctx.Done():
			return false
		case <-release:
			return true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.run(ctx)
	}()

	// The first wait is armed only after the startup policy has been applied.
	if got := <-waits; got != time.Hour {
		t.Fatalf("wait before 08:00 = %s, want 1h", got)
	}
	if controller.limitedNow() {
		t.Fatal("controller is limited before the window opens")
	}

	current = time.Date(2026, 3, 1, 8, 0, 0, 0, loc)
	release <- struct{}{}
	if got := <-waits; got != 14*time.Hour {
		t.Fatalf("wait after 08:00 = %s, want 14h", got)
	}
	if !controller.limitedNow() || controller.limiter.Limit() != rate.Limit(testUploadRate) {
		t.Fatalf("limit at 08:00 = %v, want %d", controller.limiter.Limit(), testUploadRate)
	}

	current = time.Date(2026, 3, 1, 22, 0, 0, 0, loc)
	release <- struct{}{}
	if got := <-waits; got != 10*time.Hour {
		t.Fatalf("wait after 22:00 = %s, want 10h", got)
	}
	if controller.limitedNow() {
		t.Fatal("controller is still limited after the window closes")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit after cancellation")
	}
}

// TestUploadRateControllerUsesOneSnapshotPerRound pins the boundary-round
// invariant: a single clock reading decides both the state to apply and the
// next boundary. A clock that crosses the start boundary between two reads must
// not be able to skip the window, so the wait armed from the 07:59 snapshot has
// to target the 08:00 start (one minute) and not the 22:00 window end.
func TestUploadRateControllerUsesOneSnapshotPerRound(t *testing.T) {
	policy := scheduledUploadPolicy(t, "08:00", "22:00")
	controller := newUploadRateController(policy, logging.Discard())

	loc := time.FixedZone("test", 8*60*60)
	snapshots := []time.Time{
		time.Date(2026, 3, 1, 7, 59, 0, 0, loc),
		time.Date(2026, 3, 1, 8, 0, 1, 0, loc),
	}
	var reads atomic.Int64
	controller.now = func() time.Time {
		index := int(reads.Add(1)) - 1
		if index >= len(snapshots) {
			index = len(snapshots) - 1
		}
		return snapshots[index]
	}
	waits := make(chan time.Duration, 4)
	release := make(chan struct{})
	controller.wait = func(ctx context.Context, d time.Duration) bool {
		waits <- d
		select {
		case <-ctx.Done():
			return false
		case <-release:
			return true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.run(ctx)
	}()

	if got := <-waits; got != time.Minute {
		t.Fatalf("first wait = %s, want 1m to the start boundary; a second, later clock read would have targeted the 22:00 end instead", got)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("clock reads in the first round = %d, want exactly one snapshot per round", got)
	}
	if controller.limitedNow() {
		t.Fatal("state at the 07:59 snapshot must be unlimited")
	}

	release <- struct{}{}
	if got := <-waits; got != 13*time.Hour+59*time.Minute+59*time.Second {
		t.Fatalf("second wait = %s, want 13h59m59s to the end boundary", got)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("clock reads after the second round = %d, want 2", got)
	}
	if !controller.limitedNow() {
		t.Fatal("state at the 08:00:01 snapshot must be limited")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit after cancellation")
	}
}

func TestUploadRateControllerWithoutScheduleExits(t *testing.T) {
	policy, err := newUploadRatePolicy(config.Upload{RateLimitBytesPerSecond: testUploadRate})
	if err != nil {
		t.Fatalf("newUploadRatePolicy: %v", err)
	}
	controller := newUploadRateController(policy, logging.Discard())

	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller loop did not exit without a schedule")
	}
	if controller.limiter.Limit() != rate.Limit(testUploadRate) {
		t.Fatalf("limit = %v, want %d", controller.limiter.Limit(), testUploadRate)
	}
}

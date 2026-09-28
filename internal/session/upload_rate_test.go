package session

import (
	"context"
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

package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// uploadRatePolicy is the compiled aggregate peer upload rule: one bytes/second
// limit, plus an optional same-day window in which that limit applies. Outside
// the window the effective limit is rate.Inf, i.e. unlimited.
type uploadRatePolicy struct {
	limit rate.Limit
	// scheduled reports whether a daily window constrains the limit. Without
	// one the limit applies around the clock.
	scheduled bool
	// startMinute and endMinute are minutes after local midnight. The window is
	// half-open: [startMinute, endMinute).
	startMinute int
	endMinute   int
	startLabel  string
	endLabel    string
}

// newUploadRatePolicy compiles a validated upload configuration. A non-positive
// rate compiles to the disabled policy. The error path exists for callers that
// skip validation; Load and newWithClientConfig validate first.
func newUploadRatePolicy(upload config.Upload) (uploadRatePolicy, error) {
	if upload.RateLimitBytesPerSecond <= 0 {
		return uploadRatePolicy{}, nil
	}
	policy := uploadRatePolicy{limit: rate.Limit(upload.RateLimitBytesPerSecond)}
	if upload.Schedule.Start == "" && upload.Schedule.End == "" {
		return policy, nil
	}
	start, err := config.ParseUploadScheduleTime(upload.Schedule.Start)
	if err != nil {
		return uploadRatePolicy{}, fmt.Errorf("upload.schedule.start: %w", err)
	}
	end, err := config.ParseUploadScheduleTime(upload.Schedule.End)
	if err != nil {
		return uploadRatePolicy{}, fmt.Errorf("upload.schedule.end: %w", err)
	}
	policy.scheduled = true
	policy.startMinute = start
	policy.endMinute = end
	policy.startLabel = upload.Schedule.Start
	policy.endLabel = upload.Schedule.End
	return policy, nil
}

// enabled reports whether any upload limiting is configured.
func (p uploadRatePolicy) enabled() bool { return p.limit > 0 }

// limitedAt reports whether the limit applies at t. It reads t's own location,
// so production decides on the process-local clock while tests can pin a zone.
func (p uploadRatePolicy) limitedAt(t time.Time) bool {
	if !p.scheduled {
		return true
	}
	minute := t.Hour()*60 + t.Minute()
	return minute >= p.startMinute && minute < p.endMinute
}

// effectiveLimit is the limit in force at t. A disabled policy is unlimited: a
// zero limit would reject every upload, so it must never reach a limiter.
func (p uploadRatePolicy) effectiveLimit(t time.Time) rate.Limit {
	if !p.enabled() || !p.limitedAt(t) {
		return rate.Inf
	}
	return p.limit
}

// nextTransition returns the next instant at which limitedAt changes, or the
// zero time when no window is configured. Boundaries fall on whole minutes, so
// a wakeup can re-read the clock and still land on the right state.
func (p uploadRatePolicy) nextTransition(t time.Time) time.Time {
	if !p.scheduled {
		return time.Time{}
	}
	year, month, day := t.Date()
	start := time.Date(year, month, day, p.startMinute/60, p.startMinute%60, 0, 0, t.Location())
	end := time.Date(year, month, day, p.endMinute/60, p.endMinute%60, 0, 0, t.Location())
	switch {
	case t.Before(start):
		return start
	case t.Before(end):
		return end
	default:
		return start.AddDate(0, 0, 1)
	}
}

// uploadRateController owns one session's private upload limiter and keeps it in
// step with the daily window. A transition changes only the limiter's limit:
// the client, its torrents and every in-flight upload keep running.
type uploadRateController struct {
	limiter *rate.Limiter
	policy  uploadRatePolicy
	logger  *slog.Logger

	// now and wait are seams the tests use to drive boundaries without sleeping.
	now  func() time.Time
	wait func(ctx context.Context, d time.Duration) bool
}

// newUploadRateController builds the private limiter with the limit in force at
// construction, so the client never starts under a stale policy. A zero burst
// lets the client pick a value large enough for a whole request chunk.
func newUploadRateController(policy uploadRatePolicy, logger *slog.Logger) *uploadRateController {
	now := time.Now()
	return &uploadRateController{
		limiter: rate.NewLimiter(policy.effectiveLimit(now), 0),
		policy:  policy,
		logger:  logger,
		now:     time.Now,
		wait:    waitFor,
	}
}

// configure attaches the private limiter to the client configuration. It must
// run before the client is created; the client keeps the pointer, so later
// SetLimit calls are visible to it. The default unlimited limiter is left
// untouched, which keeps an unconfigured session byte-for-byte unchanged.
func (c *uploadRateController) configure(cfg *torrent.ClientConfig) {
	cfg.UploadRateLimiter = c.limiter
}

// start registers the boundary scheduler on the session's background lifecycle.
// Only a configured window needs a goroutine: without one the limit applies
// from startup to shutdown.
func (c *uploadRateController) start(ctx context.Context, bgMu *sync.Mutex, bgWg *sync.WaitGroup) {
	if !c.policy.scheduled {
		return
	}
	bgMu.Lock()
	bgWg.Add(1)
	bgMu.Unlock()
	go func() {
		defer bgWg.Done()
		c.run(ctx)
	}()
}

// run applies the current policy and then re-evaluates it at every boundary
// until ctx ends.
func (c *uploadRateController) run(ctx context.Context) {
	c.apply(c.now())
	for {
		next := c.policy.nextTransition(c.now())
		if next.IsZero() {
			return
		}
		if !c.wait(ctx, next.Sub(c.now())) {
			return
		}
		// A late or early wakeup must land on the policy that applies now, not
		// the one that applied when the timer was armed.
		c.apply(c.now())
	}
}

// apply moves the limiter to the limit in force at when. It logs only a real
// change so a re-evaluation at startup does not repeat the ready record.
func (c *uploadRateController) apply(when time.Time) {
	limit := c.policy.effectiveLimit(when)
	if limit == c.limiter.Limit() {
		return
	}
	// SetLimit uses the limiter's own clock, so a test-supplied `when` cannot
	// disturb the token bucket's internal timing.
	c.limiter.SetLimit(limit)
	c.logger.Info("upload rate limit changed",
		"limited", limit != rate.Inf,
		"rate_limit_bytes_per_second", int64(c.policy.limit),
		"schedule_start", c.policy.startLabel,
		"schedule_end", c.policy.endLabel,
		"next_boundary", c.policy.nextTransition(when),
	)
}

// limitedNow reports whether the limiter is currently below rate.Inf.
func (c *uploadRateController) limitedNow() bool {
	return c.limiter.Limit() != rate.Inf
}

// waitFor blocks for d and reports whether the wait completed before ctx ended.
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

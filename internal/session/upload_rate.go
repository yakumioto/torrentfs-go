package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/time/rate"
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

// compileUploadRatePolicy compiles normalized settings into the runtime rule. A
// non-positive rate compiles to the disabled policy. Validation has already
// rejected a malformed window, so a boundary that does not parse is a disabled
// policy rather than a second, competing error path.
func compileUploadRatePolicy(settings UploadRateSettings) uploadRatePolicy {
	if settings.RateLimitBytesPerSecond <= 0 {
		return uploadRatePolicy{}
	}
	policy := uploadRatePolicy{limit: rate.Limit(settings.RateLimitBytesPerSecond)}
	if settings.Schedule == nil {
		return policy
	}
	start, err := parseUploadScheduleTime(settings.Schedule.Start)
	if err != nil {
		return uploadRatePolicy{}
	}
	end, err := parseUploadScheduleTime(settings.Schedule.End)
	if err != nil {
		return uploadRatePolicy{}
	}
	policy.scheduled = true
	policy.startMinute = start
	policy.endMinute = end
	policy.startLabel = settings.Schedule.Start
	policy.endLabel = settings.Schedule.End
	return policy
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

// nextTransition returns the earliest instant after t at which the limit in
// force differs from the limit at t, or the zero time when no window is
// configured.
//
// The boundary is found by searching the time line for the first instant whose
// state differs, rather than by rebuilding it from the local date. A DST
// transition breaks the date-based reconstruction: a spring-forward gap removes
// whole wall-clock times, so the boundary minute may not exist (the window
// really starts at the first minute the clock shows), and a fall-back hour
// shows the same wall clock twice, so a reconstruction can pick the wrong
// occurrence and skip the change for an hour. Searching the instant keeps this
// aligned with limitedAt for every zone, including both DST directions.
func (p uploadRatePolicy) nextTransition(t time.Time) time.Time {
	if !p.scheduled {
		return time.Time{}
	}
	const (
		probeStep    = time.Minute
		maxLookahead = 26 * time.Hour
	)
	want := p.limitedAt(t)
	previous := t
	for probe := t.Add(probeStep); probe.Sub(t) <= maxLookahead; probe = probe.Add(probeStep) {
		if p.limitedAt(probe) == want {
			previous = probe
			continue
		}
		// The change lies in (previous, probe]. Zone offsets and wall-clock
		// minutes both advance in whole seconds, so a second-resolution scan
		// finds the first changed instant without assuming the interval holds
		// only one flip.
		for candidate := previous.Add(time.Second); !candidate.After(probe); candidate = candidate.Add(time.Second) {
			if p.limitedAt(candidate) != want {
				return candidate
			}
		}
		return probe
	}
	// A validated window always flips inside the lookahead, so this is only
	// reachable for a policy the constructor rejects. Re-evaluating at the
	// horizon keeps a hypothetical bad policy self-correcting instead of
	// stopping the scheduler.
	return t.Add(maxLookahead)
}

// uploadRateController owns one session's private upload limiter and keeps it in
// step with the daily window. A transition changes only the limiter's limit:
// the client, its torrents and every in-flight upload keep running.
//
// The policy can be replaced at runtime from the settings API, so the controller
// runs for the whole session lifetime and guards the policy with its own mutex.
type uploadRateController struct {
	limiter *rate.Limiter
	logger  *slog.Logger

	// mu guards policy. Lock order across the package is Session.mu ->
	// controller.mu; the runner never takes Session.mu, and waiters hold no
	// lock while they sleep.
	mu     sync.Mutex
	policy uploadRatePolicy
	// wake has capacity one: an update makes the sleeping runner re-read the
	// policy immediately instead of waiting for a boundary of the old one.
	wake chan struct{}

	// now and wait are seams the tests use to drive boundaries without sleeping.
	now  func() time.Time
	wait uploadRateWaitFunc
}

// uploadRateWaitFunc blocks until the next boundary, a wake signal, or ctx ends.
// It reports false only when ctx ended. A zero next means the policy has no
// upcoming boundary (disabled or all-day), so the wait ends only on a wake.
type uploadRateWaitFunc func(ctx context.Context, next, now time.Time, wake <-chan struct{}) bool

// newUploadRateController builds the private limiter with the limit in force at
// construction, so the client never starts under a stale policy. A zero burst
// lets the client pick a value large enough for a whole request chunk.
func newUploadRateController(policy uploadRatePolicy, logger *slog.Logger) *uploadRateController {
	now := time.Now()
	return &uploadRateController{
		limiter: rate.NewLimiter(policy.effectiveLimit(now), 0),
		logger:  logger,
		policy:  policy,
		wake:    make(chan struct{}, 1),
		now:     time.Now,
		wait:    waitForBoundary,
	}
}

// configure attaches the private limiter to the client configuration. It must
// run before the client is created; the client keeps the pointer, so later
// SetLimit calls are visible to it. Every session installs its own limiter, even
// with limiting disabled, because the settings API can enable it later without
// replacing the pointer the client already holds.
func (c *uploadRateController) configure(cfg *torrent.ClientConfig) {
	cfg.UploadRateLimiter = c.limiter
}

// start registers the scheduler on the session's background lifecycle. It runs
// for the whole session because the policy can change at any time.
func (c *uploadRateController) start(ctx context.Context, bgMu *sync.Mutex, bgWg *sync.WaitGroup) {
	bgMu.Lock()
	bgWg.Add(1)
	bgMu.Unlock()
	go func() {
		defer bgWg.Done()
		c.run(ctx)
	}()
}

// setPolicy installs policy and applies the limit in force at when, then wakes
// the runner so it arms a boundary for the new policy instead of sleeping until
// one of the old policy's boundaries. Callers hold Session.mu, which orders
// concurrent updates against each other and against Close.
func (c *uploadRateController) setPolicy(policy uploadRatePolicy, when time.Time) {
	c.mu.Lock()
	c.policy = policy
	c.applyLocked(when)
	c.mu.Unlock()
	c.signalWake()
}

// signalWake wakes the runner without blocking when a signal is already pending.
func (c *uploadRateController) signalWake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// run applies the current policy and then re-evaluates it at every boundary or
// update until ctx ends.
//
// Each round takes a single reading of the clock and derives both the state to
// apply and the next boundary from that one snapshot. Reading the clock twice
// could straddle a boundary: the state would be applied from the earlier read
// while the next wake-up was computed from the later one, which lands after the
// whole window and leaves the limit wrong until the boundary after that.
func (c *uploadRateController) run(ctx context.Context) {
	for {
		now := c.now()
		c.mu.Lock()
		policy := c.policy
		c.applyLocked(now)
		c.mu.Unlock()
		if !c.wait(ctx, policy.nextTransition(now), now, c.wake) {
			return
		}
		// A late or early wakeup is handled by the next round: it re-reads the
		// clock and applies the policy that holds then, not the one that held
		// when the timer was armed.
	}
}

// applyLocked moves the limiter to the limit the current policy puts in force at
// when. It logs only a real change so a re-evaluation at startup does not repeat
// the ready record. The caller holds c.mu.
func (c *uploadRateController) applyLocked(when time.Time) {
	policy := c.policy
	limit := policy.effectiveLimit(when)
	if limit == c.limiter.Limit() {
		return
	}
	// SetLimit uses the limiter's own clock, so a test-supplied `when` cannot
	// disturb the token bucket's internal timing.
	c.limiter.SetLimit(limit)
	c.logger.Info("upload rate limit changed",
		"limited", limit != rate.Inf,
		"rate_limit_bytes_per_second", int64(policy.limit),
		"schedule_start", policy.startLabel,
		"schedule_end", policy.endLabel,
		"next_boundary", policy.nextTransition(when),
	)
}

// limitedNow reports whether the limiter is currently below rate.Inf.
func (c *uploadRateController) limitedNow() bool {
	return c.limiter.Limit() != rate.Inf
}

// waitForBoundary sleeps until the next boundary, a wake signal, or ctx ends.
func waitForBoundary(ctx context.Context, next, now time.Time, wake <-chan struct{}) bool {
	if next.IsZero() {
		select {
		case <-ctx.Done():
			return false
		case <-wake:
			return true
		}
	}
	d := next.Sub(now)
	if d < 0 {
		d = 0
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-wake:
		return true
	}
}

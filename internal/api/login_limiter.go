package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Login throttling constants are deliberately not configurable: the deployment
// is single-user, so the only tuning that matters is "slow enough to make
// online guessing pointless" and "short enough that a typo costs seconds".
const (
	// loginFailureThreshold is the number of consecutive failures that arms the
	// backoff. Every failure up to it still answers 401.
	loginFailureThreshold = 5
	// loginBaseBackoff is the first wait, doubling per further failure.
	loginBaseBackoff = 30 * time.Second
	// loginMaxBackoff caps the wait so a mistyped password never locks the only
	// user out for long.
	loginMaxBackoff = 5 * time.Minute
	// loginIdleTTL is how long an address may stay quiet before its counter is
	// forgotten, keeping the table bounded on a long-running process.
	loginIdleTTL = loginMaxBackoff
	// loginSweepInterval bounds how often the idle sweep walks the table.
	loginSweepInterval = time.Minute
)

type loginEntry struct {
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

// loginLimiter tracks consecutive failed logins per source address. It is
// keyed by the address the kernel reports, never by X-Forwarded-For: a header
// the client controls cannot be allowed to reset its own counter.
type loginLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	entries   map[string]*loginEntry
	lastSweep time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	if now == nil {
		now = time.Now
	}
	return &loginLimiter{now: now, entries: make(map[string]*loginEntry), lastSweep: now()}
}

// allow reports whether the address may attempt a login now. When it may not,
// the returned duration is how long the caller should wait.
func (l *loginLimiter) allow(address string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	entry, ok := l.entries[address]
	if !ok {
		return 0, true
	}
	if now.Before(entry.blockedUntil) {
		return entry.blockedUntil.Sub(now), false
	}
	return 0, true
}

func (l *loginLimiter) fail(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	entry, ok := l.entries[address]
	if !ok {
		entry = &loginEntry{}
		l.entries[address] = entry
	}
	entry.failures++
	entry.lastSeen = now
	if entry.failures >= loginFailureThreshold {
		entry.blockedUntil = now.Add(loginBackoff(entry.failures))
	}
}

func (l *loginLimiter) reset(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, address)
}

func (l *loginLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < loginSweepInterval {
		return
	}
	l.lastSweep = now
	for address, entry := range l.entries {
		if now.Sub(entry.lastSeen) > loginIdleTTL && !now.Before(entry.blockedUntil) {
			delete(l.entries, address)
		}
	}
}

// loginBackoff doubles per failure past the threshold and stops at the cap.
func loginBackoff(failures int) time.Duration {
	step := failures - loginFailureThreshold
	if step < 0 {
		return 0
	}
	if step > 8 {
		step = 8
	}
	wait := loginBaseBackoff << step
	if wait > loginMaxBackoff {
		return loginMaxBackoff
	}
	return wait
}

// loginAddress is the counter key for one request.
func loginAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

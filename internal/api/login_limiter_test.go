package api

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLoginLimiterBackoffSchedule(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	limiter := newLoginLimiter(clock.now)
	const ip = "192.0.2.10"

	for attempt := 1; attempt <= 4; attempt++ {
		limiter.fail(ip)
		if wait, allowed := limiter.allow(ip); !allowed {
			t.Fatalf("attempt %d blocked for %v, want the first four failures to stay unthrottled", attempt, wait)
		}
	}

	want := []time.Duration{30 * time.Second, 60 * time.Second, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for index, expected := range want {
		limiter.fail(ip)
		wait, allowed := limiter.allow(ip)
		if allowed {
			t.Fatalf("failure %d did not block", index+5)
		}
		if wait != expected {
			t.Fatalf("failure %d wait = %v, want %v", index+5, wait, expected)
		}
		clock.advance(expected - time.Second)
		if wait, allowed := limiter.allow(ip); allowed || wait != time.Second {
			t.Fatalf("failure %d one second before expiry: allowed=%v wait=%v, want 1s", index+5, allowed, wait)
		}
		clock.advance(time.Second)
		if wait, allowed := limiter.allow(ip); !allowed {
			t.Fatalf("failure %d still blocked for %v after expiry", index+5, wait)
		}
	}
}

func TestLoginLimiterResetsAndForgetsIdleAddresses(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	limiter := newLoginLimiter(clock.now)

	for attempt := 1; attempt <= 5; attempt++ {
		limiter.fail("192.0.2.11")
	}
	if _, allowed := limiter.allow("192.0.2.11"); allowed {
		t.Fatal("five failures did not block")
	}
	limiter.reset("192.0.2.11")
	if wait, allowed := limiter.allow("192.0.2.11"); !allowed {
		t.Fatalf("reset left the address blocked for %v", wait)
	}

	// An address that stays quiet past the backoff ceiling is swept away the
	// next time the limiter is touched.
	limiter.fail("192.0.2.12")
	clock.advance(loginIdleTTL + time.Minute)
	limiter.fail("192.0.2.13")
	limiter.mu.Lock()
	addresses := len(limiter.entries)
	limiter.mu.Unlock()
	if addresses != 1 {
		t.Fatalf("tracked addresses = %d, want the idle entry swept", addresses)
	}
}

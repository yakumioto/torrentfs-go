package auth

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const testPassword = "password"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

// testHash returns the bcrypt hash of the test password.
func testHash(t *testing.T) []byte {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	return hash
}

func newTestService(t *testing.T, clock *fakeClock, random io.Reader) *Service {
	t.Helper()
	service, err := New(config.Auth{
		Enabled:  true,
		TokenTTL: config.Duration(time.Minute),
	}, testHash(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	service.now = clock.Now
	if random != nil {
		service.random = random
	}
	return service
}

func TestLoginAndAuthenticate(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	service := newTestService(t, clock, nil)

	token, expiresAt, err := service.Login("password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(token) != 43 || strings.ContainsAny(token, ".+/=") {
		t.Fatalf("token = %q, want 32-byte raw URL-safe base64", token)
	}
	if expiresAt != clock.Now().Add(time.Minute) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, clock.Now().Add(time.Minute))
	}
	secondToken, _, err := service.Login("password")
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}
	if secondToken == token {
		t.Fatal("two successful logins returned the same token")
	}

	principal, gotExpiry, err := service.Authenticate(token, false)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal != (Principal{}) {
		t.Fatalf("principal = %+v, want the empty principal", principal)
	}
	if gotExpiry != expiresAt {
		t.Fatalf("expiry = %s, want unchanged %s", gotExpiry, expiresAt)
	}

	clock.Advance(10 * time.Second)
	_, unchangedExpiry, err := service.Authenticate(token, false)
	if err != nil {
		t.Fatalf("non-renewing Authenticate: %v", err)
	}
	if unchangedExpiry != expiresAt {
		t.Fatalf("non-renewed expiry = %s, want %s", unchangedExpiry, expiresAt)
	}

	_, renewedExpiry, err := service.Authenticate(token, true)
	if err != nil {
		t.Fatalf("renew Authenticate: %v", err)
	}
	if want := clock.Now().Add(time.Minute); renewedExpiry != want {
		t.Fatalf("renewed expiry = %s, want %s", renewedExpiry, want)
	}
}

func TestLoginRejectsInvalidPasswordsWithoutCreatingToken(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, nil)
	for _, password := range []string{"wrong", "dummy-password", ""} {
		if _, _, err := service.Login(password); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Login(%q) error = %v, want ErrInvalidCredentials", password, err)
		}
	}
	if len(service.tokens) != 0 {
		t.Fatalf("token count = %d, want 0", len(service.tokens))
	}
}

func TestAuthenticateExpiresAndRevoke(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	service := newTestService(t, clock, nil)
	token, _, err := service.Login("password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	clock.Advance(time.Minute)
	if _, _, err := service.Authenticate(token, true); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired Authenticate error = %v, want ErrInvalidToken", err)
	}
	if len(service.tokens) != 0 {
		t.Fatalf("token count after expiry = %d, want 0", len(service.tokens))
	}

	token, _, err = service.Login("password")
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}
	beforeRevoke := clock.Now().Add(time.Minute)
	if err := service.Revoke(token); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := service.Authenticate(token, false); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked Authenticate error = %v, want ErrInvalidToken", err)
	}
	if err := service.Revoke(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second Revoke error = %v, want ErrInvalidToken", err)
	}
	if beforeRevoke != clock.Now().Add(time.Minute) {
		t.Fatal("test clock changed during revoke")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("random source failed")
}

func TestLoginReturnsRandomFailureWithoutFallback(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, failingReader{})
	_, _, err := service.Login("password")
	if err == nil || !strings.Contains(err.Error(), "generate authentication token") {
		t.Fatalf("Login error = %v, want random generation error", err)
	}
	if len(service.tokens) != 0 {
		t.Fatalf("token count = %d, want 0", len(service.tokens))
	}
}

func TestCloseInvalidatesTokens(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, nil)
	token, _, err := service.Login("password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	service.Close()
	if _, _, err := service.Authenticate(token, true); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate after Close = %v, want ErrInvalidToken", err)
	}
	if _, _, err := service.Login("password"); err == nil {
		t.Fatal("Login after Close succeeded")
	}
}

func TestConcurrentAuthenticationAndRevoke(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, nil)
	token, _, err := service.Login("password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, _ = service.Authenticate(token, true)
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		_ = service.Revoke(token)
	}()
	group.Wait()

	if _, _, err := service.Authenticate(token, false); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate after concurrent revoke = %v, want ErrInvalidToken", err)
	}
}

// TestNewRejectsInvalidAndWeakPasswordHashes covers the hash contract: the
// service accepts a bcrypt hash of the default cost and rejects anything
// else, because it can no longer fall back to reading a hash from a file.
func TestNewRejectsInvalidAndWeakPasswordHashes(t *testing.T) {
	cfg := config.Auth{
		Enabled:  true,
		TokenTTL: config.Duration(time.Minute),
	}
	if _, err := New(cfg, []byte("not-a-bcrypt-hash")); err == nil {
		t.Fatal("New with invalid bcrypt hash succeeded")
	}

	weak, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	if _, err := New(cfg, weak); err == nil {
		t.Fatal("New with weak bcrypt hash succeeded")
	}

	if _, err := New(cfg, nil); err == nil {
		t.Fatal("New without a password hash succeeded")
	}
}

// TestNewRejectsDisabledAuthentication covers the completeness rule of the
// service: it exists only for an enabled authentication.
func TestNewRejectsDisabledAuthentication(t *testing.T) {
	if _, err := New(config.Auth{TokenTTL: config.Duration(time.Minute)}, testHash(t)); err == nil {
		t.Fatal("New with disabled authentication succeeded")
	}
}

// TestSetTokenTTLAppliesToNewTokensAndRenewals covers the hot-applied token
// lifetime: a replaced TTL is used by tokens created afterwards and by the
// renewal of a token issued under the previous TTL.
func TestSetTokenTTLAppliesToNewTokensAndRenewals(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.February, 2, 12, 0, 0, 0, time.UTC)}
	service := newTestService(t, clock, nil)

	oldToken, oldExpiry, err := service.Login("password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if want := clock.Now().Add(time.Minute); oldExpiry != want {
		t.Fatalf("startup expiry = %s, want %s", oldExpiry, want)
	}

	if err := service.SetTokenTTL(90 * time.Minute); err != nil {
		t.Fatalf("SetTokenTTL: %v", err)
	}
	if got := service.TokenTTL(); got != 90*time.Minute {
		t.Fatalf("TokenTTL = %s, want 90m", got)
	}

	_, newExpiry, err := service.Login("password")
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}
	if want := clock.Now().Add(90 * time.Minute); newExpiry != want {
		t.Fatalf("expiry under the replaced TTL = %s, want %s", newExpiry, want)
	}

	clock.Advance(10 * time.Second)
	_, renewedExpiry, err := service.Authenticate(oldToken, true)
	if err != nil {
		t.Fatalf("renew Authenticate: %v", err)
	}
	if want := clock.Now().Add(90 * time.Minute); renewedExpiry != want {
		t.Fatalf("renewed expiry = %s, want the replaced TTL %s", renewedExpiry, want)
	}

	_, unchangedExpiry, err := service.Authenticate(oldToken, false)
	if err != nil {
		t.Fatalf("non-renewing Authenticate: %v", err)
	}
	if unchangedExpiry != renewedExpiry {
		t.Fatalf("non-renewed expiry = %s, want the renewed %s", unchangedExpiry, renewedExpiry)
	}
}

// TestSetTokenTTLRejectsInvalidDurations covers the guard around the setter:
// a TTL the startup would reject leaves the current one in force.
func TestSetTokenTTLRejectsInvalidDurations(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.February, 2, 12, 0, 0, 0, time.UTC)}
	service := newTestService(t, clock, nil)

	for _, ttl := range []time.Duration{0, -time.Second, 25 * time.Hour} {
		if err := service.SetTokenTTL(ttl); err == nil {
			t.Errorf("SetTokenTTL(%s) succeeded, want an error", ttl)
		}
	}
	if got := service.TokenTTL(); got != time.Minute {
		t.Fatalf("TokenTTL after rejected updates = %s, want the unchanged 1m", got)
	}
}

// TestConcurrentTokenTTLUpdates drives the setter and the readers together.
// Run under -race, it proves the current TTL is guarded like the token store.
func TestConcurrentTokenTTLUpdates(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	service := newTestService(t, clock, nil)
	token, _, err := service.Login("password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var group sync.WaitGroup
	for i := range 32 {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			ttl := time.Duration(i+1) * time.Minute
			if err := service.SetTokenTTL(ttl); err != nil {
				t.Errorf("SetTokenTTL: %v", err)
				return
			}
			if got := service.TokenTTL(); got <= 0 {
				t.Errorf("TokenTTL = %s, want a positive value", got)
			}
			_, _, _ = service.Authenticate(token, true)
		}(i)
	}
	group.Wait()
}

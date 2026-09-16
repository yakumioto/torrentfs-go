package auth

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const testPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMye8fOsiTWZqYtkxvXkKm8BMzjT7t/vIdq"

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

func newTestService(t *testing.T, clock *fakeClock, random io.Reader) *Service {
	t.Helper()
	service, err := New(config.Auth{
		Enabled:      true,
		Username:     "alice",
		PasswordHash: testPasswordHash,
		TokenTTL:     config.Duration(time.Minute),
	})
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

	token, expiresAt, err := service.Login("alice", "password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(token) != 43 || strings.ContainsAny(token, ".+/=") {
		t.Fatalf("token = %q, want 32-byte raw URL-safe base64", token)
	}
	if expiresAt != clock.Now().Add(time.Minute) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, clock.Now().Add(time.Minute))
	}
	secondToken, _, err := service.Login("alice", "password")
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
	if principal.Username != "alice" {
		t.Fatalf("principal = %+v, want alice", principal)
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

func TestLoginRejectsInvalidCredentialsWithoutCreatingToken(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, nil)
	for _, credentials := range [][2]string{{"unknown", "password"}, {"alice", "wrong"}, {"", ""}} {
		if _, _, err := service.Login(credentials[0], credentials[1]); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("Login(%q, %q) error = %v, want ErrInvalidCredentials", credentials[0], credentials[1], err)
		}
	}
	if len(service.tokens) != 0 {
		t.Fatalf("token count = %d, want 0", len(service.tokens))
	}
}

func TestAuthenticateExpiresAndRevoke(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	service := newTestService(t, clock, nil)
	token, _, err := service.Login("alice", "password")
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

	token, _, err = service.Login("alice", "password")
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
	_, _, err := service.Login("alice", "password")
	if err == nil || !strings.Contains(err.Error(), "generate authentication token") {
		t.Fatalf("Login error = %v, want random generation error", err)
	}
	if len(service.tokens) != 0 {
		t.Fatalf("token count = %d, want 0", len(service.tokens))
	}
}

func TestCloseInvalidatesTokens(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, nil)
	token, _, err := service.Login("alice", "password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	service.Close()
	if _, _, err := service.Authenticate(token, true); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate after Close = %v, want ErrInvalidToken", err)
	}
	if _, _, err := service.Login("alice", "password"); err == nil {
		t.Fatal("Login after Close succeeded")
	}
}

func TestConcurrentAuthenticationAndRevoke(t *testing.T) {
	service := newTestService(t, &fakeClock{now: time.Now()}, nil)
	token, _, err := service.Login("alice", "password")
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

func TestNewLoadsRestrictedPasswordHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password.hash")
	if err := os.WriteFile(path, []byte(testPasswordHash+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	service, err := New(config.Auth{
		Enabled:          true,
		Username:         "alice",
		PasswordHashFile: path,
		TokenTTL:         config.Duration(time.Minute),
	})
	if err != nil {
		t.Fatalf("New with hash file: %v", err)
	}
	if _, _, err := service.Login("alice", "password"); err != nil {
		t.Fatalf("Login with hash file: %v", err)
	}
}

func TestNewRejectsUnsafePasswordHashFiles(t *testing.T) {
	base := config.Auth{
		Enabled:  true,
		Username: "alice",
		TokenTTL: config.Duration(time.Minute),
	}

	wide := filepath.Join(t.TempDir(), "wide.hash")
	if err := os.WriteFile(wide, []byte(testPasswordHash), 0o640); err != nil {
		t.Fatalf("WriteFile wide: %v", err)
	}
	base.PasswordHashFile = wide
	if _, err := New(base); err == nil {
		t.Fatal("New with group-readable hash file succeeded")
	}

	directory := filepath.Join(t.TempDir(), "hash-dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	base.PasswordHashFile = directory
	if _, err := New(base); err == nil {
		t.Fatal("New with directory hash file succeeded")
	}

	missing := filepath.Join(t.TempDir(), "missing.hash")
	base.PasswordHashFile = missing
	if _, err := New(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New missing hash file error = %v, want os.ErrNotExist", err)
	}

	target := filepath.Join(t.TempDir(), "target.hash")
	if err := os.WriteFile(target, []byte(testPasswordHash), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link.hash")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	base.PasswordHashFile = link
	if _, err := New(base); err == nil {
		t.Fatal("New with symlink hash file succeeded")
	}
}

func TestNewRejectsInvalidAndWeakPasswordHashes(t *testing.T) {
	cfg := config.Auth{
		Enabled:      true,
		Username:     "alice",
		PasswordHash: "not-a-bcrypt-hash",
		TokenTTL:     config.Duration(time.Minute),
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New with invalid bcrypt hash succeeded")
	}

	weak, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	cfg.PasswordHash = string(weak)
	if _, err := New(cfg); err == nil {
		t.Fatal("New with weak bcrypt hash succeeded")
	}
}

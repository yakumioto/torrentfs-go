package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/auth"
	"github.com/yakumioto/torrentfs-go/internal/config"
)

// The block is a temporary brake, not a lockout: once the window passes, the
// right password works again and the failure counter starts from zero.
func TestLoginSucceedsOnceTheBackoffExpires(t *testing.T) {
	cfg := config.Config{Password: "secret"}
	cfg.HTTP.Auth.Enabled = true
	cfg.HTTP.Auth.TokenTTL = config.Duration(30 * time.Minute)
	hash, err := cfg.PasswordHash()
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	service, err := auth.New(cfg.HTTP.Auth, hash)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	server := &Server{
		auth:         service,
		loginLimiter: newLoginLimiter(func() time.Time { return now }),
	}
	attempt := func(password string) int {
		body := strings.NewReader(`{"password":"` + password + `"}`)
		req := httptest.NewRequest(http.MethodPost, loginPath, body)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.10:40000"
		rec := httptest.NewRecorder()
		server.handleLogin(rec, req)
		return rec.Code
	}

	for i := 0; i < loginFailureThreshold; i++ {
		if got := attempt("wrong"); got != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, got)
		}
	}
	if got := attempt("secret"); got != http.StatusTooManyRequests {
		t.Fatalf("correct password during the block = %d, want 429", got)
	}

	now = now.Add(loginBaseBackoff + time.Second)
	if got := attempt("secret"); got != http.StatusOK {
		t.Fatalf("correct password after the block = %d, want 200", got)
	}
	if entries := len(server.loginLimiter.entries); entries != 0 {
		t.Fatalf("login limiter kept %d entries after a success, want 0", entries)
	}
	if got := attempt("wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong password after the reset = %d, want 401", got)
	}
}

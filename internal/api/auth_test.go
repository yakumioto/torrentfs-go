package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
)

const apiTestPassword = "password"

func dynamicAuth(cfg *config.Config) {
	cfg.HTTP.Auth = config.Auth{
		Enabled:  true,
		TokenTTL: config.Duration(time.Minute),
	}
	cfg.Password = apiTestPassword
}

func TestAuthDisabledLoginReturnsNotFound(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"password"}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, srv, req); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

func TestLoginAndDynamicBearerAuthentication(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)

	if rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	} else if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"password"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := do(t, srv, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body %s", loginRec.Code, loginRec.Body.String())
	}
	if got := loginRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := loginRec.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie = %q, want empty", got)
	}
	var response struct {
		Token     string `json:"token"`
		TokenType string `json:"token_type"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(loginRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if response.Token == "" || strings.Contains(response.Token, ".") {
		t.Fatalf("token = %q, want opaque non-JWT token", response.Token)
	}
	if response.TokenType != "Bearer" || response.ExpiresIn != 60 {
		t.Fatalf("login response = %+v, want Bearer and 60 seconds", response)
	}

	for _, scheme := range []string{"Bearer", "bearer", "bEaReR"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)
		req.Header.Set("Authorization", scheme+" "+response.Token)
		if rec := do(t, srv, req); rec.Code != http.StatusOK {
			t.Fatalf("authenticated list with %q status = %d, want 200; body %s", scheme, rec.Code, rec.Body.String())
		}
	}

	for _, header := range []string{"Basic " + response.Token, "Bearer wrong", "Bearer "} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)
		req.Header.Set("Authorization", header)
		if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want 401", header, rec.Code)
		}
	}
}

func TestLoginExpiresInRoundsSubsecondTTLUp(t *testing.T) {
	tests := []struct {
		name      string
		ttl       time.Duration
		expiresIn int64
	}{
		{name: "half second", ttl: 500 * time.Millisecond, expiresIn: 1},
		{name: "one and a half seconds", ttl: 1500 * time.Millisecond, expiresIn: 2},
		{name: "one nanosecond", ttl: time.Nanosecond, expiresIn: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{}, func(cfg *config.Config) {
				dynamicAuth(cfg)
				cfg.HTTP.Auth.TokenTTL = config.Duration(tt.ttl)
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"password"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := do(t, srv, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("login status = %d, want 200; body %s", rec.Code, rec.Body.String())
			}
			var response struct {
				ExpiresIn int64 `json:"expires_in"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode login response: %v", err)
			}
			if response.ExpiresIn != tt.expiresIn {
				t.Fatalf("expires_in = %d, want %d", response.ExpiresIn, tt.expiresIn)
			}
		})
	}
}

func TestLoginRejectsInvalidCredentialsAndStrictBodies(t *testing.T) {
	badCredentials := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"wrong"}`))
	badCredentials.Header.Set("Content-Type", "application/json")
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	rec := do(t, srv, badCredentials)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad credentials status = %d, want 401", rec.Code)
	}
	var errorBody map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &errorBody); err != nil {
		t.Fatalf("decode bad credentials response: %v", err)
	}
	if len(errorBody) != 1 || errorBody["error"] != "invalid credentials" {
		t.Fatalf("bad credentials response = %v, want generic error", errorBody)
	}

	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "missing content type", body: `{}`,
			wantStatus: http.StatusBadRequest},
		{name: "malformed JSON", contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest},
		{name: "missing password", contentType: "application/json", body: `{}`,
			wantStatus: http.StatusUnauthorized},
		{name: "empty password", contentType: "application/json", body: `{"password":""}`,
			wantStatus: http.StatusUnauthorized},
		{name: "invalid password type", contentType: "application/json", body: `{"password":123}`,
			wantStatus: http.StatusBadRequest},
		{name: "username field", contentType: "application/json", body: `{"username":"alice","password":"password"}`,
			wantStatus: http.StatusBadRequest},
		{name: "unknown field", contentType: "application/json", body: `{"password":"password","extra":true}`,
			wantStatus: http.StatusBadRequest},
		{name: "trailing JSON", contentType: "application/json", body: `{"password":"password"}{}`,
			wantStatus: http.StatusBadRequest},
		{name: "oversized body", contentType: "application/json", body: strings.Repeat("x", 16<<10),
			wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if rec := do(t, srv, req); rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestTokenSourcesAreExplicitAndLogoutRevokes(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	token := loginForTest(t, srv)

	queries := []string{
		"/api/v1/torrents?token=" + token,
		"/api/v1/torrents",
	}
	for _, path := range queries {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == queries[1] {
			req.AddCookie(&http.Cookie{Name: "token", Value: token})
		}
		if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("credential in non-header source %q status = %d, want 401", path, rec.Code)
		}
	}

	logout := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logout.Header.Set("Authorization", "Bearer "+token)
	if rec := do(t, srv, logout); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("logout response = %d %q, want 204 with empty body", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", rec.Code)
	}

	logout = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logout.Header.Set("Authorization", "Bearer "+token)
	if rec := do(t, srv, logout); rec.Code != http.StatusUnauthorized {
		t.Fatalf("second logout status = %d, want 401", rec.Code)
	}
}

func TestUnknownAuthRouteDoesNotBypassMiddleware(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	if rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/auth/unknown", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown route without token status = %d, want 401", rec.Code)
	}
	token := loginForTest(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/unknown", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if rec := do(t, srv, req); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route with token status = %d, want 404", rec.Code)
	}
}

// TestNewRejectsMissingAuthenticationMaterial covers the completeness rule:
// an enabled authentication without a password cannot serve a login.
func TestNewRejectsMissingAuthenticationMaterial(t *testing.T) {
	cfg := config.Default()
	dynamicAuth(&cfg)
	cfg.Password = ""
	if _, err := api.New(cfg, &fakeBackend{}); !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("api.New error = %v, want config.ErrInvalid", err)
	}
}

func loginForTest(t *testing.T, srv *api.Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d; body %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return response.Token
}

func loginAttempt(t *testing.T, srv *api.Server, remoteAddr, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	return do(t, srv, req)
}

// TestLoginBacksOffAfterRepeatedFailures pins the wire contract: the first
// four failures answer 401, the fifth arms the backoff, and the sixth request
// is refused with 429 and Retry-After even when the password is correct.
func TestLoginBacksOffAfterRepeatedFailures(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	const ip = "198.51.100.7:4000"
	for attempt := 1; attempt <= 5; attempt++ {
		if rec := loginAttempt(t, srv, ip, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401; body %s", attempt, rec.Code, rec.Body.String())
		}
	}

	rec := loginAttempt(t, srv, ip, apiTestPassword)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status after five failures = %d, want 429; body %s", rec.Code, rec.Body.String())
	}
	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || retryAfter <= 0 || retryAfter > 30 {
		t.Fatalf("Retry-After = %q, want 1..30 seconds", rec.Header().Get("Retry-After"))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("429 response leaked a token: %v", body)
	}

	// Another source address shares no counter with the blocked one.
	if other := loginAttempt(t, srv, "203.0.113.9:5000", "wrong"); other.Code != http.StatusUnauthorized {
		t.Fatalf("other address status = %d, want 401; body %s", other.Code, other.Body.String())
	}
}

func TestLoginSuccessResetsTheFailureCounter(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	const ip = "198.51.100.8:4000"
	for attempt := 1; attempt <= 4; attempt++ {
		if rec := loginAttempt(t, srv, ip, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt, rec.Code)
		}
	}
	if rec := loginAttempt(t, srv, ip, apiTestPassword); rec.Code != http.StatusOK {
		t.Fatalf("login after four failures = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	// The counter is gone, so four more failures still answer 401 and the
	// fifth failure only arms the backoff rather than tripping it immediately.
	for attempt := 1; attempt <= 5; attempt++ {
		if rec := loginAttempt(t, srv, ip, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d status = %d, want 401", attempt, rec.Code)
		}
	}
	if rec := loginAttempt(t, srv, ip, apiTestPassword); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status after the second five failures = %d, want 429", rec.Code)
	}
}

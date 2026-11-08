package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// magnetRequest returns a request whose body is far larger than a tiny upload
// limit and small enough to pass the default one.
func magnetRequest() *http.Request {
	body := `{"magnet_uri":"magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// addViewForTest is the torrent the fake backend returns from a successful
// add, so a request that passes the upload cap is distinguishable from one
// that never reached the handler.
func addViewForTest() *session.TorrentView {
	return &session.TorrentView{ID: "abc", InfoHash: "abc", State: session.StateAdding}
}

// TestMaxUploadApplierChangesTheRunningLimit covers http.max_upload_size: the
// running server enforces the current value, so a larger saved limit admits a
// body the previous one rejected, and a smaller one rejects it again.
func TestMaxUploadApplierChangesTheRunningLimit(t *testing.T) {
	backend := &fakeBackend{addView: addViewForTest()}
	srv := newTestServer(t, backend, func(cfg *config.Config) {
		cfg.HTTP.MaxUploadBytes = 8
	})
	if rec := do(t, srv, magnetRequest()); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status under the startup limit = %d, want 413; body %s", rec.Code, rec.Body.String())
	}

	applier := api.NewMaxUploadApplier(srv)
	next := config.Default()
	next.HTTP.MaxUploadBytes = config.ByteQuantity(1 << 20)
	if err := applier.Apply(next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec := do(t, srv, magnetRequest()); rec.Code != http.StatusCreated {
		t.Fatalf("status after the raised limit = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	next.HTTP.MaxUploadBytes = config.ByteQuantity(8)
	if err := applier.Apply(next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec := do(t, srv, magnetRequest()); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status after the lowered limit = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
}

// login performs a login and returns the bearer token and the expiry the API
// reported, in seconds. It reports errors instead of failing the test, so a
// goroutine can drive it.
func login(srv *api.Server) (string, int64, error) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return "", 0, fmt.Errorf("login status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Token     string `json:"token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		return "", 0, fmt.Errorf("decode login response: %w", err)
	}
	return response.Token, response.ExpiresIn, nil
}

// loginToken logs in from the test goroutine and fails the test on error.
func loginToken(t *testing.T, srv *api.Server) (string, int64) {
	t.Helper()
	token, expiresIn, err := login(srv)
	if err != nil {
		t.Fatal(err)
	}
	return token, expiresIn
}

// loginExpiresIn returns the lifetime the API reports for a new login.
func loginExpiresIn(t *testing.T, srv *api.Server) int64 {
	t.Helper()
	_, expiresIn := loginToken(t, srv)
	return expiresIn
}

// TestTokenTTLApplierChangesLoginExpiry covers http.auth.token_ttl: a token
// minted after the save reports the new lifetime, while the startup tokens
// keep theirs until the running service renews them.
func TestTokenTTLApplierChangesLoginExpiry(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	if got := loginExpiresIn(t, srv); got != 60 {
		t.Fatalf("startup expires_in = %d, want 60", got)
	}

	next := config.Default()
	next.HTTP.Auth = config.Auth{Enabled: true, TokenTTL: config.Duration(2 * time.Minute)}
	if err := api.NewTokenTTLApplier(srv).Apply(next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := loginExpiresIn(t, srv); got != 120 {
		t.Fatalf("expires_in after the save = %d, want 120", got)
	}
}

// TestTokenTTLApplierWithoutAuthenticationIsANoOp covers a deployment that
// started without the login: there is no token store to update, so a save must
// not fail the whole component.
func TestTokenTTLApplierWithoutAuthenticationIsANoOp(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)
	next := config.Default()
	next.HTTP.Auth.TokenTTL = config.Duration(2 * time.Minute)
	if err := api.NewTokenTTLApplier(srv).Apply(next); err != nil {
		t.Fatalf("Apply without authentication: %v", err)
	}
}

// TestConfigAppliersOwnHotAppliedKeys registers the appliers with a real
// controller, which is the same check a start performs: every key they claim
// must be marked hot-applied by the schema and unclaimed by another applier.
func TestConfigAppliersOwnHotAppliedKeys(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	controller := configctl.New("", config.Default(), nil)
	for _, applier := range []configctl.Applier{
		api.NewMaxUploadApplier(srv),
		api.NewTokenTTLApplier(srv),
	} {
		if err := controller.RegisterApplier(applier); err != nil {
			t.Errorf("RegisterApplier(%v): %v", applier.Keys(), err)
		}
	}
}

// TestConfigAppliersConcurrentWithRequests drives both setters while uploads
// and logins are served. Run under -race, it proves the current upload cap and
// token lifetime are guarded.
func TestConfigAppliersConcurrentWithRequests(t *testing.T) {
	backend := &fakeBackend{addView: addViewForTest()}
	srv := newTestServer(t, backend, func(cfg *config.Config) {
		dynamicAuth(cfg)
		cfg.HTTP.MaxUploadBytes = config.ByteQuantity(1 << 20)
	})
	small := config.Default()
	small.HTTP.MaxUploadBytes = config.ByteQuantity(8)
	small.HTTP.Auth = config.Auth{Enabled: true, TokenTTL: config.Duration(time.Minute)}
	large := config.Default()
	large.HTTP.MaxUploadBytes = config.ByteQuantity(1 << 20)
	large.HTTP.Auth = config.Auth{Enabled: true, TokenTTL: config.Duration(2 * time.Minute)}

	maxUploadApplier := api.NewMaxUploadApplier(srv)
	tokenTTLApplier := api.NewTokenTTLApplier(srv)
	token, _ := loginToken(t, srv)

	const iterations = 4
	var group sync.WaitGroup
	group.Add(4)
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			next := small
			if i%2 == 0 {
				next = large
			}
			if err := maxUploadApplier.Apply(next); err != nil {
				t.Errorf("max upload Apply: %v", err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			next := small
			if i%2 == 0 {
				next = large
			}
			if err := tokenTTLApplier.Apply(next); err != nil {
				t.Errorf("token TTL Apply: %v", err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			req := magnetRequest()
			req.Header.Set("Authorization", "Bearer "+token)
			switch rec := do(t, srv, req); rec.Code {
			case http.StatusCreated, http.StatusRequestEntityTooLarge:
			default:
				t.Errorf("upload status = %d, want 201 or 413; body %s", rec.Code, rec.Body.String())
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			_, expiresIn, err := login(srv)
			if err != nil {
				t.Errorf("login: %v", err)
				return
			}
			if expiresIn != 60 && expiresIn != 120 {
				t.Errorf("expires_in = %d, want one of the applied lifetimes", expiresIn)
				return
			}
		}
	}()
	group.Wait()
}

// TestSaveAppliesUploadCapAndTokenTTL is the acceptance path of the ticket: a
// save through the configuration controller raises the running upload cap and
// the running token lifetime, so the next requests observe both new values.
func TestSaveAppliesUploadCapAndTokenTTL(t *testing.T) {
	work := t.TempDir()
	path := filepath.Join(work, "torrentfs.toml")
	body := `username = "alice"
password = "password"

[http]
max_upload_size = "8B"

[http.auth]
enabled = true
token_ttl = "1m"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write configuration file: %v", err)
	}
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	srv, err := api.New(cfg, &fakeBackend{addView: addViewForTest()})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	controller := configctl.New(path, cfg, sources)
	for _, applier := range []configctl.Applier{
		api.NewMaxUploadApplier(srv),
		api.NewTokenTTLApplier(srv),
	} {
		if err := controller.RegisterApplier(applier); err != nil {
			t.Fatalf("RegisterApplier(%v): %v", applier.Keys(), err)
		}
	}

	view, err := controller.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	result, err := controller.Save(context.Background(), configctl.SaveRequest{
		Baseline: view.Baseline,
		Patch: map[string]string{
			"http.max_upload_size": "1MB",
			"http.auth.token_ttl":  "2m",
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	applied := map[string]bool{}
	for _, key := range result.Applied {
		applied[key] = true
	}
	for _, key := range []string{"http.max_upload_size", "http.auth.token_ttl"} {
		if !applied[key] {
			t.Errorf("applied = %v, want %s", result.Applied, key)
		}
	}
	if len(result.RestartRequired) != 0 {
		t.Errorf("restart required = %v, want none", result.RestartRequired)
	}

	token, expiresIn := loginToken(t, srv)
	if expiresIn != 120 {
		t.Fatalf("expires_in after the save = %d, want 120", expiresIn)
	}
	req := magnetRequest()
	req.Header.Set("Authorization", "Bearer "+token)
	if rec := do(t, srv, req); rec.Code != http.StatusCreated {
		t.Fatalf("upload status after the save = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}

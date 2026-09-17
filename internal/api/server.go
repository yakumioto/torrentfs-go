// Package api serves the torrent management HTTP API on top of a session.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/auth"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
	"github.com/yakumioto/torrentfs-go/web"
)

// Backend is the narrow session surface the API needs. Keeping it small lets
// tests drive the handlers with a fake instead of a real BitTorrent client.
type Backend interface {
	AddTorrentAndPersist(ctx context.Context, src session.Source) (*session.TorrentView, error)
	ListTorrents() []session.TorrentView
	TorrentViewFor(id string) (session.TorrentView, error)
	TorrentStatusFor(id string) (session.TorrentStatusView, error)
	DeleteTorrent(ctx context.Context, id string, purgeData bool) (*session.Operation, error)
	Operation(id string) (session.Operation, bool)
}

// Server is the torrent management HTTP service.
type Server struct {
	backend   Backend
	auth      *auth.Service
	tokenTTL  time.Duration
	maxUpload int64
	handler   http.Handler
	http      *http.Server
	listener  net.Listener
}

// New builds a server for cfg. A listener is created only by Serve.
func New(cfg config.Config, backend Backend) (*Server, error) {
	s := &Server{
		backend:   backend,
		tokenTTL:  time.Duration(cfg.HTTP.Auth.TokenTTL),
		maxUpload: cfg.HTTP.MaxUploadBytes,
	}
	if cfg.HTTP.Auth.Enabled {
		service, err := auth.New(cfg.HTTP.Auth)
		if err != nil {
			return nil, fmt.Errorf("api: initialize authentication: %w", err)
		}
		s.auth = service
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/v1/torrents", s.handleAdd)
	mux.HandleFunc("GET /api/v1/torrents", s.handleList)
	mux.HandleFunc("GET /api/v1/torrents/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/torrents/{id}", s.handleDetail)
	mux.HandleFunc("DELETE /api/v1/torrents/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/v1/operations/{id}", s.handleOperation)
	s.handler = dispatchAPIAndStatic(s.authenticate(mux), web.Handler())
	return s, nil
}

// Handler returns the wrapped handler, for use with httptest.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr reports the address Serve bound.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve listens on cfg.HTTP.ListenAddr and serves until ctx is cancelled or
// Shutdown is called.
func (s *Server) Serve(ctx context.Context, listenAddr string) error {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("api: listen %s: %w", listenAddr, err)
	}
	s.listener = listener
	s.http = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.http.Shutdown(shutdownCtx)
		case <-done:
		}
	}()

	err = s.http.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	if s.http != nil {
		err = s.http.Shutdown(ctx)
	}
	if s.auth != nil {
		s.auth.Close()
	}
	return err
}

func dispatchAPIAndStatic(apiHandler, staticHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == loginPath {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := authorizationToken(r)
		if !ok {
			writeUnauthorized(w)
			return
		}
		renew := r.Method != http.MethodPost || r.URL.Path != logoutPath
		principal, _, err := s.auth.Authenticate(token, renew)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		next.ServeHTTP(w, r)
	})
}

func authorizationToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

type principalContextKey struct{}

// PrincipalFromContext returns the authenticated principal attached by the API middleware.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal, ok
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(r io.Reader, value any) error {
	return json.NewDecoder(r).Decode(value)
}

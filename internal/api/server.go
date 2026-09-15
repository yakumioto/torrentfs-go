// Package api serves the torrent management HTTP API on top of a session.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
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
	token     string
	maxUpload int64
	handler   http.Handler
	http      *http.Server
	listener  net.Listener
}

// New builds a server for cfg. A listener is created only by Serve.
func New(cfg config.Config, backend Backend) *Server {
	s := &Server{
		backend:   backend,
		token:     cfg.HTTP.BearerToken,
		maxUpload: cfg.HTTP.MaxUploadBytes,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/torrents", s.handleAdd)
	mux.HandleFunc("GET /api/v1/torrents", s.handleList)
	mux.HandleFunc("GET /api/v1/torrents/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/torrents/{id}", s.handleDetail)
	mux.HandleFunc("DELETE /api/v1/torrents/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/v1/operations/{id}", s.handleOperation)
	s.handler = s.authenticate(mux)
	return s
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
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// authenticate enforces the Bearer token when one is configured.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			header := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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

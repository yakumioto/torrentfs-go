package api

import (
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// setMaxUploadBytes replaces the cap every upload handler applies to a request
// body. Handlers read the current value when a request starts, so a save
// reaches new requests without a restart.
func (s *Server) setMaxUploadBytes(limit int64) {
	s.maxUpload.Store(limit)
}

// maxUploadBytes returns the cap an upload handler must apply.
func (s *Server) maxUploadBytes() int64 {
	return s.maxUpload.Load()
}

// MaxUploadApplier applies http.max_upload_size to the running server.
type MaxUploadApplier struct {
	server *Server
}

// NewMaxUploadApplier returns the applier of the upload cap to server.
func NewMaxUploadApplier(server *Server) *MaxUploadApplier {
	return &MaxUploadApplier{server: server}
}

// Keys returns the configuration keys the applier owns.
func (a *MaxUploadApplier) Keys() []string { return []string{"http.max_upload_size"} }

// Apply sets the configured upload cap on the running server.
func (a *MaxUploadApplier) Apply(cfg config.Config) error {
	a.server.setMaxUploadBytes(cfg.HTTP.MaxUploadBytes.Int64())
	return nil
}

// TokenTTLApplier applies http.auth.token_ttl to the running authentication
// service.
type TokenTTLApplier struct {
	server *Server
}

// NewTokenTTLApplier returns the applier of the token lifetime to server.
func NewTokenTTLApplier(server *Server) *TokenTTLApplier {
	return &TokenTTLApplier{server: server}
}

// Keys returns the configuration keys the applier owns.
func (a *TokenTTLApplier) Keys() []string { return []string{"http.auth.token_ttl"} }

// Apply sets the configured token lifetime on the running service. A server
// that started without authentication has no token store to update, so the
// save succeeds without effect; a start that enables the login reads the file
// that already holds the lifetime.
func (a *TokenTTLApplier) Apply(cfg config.Config) error {
	if a.server.auth == nil {
		return nil
	}
	return a.server.auth.SetTokenTTL(time.Duration(cfg.HTTP.Auth.TokenTTL))
}

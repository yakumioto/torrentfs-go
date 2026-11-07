// Package auth provides password authentication and in-memory tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const (
	tokenSize           = 32
	minimumPasswordCost = 10
)

var (
	// ErrInvalidCredentials is returned when a login does not match the configured password.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrInvalidToken is returned when a token is missing, expired, unknown, or revoked.
	ErrInvalidToken = errors.New("invalid token")
	errClosed       = errors.New("authentication service is closed")
)

// ConfigError identifies invalid authentication configuration without exposing secrets.
type ConfigError struct {
	Field string
	Cause error
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("auth: %s: %v", e.Field, e.Cause)
}

func (e *ConfigError) Unwrap() error {
	return e.Cause
}

func (e *ConfigError) Is(target error) bool {
	return target == config.ErrInvalid
}

// Principal identifies the authenticated caller of a token.
//
// The login checks the password only, so the principal carries no user name:
// the credentials are a single password, and the HTTP login does not ask for
// one.
type Principal struct{}

type tokenEntry struct {
	expiresAt time.Time
}

// Service authenticates the configured password and owns the in-memory token store.
type Service struct {
	mu           sync.Mutex
	passwordHash []byte
	tokenTTL     time.Duration
	tokens       map[[sha256.Size]byte]tokenEntry
	now          func() time.Time
	random       io.Reader
	closed       bool
}

// New creates an authentication service from the configured settings and the
// bcrypt hash of the password.
//
// The hash is the input rather than the plaintext password: the configuration
// layer owns the credentials and turns the plaintext into a hash once, so the
// service never sees the plaintext and never reads a file to obtain it.
func New(cfg config.Auth, passwordHash []byte) (*Service, error) {
	if !cfg.Enabled {
		return nil, invalidConfig("http.auth.enabled", errors.New("authentication is disabled"))
	}
	if cfg.TokenTTL <= 0 {
		return nil, invalidConfig("http.auth.token_ttl", errors.New("must be positive"))
	}
	if time.Duration(cfg.TokenTTL) > 24*time.Hour {
		return nil, invalidConfig("http.auth.token_ttl", errors.New("must not exceed 24 hours"))
	}
	if len(passwordHash) == 0 {
		return nil, invalidConfig("password", errors.New("a password hash is required"))
	}
	cost, err := bcrypt.Cost(passwordHash)
	if err != nil {
		return nil, invalidConfig("password", errors.New("must be a valid bcrypt hash"))
	}
	if cost < minimumPasswordCost {
		return nil, invalidConfig("password", fmt.Errorf("bcrypt cost must be at least %d", minimumPasswordCost))
	}

	return &Service{
		passwordHash: passwordHash,
		tokenTTL:     time.Duration(cfg.TokenTTL),
		tokens:       make(map[[sha256.Size]byte]tokenEntry),
		now:          time.Now,
		random:       rand.Reader,
	}, nil
}

func invalidConfig(field string, cause error) error {
	return &ConfigError{Field: field, Cause: cause}
}

// Login verifies credentials, creates a random opaque token, and returns its expiry.
func (s *Service) Login(password string) (string, time.Time, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", time.Time{}, errClosed
	}
	passwordHash := s.passwordHash
	s.mu.Unlock()

	if err := bcrypt.CompareHashAndPassword(passwordHash, []byte(password)); err != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}

	for {
		raw := make([]byte, tokenSize)
		if _, err := io.ReadFull(s.random, raw); err != nil {
			return "", time.Time{}, fmt.Errorf("generate authentication token: %w", err)
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		key := tokenKey(token)

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return "", time.Time{}, errClosed
		}
		now := s.now()
		s.removeExpiredLocked(now)
		if _, exists := s.tokens[key]; exists {
			s.mu.Unlock()
			continue
		}
		expiresAt := now.Add(s.tokenTTL)
		s.tokens[key] = tokenEntry{expiresAt: expiresAt}
		s.mu.Unlock()
		return token, expiresAt, nil
	}
}

// Authenticate validates a token and optionally slides its expiry window.
func (s *Service) Authenticate(token string, renew bool) (Principal, time.Time, error) {
	key := tokenKey(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Principal{}, time.Time{}, ErrInvalidToken
	}
	now := s.now()
	s.removeExpiredLocked(now)
	entry, ok := s.tokens[key]
	if !ok || !now.Before(entry.expiresAt) {
		delete(s.tokens, key)
		return Principal{}, time.Time{}, ErrInvalidToken
	}
	if renew {
		entry.expiresAt = now.Add(s.tokenTTL)
		s.tokens[key] = entry
	}
	return Principal{}, entry.expiresAt, nil
}

// Revoke removes a token without extending its expiry.
func (s *Service) Revoke(token string) error {
	key := tokenKey(token)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrInvalidToken
	}
	now := s.now()
	s.removeExpiredLocked(now)
	if _, ok := s.tokens[key]; !ok {
		return ErrInvalidToken
	}
	delete(s.tokens, key)
	return nil
}

// Close clears all tokens and prevents further authentication operations.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.tokens = make(map[[sha256.Size]byte]tokenEntry)
}

func (s *Service) removeExpiredLocked(now time.Time) {
	for key, entry := range s.tokens {
		if !now.Before(entry.expiresAt) {
			delete(s.tokens, key)
		}
	}
}

func tokenKey(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

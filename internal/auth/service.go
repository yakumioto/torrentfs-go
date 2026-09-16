// Package auth provides single-user password authentication and in-memory tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const (
	tokenSize            = 32
	minimumPasswordCost  = 10
	maxPasswordHashBytes = 1024
	dummyPasswordHash    = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
)

var (
	// ErrInvalidCredentials is returned when a login does not match the configured user.
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

// Principal identifies the configured user authenticated by a token.
type Principal struct {
	Username string
}

type tokenEntry struct {
	username  string
	expiresAt time.Time
}

// Service authenticates the configured user and owns the in-memory token store.
type Service struct {
	mu           sync.Mutex
	username     string
	passwordHash []byte
	tokenTTL     time.Duration
	tokens       map[[sha256.Size]byte]tokenEntry
	now          func() time.Time
	random       io.Reader
	closed       bool
}

// New validates the authentication configuration and creates an empty token store.
func New(cfg config.Auth) (*Service, error) {
	if !cfg.Enabled {
		return nil, invalidConfig("http.auth.enabled", errors.New("authentication is disabled"))
	}
	if cfg.Username == "" {
		return nil, invalidConfig("http.auth.username", errors.New("must not be empty"))
	}
	if strings.IndexFunc(cfg.Username, unicode.IsControl) >= 0 {
		return nil, invalidConfig("http.auth.username", errors.New("must not contain control characters"))
	}
	if cfg.TokenTTL <= 0 {
		return nil, invalidConfig("http.auth.token_ttl", errors.New("must be positive"))
	}
	if time.Duration(cfg.TokenTTL) > 24*time.Hour {
		return nil, invalidConfig("http.auth.token_ttl", errors.New("must not exceed 24 hours"))
	}

	hasPasswordHash := cfg.PasswordHash != ""
	hasPasswordHashFile := cfg.PasswordHashFile != ""
	if hasPasswordHash == hasPasswordHashFile {
		return nil, invalidConfig("http.auth.password_hash", errors.New("exactly one password hash source is required"))
	}

	passwordHash, field, err := loadPasswordHash(cfg)
	if err != nil {
		return nil, invalidConfig(field, err)
	}
	cost, err := bcrypt.Cost(passwordHash)
	if err != nil {
		return nil, invalidConfig(field, errors.New("must be a valid bcrypt hash"))
	}
	if cost < minimumPasswordCost {
		return nil, invalidConfig(field, fmt.Errorf("bcrypt cost must be at least %d", minimumPasswordCost))
	}

	return &Service{
		username:     cfg.Username,
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

func loadPasswordHash(cfg config.Auth) ([]byte, string, error) {
	if cfg.PasswordHashFile != "" {
		passwordHash, err := readPasswordHashFile(cfg.PasswordHashFile)
		return passwordHash, "http.auth.password_hash_file", err
	}
	return []byte(cfg.PasswordHash), "http.auth.password_hash", nil
}

func readPasswordHashFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read password hash file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("password hash file must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("password hash file must be regular")
	}
	permissions := info.Mode().Perm()
	if permissions&0o400 == 0 || permissions&0o077 != 0 {
		return nil, errors.New("password hash file must be owner-readable only")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read password hash file: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat password hash file: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, errors.New("password hash file changed while opening")
	}

	data, err := io.ReadAll(io.LimitReader(file, maxPasswordHashBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read password hash file: %w", err)
	}
	if len(data) > maxPasswordHashBytes {
		return nil, errors.New("password hash file is too large")
	}
	hash := strings.TrimSpace(string(data))
	if hash == "" {
		return nil, errors.New("password hash file is empty")
	}
	return []byte(hash), nil
}

// Login verifies credentials, creates a random opaque token, and returns its expiry.
func (s *Service) Login(username, password string) (string, time.Time, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", time.Time{}, errClosed
	}
	configuredUsername := s.username
	passwordHash := s.passwordHash
	s.mu.Unlock()

	candidate := passwordHash
	if username != configuredUsername {
		candidate = []byte(dummyPasswordHash)
	}
	if err := bcrypt.CompareHashAndPassword(candidate, []byte(password)); err != nil {
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
		s.tokens[key] = tokenEntry{username: s.username, expiresAt: expiresAt}
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
	return Principal{Username: entry.username}, entry.expiresAt, nil
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

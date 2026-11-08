package session

import (
	"slices"
	"sync"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// configSnapshot is the session's current configuration and the lock that
// guards it. Paths that consume configuration while the session is running
// read the snapshot instead of a copy frozen at construction, so a later
// replacement reaches them without restarting. Paths that configure a
// component at construction keep using the validated configuration New was
// given.
type configSnapshot struct {
	mu  sync.RWMutex
	cfg config.Config
}

// load returns the current configuration. The caller must not modify the
// BootstrapNodes slice of the result; Config hides that requirement from
// package users by cloning it.
func (c *configSnapshot) load() config.Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// store replaces the current configuration.
func (c *configSnapshot) store(cfg config.Config) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

// Config returns the session's current configuration snapshot. The returned
// value is a copy: changing it, including appending to BootstrapNodes, cannot
// affect the session. The configuration is valid because it either came from
// the loaders or was replaced through SetConfig.
func (s *Session) Config() config.Config {
	return cloneConfig(s.config.load())
}

// SetConfig atomically replaces the session's current configuration snapshot.
//
// The configuration must come from the loaders, which are the one place that
// validates it; SetConfig does not validate and cannot fail. Replacing the
// snapshot does not reconfigure components that consume configuration only at
// construction, such as the client listener, the piece cache, or the output
// backends.
func (s *Session) SetConfig(cfg config.Config) {
	s.config.store(cloneConfig(cfg))
}

// cloneConfig returns a configuration whose slice fields do not alias the
// original. Every other field is a value.
func cloneConfig(cfg config.Config) config.Config {
	cfg.Connections.BootstrapNodes = slices.Clone(cfg.Connections.BootstrapNodes)
	return cfg
}

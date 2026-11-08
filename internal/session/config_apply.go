package session

import (
	"github.com/yakumioto/torrentfs-go/internal/config"
)

// ConfigApplier applies the connection allowance configuration to the running
// session.
//
// The allocator reads the current configuration snapshot on every pass, so a
// replacement reaches it without recreating the session, the client or the
// torrents. Only the keys the applier owns are copied from the applied
// configuration: the other keys are written to the file but wait for a
// restart, and a running component that never received them must keep its
// previous value.
type ConfigApplier struct {
	session *Session
}

// NewConfigApplier returns the applier of the connection allowance
// configuration to session.
func NewConfigApplier(session *Session) *ConfigApplier {
	return &ConfigApplier{session: session}
}

// Keys returns the configuration keys the applier owns.
func (a *ConfigApplier) Keys() []string {
	return []string{
		"connections.reading_conns",
		"connections.uploading_conns",
		"connections.read_grace",
	}
}

// Apply replaces the connection allowance values in the session's current
// configuration snapshot and wakes the allocator.
//
// The snapshot is read and replaced under the applier's call, which the
// controller serializes, so the three values change together. The wake-up is
// best effort: the allocator's next tick applies the values anyway.
func (a *ConfigApplier) Apply(cfg config.Config) error {
	current := a.session.Config()
	if err := config.MergeKeys(&current, cfg, a.Keys()); err != nil {
		return err
	}
	a.session.SetConfig(current)
	a.session.kickConnectionAllocator()
	return nil
}

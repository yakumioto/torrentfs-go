package session

import "github.com/anacrolix/torrent"

// NewWithClientConfig exposes the test-only client configuration seam so
// integration tests can disable discovery traffic (DHT, UTP) and keep every
// connection on the loopback interface. It is not part of the public API.
var NewWithClientConfig = newWithClientConfig

// TorrentClientConfig is the concrete client configuration type tests adjust
// through NewWithClientConfig.
type TorrentClientConfig = torrent.ClientConfig

// SetReadGate installs hook as the session read gate and returns a function
// that restores the previous gate. hook runs at the start of every real read
// and may block, which lets a test hold a read outstanding (its ReadAt has
// entered and not returned) while it races Unmount and Session.Close. A nil
// hook clears the gate. Restoring always writes the saved value back, so an
// install that saw no previous gate still clears it rather than leaving the
// hook installed for later tests. This is a test-only seam; production never
// sets it.
func SetReadGate(hook func()) (restore func()) {
	var next *func()
	if hook != nil {
		next = &hook
	}
	previous := readGate.Swap(next)
	return func() { readGate.Store(previous) }
}

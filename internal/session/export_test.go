package session

import "github.com/anacrolix/torrent"

// NewWithClientConfig exposes the test-only client configuration seam so
// integration tests can disable discovery traffic (DHT, UTP) and keep every
// connection on the loopback interface. It is not part of the public API.
var NewWithClientConfig = newWithClientConfig

// TorrentClientConfig is the concrete client configuration type tests adjust
// through NewWithClientConfig.
type TorrentClientConfig = torrent.ClientConfig

package session

import (
	"context"
	"io"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// StartMetadataFetch starts the real magnet metadata-persist worker for a
// torrent whose info is already known, so tests can drive the persistence path
// deterministically. Test-only.
func (s *Session) StartMetadataFetch(st *Torrent) {
	s.startMetadataFetch(st.InfoHash(), st)
}

// PendingMetadataFetches reports how many metadata workers are still tracked.
// Test-only.
func (s *Session) PendingMetadataFetches() int {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	return len(s.metadataFetches)
}

// SetMetadataFetchHook installs fn as the post-resolution metadata fetch hook
// and returns a function that restores the previous value. A nil hook clears
// it. Test-only.
func SetMetadataFetchHook(fn func(metainfo.Hash)) func() {
	previous := metadataFetchHook
	metadataFetchHook = fn
	return func() { metadataFetchHook = previous }
}

// WriteMetadataForTest exercises the metadata persistence path with a caller
// supplied context. Test-only.
func (s *Session) WriteMetadataForTest(ctx context.Context, hash metainfo.Hash, data []byte) error {
	return s.writeMetadataBytes(ctx, hash, data)
}

// NewWithClientConfig exposes the test-only client configuration seam so
// integration tests can disable discovery traffic (DHT, UTP) and keep every
// connection on the loopback interface. It is not part of the public API.
var NewWithClientConfig = newWithClientConfig

// TorrentClientConfig is the concrete client configuration type tests adjust
// through NewWithClientConfig.
type TorrentClientConfig = torrent.ClientConfig

// SetReadaheadForTest changes the session reader's byte window for a cold-read
// integration test. It is not part of the public API.
func SetReadaheadForTest(reader io.ReaderAt, readahead int64) bool {
	file, ok := reader.(*raFile)
	if !ok {
		return false
	}
	file.mu.Lock()
	file.readahead = readahead
	file.mu.Unlock()
	return true
}

// PieceStateRunsForTest exposes the underlying torrent priority/completion
// snapshot to session integration tests. It is not part of the public API.
func PieceStateRunsForTest(st *Torrent) torrent.PieceStateRuns {
	return st.tor.PieceStateRuns()
}

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

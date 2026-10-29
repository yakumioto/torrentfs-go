package vfs

import (
	"sync"
	"testing"
)

// listingGateBackend pauses before taking the library snapshot, unlike the
// contentsTestBackend listingHook, which pauses after taking that snapshot.
type listingGateBackend struct {
	*contentsTestBackend
	listingStarted chan struct{}
	listingRelease chan struct{}
}

func (b *listingGateBackend) Torrents() []TorrentView {
	close(b.listingStarted)
	<-b.listingRelease
	return b.contentsTestBackend.Torrents()
}

func newListingGateBackend(t *testing.T) (*listingGateBackend, func()) {
	t.Helper()
	b := &listingGateBackend{contentsTestBackend: newContentsTestBackend(),
		listingStarted: make(chan struct{}), listingRelease: make(chan struct{})}
	release := sync.OnceFunc(func() { close(b.listingRelease) })
	t.Cleanup(release)
	return b, release
}

type legacyCountingBackend struct {
	Backend
	calls int
}

func (b *legacyCountingBackend) Torrents() []TorrentView {
	b.calls++
	return b.Backend.Torrents()
}

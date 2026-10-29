package filesystem

import (
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// contentsTestBackend publishes immutable payloads while allowing current
// namespace/subtitle snapshots and URL lengths to change between operations.
type contentsTestBackend struct {
	*strmTestBackend
	contentsCalls int
	suffix        string
	contentsErr   error
	contentsHook  func(metainfo.Hash, TorrentContents)
	listingHook   func([]TorrentView)
	urlResultHook func(string)
}

func (b *contentsTestBackend) TorrentContents(hash metainfo.Hash) (TorrentContents, error) {
	b.mu.Lock()
	b.contentsCalls++
	if err := b.contentsErr; err != nil {
		b.mu.Unlock()
		return TorrentContents{}, err
	}
	var contents TorrentContents
	found := false
	for _, view := range b.views {
		if view.Hash == hash {
			contents, found = contentsOfView(view), true
			break
		}
	}
	hook := b.contentsHook
	b.contentsHook = nil
	b.mu.Unlock()
	if hook != nil {
		hook(hash, contents)
	}
	if !found {
		return TorrentContents{}, ErrNotFound
	}
	return contents, nil
}

func (b *contentsTestBackend) Torrents() []TorrentView {
	views := b.strmTestBackend.Torrents()
	b.mu.Lock()
	hook := b.listingHook
	b.listingHook = nil
	b.mu.Unlock()
	if hook != nil {
		hook(views)
	}
	return views
}

func (b *contentsTestBackend) StreamURL(hash metainfo.Hash, path string) (string, error) {
	link, err := b.strmTestBackend.StreamURL(hash, path)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	link += b.suffix
	hook := b.urlResultHook
	b.urlResultHook = nil
	b.mu.Unlock()
	if hook != nil {
		hook(link)
	}
	return link, nil
}

func newContentsTestBackend() *contentsTestBackend {
	b := &contentsTestBackend{strmTestBackend: &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
		{Name: "Collection", Hash: hashN(1), FilesID: &FileListID{}, CreatedAt: time.Unix(1, 0).UTC()},
		{Name: "Solo.mkv", Hash: hashN(2), FilesID: &FileListID{}, SingleFile: true},
	}}}}
	b.addFile(&b.views[0], "deep/Movie.mkv", []byte("video"))
	b.addFile(&b.views[0], "notes.txt", []byte("notes"))
	b.addFile(&b.views[1], "Solo.mkv", []byte("video"))
	return b
}

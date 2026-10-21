package filesystem

import "github.com/anacrolix/torrent/metainfo"

// torrentContents takes exactly one current snapshot. Legacy backends pay for
// one library scan, but a missing/closed result never falls back to inode data.
func torrentContents(backend Backend, hash metainfo.Hash) (TorrentContents, error) {
	if provider, ok := backend.(TorrentContentsBackend); ok {
		return provider.TorrentContents(hash)
	}
	for _, view := range backend.Torrents() {
		if view.Hash == hash {
			return contentsOfView(view), nil
		}
	}
	return TorrentContents{}, ErrNotFound
}

func contentsOfView(view TorrentView) TorrentContents {
	return TorrentContents{
		Files: view.Files, FilesID: view.FilesID, Subtitles: view.Subtitles,
		CreatedAt: view.CreatedAt, SingleFile: view.SingleFile,
	}
}

package vfs

import "github.com/anacrolix/torrent/metainfo"

// LayoutEntry and RootEntry expose immutable layout observations to adapters.
type LayoutEntry = fsEntry
type RootEntry = rootEntry

func (e rootEntry) IsDir() bool { return e.isDir() }

func ChildrenOf(files []FileView, prefix string) []LayoutEntry {
	return childrenOf(files, prefix)
}
func ChildrenOfViews(files []FileView, subtitles []SubtitleView, prefix string) []LayoutEntry {
	return childrenOfViews(files, subtitles, prefix)
}
func MediaRoot(view TorrentView) (FileView, bool) { return mediaRoot(view) }
func RootEntries(views []TorrentView) []RootEntry { return rootEntries(views) }
func RootEntriesForCategory(views []TorrentView, category string, reserved map[string]bool) []RootEntry {
	return rootEntriesForCategory(views, category, reserved)
}
func CategoryReservations(categories []string) map[string]bool {
	return categoryReservations(categories)
}
func UniqueTorrentName(view TorrentView, used map[string]bool) string {
	return uniqueTorrentName(view, used)
}
func TorrentKey(hash metainfo.Hash) string            { return torrentKey(hash) }
func CategoryKey(category string) string              { return categoryKey(category) }
func FileKey(hash metainfo.Hash, rel string) string   { return fileKey(hash, rel) }
func DirKey(hash metainfo.Hash, prefix string) string { return dirKey(hash, prefix) }
func JoinRel(prefix, name string) string              { return joinRel(prefix, name) }

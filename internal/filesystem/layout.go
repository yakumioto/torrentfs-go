package filesystem

import (
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

type fsEntry = vfs.LayoutEntry
type rootEntry = vfs.RootEntry

func childrenOf(files []FileView, prefix string) []fsEntry {
	return vfs.ChildrenOf(files, prefix)
}
func childrenOfViews(files []FileView, subtitles []SubtitleView, prefix string) []fsEntry {
	return vfs.ChildrenOfViews(files, subtitles, prefix)
}
func lookupChild(files []FileView, prefix, name string) (fsEntry, bool) {
	return lookupChildWithSubtitles(files, nil, prefix, name)
}
func lookupChildWithSubtitles(files []FileView, subtitles []SubtitleView, prefix, name string) (fsEntry, bool) {
	for _, entry := range childrenOfViews(files, subtitles, prefix) {
		if entry.Name == name {
			return entry, true
		}
	}
	return fsEntry{}, false
}
func mediaRoot(view TorrentView) (FileView, bool) { return vfs.MediaRoot(view) }
func rootEntries(views []TorrentView) []rootEntry { return vfs.RootEntries(views) }
func rootEntriesForCategory(views []TorrentView, category string, reserved map[string]bool) []rootEntry {
	return vfs.RootEntriesForCategory(views, category, reserved)
}
func categoryReservations(categories []string) map[string]bool {
	return vfs.CategoryReservations(categories)
}
func RootNameFor(view TorrentView, views []TorrentView) (string, bool) {
	return vfs.RootNameFor(view, views)
}
func RootNameForWithCategories(view TorrentView, views []TorrentView, categories []string) (string, bool) {
	return vfs.RootNameForWithCategories(view, views, categories)
}
func TorrentMountPath(view TorrentView, views []TorrentView, categories []string, rel string) (string, bool) {
	return vfs.TorrentMountPath(view, views, categories, rel)
}
func CategoryNameConflicts(name string, views []TorrentView, categories []string) bool {
	return vfs.CategoryNameConflicts(name, views, categories)
}
func uniqueTorrentName(view TorrentView, used map[string]bool) string {
	return vfs.UniqueTorrentName(view, used)
}
func torrentKey(hash metainfo.Hash) string            { return vfs.TorrentKey(hash) }
func categoryKey(category string) string              { return vfs.CategoryKey(category) }
func fileKey(hash metainfo.Hash, rel string) string   { return vfs.FileKey(hash, rel) }
func dirKey(hash metainfo.Hash, prefix string) string { return vfs.DirKey(hash, prefix) }
func joinRel(prefix, name string) string              { return vfs.JoinRel(prefix, name) }

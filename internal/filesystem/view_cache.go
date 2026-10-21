package filesystem

import (
	"slices"

	"github.com/anacrolix/torrent/metainfo"
)

// torrentDirectoryVersion contains only immutable membership. Added times are
// taken from each operation's live contents snapshot, not frozen in this cache.
type torrentDirectoryVersion struct {
	files     []FileView
	filesID   *FileListID
	subtitles []SubtitleView
	prefixes  map[string]*directoryIndexBuild // guarded by fsState.views.mu
	building  chan struct{}                   // serializes first builds per hash/version
}

type directoryIndex struct {
	entries []fsEntry
	byName  map[string]fsEntry
}

type directoryIndexBuild struct {
	ready chan struct{}
	index *directoryIndex
	err   error
}

func (v *torrentDirectoryVersion) matches(contents TorrentContents) bool {
	if v.filesID != contents.FilesID {
		return false
	}
	if contents.FilesID == nil && !slices.Equal(v.files, contents.Files) {
		return false
	}
	return slices.Equal(v.subtitles, contents.Subtitles)
}

// torrentViews keeps root/category names live and prunes deleted entries and
// unpublished placeholders without evicting newer per-hash versions. No
// whole-library root-name cache is kept.
func (s *fsState) torrentViews() []TorrentView {
	observed := s.views.observeListing()
	views := s.backend.Torrents()
	live := make(map[metainfo.Hash]bool, len(views))
	for _, view := range views {
		live[view.Hash] = true
	}
	s.views.finishListing(observed, live)
	return views
}

func (s *fsState) directoryVersion(hash metainfo.Hash, contents TorrentContents, observed snapshotObservation[torrentDirectoryVersion]) *torrentDirectoryVersion {
	s.views.mu.Lock()
	if current := s.views.currentLocked(hash); current != nil && current.matches(contents) {
		s.views.mu.Unlock()
		return current
	}
	s.views.mu.Unlock()

	files := contents.Files
	if contents.FilesID == nil {
		files = slices.Clone(files)
	}
	built := &torrentDirectoryVersion{files: files, filesID: contents.FilesID,
		subtitles: slices.Clone(contents.Subtitles), prefixes: make(map[string]*directoryIndexBuild)}
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if current := s.views.currentLocked(hash); current != nil && current.matches(contents) {
		return current
	}
	s.views.publishLocked(hash, observed, built)
	return built
}

// directory takes one current contents snapshot before using an index. Missing
// hashes and prefixes are never retained. In-flight readers may finish their
// own immutable version, but only the current version can retain a built index.
func (s *fsState) directory(hash metainfo.Hash, prefix string) (TorrentContents, *directoryIndex, error) {
	observed := s.views.observe(hash)
	defer s.views.finish(hash, observed)
	contents, err := torrentContents(s.backend, hash)
	if err != nil {
		s.views.forget(hash, observed)
		return TorrentContents{}, nil, err
	}
	if contents.SingleFile && len(contents.Files) == 1 {
		// A retained directory inode cannot keep a root that is now a file.
		s.views.forget(hash, observed)
		return TorrentContents{}, nil, ErrNotFound
	}
	version := s.directoryVersion(hash, contents, observed)
	index, err := s.indexForVersion(hash, prefix, version)
	return contents, index, err
}

func (s *fsState) indexForVersion(hash metainfo.Hash, prefix string, version *torrentDirectoryVersion) (*directoryIndex, error) {
	var pending *directoryIndexBuild
	for {
		s.views.mu.Lock()
		if existing := version.prefixes[prefix]; existing != nil {
			s.views.mu.Unlock()
			<-existing.ready
			return existing.index, existing.err
		}
		if building := version.building; building != nil {
			s.views.mu.Unlock()
			<-building
			continue
		}
		pending = &directoryIndexBuild{ready: make(chan struct{})}
		version.prefixes[prefix] = pending
		version.building = pending.ready
		s.directoryBuilds++
		s.views.mu.Unlock()
		break
	}

	// Reuse the original layout function unchanged: directories win over
	// leaves, duplicate payloads keep their first entry, and subtitles cannot
	// shadow payloads. Prefix matching is intentionally purely textual.
	entries := childrenOfViews(version.files, version.subtitles, prefix)
	var index *directoryIndex
	if prefix == "" || len(entries) != 0 {
		index = &directoryIndex{entries: entries, byName: make(map[string]fsEntry, len(entries))}
		for _, entry := range entries {
			index.byName[entry.Name] = entry
		}
	}
	s.views.mu.Lock()
	pending.index = index
	if index == nil {
		pending.err = ErrNotFound
	}
	if index == nil || s.views.currentLocked(hash) != version {
		delete(version.prefixes, prefix)
	}
	version.building = nil
	close(pending.ready)
	s.views.mu.Unlock()
	return pending.index, pending.err
}

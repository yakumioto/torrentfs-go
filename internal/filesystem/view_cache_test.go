package filesystem

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
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

func TestTorrentContentsHelperUsesOneSnapshotAndNeverFallsBack(t *testing.T) {
	legacy := &legacyCountingBackend{Backend: &fakeBackend{views: []TorrentView{{Hash: hashN(1), Files: []FileView{{Path: "one"}}}}}}
	if contents, err := torrentContents(legacy, hashN(1)); err != nil || len(contents.Files) != 1 || legacy.calls != 1 {
		t.Fatalf("legacy contents = %+v/%v, scans %d", contents, err, legacy.calls)
	}
	if _, err := torrentContents(legacy, hashN(2)); !errors.Is(err, ErrNotFound) || legacy.calls != 2 {
		t.Fatalf("missing legacy contents = %v, scans %d", err, legacy.calls)
	}
	trusted := newContentsTestBackend()
	trusted.contentsErr = ErrClosed
	if _, err := torrentContents(trusted, hashN(1)); !errors.Is(err, ErrClosed) || trusted.snapshots() != 0 {
		t.Fatalf("optional interface error fell back to a listing: %v, scans %d", err, trusted.snapshots())
	}
}

func TestDirectoryIndexPreservesLayoutAndExactSubtitleVersions(t *testing.T) {
	b := newContentsTestBackend()
	view := TorrentView{Name: "Collection", Hash: hashN(1), FilesID: &FileListID{}, Files: []FileView{
		{Path: "duplicate", Size: 1}, {Path: "duplicate", Size: 2},
		{Path: "dir", Size: 3}, {Path: "dir/child", Size: 4},
		{Path: "dir-sibling", Size: 5}, {Path: "zero", Size: 0},
		{Path: "special %#?/中文.mkv", Size: 6}, {Path: "literal/../leaf", Size: 7},
	}, Subtitles: []SubtitleView{
		{Path: "duplicate", Size: 99}, {Path: "only.srt", VideoPath: "old.mkv", Size: 1},
		{Path: "only.srt", VideoPath: "old.mkv", Size: 2},
		{Path: "sidecars/deep/one.srt", VideoPath: "dir/child", Size: 0},
	}}
	b.setViews([]TorrentView{view})
	state := newFSState(b)
	for _, prefix := range []string{"", "dir", "special %#?", "literal/..", "sidecars/deep"} {
		_, index, err := state.directory(hashN(1), prefix)
		if err != nil || !reflect.DeepEqual(index.entries, childrenOfViews(view.Files, view.Subtitles, prefix)) {
			t.Fatalf("cached layout prefix %q = %+v/%v", prefix, index, err)
		}
		for _, entry := range index.entries {
			if got := index.byName[entry.Name]; got != entry {
				t.Fatalf("name index disagrees for %q: %+v/%+v", entry.Name, got, entry)
			}
		}
	}
	_, rootIndex, err := state.directory(hashN(1), "")
	if err != nil {
		t.Fatal(err)
	}
	if rootIndex.byName["duplicate"].Size != 1 || rootIndex.byName["duplicate"].IsSubtitle || !rootIndex.byName["dir"].IsDir || rootIndex.byName["only.srt"].Size != 2 {
		t.Fatal("directory, duplicate payload, or subtitle priority changed")
	}
	if _, index, err := state.directory(hashN(1), "dir-"); !errors.Is(err, ErrNotFound) || index != nil {
		t.Fatalf("prefix matched a textual sibling: %+v/%v", index, err)
	}
	previous := state.views.slots[hashN(1)].current
	builds := state.directoryBuilds
	for _, field := range []string{"Path", "VideoPath", "Size", "ModifiedAt"} {
		changed := slices.Clone(b.views)
		changed[0].Subtitles = slices.Clone(changed[0].Subtitles)
		subtitle := &changed[0].Subtitles[1]
		switch field {
		case "Path":
			subtitle.Path = "changed.srt"
		case "VideoPath":
			subtitle.VideoPath = "new.mkv"
		case "Size":
			subtitle.Size++
		case "ModifiedAt":
			subtitle.ModifiedAt = time.Unix(42, 123).UTC()
		}
		b.setViews(changed)
		_, current, err := state.directory(hashN(1), "")
		if err != nil || current == rootIndex || state.views.slots[hashN(1)].current == previous {
			t.Fatalf("subtitle %s was not part of exact version comparison: %v", field, err)
		}
		previous = state.views.slots[hashN(1)].current
		if len(previous.prefixes) != 1 {
			t.Fatalf("new version retained old prefix indexes: %d", len(previous.prefixes))
		}
	}
	if state.directoryBuilds != builds+4 || b.snapshots() != 0 {
		t.Fatalf("version builds/lists = %d/%d", state.directoryBuilds-builds, b.snapshots())
	}
	// A disappeared subtitle-only prefix must disappear on its existing inode.
	node := &torrentDirNode{state: state, hash: hashN(1), prefix: "sidecars/deep"}
	if errno := node.Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != 0 {
		t.Fatal(errno)
	}
	changed := slices.Clone(b.views)
	changed[0].Subtitles = nil
	b.setViews(changed)
	if errno := node.Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != syscall.ENOENT {
		t.Fatalf("removed prefix Getattr = %v, want ENOENT", errno)
	}
	if stream, errno := node.Readdir(context.Background()); stream != nil || errno != syscall.ENOENT {
		t.Fatalf("removed prefix Readdir = %T/%v", stream, errno)
	}
	if _, errno := node.Lookup(context.Background(), "one.srt", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("removed prefix Lookup = %v", errno)
	}
	for prefix := range state.views.slots[hashN(1)].current.prefixes {
		if prefix == "sidecars/deep" {
			t.Fatal("missing prefix was retained in the cache")
		}
	}
}

func TestDirectoryLegacyInPlaceMutationsKeepOwnedComparisonSnapshots(t *testing.T) {
	fake := &fakeBackend{views: []TorrentView{{Hash: hashN(1), Files: []FileView{{Path: "deep/old", Size: 1}},
		Subtitles: []SubtitleView{{Path: "deep/old.srt", Size: 2}}}}}
	b := &legacyCountingBackend{Backend: fake}
	state := newFSState(b)
	_, first, err := state.directory(hashN(1), "deep")
	if err != nil {
		t.Fatal(err)
	}
	fake.views[0].Files[0] = FileView{Path: "deep/new", Size: 99}
	fake.views[0].Subtitles[0].Size = 42
	_, second, err := state.directory(hashN(1), "deep")
	if err != nil || first == second || second.byName["new"].Size != 99 || second.byName["old.srt"].Size != 42 {
		t.Fatalf("in-place legacy update was missed: %+v/%v", second, err)
	}
	if first.byName["old"].Size != 1 || first.byName["old.srt"].Size != 2 {
		t.Fatal("legacy backend mutated the cached comparison snapshot")
	}
	_, third, err := state.directory(hashN(1), "deep")
	if err != nil || third != second || state.directoryBuilds != 2 || b.calls != 3 {
		t.Fatalf("legacy hot lookup builds/scans = %d/%d, error %v", state.directoryBuilds, b.calls, err)
	}
}

func TestDirectoryConcurrentFirstIndexBuildsOnce(t *testing.T) {
	b := newContentsTestBackend()
	state := newFSState(b)
	start := make(chan struct{})
	results := make(chan *directoryIndex, 32)
	errors := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			<-start
			_, index, err := state.directory(hashN(1), "deep")
			if err != nil {
				errors <- err
				return
			}
			results <- index
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	var first *directoryIndex
	for index := range results {
		if first == nil {
			first = index
		} else if index != first {
			t.Fatal("concurrent readers did not share a single prefix index")
		}
	}
	if state.directoryBuilds != 1 || b.snapshots() != 0 {
		t.Fatalf("actual directory constructions/lists = %d/%d, want 1/0", state.directoryBuilds, b.snapshots())
	}
}

func TestDirectoryLateSnapshotCannotReplaceNewVersionOrReviveDeletion(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted-%v", deleted), func(t *testing.T) {
			b := newContentsTestBackend()
			state := newFSState(b)
			if _, _, err := state.directory(hashN(1), "deep"); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			b.contentsHook = func(metainfo.Hash, TorrentContents) { close(started); <-release }
			done := make(chan error, 1)
			go func() {
				_, index, err := state.directory(hashN(1), "deep")
				if err == nil && index.byName["Movie.mkv"].Size != 5 {
					err = errors.New("old request did not keep its original whole snapshot")
				}
				done <- err
			}()
			<-started
			if deleted {
				b.setViews(nil)
				if _, _, err := state.directory(hashN(1), "deep"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("deleted contents error = %v", err)
				}
			} else {
				views := strmTestClone(b.views)
				views[0].FilesID = &FileListID{}
				views[0].Files[0].Size = 99
				b.setViews(views)
				if _, index, err := state.directory(hashN(1), "deep"); err != nil || index.byName["Movie.mkv"].Size != 99 {
					t.Fatalf("new contents = %+v/%v", index, err)
				}
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if deleted {
				if len(state.views.slots) != 0 {
					t.Fatal("late snapshot resurrected the deleted hash")
				}
			} else if current := state.views.slots[hashN(1)].current; current.files[0].Size != 99 || len(current.prefixes) != 1 {
				t.Fatal("late old snapshot replaced the current cached version")
			}
		})
	}
}

func TestDirectoryListingFencesLatePlaceholderPublication(t *testing.T) {
	b, releaseListing := newListingGateBackend(t)
	readStarted, readRelease := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	t.Cleanup(releaseRead)
	b.contentsHook = func(metainfo.Hash, TorrentContents) { close(readStarted); <-readRelease }
	state := newFSState(b)
	listed := make(chan []TorrentView, 1)
	go func() { listed <- state.torrentViews() }()
	<-b.listingStarted

	read := make(chan error, 1)
	go func() {
		_, index, err := state.directory(hashN(1), "deep")
		if err == nil && index.byName["Movie.mkv"].Size != 5 {
			err = errors.New("old request did not keep its original whole snapshot")
		}
		read <- err
	}()
	<-readStarted
	b.setViews(nil)
	releaseListing()
	if views := <-listed; len(views) != 0 {
		t.Fatalf("deleted listing contains %d torrents", len(views))
	}
	releaseRead()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	state.views.mu.Lock()
	retained, pending := len(state.views.slots), len(state.views.listings)
	state.views.mu.Unlock()
	if retained != 0 || pending != 0 {
		t.Fatalf("late old directory read republished %d deleted cache entries after the empty listing completed; pending listings %d", retained, pending)
	}
}

func TestDirectoryListingFencesRecreatedPlaceholderPublication(t *testing.T) {
	b, releaseListing := newListingGateBackend(t)
	state := newFSState(b)
	if _, _, err := state.directory(hashN(1), "deep"); err != nil {
		t.Fatal(err)
	}
	views := strmTestClone(b.views)
	state.views.mu.Lock()
	firstSlot := state.views.slots[hashN(1)]
	state.views.mu.Unlock()
	listed := make(chan []TorrentView, 1)
	go func() { listed <- state.torrentViews() }()
	<-b.listingStarted

	b.setViews(nil)
	if _, _, err := state.directory(hashN(1), "deep"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authoritative miss = %v", err)
	}
	state.views.mu.Lock()
	cleared := len(state.views.slots) == 0
	state.views.mu.Unlock()
	if !cleared {
		t.Fatal("authoritative miss did not clear the original slot")
	}
	views[0].FilesID = &FileListID{}
	b.setViews(views)
	readStarted, readRelease := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	t.Cleanup(releaseRead)
	b.contentsHook = func(metainfo.Hash, TorrentContents) { close(readStarted); <-readRelease }
	read := make(chan error, 1)
	go func() {
		contents, index, err := state.directory(hashN(1), "deep")
		if err == nil && (contents.FilesID != views[0].FilesID || index.byName["Movie.mkv"].Size != 5) {
			err = errors.New("re-added request did not keep its original whole snapshot")
		}
		read <- err
	}()
	<-readStarted
	state.views.mu.Lock()
	slot := state.views.slots[hashN(1)]
	recreated := slot != nil && slot != firstSlot && slot.current == nil
	state.views.mu.Unlock()
	if !recreated {
		t.Fatal("re-added read did not create a new empty slot")
	}
	b.setViews(nil)
	releaseListing()
	if old := <-listed; len(old) != 0 {
		t.Fatalf("deleted listing contains %d torrents", len(old))
	}
	releaseRead()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	state.views.mu.Lock()
	retained, pending := len(state.views.slots), len(state.views.listings)
	state.views.mu.Unlock()
	if retained != 0 || pending != 0 {
		t.Fatalf("late directory read republished a recreated deleted slot: slots=%d, pending=%d", retained, pending)
	}

	readded := strmTestClone(views)
	readded[0].FilesID = &FileListID{}
	readded[0].Files[0].Size = 99
	b.setViews(readded)
	_, index, err := state.directory(hashN(1), "deep")
	if err != nil || index.byName["Movie.mkv"].Size != 99 {
		t.Fatalf("legitimate re-add contents = %+v/%v", index, err)
	}
	state.views.mu.Lock()
	current := state.views.currentLocked(hashN(1))
	retained = len(state.views.slots)
	state.views.mu.Unlock()
	if current == nil || current.filesID != readded[0].FilesID || retained != 1 {
		t.Fatal("legitimate re-add did not publish a fresh directory cache entry")
	}
}

func TestDirectoryOldEmptyListingPreservesPublishedVersion(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached-%v", cached), func(t *testing.T) {
			b := newContentsTestBackend()
			state := newFSState(b)
			if cached {
				if _, _, err := state.directory(hashN(1), "deep"); err != nil {
					t.Fatal(err)
				}
			}
			views := strmTestClone(b.views)
			b.setViews(nil)
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			b.listingHook = func([]TorrentView) { close(started); <-release }
			listed := make(chan []TorrentView, 1)
			go func() { listed <- state.torrentViews() }()
			<-started

			views[0].FilesID = &FileListID{}
			views[0].Files[0].Size = 99
			b.setViews(views)
			_, published, err := state.directory(hashN(1), "deep")
			if err != nil || published.byName["Movie.mkv"].Size != 99 {
				t.Fatalf("new contents = %+v/%v", published, err)
			}
			state.views.mu.Lock()
			version := state.views.currentLocked(hashN(1))
			state.views.mu.Unlock()
			if version == nil || version.filesID != views[0].FilesID {
				t.Fatal("new directory version was not published")
			}
			unblock()
			if old := <-listed; len(old) != 0 {
				t.Fatalf("old listing lost its empty snapshot: %d torrents", len(old))
			}
			state.views.mu.Lock()
			retained := state.views.currentLocked(hashN(1)) == version
			state.views.mu.Unlock()
			if !retained {
				t.Fatal("old empty listing evicted a newer published directory version")
			}
			_, current, err := state.directory(hashN(1), "deep")
			if err != nil || current != published || current.byName["Movie.mkv"].Size != 99 {
				t.Fatalf("lookup after the old listing did not reuse the new index: %+v/%v", current, err)
			}
		})
	}
}

func TestDirectoryCacheChurnPrunesHashesAndNeverCachesMissingPrefixes(t *testing.T) {
	b := newContentsTestBackend()
	state := newFSState(b)
	for i := 1; i <= 200; i++ {
		hash := hashN(byte(i))
		view := TorrentView{Hash: hash, Name: fmt.Sprintf("Torrent-%d", i), FilesID: &FileListID{},
			Files: []FileView{{Path: "deep/movie.mkv", Size: 1}}}
		b.setViews([]TorrentView{view})
		state.torrentViews()
		if _, _, err := state.directory(hash, "deep"); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 5; j++ {
			if _, _, err := state.directory(hash, fmt.Sprintf("missing-%d-%d", i, j)); !errors.Is(err, ErrNotFound) {
				t.Fatal("missing prefix did not fail closed")
			}
		}
		if len(state.views.slots) != 1 || len(state.views.slots[hash].current.prefixes) != 1 {
			t.Fatal("churn retained old hashes or missing prefixes")
		}
		state.inoFor(fileKey(hash, "deep/movie.mkv"))
	}
	b.setViews(nil)
	state.torrentViews()
	if len(state.views.slots) != 0 || len(state.inoByKey) != 200 {
		t.Fatalf("cache/inode retention = %d/%d, want 0/200", len(state.views.slots), len(state.inoByKey))
	}
	for i := 0; i < 200; i++ {
		if _, _, err := state.directory(hashN(byte(i)), "deep"); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing hash did not fail closed")
		}
	}
	if len(state.views.slots) != 0 {
		t.Fatal("missing hashes were retained")
	}
}

func TestDirectoryOldBridgeNodesFailClosedAndReaddWithStableIdentity(t *testing.T) {
	b := newContentsTestBackend()
	state := newFSState(b)
	root := &rootNode{state: state}
	raw := fs.NewNodeFS(root, nil)
	top := fileSnapshotRawLookup(t, raw, 1, "Collection", fuse.OK).NodeId
	deep := fileSnapshotRawLookup(t, raw, top, "deep", fuse.OK).NodeId
	fileSnapshotRawLookup(t, raw, deep, "Movie.mkv", fuse.OK)
	original := strmTestClone(b.views)
	b.setViews(nil)
	for _, node := range []uint64{top, deep} {
		if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: node}}, &fuse.AttrOut{}); status != fuse.Status(syscall.ENOENT) {
			t.Fatalf("deleted directory GetAttr = %v", status)
		}
		fileSnapshotRawLookup(t, raw, node, "Movie.mkv", fuse.Status(syscall.ENOENT))
	}
	original[0].FilesID = &FileListID{}
	original[0].CreatedAt = time.Unix(1234, 567).UTC()
	b.setViews(original)
	if again := fileSnapshotRawLookup(t, raw, 1, "Collection", fuse.OK).NodeId; again != top {
		t.Fatalf("readded top NodeId = %d, want retained %d", again, top)
	}
	if again := fileSnapshotRawLookup(t, raw, top, "deep", fuse.OK).NodeId; again != deep {
		t.Fatal("readded nested inode identity changed")
	}
	var attr fuse.AttrOut
	if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: deep}}, &attr); status != fuse.OK {
		t.Fatal(status)
	}
	assertAttrTimes(t, &attr.Attr, original[0].CreatedAt)
}

func TestDirectoryConcurrentLookupReaddirGetattrWithUpdatesAndClose(t *testing.T) {
	for _, strm := range []bool{false, true} {
		t.Run(fmt.Sprintf("strm-%v", strm), func(t *testing.T) {
			b := newContentsTestBackend()
			var backend Backend = b
			if strm {
				backend = NewSTRMBackend(b)
			}
			state := newFSState(backend)
			node := &torrentDirNode{state: state, hash: hashN(1), prefix: "deep"}
			_ = fs.NewNodeFS(node, nil)
			initial := strmTestClone(b.views)
			leaf := "Movie.mkv"
			if strm {
				leaf = "Movie.strm"
			}
			errCh := make(chan error, 8)
			var wg sync.WaitGroup
			wg.Go(func() {
				for i := 0; i < 150; i++ {
					views := slices.Clone(initial)
					views[0].Name, views[0].Category = fmt.Sprint(i), "movies"
					if i%3 == 0 {
						views[0].Subtitles = []SubtitleView{{Path: "deep/Movie.srt", VideoPath: "deep/Movie.mkv", Size: int64(i)}}
					}
					if i%7 == 0 {
						views = nil
					}
					b.setViews(views)
				}
				b.mu.Lock()
				b.contentsErr, b.views = ErrClosed, nil
				b.mu.Unlock()
			})
			for worker := 0; worker < 4; worker++ {
				wg.Go(func() {
					for i := 0; i < 100; i++ {
						if errno := node.Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != 0 && errno != syscall.ENOENT && errno != syscall.EBADF {
							errCh <- fmt.Errorf("concurrent Getattr: %v", errno)
							return
						}
						if _, errno := node.Lookup(context.Background(), leaf, &fuse.EntryOut{}); errno != 0 && errno != syscall.ENOENT && errno != syscall.EBADF {
							errCh <- fmt.Errorf("concurrent Lookup: %v", errno)
							return
						}
						stream, errno := node.Readdir(context.Background())
						if errno != 0 && errno != syscall.ENOENT && errno != syscall.EBADF {
							errCh <- fmt.Errorf("concurrent Readdir: %v", errno)
							return
						}
						if stream != nil {
							stream.Close()
						}
					}
				})
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				t.Error(err)
			}
			if _, _, err := state.directory(hashN(1), "deep"); !errors.Is(err, ErrClosed) || len(state.views.slots) != 0 {
				t.Fatalf("closed directory cache = %d/%v", len(state.views.slots), err)
			}
			if _, payload := b.counts(); payload != 0 {
				t.Fatalf("directory operations opened payload %d times", payload)
			}
		})
	}
}

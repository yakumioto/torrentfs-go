package filesystem

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
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

func TestSTRMContentsSharesTrustedProjectionWithoutLibraryEnumeration(t *testing.T) {
	b := newContentsTestBackend()
	original := strmTestClone(b.views)
	projection := NewSTRMBackend(b).(*struct {
		*strmBackend
		CategoryBackend
		SubtitleBackend
		SubtitleStatBackend
	}).strmBackend
	first, err := projection.TorrentContents(hashN(1))
	if err != nil || first.FilesID == nil || first.FilesID == b.views[0].FilesID {
		t.Fatalf("derived contents identity = %p/%v", first.FilesID, err)
	}
	for i := 0; i < 5; i++ {
		again, err := projection.TorrentContents(hashN(1))
		if err != nil || again.FilesID != first.FilesID || &again.Files[0] != &first.Files[0] {
			t.Fatalf("hot projection did not share its immutable slice: %v", err)
		}
	}
	if b.snapshots() != 0 || projection.mappingBuilds != 1 {
		t.Fatalf("narrow projection lists/builds = %d/%d, want 0/1", b.snapshots(), projection.mappingBuilds)
	}
	if stream, payload := b.counts(); stream != 1 || payload != 0 {
		t.Fatalf("hot projection URL/payload calls = %d/%d, want 1/0", stream, payload)
	}
	views := strmTestClone(original)
	views[0].Name, views[0].Category = "renamed", "movies"
	views[0].Subtitles = []SubtitleView{{Path: "deep/Movie.srt", VideoPath: "deep/Movie.mkv", Size: 17}}
	b.setViews(views)
	current, err := projection.TorrentContents(hashN(1))
	if err != nil || current.FilesID != first.FilesID || len(current.Subtitles) != 1 {
		t.Fatalf("dynamic metadata changed immutable projection: %+v/%v", current, err)
	}
	current.Subtitles[0].Path = "caller-only.srt"
	listed := projection.Torrents()
	if listed[0].FilesID != first.FilesID || listed[0].Name != "renamed" || listed[0].Subtitles[0].Path != "deep/Movie.srt" {
		t.Fatal("root naming or subtitle copying violated the narrow snapshot contract")
	}
	if !reflect.DeepEqual(b.views[0].Files, original[0].Files) {
		t.Fatal("projection modified source payload")
	}
	// SingleFile changes the payload path rule, even with the same source ID.
	shape := []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), FilesID: &FileListID{},
		Files: []FileView{{Path: "deep/Movie.mkv", Size: 5}}}}
	b.setViews(shape)
	multi, err := projection.TorrentContents(hashN(1))
	if err != nil || multi.Files[0].Path != "deep/Movie.strm" {
		t.Fatal("directory projection did not transform the path")
	}
	shape = slices.Clone(shape)
	shape[0].SingleFile = true
	b.setViews(shape)
	single, err := projection.TorrentContents(hashN(1))
	if err != nil || single.Files[0].Path != "deep/Movie.mkv" || single.FilesID == multi.FilesID {
		t.Fatalf("SingleFile was omitted from the mapping key: %+v/%v", single, err)
	}
}

func TestSTRMLegacyContentsDetectsInPlaceChangesAndReturnsCopies(t *testing.T) {
	b := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "legacy", Hash: hashN(1),
		Files:     []FileView{{Path: "Movie.mkv", Size: 5}, {Path: "notes.txt", Size: 7}},
		Subtitles: []SubtitleView{{Path: "Movie.srt", Size: 2}},
	}}}}
	projection := NewSTRMBackend(b).(TorrentContentsBackend)
	first, err := projection.TorrentContents(hashN(1))
	if err != nil || first.FilesID != nil || b.snapshots() != 1 {
		t.Fatalf("legacy contents identity/listings = %p/%d/%v", first.FilesID, b.snapshots(), err)
	}
	first.Files[0].Path, first.Subtitles[0].Path = "caller-only", "caller-only.srt"
	// Intentionally mutate the legacy backend's existing arrays in place.
	b.views[0].Files[1].Path, b.views[0].Files[1].Size = "renamed.txt", 99
	b.views[0].Subtitles[0].Size = 33
	again, err := projection.TorrentContents(hashN(1))
	if err != nil || again.FilesID != nil || b.snapshots() != 2 || again.Files[0].Path != "Movie.strm" ||
		again.Files[1] != (FileView{Path: "renamed.txt", Size: 99}) || again.Subtitles[0].Size != 33 {
		t.Fatalf("legacy mutation was missed: %+v/%v", again, err)
	}
	if b.views[0].Files[0].Path != "Movie.mkv" || b.views[0].Subtitles[0].Path != "Movie.srt" {
		t.Fatal("legacy returned slices alias their source")
	}
}

func TestSTRMContentsRetriesFailuresAndPublishesNewURLSizeIdentity(t *testing.T) {
	b := newContentsTestBackend()
	key := hashN(1).HexString() + "\x00deep/Movie.mkv"
	b.streamErrors = map[string]error{key: ErrPermission}
	projection := NewSTRMBackend(b)
	contents := projection.(TorrentContentsBackend)
	first, err := contents.TorrentContents(hashN(1))
	if err != nil || first.FilesID != nil || first.Files[0].Size != 0 {
		t.Fatalf("incomplete projection = %+v/%v", first, err)
	}
	if stream, _ := b.counts(); stream != 1 {
		t.Fatalf("failed operation retried a signature %d times", stream)
	}
	b.mu.Lock()
	b.streamErrors = nil
	b.mu.Unlock()
	recovered, err := contents.TorrentContents(hashN(1))
	if err != nil || recovered.FilesID == nil || recovered.Files[0].Size == 0 || first.Files[0].Size != 0 {
		t.Fatalf("recovery froze or mutated the failed projection: %+v/%v", recovered, err)
	}
	previous := recovered
	for i, suffix := range []string{"?long=" + strings.Repeat("x", 100), "?short=x", "?open=" + strings.Repeat("y", 250)} {
		b.mu.Lock()
		b.suffix = suffix
		b.mu.Unlock()
		want := int64(len(strmTestURL(hashN(1), "deep/Movie.mkv")+suffix) + 1)
		if i < 2 {
			stat, err := projection.(FileStatBackend).FileStat(hashN(1), "deep/Movie.strm")
			if err != nil || stat.Size != want {
				t.Fatalf("live size = %d/%v, want %d", stat.Size, err, want)
			}
		} else {
			opened, err := projection.(FileSnapshotBackend).OpenFileSnapshot(hashN(1), "deep/Movie.strm")
			if err != nil || opened.Size != want || int64(len(strmTestRead(t, opened.Reader))) != want {
				t.Fatalf("open text and size were not bound: size %d/%v", opened.Size, err)
			}
		}
		current, err := contents.TorrentContents(hashN(1))
		if err != nil || current.FilesID == nil || current.FilesID == previous.FilesID || current.Files[0].Size != want {
			t.Fatalf("known size change did not publish a new identity: %+v/%v", current, err)
		}
		if previous.Files[0].Size == want {
			t.Fatal("previous immutable projection was modified in place")
		}
		previous = current
	}
	if recovered.Files[0].Size != int64(len(strmTestURL(hashN(1), "deep/Movie.mkv"))+1) {
		t.Fatal("original completed projection was mutated")
	}
	if b.snapshots() != 0 {
		t.Fatalf("narrow size refresh enumerated library %d times", b.snapshots())
	}
}

func TestSTRMConcurrentDifferentFileSizeBackfillsBothPublish(t *testing.T) {
	b := newContentsTestBackend()
	b.addFile(&b.views[0], "deep/Other.mkv", []byte("other video"))
	projection := NewSTRMBackend(b)
	contents := projection.(TorrentContentsBackend)
	old, err := contents.TorrentContents(hashN(1))
	if err != nil {
		t.Fatal(err)
	}
	oldFiles := slices.Clone(old.Files)
	startedA, releaseA := make(chan struct{}), make(chan struct{})
	startedB, releaseB := make(chan struct{}), make(chan struct{})
	unblockA := sync.OnceFunc(func() { close(releaseA) })
	unblockB := sync.OnceFunc(func() { close(releaseB) })
	t.Cleanup(unblockA)
	t.Cleanup(unblockB)
	b.mu.Lock()
	b.suffix = "?changed=" + strings.Repeat("x", 100)
	b.urlResultHook = func(string) { close(startedA); <-releaseA }
	b.mu.Unlock()
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := projection.(FileStatBackend).FileStat(hashN(1), "deep/Movie.strm")
		doneA <- err
	}()
	<-startedA
	b.mu.Lock()
	b.urlResultHook = func(string) { close(startedB); <-releaseB }
	b.mu.Unlock()
	go func() {
		opened, err := projection.(FileSnapshotBackend).OpenFileSnapshot(hashN(1), "deep/Other.strm")
		if err == nil {
			buf := make([]byte, opened.Size)
			n, readErr := opened.Reader.ReadAt(buf, 0)
			if (readErr != nil && !errors.Is(readErr, io.EOF)) || int64(n) != opened.Size {
				err = fmt.Errorf("open length did not match its reader: %d/%d/%v", n, opened.Size, readErr)
			}
		}
		doneB <- err
	}()
	<-startedB
	// Both payload references were acquired before either URL result returned.
	unblockA()
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	unblockB()
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
	current, err := contents.TorrentContents(hashN(1))
	if err != nil || current.FilesID == nil || current.FilesID == old.FilesID {
		t.Fatalf("updated projection = %+v/%v", current, err)
	}
	for _, i := range []int{0, 2} {
		want := int64(len(strmTestURL(hashN(1), b.views[0].Files[i].Path)+b.suffix) + 1)
		if current.Files[i].Size != want {
			t.Fatalf("concurrent file %q backfill was discarded: size %d, want %d", current.Files[i].Path, current.Files[i].Size, want)
		}
	}
	if !reflect.DeepEqual(old.Files, oldFiles) {
		t.Fatal("concurrent backfills changed an already published slice")
	}
	if stream, payload := b.counts(); stream != 4 || payload != 0 {
		t.Fatalf("size backfill URLs/payload calls = %d/%d, want 4/0", stream, payload)
	}
}

func TestSTRMLateSameFileBackfillCannotRollBackNewerSize(t *testing.T) {
	b := newContentsTestBackend()
	projection := NewSTRMBackend(b)
	contents := projection.(TorrentContentsBackend)
	if _, err := contents.TorrentContents(hashN(1)); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	b.urlResultHook = func(string) { close(started); <-release }
	done := make(chan error, 1)
	go func() { _, err := projection.(FileStatBackend).FileStat(hashN(1), "deep/Movie.strm"); done <- err }()
	<-started
	b.mu.Lock()
	b.suffix = "?new=" + strings.Repeat("n", 100)
	b.mu.Unlock()
	stat, err := projection.(FileStatBackend).FileStat(hashN(1), "deep/Movie.strm")
	if err != nil {
		t.Fatal(err)
	}
	current, err := contents.TorrentContents(hashN(1))
	if err != nil || current.Files[0].Size != stat.Size {
		t.Fatalf("new size was not published: %+v/%v", current, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	again, err := contents.TorrentContents(hashN(1))
	if err != nil || again.FilesID != current.FilesID || again.Files[0].Size != stat.Size {
		t.Fatalf("late same-file size replaced the newer snapshot: %+v/%v", again, err)
	}
}

func TestSTRMLateSizeBackfillDoesNotModifyReaddedHash(t *testing.T) {
	b := newContentsTestBackend()
	projection := NewSTRMBackend(b)
	contents := projection.(TorrentContentsBackend)
	old, err := contents.TorrentContents(hashN(1))
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	b.mu.Lock()
	b.urlResultHook = func(string) { close(started); <-release }
	b.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := projection.(FileStatBackend).FileStat(hashN(1), "deep/Movie.strm"); done <- err }()
	<-started
	views := strmTestClone(b.views)
	b.setViews(nil)
	projection.Torrents()
	views[0].FilesID = &FileListID{}
	b.mu.Lock()
	b.views, b.suffix = views, "?new="+strings.Repeat("n", 100)
	b.mu.Unlock()
	readded, err := contents.TorrentContents(hashN(1))
	if err != nil || readded.FilesID == nil || readded.FilesID == old.FilesID {
		t.Fatalf("readded projection = %+v/%v", readded, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	again, err := contents.TorrentContents(hashN(1))
	if err != nil || again.FilesID != readded.FilesID || again.Files[0].Size != readded.Files[0].Size {
		t.Fatalf("late old result changed the new entry: %+v/%v", again, err)
	}
}

func TestSTRMListingFencesLatePlaceholderPublication(t *testing.T) {
	b, releaseListing := newListingGateBackend(t)
	readStarted, readRelease := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(readRelease) })
	t.Cleanup(releaseRead)
	b.contentsHook = func(metainfo.Hash, TorrentContents) { close(readStarted); <-readRelease }
	projection := &strmBackend{backend: b}
	listed := make(chan []TorrentView, 1)
	go func() { listed <- projection.Torrents() }()
	<-b.listingStarted

	read := make(chan error, 1)
	go func() {
		contents, err := projection.TorrentContents(hashN(1))
		if err == nil && (len(contents.Files) != 2 || contents.Files[1] != (FileView{Path: "notes.txt", Size: 5})) {
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
	projection.cache.mu.Lock()
	retained := len(projection.cache.slots)
	projection.cache.mu.Unlock()
	stat, err := projection.FileStat(hashN(1), "notes.txt")
	projection.cache.mu.Lock()
	afterStat, pending := len(projection.cache.slots), len(projection.cache.listings)
	projection.cache.mu.Unlock()
	if retained != 0 || afterStat != 0 || pending != 0 || !errors.Is(err, ErrNotFound) {
		t.Fatalf("late old STRM read crossed the completed empty listing: slots=%d, after-stat=%d, pending=%d, stat.Size=%d, err=%v", retained, afterStat, pending, stat.Size, err)
	}
}

func TestSTRMListingFencesRecreatedPlaceholderPublication(t *testing.T) {
	b, releaseListing := newListingGateBackend(t)
	projection := &strmBackend{backend: b}
	if _, err := projection.TorrentContents(hashN(1)); err != nil {
		t.Fatal(err)
	}
	views := strmTestClone(b.views)
	projection.cache.mu.Lock()
	firstSlot := projection.cache.slots[hashN(1)]
	projection.cache.mu.Unlock()
	listed := make(chan []TorrentView, 1)
	go func() { listed <- projection.Torrents() }()
	<-b.listingStarted

	b.setViews(nil)
	if _, err := projection.TorrentContents(hashN(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authoritative miss = %v", err)
	}
	projection.cache.mu.Lock()
	cleared := len(projection.cache.slots) == 0
	projection.cache.mu.Unlock()
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
		contents, err := projection.TorrentContents(hashN(1))
		if err == nil && (len(contents.Files) != 2 || contents.Files[1] != (FileView{Path: "notes.txt", Size: 5})) {
			err = errors.New("re-added request did not keep its original whole snapshot")
		}
		read <- err
	}()
	<-readStarted
	projection.cache.mu.Lock()
	slot := projection.cache.slots[hashN(1)]
	recreated := slot != nil && slot != firstSlot && slot.current == nil
	projection.cache.mu.Unlock()
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
	projection.cache.mu.Lock()
	retained := len(projection.cache.slots)
	projection.cache.mu.Unlock()
	stat, err := projection.FileStat(hashN(1), "notes.txt")
	projection.cache.mu.Lock()
	afterStat, pending := len(projection.cache.slots), len(projection.cache.listings)
	projection.cache.mu.Unlock()
	if retained != 0 || afterStat != 0 || pending != 0 || !errors.Is(err, ErrNotFound) {
		t.Fatalf("late STRM read republished a recreated deleted slot: slots=%d, after-stat=%d, pending=%d, stat.Size=%d, err=%v", retained, afterStat, pending, stat.Size, err)
	}

	readded := strmTestClone(views)
	readded[0].FilesID = &FileListID{}
	readded[0].Files[1].Size = 99
	b.setViews(readded)
	contents, err := projection.TorrentContents(hashN(1))
	if err != nil || contents.FilesID == nil || contents.Files[1].Size != 99 {
		t.Fatalf("legitimate re-add contents = %+v/%v", contents, err)
	}
	projection.cache.mu.Lock()
	current := projection.cache.currentLocked(hashN(1))
	retained = len(projection.cache.slots)
	projection.cache.mu.Unlock()
	if current == nil || current.sourceID != readded[0].FilesID || retained != 1 {
		t.Fatal("legitimate re-add did not publish a fresh STRM cache entry")
	}
	stat, err = projection.FileStat(hashN(1), "notes.txt")
	if err != nil || stat.Size != 99 {
		t.Fatalf("legitimate re-add stat = %+v/%v", stat, err)
	}
}

func TestSTRMOldEmptyListingPreservesPublishedVersion(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached-%v", cached), func(t *testing.T) {
			b := newContentsTestBackend()
			projection := &strmBackend{backend: b}
			if cached {
				if _, err := projection.TorrentContents(hashN(1)); err != nil {
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
			go func() { listed <- projection.Torrents() }()
			<-started

			views[0].FilesID = &FileListID{}
			views[0].Files[1].Size = 99
			b.setViews(views)
			published, err := projection.TorrentContents(hashN(1))
			if err != nil || published.FilesID == nil || published.Files[1].Size != 99 {
				t.Fatalf("new projection = %+v/%v", published, err)
			}
			projection.cache.mu.Lock()
			entry := projection.cache.currentLocked(hashN(1))
			projection.cache.mu.Unlock()
			if entry == nil || entry.sourceID != views[0].FilesID {
				t.Fatal("new STRM version was not published")
			}
			unblock()
			if old := <-listed; len(old) != 0 {
				t.Fatalf("old listing lost its empty snapshot: %d torrents", len(old))
			}
			projection.cache.mu.Lock()
			retained := projection.cache.currentLocked(hashN(1)) == entry
			projection.cache.mu.Unlock()
			if !retained {
				t.Fatal("old empty listing evicted a newer published STRM version")
			}
			stat, err := projection.FileStat(hashN(1), "notes.txt")
			if err != nil || stat.Size != 99 {
				t.Fatalf("stat after the old listing = %+v/%v", stat, err)
			}
			current, err := projection.TorrentContents(hashN(1))
			if err != nil || current.FilesID != published.FilesID || current.Files[1].Size != 99 {
				t.Fatalf("contents after the old listing did not reuse the new projection: %+v/%v", current, err)
			}
		})
	}
}

func TestSTRMConcurrentFirstProjectionBuildsOnce(t *testing.T) {
	b := newContentsTestBackend()
	projection := NewSTRMBackend(b)
	contents := projection.(TorrentContentsBackend)
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make(chan *FileListID, 32)
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			<-start
			got, err := contents.TorrentContents(hashN(1))
			if err != nil || got.FilesID == nil {
				errCh <- fmt.Errorf("first projection: %v", err)
				return
			}
			ids <- got.FilesID
		})
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	var first *FileListID
	for id := range ids {
		if first == nil {
			first = id
		} else if id != first {
			t.Fatal("concurrent first requests published different identities")
		}
	}
	// Inspect the core through the already checked constructor capability set.
	core := projection.(*struct {
		*strmBackend
		CategoryBackend
		SubtitleBackend
		SubtitleStatBackend
	}).strmBackend
	if core.mappingBuilds != 1 {
		t.Fatalf("actual projection mapping builds = %d, want 1", core.mappingBuilds)
	}
	if stream, payload := b.counts(); stream != 1 || payload != 0 {
		t.Fatalf("first projection URL/payload = %d/%d, want 1/0", stream, payload)
	}
}

func TestSTRMLateListingCannotRollBackAddedTime(t *testing.T) {
	b := newContentsTestBackend()
	projection := NewSTRMBackend(b)
	projection.Torrents()
	started, release := make(chan struct{}), make(chan struct{})
	b.listingHook = func([]TorrentView) { close(started); <-release }
	done := make(chan []TorrentView, 1)
	go func() { done <- projection.Torrents() }()
	<-started
	views := strmTestClone(b.views)
	newTime := time.Unix(12345, 678).UTC()
	views[0].CreatedAt = newTime
	b.setViews(views)
	projection.Torrents()
	close(release)
	<-done
	stat, err := projection.(FileStatBackend).FileStat(hashN(1), "notes.txt")
	if err != nil || !stat.CreatedAt.Equal(newTime) {
		t.Fatalf("late listing rolled back current added time: %v/%v, want %v", stat.CreatedAt, err, newTime)
	}
}

func TestSTRMLateListingDoesNotResurrectUncachedDeletedHash(t *testing.T) {
	b := newContentsTestBackend()
	projection := NewSTRMBackend(b)
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	b.listingHook = func([]TorrentView) { close(started); <-release }
	done := make(chan []TorrentView, 1)
	go func() { done <- projection.Torrents() }()
	<-started
	b.setViews(nil)
	if got := projection.Torrents(); len(got) != 0 {
		t.Fatal("deleted listing was not empty")
	}
	unblock()
	if got := <-done; len(got) != 2 {
		t.Fatal("overlapping old request lost its complete source snapshot")
	}
	core := projection.(*struct {
		*strmBackend
		CategoryBackend
		SubtitleBackend
		SubtitleStatBackend
	}).strmBackend
	if len(core.cache.slots) != 0 || len(core.cache.listings) != 0 {
		t.Fatalf("late listing revived deleted cache entries: %d/%d", len(core.cache.slots), len(core.cache.listings))
	}
	if _, err := projection.(TorrentContentsBackend).TorrentContents(hashN(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing contents = %v", err)
	}
}

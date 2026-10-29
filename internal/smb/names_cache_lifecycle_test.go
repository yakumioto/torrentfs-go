package smb

import (
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

type namesLifecycleFixture struct {
	mu       sync.Mutex
	views    []vfs.TorrentView
	block    atomic.Bool
	started  chan struct{}
	release  chan struct{}
	listings atomic.Int64
}

func (b *namesLifecycleFixture) Torrents() []vfs.TorrentView {
	b.listings.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.views)
}

func (b *namesLifecycleFixture) TorrentContents(hash metainfo.Hash) (vfs.TorrentContents, error) {
	b.mu.Lock()
	var contents vfs.TorrentContents
	err := vfs.ErrNotFound
	for _, view := range b.views {
		if view.Hash == hash {
			contents = vfs.TorrentContents{Files: view.Files, FilesID: view.FilesID, CreatedAt: view.CreatedAt,
				Subtitles: slices.Clone(view.Subtitles), SingleFile: view.SingleFile}
			err = nil
			break
		}
	}
	b.mu.Unlock()
	if b.block.CompareAndSwap(true, false) {
		close(b.started)
		<-b.release
	}
	return contents, err
}

func (b *namesLifecycleFixture) OpenFile(metainfo.Hash, string) (io.ReaderAt, error) {
	return nil, vfs.ErrNotFound
}

func namesCacheSize(adapter *filesystem) int {
	adapter.namesMu.Lock()
	defer adapter.namesMu.Unlock()
	return len(adapter.indexes)
}

func TestSMBNameCacheReleasesDeletedTaskChurnAndTemporaryRoots(t *testing.T) {
	b := &namesLifecycleFixture{}
	tree := vfs.New(b)
	adapter := &filesystem{tree: tree}
	files := make([]vfs.FileView, 300)
	for i := range files {
		files[i] = vfs.FileView{Path: fmt.Sprintf("file-%04d.bin", i), Size: 1}
	}
	for cycle := 0; cycle < 40; cycle++ {
		b.mu.Lock()
		b.views = []vfs.TorrentView{{Name: "Library", Hash: metainfo.Hash{byte(cycle + 1)}, FilesID: &vfs.FileListID{}, Files: files}}
		b.mu.Unlock()
		if _, _, err := adapter.resolve(`Library\file-0299.bin`); err != nil {
			t.Fatal(err)
		}
		opened, err := tree.OpenDir("/Library")
		if err != nil {
			t.Fatal(err)
		}
		index := adapter.nameIndex(opened)
		root, err := tree.OpenDir("/")
		if err != nil {
			t.Fatal(err)
		}
		adapter.nameIndex(root)
		if got := namesCacheSize(adapter); got != 1 {
			t.Fatalf("cycle %d retained %d name indexes; temporary root must not be cached", cycle, got)
		}
		b.mu.Lock()
		b.views = nil
		b.mu.Unlock()
		root, err = tree.OpenDir("/")
		if err != nil {
			t.Fatal(err)
		}
		adapter.nameIndex(root)
		if got := namesCacheSize(adapter); got != 0 {
			t.Fatalf("cycle %d retained %d deleted-task name indexes", cycle, got)
		}
		if len(opened.Entries()) != len(files) || len(index.entries) != len(files) {
			t.Fatal("cache eviction changed a scan which was already handed to a client")
		}
		adapter.nameIndex(opened)
		if got := namesCacheSize(adapter); got != 0 {
			t.Fatalf("late old-directory subscription resurrected %d indexes", got)
		}
	}
}

func TestSMBLateOldContentsCannotReplaceNewerNameCache(t *testing.T) {
	hash := metainfo.Hash{55}
	b := &namesLifecycleFixture{views: []vfs.TorrentView{{Name: "Library", Hash: hash, FilesID: &vfs.FileListID{},
		Files: []vfs.FileView{{Path: "old.bin", Size: 1}}}}, started: make(chan struct{}), release: make(chan struct{})}
	b.block.Store(true)
	release := sync.OnceFunc(func() { close(b.release) })
	defer release()
	tree := vfs.New(b)
	adapter := &filesystem{tree: tree}
	type result struct {
		directory *vfs.Directory
		err       error
	}
	done := make(chan result, 1)
	go func() { directory, err := tree.OpenDir("/Library"); done <- result{directory, err} }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("old contents did not reach the gate")
	}
	b.mu.Lock()
	b.views = []vfs.TorrentView{{Name: "Library", Hash: hash, FilesID: &vfs.FileListID{}, Files: []vfs.FileView{{Path: "new.bin", Size: 1}}}}
	b.mu.Unlock()
	fresh, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	freshIndex := adapter.nameIndex(fresh)
	release()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.directory.Version() <= fresh.Version() {
			t.Fatal("test did not produce the critical late higher-version snapshot")
		}
		oldIndex := adapter.nameIndex(result.directory)
		if oldIndex.byName[foldName("old.bin")].Path != "/Library/old.bin" {
			t.Fatal("late in-flight scan lost its own immutable data")
		}
	case <-time.After(time.Second):
		t.Fatal("old contents did not finish")
	}
	if got := adapter.nameIndex(fresh); got != freshIndex || !reflect.DeepEqual(treeNamesForIndex(got), []string{"new.bin"}) {
		t.Fatal("late old contents replaced the authoritative newer name index")
	}
	if namesCacheSize(adapter) != 1 {
		t.Fatalf("retained %d indexes after stale publication", namesCacheSize(adapter))
	}
}

func treeNamesForIndex(index *nameIndex) []string {
	out := make([]string, len(index.entries))
	for i, entry := range index.entries {
		out[i] = entry.Name
	}
	return out
}

func TestSMBNameCacheDoesNotAddWholeLibraryQueriesToHotResolve(t *testing.T) {
	b := &namesLifecycleFixture{views: []vfs.TorrentView{{Name: "Library", Hash: metainfo.Hash{56}, FilesID: &vfs.FileListID{},
		Files: []vfs.FileView{{Path: "file.bin", Size: 1}}}}}
	adapter := &filesystem{tree: vfs.New(b)}
	for range 20 {
		before := b.listings.Load()
		if _, _, err := adapter.resolve(`Library\file.bin`); err != nil {
			t.Fatal(err)
		}
		// The existing namespace traversal observes root membership once for
		// each directory level; pruning must reuse those same observations.
		if got := b.listings.Load() - before; got != 2 {
			t.Fatalf("hot resolve performed %d whole-library queries, want existing 2", got)
		}
	}
}

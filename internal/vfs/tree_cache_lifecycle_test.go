package vfs

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

func treeDirectoryCacheSize(tree *Tree) int {
	tree.directories.mu.Lock()
	defer tree.directories.mu.Unlock()
	return len(tree.directories.slots)
}

func TestTreeDeletedDirectoryEvictsAuxiliaryCachesButKeepsOpenedScan(t *testing.T) {
	hash := metainfo.Hash{51}
	b := newTreeFixture(TorrentView{Name: "Library", Hash: hash, FilesID: &FileListID{}, Files: []FileView{{Path: "file.bin", Size: 4}}})
	tree := New(b)
	opened, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	var cleanups atomic.Int64
	opened.OnCacheEviction(func() {
		// A cleanup must not execute under either the tree cache mutex or
		// the snapshot mutex: both re-entry paths below must remain legal.
		if _, err := tree.ReadDir("/"); err != nil {
			t.Errorf("reentrant root enumeration: %v", err)
		}
		opened.OnCacheEviction(func() { cleanups.Add(1) })
		cleanups.Add(1)
	})
	b.mu.Lock()
	b.views = nil
	b.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := tree.ReadDir("/"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("directory eviction held a cache/snapshot lock during cleanup")
	}
	if cleanups.Load() != 2 || treeDirectoryCacheSize(tree) != 0 {
		t.Fatalf("cleanup count=%d retained directory versions=%d", cleanups.Load(), treeDirectoryCacheSize(tree))
	}
	if names := treeEntryNames(opened.Entries()); !reflect.DeepEqual(names, []string{"file.bin"}) {
		t.Fatalf("eviction changed an opened scan: %v", names)
	}
	opened.OnCacheEviction(func() { cleanups.Add(1) })
	if cleanups.Load() != 3 {
		t.Fatal("late eviction subscriber was not notified immediately")
	}
}

func TestTreeDirectoryCacheDoesNotAccumulateDeletedTaskChurn(t *testing.T) {
	b := newTreeFixture()
	tree := New(b)
	for cycle := 0; cycle < 40; cycle++ {
		hash := metainfo.Hash{byte(cycle + 1)}
		files := make([]FileView, 200)
		for i := range files {
			files[i] = FileView{Path: fmt.Sprintf("file-%04d.bin", i), Size: 1}
		}
		b.mu.Lock()
		b.views = []TorrentView{{Name: "Library", Hash: hash, FilesID: &FileListID{}, Files: files}}
		b.mu.Unlock()
		opened, err := tree.OpenDir("/Library")
		if err != nil {
			t.Fatal(err)
		}
		if got := treeDirectoryCacheSize(tree); got != 1 {
			t.Fatalf("cycle %d retained %d directory versions before deletion", cycle, got)
		}
		b.mu.Lock()
		b.views = nil
		b.mu.Unlock()
		if _, err := tree.ReadDir("/"); err != nil {
			t.Fatal(err)
		}
		if got := treeDirectoryCacheSize(tree); got != 0 {
			t.Fatalf("cycle %d retained %d deleted directory versions", cycle, got)
		}
		if len(opened.Entries()) != len(files) {
			t.Fatal("deletion prevented the old scan from finishing")
		}
	}
}

type treeContentsGate struct {
	*treeFixture
	block   atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (b *treeContentsGate) TorrentContents(hash metainfo.Hash) (TorrentContents, error) {
	contents, err := b.treeFixture.TorrentContents(hash)
	if b.block.CompareAndSwap(true, false) {
		close(b.started)
		<-b.release
	}
	return contents, err
}

func TestTreeLateContentsCannotResurrectDeletedDirectoryCache(t *testing.T) {
	hash := metainfo.Hash{52}
	b := &treeContentsGate{treeFixture: newTreeFixture(TorrentView{Name: "Library", Hash: hash,
		FilesID: &FileListID{}, Files: []FileView{{Path: "old.bin", Size: 1}}}),
		started: make(chan struct{}), release: make(chan struct{})}
	b.block.Store(true)
	release := sync.OnceFunc(func() { close(b.release) })
	defer release()
	tree := New(b)
	type result struct {
		directory *Directory
		err       error
	}
	done := make(chan result, 1)
	go func() { directory, err := tree.OpenDir("/Library"); done <- result{directory, err} }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("old contents read did not reach the publication gate")
	}
	b.mu.Lock()
	b.views = nil
	b.mu.Unlock()
	if _, err := tree.ReadDir("/"); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case result := <-done:
		if result.err != nil || !reflect.DeepEqual(treeEntryNames(result.directory.Entries()), []string{"old.bin"}) {
			t.Fatalf("old in-flight snapshot = %+v, %v", result.directory, result.err)
		}
		var evicted bool
		result.directory.OnCacheEviction(func() { evicted = true })
		if !evicted {
			t.Fatal("a nonpublished late snapshot retained cache ownership")
		}
	case <-time.After(time.Second):
		t.Fatal("old contents read did not complete")
	}
	if got := treeDirectoryCacheSize(tree); got != 0 {
		t.Fatalf("late old contents resurrected %d cache slots", got)
	}
}

type treeListingGate struct {
	*treeFixture
	block   atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (b *treeListingGate) Torrents() []TorrentView {
	views := b.treeFixture.Torrents()
	if b.block.CompareAndSwap(true, false) {
		close(b.started)
		<-b.release
	}
	return views
}

func TestTreeOlderListingCannotEvictNewerDirectoryVersion(t *testing.T) {
	b := &treeListingGate{treeFixture: newTreeFixture(), started: make(chan struct{}), release: make(chan struct{})}
	b.block.Store(true)
	release := sync.OnceFunc(func() { close(b.release) })
	defer release()
	tree := New(b)
	done := make(chan error, 1)
	go func() { _, err := tree.ReadDir("/"); done <- err }()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("old listing did not reach the publication gate")
	}
	b.mu.Lock()
	b.views = []TorrentView{{Name: "Library", Hash: metainfo.Hash{53}, FilesID: &FileListID{}, Files: []FileView{{Path: "new.bin", Size: 1}}}}
	b.mu.Unlock()
	fresh, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	var evicted atomic.Bool
	fresh.OnCacheEviction(func() { evicted.Store(true) })
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old listing did not finish")
	}
	if evicted.Load() || treeDirectoryCacheSize(tree) != 1 {
		t.Fatal("an older listing discarded a newer published directory version")
	}
	current, err := tree.OpenDir("/Library")
	if err != nil || current != fresh {
		t.Fatalf("newer snapshot was rebuilt after stale listing: %p/%p %v", current, fresh, err)
	}
}

func TestTreeConcurrentEvictionSubscribersAreNotLost(t *testing.T) {
	b := newTreeFixture(TorrentView{Name: "Library", Hash: metainfo.Hash{54}, FilesID: &FileListID{}, Files: []FileView{{Path: "file.bin", Size: 1}}})
	tree := New(b)
	opened, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	start := make(chan struct{})
	var subscribers sync.WaitGroup
	for range 32 {
		subscribers.Add(1)
		go func() {
			defer subscribers.Done()
			<-start
			opened.OnCacheEviction(func() { calls.Add(1) })
		}()
	}
	close(start)
	b.mu.Lock()
	b.views = nil
	b.mu.Unlock()
	if _, err := tree.ReadDir("/"); err != nil {
		t.Fatal(err)
	}
	subscribers.Wait()
	if calls.Load() != 32 {
		t.Fatalf("eviction subscribers called %d times, want 32", calls.Load())
	}
	opened.evict()
	if calls.Load() != 32 {
		t.Fatal("eviction callbacks ran more than once")
	}
}

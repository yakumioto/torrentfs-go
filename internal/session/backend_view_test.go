package session

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

func TestFilesystemViewsShareOnlyImmutablePayload(t *testing.T) {
	s := filesystemLibrarySession(t)
	first := s.Torrents()
	if len(first) != 111 {
		t.Fatalf("torrents = %d, want 111", len(first))
	}
	var count int
	for i, view := range first {
		count += len(view.Files)
		if view.FilesID == nil {
			t.Fatalf("torrent %d lacks immutable payload identity", i)
		}
		if i > 0 && first[i-1].Hash.HexString() >= view.Hash.HexString() {
			t.Fatal("byte sorting changed the historical hexadecimal hash order")
		}
	}
	if count != 19792 {
		t.Fatalf("files = %d, want 19792", count)
	}
	hash := first[0].Hash
	originalFiles := slices.Clone(first[0].Files)
	first[0].Category = "caller-only"
	s.mu.Lock()
	s.states[hash].Category = "movies"
	s.states[hash].CreatedAt = time.Unix(123, 456).UTC()
	s.subtitles = map[metainfo.Hash]map[string]managedSubtitle{hash: {
		"new.srt": {Path: "new.srt", VideoPath: originalFiles[0].Path, Size: 42, UpdatedAt: time.Unix(456, 0).UTC()},
	}}
	// These paths are also used by namespace/category writers under the write
	// lock; the internal helper must not call the public locking method.
	locked := s.filesystemViewsLocked()
	s.mu.Unlock()
	if locked[0].Category != "movies" || len(locked[0].Subtitles) != 1 || locked[0].CreatedAt.Unix() != 123 {
		t.Fatalf("dynamic fields did not refresh: %+v", locked[0])
	}
	contents, err := s.TorrentContents(hash)
	if err != nil {
		t.Fatal(err)
	}
	if contents.FilesID != first[0].FilesID || &contents.Files[0] != &first[0].Files[0] ||
		!reflect.DeepEqual(contents.Files, originalFiles) || len(contents.Subtitles) != 1 {
		t.Fatal("single-torrent contents did not reuse exactly the immutable payload")
	}
	locked[0].Subtitles[0].Path = "caller-only.srt"
	again := s.Torrents()
	if again[0].Subtitles[0].Path != "new.srt" || again[0].Category != "movies" || first[0].Category != "caller-only" {
		t.Fatal("mutable outer views or subtitles were shared across calls")
	}
	s.mu.Lock()
	old := s.torrents[hash]
	s.states[hash].State = StateDeleting
	s.mu.Unlock()
	if _, err := s.TorrentContents(hash); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("deleting contents error = %v", err)
	}
	s.mu.Lock()
	s.torrents[hash] = &Torrent{tor: old.tor, session: s}
	s.states[hash].State = StateReady
	s.mu.Unlock()
	readded, err := s.TorrentContents(hash)
	if err != nil || readded.FilesID == first[0].FilesID || readded.FilesID == nil {
		t.Fatalf("new Torrent object retained the old holder: %v", err)
	}
	if !reflect.DeepEqual(first[0].Files, originalFiles) {
		t.Fatal("readding modified a previously published snapshot")
	}
	s.mu.Lock()
	s.state = stateClosed
	s.mu.Unlock()
	if _, err := s.TorrentContents(hash); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("closed contents error = %v", err)
	}
	if views := s.Torrents(); views != nil {
		t.Fatalf("closed session exposed %d torrents", len(views))
	}
}

func TestPayloadHolderWaitsForMetadataAndKeepsFirstDuplicate(t *testing.T) {
	s := filesystemLibrarySession(t)
	info := metainfo.Info{Name: "pending", PieceLength: 1 << 20, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
		{Path: []string{"duplicate.mkv"}, Length: 1},
		{Path: []string{"duplicate.mkv"}, Length: 2},
		{Path: []string{"zero.txt"}, Length: 0},
	}}
	encoded, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	hash := metainfo.HashBytes(encoded)
	tor, _ := s.cl.AddTorrentInfoHash(hash)
	st := &Torrent{tor: tor, session: s}
	s.torrents[hash] = st
	s.states[hash] = &registryEntry{State: StateReady}
	for i := 0; i < 3; i++ {
		if got := st.payloadSnapshot(); got != nil {
			t.Fatal("unresolved magnet published a payload")
		}
		if _, err := s.TorrentContents(hash); !errors.Is(err, filesystem.ErrNotFound) {
			t.Fatalf("pending metadata error = %v", err)
		}
	}
	if err := tor.SetInfoBytes(encoded); err != nil {
		t.Fatal(err)
	}
	contents, err := s.TorrentContents(hash)
	if err != nil || contents.FilesID == nil || len(contents.Files) != 3 || contents.SingleFile {
		t.Fatalf("metadata transition = %+v/%v", contents, err)
	}
	if file := st.fileByPath("duplicate.mkv"); file != tor.Files()[0] || file.Length() != 1 {
		t.Fatal("duplicate path did not keep the first payload file")
	}
	if st.payloadBuilds != 1 {
		t.Fatalf("payload builds = %d, want 1 after metadata arrival", st.payloadBuilds)
	}
}

func TestPayloadHolderConcurrentFirstBuildDoesNotReenterTorrentLock(t *testing.T) {
	s := filesystemLibrarySession(t)
	var target *Torrent
	for _, st := range s.torrents {
		if st.Name() == "Torrent-000" {
			target = st
			break
		}
	}
	if target == nil {
		t.Fatal("large torrent missing")
	}
	errCh := make(chan error, 32)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			<-start
			for j := 0; j < 5; j++ {
				contents, err := s.TorrentContents(target.InfoHash())
				if err != nil || len(contents.Files) != 18920 || contents.FilesID == nil {
					errCh <- fmt.Errorf("concurrent contents size/identity/error: %d/%p/%v", len(contents.Files), contents.FilesID, err)
					return
				}
				target.mu.Lock()
				file := target.fileByPath("group-00/season-00/file-00000.mkv")
				target.mu.Unlock()
				if file == nil {
					errCh <- errors.New("reader-side lookup lost a payload file")
					return
				}
			}
		})
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("payload initialization deadlocked")
	}
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if target.payloadBuilds != 1 {
		t.Fatalf("actual concurrent payload constructions = %d, want exactly 1", target.payloadBuilds)
	}
}

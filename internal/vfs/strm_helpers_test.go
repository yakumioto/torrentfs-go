package vfs

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type fakeBackend struct {
	views         []TorrentView
	categories    []string
	data          map[string][]byte
	subtitle      map[string][]byte
	subtitleMtime map[string]time.Time
}

func (b *fakeBackend) Torrents() []TorrentView { return b.views }

func (b *fakeBackend) Categories() []string { return b.categories }

func (b *fakeBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	data, ok := b.data[hash.HexString()+"\x00"+path]
	if !ok {
		return nil, fmt.Errorf("fake backend: no file %q", path)
	}
	return bytes.NewReader(data), nil
}

func (b *fakeBackend) OpenSubtitle(hash metainfo.Hash, path string) (SubtitleSnapshot, error) {
	key := hash.HexString() + "\x00" + path
	data, ok := b.subtitle[key]
	if !ok {
		return SubtitleSnapshot{}, fmt.Errorf("fake backend: no subtitle %q", path)
	}
	// One version serves both the bytes and the metadata, like the real backend.
	return SubtitleSnapshot{Reader: bytes.NewReader(data), Size: int64(len(data)), ModifiedAt: b.subtitleMtime[key]}, nil
}

func (b *fakeBackend) addSubtitle(view *TorrentView, path, videoPath string, data []byte) {
	view.Subtitles = append(view.Subtitles, SubtitleView{Path: path, VideoPath: videoPath, Size: int64(len(data))})
	if b.subtitle == nil {
		b.subtitle = make(map[string][]byte)
		b.subtitleMtime = make(map[string]time.Time)
	}
	b.subtitle[view.Hash.HexString()+"\x00"+path] = data
}

// replaceSubtitle swaps the stored bytes and their mtime without touching the
// view snapshot, so a test can prove which source the node reads its metadata
// from.
func (b *fakeBackend) replaceSubtitle(hash metainfo.Hash, path string, data []byte, modifiedAt time.Time) {
	key := hash.HexString() + "\x00" + path
	b.subtitle[key] = data
	b.subtitleMtime[key] = modifiedAt
}

func (b *fakeBackend) SubtitleStat(hash metainfo.Hash, path string) (SubtitleStat, error) {
	key := hash.HexString() + "\x00" + path
	data, ok := b.subtitle[key]
	if !ok {
		return SubtitleStat{}, fmt.Errorf("fake backend: no subtitle %q", path)
	}
	return SubtitleStat{Size: int64(len(data)), ModifiedAt: b.subtitleMtime[key]}, nil
}

func hashN(n byte) metainfo.Hash {
	var hash metainfo.Hash
	hash[0] = n
	return hash
}

func (b *fakeBackend) addFile(view *TorrentView, path string, data []byte) {
	view.Files = append(view.Files, FileView{Path: path, Size: int64(len(data))})
	if b.data == nil {
		b.data = make(map[string][]byte)
	}
	b.data[view.Hash.HexString()+"\x00"+path] = data
}

type strmTestBackend struct {
	*fakeBackend
	mu            sync.Mutex
	streamCalls   int
	payloadOpens  int
	torrentsCalls int
	streamErrors  map[string]error
	urlHook       func(metainfo.Hash, string) error
}

func (b *strmTestBackend) Torrents() []TorrentView {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.torrentsCalls++
	return b.views
}

// snapshots reports how often the whole-library snapshot was taken.
func (b *strmTestBackend) snapshots() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.torrentsCalls
}

func (b *strmTestBackend) Categories() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.categories)
}

func (b *strmTestBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payloadOpens++
	return b.fakeBackend.OpenFile(hash, path)
}

func (b *strmTestBackend) OpenSubtitle(hash metainfo.Hash, path string) (SubtitleSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.OpenSubtitle(hash, path)
}

func (b *strmTestBackend) SubtitleStat(hash metainfo.Hash, path string) (SubtitleStat, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.SubtitleStat(hash, path)
}

func (b *strmTestBackend) StreamURL(hash metainfo.Hash, path string) (string, error) {
	b.mu.Lock()
	b.streamCalls++
	hook := b.urlHook
	b.urlHook = nil
	b.mu.Unlock()
	if hook != nil {
		if err := hook(hash, path); err != nil {
			return "", err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.streamErrors[hash.HexString()+"\x00"+path]; err != nil {
		return "", err
	}
	for _, view := range b.views {
		if view.Hash != hash {
			continue
		}
		for _, file := range view.Files {
			if file.Path == path {
				return strmTestURL(hash, path), nil
			}
		}
	}
	return "", ErrNotFound
}

func (b *strmTestBackend) counts() (stream, payload int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streamCalls, b.payloadOpens
}

func (b *strmTestBackend) setViews(views []TorrentView) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.views = views
}

func strmTestURL(hash metainfo.Hash, path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "http://media.test/stream/" + hash.HexString() + "/signature/" + strings.Join(parts, "/")
}

func strmTestRead(t *testing.T, reader io.ReaderAt) string {
	t.Helper()
	var out strings.Builder
	buf := make([]byte, 17)
	for offset := int64(0); ; {
		n, err := reader.ReadAt(buf, offset)
		out.Write(buf[:n])
		offset += int64(n)
		if err == io.EOF {
			return out.String()
		}
		if err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if n == 0 {
			t.Fatal("ReadAt returned no data and no error")
		}
	}
}

func strmTestList(t *testing.T, backend Backend) []TorrentView {
	t.Helper()
	listed := make(chan []TorrentView, 1)
	go func() { listed <- backend.Torrents() }()
	select {
	case views := <-listed:
		return views
	case <-time.After(5 * time.Second):
		t.Fatal("STRM listing did not return")
		return nil
	}
}

func strmTestClone(views []TorrentView) []TorrentView {
	out := slices.Clone(views)
	for i := range out {
		out[i].Files = slices.Clone(out[i].Files)
		out[i].Subtitles = slices.Clone(out[i].Subtitles)
	}
	return out
}

func strmTestPaths(original, projected TorrentView) map[string]string {
	out := make(map[string]string, len(original.Files))
	for i := range original.Files {
		out[original.Files[i].Path] = projected.Files[i].Path
	}
	return out
}

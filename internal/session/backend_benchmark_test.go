package session

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/piecestore"
)

// filesystemLibrarySession uses real anacrolix metadata and the real session
// view builder, without discovery, disk payloads, or a persistent registry.
// The largest torrent has 18,920 files in 12 + 204 virtual directories.
func filesystemLibrarySession(tb testing.TB) *Session {
	tb.Helper()
	store := piecestore.New(cache.New(1<<20), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cc := torrent.NewDefaultClientConfig()
	cc.DataDir = tb.TempDir()
	cc.DefaultStorage = store
	cc.NoDHT, cc.DisableTrackers, cc.DisablePEX = true, true, true
	cc.NoDefaultPortForwarding, cc.DisableWebseeds, cc.DisableWebtorrent = true, true, true
	cc.DisableTCP, cc.DisableUTP = true, true
	client, err := torrent.NewClient(cc)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if errs := client.Close(); len(errs) != 0 {
			tb.Errorf("close benchmark client: %v", errs)
		}
		if err := store.Close(); err != nil {
			tb.Errorf("close benchmark storage: %v", err)
		}
	})
	s := &Session{cl: client, cfg: config.Default(), torrents: make(map[metainfo.Hash]*Torrent), states: make(map[metainfo.Hash]*registryEntry)}
	for i := 0; i < 111; i++ {
		count := 8
		if i == 0 {
			count = 18920
		} else if i > 102 {
			count = 7
		}
		info := metainfo.Info{Name: fmt.Sprintf("Torrent-%03d", i), PieceLength: 1 << 20, Pieces: make([]byte, 20)}
		for j := 0; j < count; j++ {
			dir := j % 204
			info.Files = append(info.Files, metainfo.FileInfo{Path: []string{
				fmt.Sprintf("group-%02d", dir/17), fmt.Sprintf("season-%02d", dir%17), fmt.Sprintf("file-%05d.mkv", j),
			}, Length: 1})
		}
		encoded, err := bencode.Marshal(info)
		if err != nil {
			tb.Fatal(err)
		}
		mi := &metainfo.MetaInfo{InfoBytes: encoded}
		spec := torrent.TorrentSpecFromMetaInfo(mi)
		spec.DisableInitialPieceCheck = true
		tor, _, err := client.AddTorrentSpec(spec)
		if err != nil {
			tb.Fatal(err)
		}
		hash := tor.InfoHash()
		s.torrents[hash] = &Torrent{tor: tor, session: s}
		s.states[hash] = &registryEntry{State: StateReady, CreatedAt: time.Unix(1, 0).UTC()}
	}
	return s
}

func BenchmarkFilesystemView(b *testing.B) {
	for _, cold := range []bool{false, true} {
		name := "Hot"
		if cold {
			name = "Cold"
		}
		b.Run(name, func(b *testing.B) {
			s := filesystemLibrarySession(b)
			previous := make(map[metainfo.Hash]*filesystem.FileView)
			var builds int
			for _, view := range s.Torrents() {
				previous[view.Hash] = &view.Files[0]
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if cold {
					for hash, old := range s.torrents {
						s.torrents[hash] = &Torrent{tor: old.tor, session: s}
					}
				}
				views := s.Torrents()
				if len(views) != 111 {
					b.Fatalf("visible torrents = %d, want 111", len(views))
				}
				// Count newly returned payload allocations, not a cache identity.
				for _, view := range views {
					if previous[view.Hash] != &view.Files[0] {
						builds++
					}
					previous[view.Hash] = &view.Files[0]
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(builds)/float64(b.N), "payload-builds/op")
		})
	}
}

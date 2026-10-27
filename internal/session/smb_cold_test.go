package session_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/yakumioto/torrentfs-go/internal/config"
)

func TestSMBColdWebseedSmallCache(t *testing.T) {
	const pieceLength = 1 << 20
	data := make([]byte, 16*pieceLength+123)
	for i := range data {
		data[i] = byte(i*31 + i/251)
	}
	webseed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "cold.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer webseed.Close()
	var pieces []byte
	for start := 0; start < len(data); start += pieceLength {
		sum := sha1.Sum(data[start:min(start+pieceLength, len(data))])
		pieces = append(pieces, sum[:]...)
	}
	infoBytes, err := bencode.Marshal(metainfo.Info{Name: "cold.bin", Length: int64(len(data)), PieceLength: pieceLength, Pieces: pieces})
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes, UrlList: []string{webseed.URL + "/cold.bin"}}
	meta, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"direct", "direct-paced", "smb"} {
		t.Run(protocol, func(t *testing.T) {
			cfg := smbConfig()
			cfg.Cache.CapacityBytes = 8 << 20
			cfg.Mount.ReadTimeout = config.Duration(10 * time.Second)
			sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
			addTorrentBytes(t, sess, meta)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			var read func([]byte, int64) (int, error)
			if protocol != "smb" {
				r, err := sess.OpenFile(mi.HashInfoBytes(), "cold.bin")
				if err != nil {
					t.Fatal(err)
				}
				defer r.(io.Closer).Close()
				read = func(p []byte, off int64) (int, error) {
					return r.(interface {
						ReadAtContext(context.Context, []byte, int64) (int, error)
					}).ReadAtContext(ctx, p, off)
				}
			} else {
				addr, stop := serveSMBSession(t, cfg, sess)
				defer stop()
				share, closeClient := connectSMBSession(t, addr, "torrentfs")
				defer closeClient()
				f, err := share.WithContext(ctx).Open("cold.bin")
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				read = f.ReadAt
			}
			started := time.Now()
			got := make([]byte, len(data))
			for off := 0; off < len(got); off += pieceLength {
				if protocol == "direct-paced" {
					time.Sleep(5 * time.Millisecond)
				}
				p := got[off:min(off+pieceLength, len(got))]
				if n, err := read(p, int64(off)); err != nil || n != len(p) {
					t.Fatalf("cold read offset=%d bytes=%d elapsed=%v: %v", off, n, time.Since(started), err)
				}
			}
			if sha256.Sum256(got) != sha256.Sum256(data) {
				t.Fatal("cold hash mismatch")
			}
			t.Log(fmt.Sprintf("%s cold read %d bytes in %v", protocol, len(got), time.Since(started)))
		})
	}
}

package piecestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func TestTorrentCapacityIsOneSharedStoreBudget(t *testing.T) {
	const capacity = 8 << 20
	store, _ := testStore(t, capacity)
	first, err := store.OpenTorrent(context.Background(), singlePieceInfo(256<<10), metainfo.Hash{1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenTorrent(context.Background(), singlePieceInfo(256<<10), metainfo.Hash{2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Capacity == nil || first.Capacity != second.Capacity {
		t.Fatal("torrents do not identify one shared capacity")
	}
	if size, capped := (*first.Capacity)(); size != capacity || !capped {
		t.Fatalf("capacity = %d, capped = %v", size, capped)
	}
	other, _ := testStore(t, capacity)
	independent, err := other.OpenTorrent(context.Background(), singlePieceInfo(256<<10), metainfo.Hash{1})
	if err != nil {
		t.Fatal(err)
	}
	if independent.Capacity == first.Capacity {
		t.Fatal("independent stores share budget identity")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenTorrent(context.Background(), singlePieceInfo(256<<10), metainfo.Hash{1})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Capacity != second.Capacity {
		t.Fatal("reopen changed the store's capacity identity")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if size, capped := (*second.Capacity)(); size != capacity || !capped {
		t.Fatalf("close changed capacity contract: %d %v", size, capped)
	}
	if _, err := store.OpenTorrent(context.Background(), singlePieceInfo(1), metainfo.Hash{3}); err == nil {
		t.Fatal("closed store accepted a torrent")
	}
}

func TestSharedCapacityEvictionRereadAndTorrentClose(t *testing.T) {
	const pieceLength = 32
	store, c := testStore(t, 128)
	info := singlePieceInfo(pieceLength)
	firstHash, secondHash := metainfo.Hash{1}, metainfo.Hash{2}
	first, err := store.OpenTorrent(context.Background(), info, firstHash)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenTorrent(context.Background(), info, secondHash)
	if err != nil {
		t.Fatal(err)
	}
	if first.Capacity == nil || first.Capacity != second.Capacity {
		t.Fatal("missing shared capacity")
	}
	piece := first.PieceWithHash(info.Piece(0), g.None[[]byte]())
	data := bytes.Repeat([]byte{7}, pieceLength)
	if n, err := piece.WriteAt(data, 0); n != len(data) || err != nil {
		t.Fatal(n, err)
	}
	hashPiece(t, piece, pieceLength)
	if err := piece.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	for index := range 8 {
		c.Put(cache.Key{Torrent: secondHash.HexString(), Piece: index}, bytes.Repeat([]byte{byte(index)}, pieceLength))
	}
	if c.SizeOf(firstHash.HexString())+c.SizeOf(secondHash.HexString()) > 128 {
		t.Fatal("shared cache exceeded global capacity")
	}
	completion := piece.Completion()
	if !completion.Ok || completion.Complete || completion.Err != nil {
		t.Fatalf("evicted completion: %+v", completion)
	}
	if n, err := piece.ReadAt(make([]byte, 1), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("evicted read: %d %v", n, err)
	}
	if n, err := piece.WriteAt(data, 0); n != len(data) || err != nil {
		t.Fatal(n, err)
	}
	hashPiece(t, piece, pieceLength)
	if err := piece.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, pieceLength)
	if n, err := piece.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, data) {
		t.Fatalf("reread: %d %v", n, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := piece.ReadAt(got, 0); !errors.Is(err, errTorrentClosed) {
		t.Fatalf("closed read: %v", err)
	}
	if size, capped := (*second.Capacity)(); size != 128 || !capped {
		t.Fatalf("remaining shared budget: %d %v", size, capped)
	}
}

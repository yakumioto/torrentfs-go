package smb

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	independent "github.com/hirochachacha/go-smb2"
	wire "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

type emptySMBBackend struct{}

func (emptySMBBackend) Torrents() []vfs.TorrentView { return nil }
func (emptySMBBackend) OpenFile(metainfo.Hash, string) (io.ReaderAt, error) {
	return nil, vfs.ErrNotFound
}

func TestEmptySMBDirectoryIncludesVirtualDotEntries(t *testing.T) {
	adapter := &filesystem{tree: vfs.New(emptySMBBackend{})}
	opened, status, err := adapter.Create(context.Background(), nil, server.CreateRequest{
		Path: "", DesiredAccess: readAccess, CreateDisposition: fileOpen, CreateOptions: fileDirectoryFile,
	})
	if err != nil || status != wire.StatusOk {
		t.Fatalf("open directory status=%x err=%v", status, err)
	}
	defer func() { _ = adapter.Close(context.Background(), opened.Handle) }()
	entries, status, err := adapter.QueryDirectory(context.Background(), opened.Handle, "*", true)
	if err != nil || status != wire.StatusOk {
		t.Fatalf("query directory status=%x err=%v", status, err)
	}
	if len(entries) != 2 || entries[0].Name != "." || entries[1].Name != ".." {
		t.Fatalf("empty directory entries=%v; want virtual . and ..", entries)
	}
	for _, entry := range entries {
		if entry.FileID != 1 || entry.Attributes&0x10 == 0 {
			t.Fatalf("invalid root dot entry: %+v", entry)
		}
	}
}

func TestIndependentClientListsEmptySMBLibrary(t *testing.T) {
	_, addr := testServer(t, emptySMBBackend{})
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	dialer := independent.Dialer{Initiator: &independent.NTLMInitiator{User: testUser, Password: testPassword}}
	sess, err := dialer.Dial(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sess.Logoff(); err != nil {
			t.Errorf("Logoff: %v", err)
		}
	}()
	share, err := sess.Mount(ShareName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := share.Umount(); err != nil {
			t.Errorf("Umount: %v", err)
		}
	}()
	entries, err := share.ReadDir(".")
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty library entries=%v err=%v", entries, err)
	}
}

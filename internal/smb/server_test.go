package smb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	independent "github.com/hirochachacha/go-smb2"
	client "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/spnego"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

const testUser = "reader"
const testPassword = "test-only-SMB-password"

type testBackend struct {
	view     vfs.TorrentView
	data     map[string][]byte
	entered  chan string
	canceled chan string
	opens    atomic.Int64
}

func newTestBackend(count int) *testBackend {
	b := &testBackend{data: map[string][]byte{"healthy.bin": []byte("healthy random-access data")},
		entered: make(chan string, 64), canceled: make(chan string, 64)}
	b.view = vfs.TorrentView{Name: "media", Hash: metainfo.Hash{1}, CreatedAt: time.Unix(123456, 0)}
	b.view.Files = append(b.view.Files, vfs.FileView{Path: "healthy.bin", Size: int64(len(b.data["healthy.bin"]))})
	for i := 0; i < count; i++ {
		b.view.Files = append(b.view.Files, vfs.FileView{Path: fmt.Sprintf("blocked-%02d.bin", i), Size: 1024})
	}
	return b
}
func (b *testBackend) Torrents() []vfs.TorrentView { return []vfs.TorrentView{b.view} }
func (b *testBackend) OpenFile(_ metainfo.Hash, name string) (io.ReaderAt, error) {
	b.opens.Add(1)
	if data, ok := b.data[name]; ok {
		return bytes.NewReader(data), nil
	}
	if strings.HasPrefix(name, "blocked-") {
		return &blockedReader{backend: b, name: name}, nil
	}
	return nil, vfs.ErrNotFound
}

type blockedReader struct {
	backend *testBackend
	name    string
}

func (b *blockedReader) ReadAt(p []byte, off int64) (int, error) {
	return b.ReadAtContext(context.Background(), p, off)
}
func (b *blockedReader) ReadAtContext(ctx context.Context, _ []byte, _ int64) (int, error) {
	b.backend.entered <- b.name
	<-ctx.Done()
	b.backend.canceled <- b.name
	return 0, ctx.Err()
}

func testServer(t *testing.T, b vfs.Backend) (*Server, *net.TCPAddr) {
	t.Helper()
	srv, err := New(map[string]*vfs.Tree{ShareName: vfs.New(b)}, Options{
		Username: testUser, Password: testPassword, ReadTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-ctx.Done():
			t.Error("SMB listener did not stop")
		}
	})
	return srv, l.Addr().(*net.TCPAddr)
}

func dialReadClient(t *testing.T, addr *net.TCPAddr) *client.Connection {
	t.Helper()
	c, err := client.NewConnection(client.Options{Host: "127.0.0.1", Port: addr.Port,
		Initiator:  &spnego.NTLMInitiator{User: testUser, Password: testPassword},
		Encryption: client.EncryptionDisabled, Dialects: client.DialectsSMB2Only, DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.TreeConnect(ShareName); err != nil {
		t.Fatal(err)
	}
	return c
}

func await(t *testing.T, channel <-chan string, want string) {
	t.Helper()
	select {
	case got := <-channel:
		if got != want {
			t.Fatalf("event=%q want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", want)
	}
}

func TestSameConnectionSlowReadsCancelIndependently(t *testing.T) {
	b := newTestBackend(16)
	_, addr := testServer(t, b)
	c := dialReadClient(t, addr)
	var files []*client.File
	var cancels []context.CancelFunc
	var done []chan error
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
		for _, f := range files {
			_ = f.CloseFile()
		}
	}()
	for i := 0; i < 16; i++ {
		name := fmt.Sprintf("blocked-%02d.bin", i)
		f, err := c.OpenFile(ShareName, `media\`+name)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		result := make(chan error, 1)
		done = append(done, result)
		go func() { _, err := f.ReadFileContext(ctx, make([]byte, 32), 0); result <- err }()
		await(t, b.entered, name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.EchoContext(ctx); err != nil {
		t.Fatalf("ECHO behind slow reads: %v", err)
	}
	healthy, err := c.OpenFile(ShareName, `media\healthy.bin`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := healthy.CloseFile(); err != nil {
			t.Errorf("healthy CloseFile: %v", err)
		}
	}()
	buf := make([]byte, 7)
	if n, err := healthy.ReadFileContext(ctx, buf, 8); err != nil || string(buf[:n]) != "random-" {
		t.Fatalf("healthy range n=%d bytes=%q err=%v", n, buf, err)
	}
	cancels[0]()
	select {
	case err := <-done[0]:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client cancellation stalled")
	}
	await(t, b.canceled, "blocked-00.bin")
	for _, result := range done[1:] {
		select {
		case err := <-result:
			t.Fatalf("another read was canceled: %v", err)
		default:
		}
	}
	if err := c.EchoContext(ctx); err != nil {
		t.Fatalf("connection after CANCEL: %v", err)
	}
	for _, cancel := range cancels[1:] {
		cancel()
	}
	for _, result := range done[1:] {
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("pending read did not stop")
		}
	}
}

func TestShutdownInterruptsActiveSMBRead(t *testing.T) {
	b := newTestBackend(1)
	srv, addr := testServer(t, b)
	c := dialReadClient(t, addr)
	f, err := c.OpenFile(ShareName, `media\blocked-00.bin`)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.ReadFile(make([]byte, 32), 0); done <- err }()
	await(t, b.entered, "blocked-00.bin")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	await(t, b.canceled, "blocked-00.bin")
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("read survived shutdown")
	}
}

func TestIndependentClientReadOnlyAndShareEnumeration(t *testing.T) {
	b := newTestBackend(0)
	_, addr := testServer(t, b)
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
	names, err := sess.ListSharenames()
	if err != nil || !slices.Contains(names, ShareName) {
		t.Fatalf("shares=%v err=%v", names, err)
	}
	share, err := sess.Mount(ShareName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := share.Umount(); err != nil {
			t.Errorf("Umount: %v", err)
		}
	}()
	entries, err := share.ReadDir("media")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if b.opens.Load() != 0 {
		t.Fatal("listing opened payload bytes")
	}
	data, err := share.ReadFile("media/healthy.bin")
	if err != nil || !bytes.Equal(data, b.data["healthy.bin"]) {
		t.Fatalf("read=%q err=%v", data, err)
	}
	if err := share.WriteFile("media/new.bin", []byte("forbidden"), 0600); err == nil {
		t.Fatal("write was accepted")
	}
	if err := share.Remove("media/healthy.bin"); err == nil {
		t.Fatal("delete was accepted")
	}
}

func TestIndependentClientLargeDirectory(t *testing.T) {
	b := newTestBackend(0)
	for i := 0; i < 5000; i++ {
		b.view.Files = append(b.view.Files, vfs.FileView{Path: fmt.Sprintf("%04d-%s.bin", i, strings.Repeat("long-name", 14)), Size: 1})
	}
	_, addr := testServer(t, b)
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
	entries, err := share.ReadDir("media")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(b.view.Files) {
		t.Fatalf("truncated directory: got %d want %d", len(entries), len(b.view.Files))
	}
	if b.opens.Load() != 0 {
		t.Fatal("directory enumeration opened payload")
	}
}

// Pin the compatibility cap used after an observed Infuse connection closed
// following our SMB 3.1.1 response. This checks dialect selection, not client
// compatibility or the cause of that disconnect.
func TestServerCapsNegotiatedDialectAtSMB302(t *testing.T) {
	b := newTestBackend(0)
	_, addr := testServer(t, b)
	c, err := client.NewConnection(client.Options{
		Host: "127.0.0.1", Port: addr.Port,
		Initiator:  &spnego.NTLMInitiator{User: testUser, Password: testPassword},
		Encryption: client.EncryptionDisabled,
		Dialects: []uint16{
			client.DialectSmb_3_1_1, client.DialectSmb_3_0_2, client.DialectSmb_2_1,
		},
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Dialect(); got != client.DialectSmb_3_0_2 {
		t.Fatalf("negotiated dialect = 0x%04x, want SMB 3.0.2", got)
	}
}

// Windows' RPC client queries FileStandardInformation on a freshly opened
// named pipe before binding. IPC$ has no VFS; this used to drop the connection
// and surface as "the remote procedure call failed" in Explorer.
func TestPipeQueryInfoStandardInformation(t *testing.T) {
	b := newTestBackend(0)
	_, addr := testServer(t, b)
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
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
	share, err := sess.Mount("IPC$")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := share.Umount(); err != nil {
			t.Errorf("Umount: %v", err)
		}
	}()
	f, err := share.Open("srvsvc")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// SeekEnd issues FileStandardInformation; Stat additionally queries AllInformation.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("FileStandardInformation on pipe: %v", err)
	}
	if size != 0 {
		t.Fatalf("pipe end = %d, want 0", size)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat on pipe: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("pipe size = %d, want 0", info.Size())
	}
}

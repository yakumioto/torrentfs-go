package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func TestDirectoryPagesSnapshotRestartAndSingle(t *testing.T) {
	ctx := context.Background()
	backend := newMemBackend()
	var want []string
	for i := range 123 {
		name := fmt.Sprintf("%03d-电影😀.txt", i)
		want = append(want, name)
		if _, err := backend.Open(ctx, vfs.OpenOptions{Path: name, Disposition: vfs.DispositionCreate}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := backend.Open(ctx, vfs.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fid := [16]byte{1}
	tr := &tree{opens: map[[16]byte]*openHandle{fid: {h: h}}}
	c := &conn{srv: newTestServer(backend)}
	query := func(pattern string, flags uint8, limit uint32) (uint32, []string) {
		t.Helper()
		msg := buildQueryDirectory(0, 0, fid, pattern)
		msg[64+3] = flags
		binary.LittleEndian.PutUint32(msg[64+28:], limit)
		c.out = make([]byte, 64)
		status := c.handleQueryDirectory(ctx, msg, tr)
		if status != wire.StatusSuccess {
			return status, nil
		}
		var names []string
		for offset := 72; ; {
			entry := c.out[offset:]
			n := binary.LittleEndian.Uint32(entry[60:64])
			names = append(names, wire.UTF16FromBytes(entry[64:64+n]))
			next := binary.LittleEndian.Uint32(entry[:4])
			if next == 0 {
				break
			}
			offset += int(next)
		}
		if len(c.out)-72 > int(limit) {
			t.Fatal("exceeded buffer")
		}
		return status, names
	}
	if status, _ := query("*.txt", 0, 1); status != wire.StatusBufferTooSmall {
		t.Fatalf("small buffer: %x", status)
	}
	var got []string
	for {
		status, names := query("", 0, 256)
		if status == wire.StatusNoMoreFiles {
			break
		}
		if status != wire.StatusSuccess {
			t.Fatalf("query: %x", status)
		}
		got = append(got, names...)
		if len(got) > len(want) {
			t.Fatal("directory cursor did not advance")
		}
		if len(got) == len(names) {
			_, _ = backend.Open(ctx, vfs.OpenOptions{Path: "new.txt", Disposition: vfs.DispositionCreate})
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages: got %d entries, want %d", len(got), len(want))
	}
	status, names := query("new*", wire.QueryDirRestartScans|wire.QueryDirReturnSingle, 256)
	if status != wire.StatusSuccess || !reflect.DeepEqual(names, []string{"new.txt"}) {
		t.Fatalf("restart: %x %v", status, names)
	}
	if status, _ := query("", 0, 256); status != wire.StatusNoMoreFiles {
		t.Fatalf("end: %x", status)
	}
	status, names = query("*", wire.QueryDirReopen|wire.QueryDirReturnSingle, 256)
	if status != wire.StatusSuccess || len(names) != 1 || names[0] != want[0] {
		t.Fatalf("single: %x %v", status, names)
	}
}

func TestReadBoundsBeforeAllocation(t *testing.T) {
	b := newMemBackend()
	h, _ := b.Open(context.Background(), vfs.OpenOptions{})
	fid := [16]byte{1}
	tr := &tree{opens: map[[16]byte]*openHandle{fid: {h: h}}}
	for _, tc := range []struct {
		length uint32
		offset uint64
	}{{math.MaxUint32, 0}, {1, math.MaxUint64}} {
		c := &conn{srv: newTestServer(b)}
		if status := c.handleRead(context.Background(), buildRead(0, 0, fid, tc.offset, tc.length), tr); status != wire.StatusInvalidParameter {
			t.Fatalf("read %+v: %x", tc, status)
		}
		if len(c.out) > 16 {
			t.Fatal("allocated read body before validation")
		}
	}
}

type captureBackend struct {
	*memBackend
	opts vfs.OpenOptions
}

func (b *captureBackend) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	b.opts = opts
	return b.memBackend.Open(ctx, opts)
}

func TestCreateForwardsAccessAndOptions(t *testing.T) {
	b := &captureBackend{memBackend: newMemBackend()}
	c := &conn{srv: newTestServer(b)}
	tr := &tree{share: vfs.NewDiskShare("share", b), opens: make(map[[16]byte]*openHandle)}
	msg := buildCreate(0, 0, "x", wire.FileOpenIf)
	binary.LittleEndian.PutUint32(msg[64+24:], 0x100002)
	binary.LittleEndian.PutUint32(msg[64+40:], wire.FileDeleteOnClose)
	var fid [16]byte
	if status := c.handleCreate(context.Background(), msg, &wire.Header{}, tr, &fid); status != wire.StatusSuccess {
		t.Fatalf("create: %x", status)
	}
	if b.opts.DesiredAccess != 0x100002 || b.opts.CreateOptions != wire.FileDeleteOnClose {
		t.Fatalf("options: %+v", b.opts)
	}
}

type pipeListener struct {
	c        net.Conn
	closed   chan struct{}
	accepted chan struct{}
	once     sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	if l.c != nil {
		c := l.c
		l.c = nil
		close(l.accepted)
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

type observedWrite struct {
	net.Conn
	writing chan struct{}
	once    sync.Once
}

func (c *observedWrite) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.writing) })
	return c.Conn.Write(p)
}

func TestServeCancelDrainsIdleAndBlockedWrite(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			writing := make(chan struct{})
			ln := &pipeListener{c: &observedWrite{Conn: peer, writing: writing}, closed: make(chan struct{}), accepted: make(chan struct{})}
			srv := newTestServer(newMemBackend())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx, ln) }()
			<-ln.accepted
			if blocked {
				hdr := wire.NewHeader(wire.CmdEcho)
				msg := hdr.Append(nil)
				if err := transport.NewFramedConn(client).WriteMessage(msg); err != nil {
					t.Fatal(err)
				}
				select {
				case <-writing:
				case <-time.After(2 * time.Second):
					t.Fatal("no write")
				}
			}
			if blocked {
				cancel()
			} else if err := srv.Shutdown(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Serve did not drain")
			}
			if err := srv.Shutdown(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShutdownBeforeServeAndFailedAccept(t *testing.T) {
	srv := newTestServer(newMemBackend())
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background(), ln); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve after shutdown: %v", err)
	}
	srv = newTestServer(newMemBackend())
	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := srv.Serve(context.Background(), ln); err == nil {
		t.Fatal("failed accept succeeded")
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

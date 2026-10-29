package smb

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/spnego"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

func lifecycleServer(t *testing.T, b *testBackend, configure func(*server.ServerConfig)) (*Server, *net.TCPAddr) {
	t.Helper()
	s, err := New(map[string]*vfs.Tree{ShareName: vfs.New(b)}, Options{Username: testUser, Password: testPassword, ReadTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(s.server.Config)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-ctx.Done():
			t.Error("listener did not exit")
		}
	})
	return s, l.Addr().(*net.TCPAddr)
}

func TestSMB3RequiredEncryptionReadAndCancel(t *testing.T) {
	b := newTestBackend(1)
	var encrypted atomic.Int64
	_, addr := lifecycleServer(t, b, func(cfg *server.ServerConfig) {
		cfg.RequireEncryption = true
		cfg.OnRawRequest = func(_ *server.Conn, raw []byte) (bool, error) {
			if len(raw) >= 4 && string(raw[:4]) == wire.ProtocolTransformHdr {
				encrypted.Add(1)
			}
			return false, nil
		}
	})
	c, err := wire.NewConnection(wire.Options{Host: "127.0.0.1", Port: addr.Port,
		Initiator:  &spnego.NTLMInitiator{User: testUser, Password: testPassword},
		Encryption: wire.EncryptionRequired, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Dialect() < wire.DialectSmb_3_0 {
		t.Fatal("encryption test negotiated SMB2")
	}
	if err := c.TreeConnect(ShareName); err != nil {
		t.Fatal(err)
	}
	f, err := c.OpenFile(ShareName, `media\blocked-00.bin`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.CloseFile(); err != nil {
			t.Errorf("close canceled file: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.ReadFileContext(ctx, make([]byte, 32), 0); done <- err }()
	await(t, b.entered, "blocked-00.bin")
	cancel()
	await(t, b.canceled, "blocked-00.bin")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("encrypted read cancellation stalled")
	}
	healthy, err := c.OpenFile(ShareName, `media\healthy.bin`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := healthy.CloseFile(); err != nil {
			t.Errorf("close healthy file: %v", err)
		}
	}()
	buf := make([]byte, len(b.data["healthy.bin"]))
	if n, err := healthy.ReadFile(buf, 0); err != nil || !bytes.Equal(buf[:n], b.data["healthy.bin"]) {
		t.Fatalf("encrypted healthy read n=%d err=%v", n, err)
	}
	if err := c.Echo(); err != nil {
		t.Fatal(err)
	}
	if encrypted.Load() < 4 {
		t.Fatalf("only %d encrypted requests observed", encrypted.Load())
	}
}

func TestSMBControlOperationsDrainActiveRead(t *testing.T) {
	for _, operation := range []string{"close", "tree-disconnect", "logoff"} {
		t.Run(operation, func(t *testing.T) {
			b := newTestBackend(1)
			_, addr := lifecycleServer(t, b, nil)
			c := dialReadClient(t, addr)
			f, err := c.OpenFile(ShareName, `media\blocked-00.bin`)
			if err != nil {
				t.Fatal(err)
			}
			readDone := make(chan error, 1)
			go func() { _, err := f.ReadFile(make([]byte, 32), 0); readDone <- err }()
			await(t, b.entered, "blocked-00.bin")
			controlDone := make(chan error, 1)
			go func() {
				switch operation {
				case "close":
					controlDone <- f.CloseFile()
				case "tree-disconnect":
					controlDone <- c.TreeDisconnect(ShareName)
				case "logoff":
					// Connection.Logoff disconnects trees first; send the real
					// LOGOFF directly to exercise session cleanup with a live read.
					req := c.NewLogoffReq()
					raw, err := req.MarshalBinary()
					if err == nil {
						raw, err = c.SendRawPDU(raw)
					}
					if err == nil {
						var h wire.Header
						err = h.UnmarshalBinary(raw)
						if err == nil && h.Status != wire.StatusOk {
							err = fmt.Errorf("LOGOFF status=0x%x", h.Status)
						}
					}
					controlDone <- err
				}
			}()
			await(t, b.canceled, "blocked-00.bin")
			select {
			case err := <-readDone:
				if err == nil {
					t.Fatal("read completed successfully after teardown")
				}
			case <-time.After(time.Second):
				t.Fatal("read survived teardown")
			}
			select {
			case err := <-controlDone:
				if err != nil {
					t.Fatalf("%s: %v", operation, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("%s blocked", operation)
			}
			if operation == "close" {
				if err := c.Echo(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// This transport routes only the manually assigned compound response IDs to
// the test. Authentication, tree connect and concurrent ECHO still use the
// ordinary client sender/receiver, over exactly the same real TCP connection.
type lifecycleCaptureConn struct {
	net.Conn
	writeMu  sync.Mutex
	buffer   []byte
	compound chan []byte
}

func (c *lifecycleCaptureConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.Conn.Write(p)
}
func (c *lifecycleCaptureConn) Read(p []byte) (int, error) {
	for len(c.buffer) == 0 {
		var size [4]byte
		if _, err := io.ReadFull(c.Conn, size[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(size[:])
		if n > 2<<20 {
			return 0, io.ErrShortBuffer
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(c.Conn, body); err != nil {
			return 0, err
		}
		if len(body) >= 64 && string(body[:4]) == wire.ProtocolSmb2 {
			id := binary.LittleEndian.Uint64(body[24:32])
			if id >= 32 && id <= 34 {
				c.compound <- body
				continue
			}
		}
		c.buffer = append(size[:], body...)
	}
	n := copy(p, c.buffer)
	c.buffer = c.buffer[n:]
	return n, nil
}

type lifecycleDialer struct{ conn *lifecycleCaptureConn }

func (d *lifecycleDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
func (d *lifecycleDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	d.conn = &lifecycleCaptureConn{Conn: c, compound: make(chan []byte, 4)}
	return d.conn, nil
}

func lifecycleCompound(t *testing.T, c *wire.Connection, name string, sign bool) []byte {
	t.Helper()
	opts := wire.NewCreateReqOpts()
	create, err := c.NewCreateReq(ShareName, name, opts.OpLockLevel, opts.ImpersonationLevel,
		opts.DesiredAccess, opts.FileAttr, opts.ShareAccess, opts.CreateDisp, opts.CreateOpts)
	if err != nil {
		t.Fatal(err)
	}
	read, err := c.NewReadReq(ShareName, bytes.Repeat([]byte{0xff}, 16), 7, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	closeReq, err := c.NewCloseReq(ShareName, bytes.Repeat([]byte{0xff}, 16))
	if err != nil {
		t.Fatal(err)
	}
	create.Header.MessageID = 32
	read.Header.MessageID, closeReq.Header.MessageID = 33, 34
	read.Header.SessionID, closeReq.Header.SessionID = ^uint64(0), ^uint64(0)
	read.Header.TreeID, closeReq.Header.TreeID = ^uint32(0), ^uint32(0)
	read.Header.Flags, closeReq.Header.Flags = wire.SMB2_FLAGS_RELATED_OPERATIONS, wire.SMB2_FLAGS_RELATED_OPERATIONS
	parts := make([][]byte, 3)
	parts[0], err = create.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parts[1], err = read.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parts[2], err = closeReq.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for i, part := range parts {
		if i != len(parts)-1 {
			part = append(part, make([]byte, (8-len(part)%8)%8)...)
			binary.LittleEndian.PutUint32(part[20:24], uint32(len(part)))
		}
		if sign {
			binary.LittleEndian.PutUint32(part[16:20], binary.LittleEndian.Uint32(part[16:20])|wire.SMB2_FLAGS_SIGNED)
			clear(part[48:64])
			mac := hmac.New(sha256.New, c.GetSessionKey())
			_, _ = mac.Write(part)
			copy(part[48:64], mac.Sum(nil)[:16])
		}
		out = append(out, part...)
	}
	return out
}

func checkLifecycleSignatures(t *testing.T, raw, key []byte) {
	t.Helper()
	for len(raw) != 0 {
		if len(raw) < 64 {
			t.Fatal("truncated signed compound")
		}
		end := len(raw)
		if next := int(binary.LittleEndian.Uint32(raw[20:24])); next != 0 {
			end = next
		}
		if end < 64 || end > len(raw) {
			t.Fatal("invalid signed compound offset")
		}
		segment := append([]byte(nil), raw[:end]...)
		if binary.LittleEndian.Uint32(segment[16:20])&wire.SMB2_FLAGS_SIGNED == 0 {
			t.Fatal("unsigned compound response")
		}
		signature := append([]byte(nil), segment[48:64]...)
		clear(segment[48:64])
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(segment)
		if !hmac.Equal(signature, mac.Sum(nil)[:16]) {
			t.Fatal("invalid per-operation response signature")
		}
		raw = raw[end:]
	}
}

func checkLifecycleCompound(t *testing.T, raw []byte, canceled bool) {
	t.Helper()
	commands := []uint16{wire.CommandCreate, wire.CommandRead, wire.CommandClose}
	for i, command := range commands {
		if len(raw) < 64 {
			t.Fatal("truncated compound reply")
		}
		var h wire.Header
		if err := h.UnmarshalBinary(raw[:64]); err != nil {
			t.Fatal(err)
		}
		if h.Command != command {
			t.Fatalf("reply %d command=%d want %d", i, h.Command, command)
		}
		want := uint32(wire.StatusOk)
		if i == 1 && canceled {
			want = wire.StatusCancelled
		}
		if h.Status != want {
			t.Fatalf("reply %d status=0x%x want 0x%x", i, h.Status, want)
		}
		end := len(raw)
		if h.NextCommand != 0 {
			end = int(h.NextCommand)
		}
		if end > len(raw) || end < 64 {
			t.Fatal("invalid compound response offset")
		}
		if i == 1 && !canceled {
			var result wire.ReadRes
			if err := result.UnmarshalBinary(raw[:end]); err != nil {
				t.Fatal(err)
			}
			if result.DataLength != 7 || len(result.Buffer) < 7 || string(result.Buffer[:7]) != "random-" {
				t.Fatal("related READ returned incorrect data")
			}
		}
		if i < 2 && h.NextCommand == 0 {
			t.Fatal("missing related reply")
		}
		if i == 2 && h.NextCommand != 0 {
			t.Fatal("unexpected extra compound reply")
		}
		raw = raw[end:]
	}
}

func TestSMBSignedRelatedCompoundReadCloseAndCancel(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "cancel-read"}[blocked], func(t *testing.T) {
			b := newTestBackend(1)
			_, addr := lifecycleServer(t, b, nil)
			dialer := &lifecycleDialer{}
			c, err := wire.NewConnection(wire.Options{Host: "127.0.0.1", Port: addr.Port,
				Initiator:  &spnego.NTLMInitiator{User: testUser, Password: testPassword},
				Encryption: wire.EncryptionDisabled, Dialects: wire.DialectsSMB2Only, DialTimeout: time.Second, ProxyDialer: dialer})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.TreeConnect(ShareName); err != nil {
				t.Fatal(err)
			}
			name := `media\healthy.bin`
			if blocked {
				name = `media\blocked-00.bin`
			}
			packet := lifecycleCompound(t, c, name, true)
			frame := make([]byte, 4+len(packet))
			binary.BigEndian.PutUint32(frame, uint32(len(packet)))
			copy(frame[4:], packet)
			if _, err := dialer.conn.Write(frame); err != nil {
				t.Fatal(err)
			}
			if blocked {
				await(t, b.entered, "blocked-00.bin")
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := c.EchoContext(ctx); err != nil {
					t.Fatalf("compound READ blocked ECHO: %v", err)
				}
				if err := c.SendCancel(33, 0); err != nil {
					t.Fatal(err)
				}
				await(t, b.canceled, "blocked-00.bin")
			}
			select {
			case result := <-dialer.conn.compound:
				checkLifecycleSignatures(t, result, c.GetSessionKey())
				checkLifecycleCompound(t, result, blocked)
			case <-time.After(2 * time.Second):
				t.Fatal("compound reply timed out")
			}
			if err := c.Echo(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSMBEncryptedRelatedCompoundReadClose(t *testing.T) {
	b := newTestBackend(0)
	_, addr := lifecycleServer(t, b, func(cfg *server.ServerConfig) { cfg.RequireEncryption = true })
	c, err := wire.NewConnection(wire.Options{Host: "127.0.0.1", Port: addr.Port, Initiator: &spnego.NTLMInitiator{User: testUser, Password: testPassword}, Encryption: wire.EncryptionRequired, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.TreeConnect(ShareName); err != nil {
		t.Fatal(err)
	}
	packet := lifecycleCompound(t, c, `media\healthy.bin`, false)
	result, err := c.SendRawPDU(packet)
	if err != nil {
		t.Fatal(err)
	}
	checkLifecycleCompound(t, result, false)
}

func TestSMBConcurrentReadLimitRejectsWithoutBlocking(t *testing.T) {
	b := newTestBackend(64)
	_, addr := lifecycleServer(t, b, nil)
	c := dialReadClient(t, addr)
	var cancels []context.CancelFunc
	var results []chan error
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
	})
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("blocked-%02d.bin", i)
		f, err := c.OpenFile(ShareName, `media\`+name)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		result := make(chan error, 1)
		results = append(results, result)
		go func() { _, err := f.ReadFileContext(ctx, make([]byte, 32), 0); result <- err }()
		await(t, b.entered, name)
	}
	healthy, err := c.OpenFile(ShareName, `media\healthy.bin`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := healthy.ReadFileContext(ctx, make([]byte, 1), 0); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read at capacity must fail immediately with a protocol error: %v", err)
	}
	if err := c.EchoContext(ctx); err != nil {
		t.Fatalf("capacity blocked ECHO: %v", err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	for _, result := range results {
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("blocked read succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("read failed to cancel")
		}
	}
}

func TestSMBRequestLimitRejectsBodyBeforeReadingIt(t *testing.T) {
	b := newTestBackend(0)
	_, addr := lifecycleServer(t, b, func(cfg *server.ServerConfig) { cfg.MaxRequestSize = 2 << 20 })
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], (2<<20)+1)
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var sample [1]byte
	if _, err := conn.Read(sample[:]); err == nil {
		t.Fatal("oversized frame was accepted")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("server waited for the oversized body instead of rejecting its header")
	}
	c := dialReadClient(t, addr)
	if err := c.Echo(); err != nil {
		t.Fatalf("oversized peer damaged listener: %v", err)
	}
}

func TestSMBQFidAndOpenedFileInformationUseOneIdentity(t *testing.T) {
	b := newTestBackend(0)
	_, addr := lifecycleServer(t, b, nil)
	c := dialReadClient(t, addr)
	opts := wire.NewCreateReqOpts()
	request, err := c.NewCreateReq(ShareName, `media\healthy.bin`, opts.OpLockLevel,
		opts.ImpersonationLevel, opts.DesiredAccess, opts.FileAttr, opts.ShareAccess, opts.CreateDisp, opts.CreateOpts)
	if err != nil {
		t.Fatal(err)
	}
	contextBytes := make([]byte, 24)
	binary.LittleEndian.PutUint16(contextBytes[4:], 16)
	binary.LittleEndian.PutUint16(contextBytes[6:], 4)
	copy(contextBytes[16:], "QFid")
	offset := (int(request.NameOffset) + len(request.Buffer) + 7) &^ 7
	request.Buffer = append(request.Buffer, make([]byte, offset-int(request.NameOffset)-len(request.Buffer))...)
	request.Buffer = append(request.Buffer, contextBytes...)
	request.CreateContextsOffset, request.CreateContextsLength = uint32(offset), uint32(len(contextBytes))
	encoded, err := request.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.SendRawPDU(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var opened wire.CreateRes
	if err := opened.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	if opened.Status != wire.StatusOk {
		t.Fatalf("CREATE status=0x%x", opened.Status)
	}
	defer func() {
		closeReq, err := c.NewCloseReq(ShareName, opened.FileId)
		if err == nil {
			encoded, err = closeReq.MarshalBinary()
		}
		if err == nil {
			_, err = c.SendRawPDU(encoded)
		}
		if err != nil {
			t.Errorf("close identity test handle: %v", err)
		}
	}()
	var diskID uint64
	for contexts := opened.Buffer; len(contexts) >= 16; {
		nameOffset, nameLength := int(binary.LittleEndian.Uint16(contexts[4:])), int(binary.LittleEndian.Uint16(contexts[6:]))
		dataOffset, dataLength := int(binary.LittleEndian.Uint16(contexts[10:])), int(binary.LittleEndian.Uint32(contexts[12:]))
		if nameOffset+nameLength > len(contexts) || dataOffset+dataLength > len(contexts) {
			t.Fatal("invalid CREATE context bounds")
		}
		if string(contexts[nameOffset:nameOffset+nameLength]) == "QFid" {
			if dataLength != 32 {
				t.Fatalf("QFid data length=%d", dataLength)
			}
			diskID = binary.LittleEndian.Uint64(contexts[dataOffset:])
		}
		next := int(binary.LittleEndian.Uint32(contexts))
		if next == 0 {
			break
		}
		if next < 16 || next > len(contexts) {
			t.Fatal("invalid CREATE context continuation")
		}
		contexts = contexts[next:]
	}
	if diskID == 0 {
		t.Fatal("requested QFid missing or zero")
	}
	for _, kind := range []struct {
		class    byte
		idOffset int
	}{{wire.FileInternalInformation, 0}, {wire.FileAllInformation, 64}} {
		header := request.Header
		header.Command = wire.CommandQueryInfo
		query := wire.QueryInfoReq{Header: header, StructureSize: 41, InfoType: wire.OInfoFile,
			FileInfoClass: kind.class, OutputBufferLength: 1024, FileId: opened.FileId, Buffer: []byte{0}}
		encoded, err := query.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := c.SendRawPDU(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var info wire.QueryInfoRes
		if err := info.UnmarshalBinary(raw); err != nil {
			t.Fatal(err)
		}
		if info.Status != wire.StatusOk || len(info.Buffer) < kind.idOffset+8 {
			t.Fatalf("file info class=%d status=0x%x length=%d", kind.class, info.Status, len(info.Buffer))
		}
		if got := binary.LittleEndian.Uint64(info.Buffer[kind.idOffset:]); got != diskID {
			t.Fatalf("file info class=%d ID=%d QFid=%d", kind.class, got, diskID)
		}
	}
}

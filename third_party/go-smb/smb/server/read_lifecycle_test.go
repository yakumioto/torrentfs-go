package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/unicode"
)

type lateLifecycleHandle struct{}

func (*lateLifecycleHandle) Stat() (FileInfo, error) {
	return FileInfo{Name: "late.bin", Size: 7, Attributes: FileAttributeReadonly}, nil
}
func (*lateLifecycleHandle) Path() string { return "late.bin" }
func (*lateLifecycleHandle) IsDir() bool  { return false }

type lateLifecycleVFS struct {
	VFS
	started chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Int64
}

func (v *lateLifecycleVFS) Create(context.Context, *Session, CreateRequest) (CreateResult, uint32, error) {
	h := &lateLifecycleHandle{}
	v.once.Do(func() { close(v.started) })
	// Deliberately ignore cancellation to model a backend which has already
	// opened a descriptor but only publishes the result after teardown.
	<-v.release
	info, _ := h.Stat()
	return CreateResult{Handle: h, CreateAction: 1, Info: info}, smb.StatusOk, nil
}
func (v *lateLifecycleVFS) Close(context.Context, Handle) error { v.closed.Add(1); return nil }
func (v *lateLifecycleVFS) Read(ctx context.Context, _ Handle, _ int64, buf []byte) (int, uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, smb.StatusCancelled, nil
	}
	return copy(buf, []byte("content")), smb.StatusOk, nil
}

func lifecycleWireHeader(command uint16, id uint64) smb.Header {
	return smb.Header{ProtocolID: []byte(smb.ProtocolSmb2), StructureSize: 64, Command: command,
		CreditCharge: 1, Credits: 1, MessageID: id, SessionID: 1, TreeID: 1, Signature: make([]byte, 16)}
}
func lateLifecycleCompound(t *testing.T) []byte {
	t.Helper()
	name := unicode.ToUnicode("late.bin")
	create := smb.CreateReq{Header: lifecycleWireHeader(smb.CommandCreate, 10), StructureSize: 57,
		ImpersonationLevel: 2, DesiredAccess: smb.FAccMaskFileReadData, ShareAccess: smb.FileShareRead,
		CreateDisposition: smb.FileOpen, NameOffset: 120, NameLength: uint16(len(name)), Buffer: name}
	read := smb.ReadReq{Header: lifecycleWireHeader(smb.CommandRead, 11), StructureSize: 49, Length: 7,
		FileId: bytes.Repeat([]byte{0xff}, 16), Buffer: []byte{0}}
	closeReq := smb.CloseReq{Header: lifecycleWireHeader(smb.CommandClose, 12), StructureSize: 24, FileId: bytes.Repeat([]byte{0xff}, 16)}
	read.Header.Flags, closeReq.Header.Flags = smb.SMB2_FLAGS_RELATED_OPERATIONS, smb.SMB2_FLAGS_RELATED_OPERATIONS
	read.Header.SessionID, closeReq.Header.SessionID = ^uint64(0), ^uint64(0)
	read.Header.TreeID, closeReq.Header.TreeID = ^uint32(0), ^uint32(0)
	var parts [][]byte
	for _, marshal := range []func() ([]byte, error){create.MarshalBinary, read.MarshalBinary, closeReq.MarshalBinary} {
		part, err := marshal()
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, part)
	}
	var out []byte
	for i, part := range parts {
		if i < 2 {
			part = append(part, make([]byte, (8-len(part)%8)%8)...)
			binary.LittleEndian.PutUint32(part[20:24], uint32(len(part)))
		}
		out = append(out, part...)
	}
	return out
}
func writeLifecycleWire(t *testing.T, c net.Conn, raw []byte) {
	t.Helper()
	frame := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(frame, uint32(len(raw)))
	copy(frame[4:], raw)
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
}

// Deterministic authenticated-state wire tests complement the root tests which
// perform actual NTLM handshakes and verify signing/encryption independently.
func TestLateCreateCannotRepopulateTornDownTreeOrSession(t *testing.T) {
	for _, scope := range []string{"tree", "session"} {
		t.Run(scope, func(t *testing.T) {
			v := &lateLifecycleVFS{started: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(v.release) })
			t.Cleanup(release)
			type state struct {
				conn *Conn
				sess *Session
				tree *Tree
			}
			ready := make(chan state, 1)
			cfg := &ServerConfig{ConcurrentReads: true, Shares: map[string]Share{"files": {Name: "files", Type: smb.ShareTypeDisk, VFS: v}},
				OnConnect: func(c *Conn) error {
					c.Dialect = smb.DialectSmb_2_1
					s := c.addSession()
					s.Authenticated = true
					tree := s.addTree(Share{Name: "files", Type: smb.ShareTypeDisk, VFS: v})
					ready <- state{c, s, tree}
					return nil
				}}
			addr, stop := startBoundedServer(t, cfg)
			defer stop()
			client, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var current state
			select {
			case current = <-ready:
			case <-time.After(time.Second):
				t.Fatal("connection did not initialize")
			}
			writeLifecycleWire(t, client, lateLifecycleCompound(t))
			select {
			case <-v.started:
			case <-time.After(time.Second):
				t.Fatal("CREATE did not reach backend")
			}
			echo := smb.EchoReq{Header: lifecycleWireHeader(smb.CommandEcho, 100), StructureSize: 4}
			raw, err := echo.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			writeLifecycleWire(t, client, raw)
			response, err := readPDU(client, time.Second)
			if err != nil {
				t.Fatalf("slow CREATE blocked ECHO: %v", err)
			}
			var h smb.Header
			if err := h.UnmarshalBinary(response); err != nil || h.Command != smb.CommandEcho || h.Status != smb.StatusOk {
				t.Fatalf("ECHO response=%+v err=%v", h, err)
			}
			if scope == "tree" {
				req := smb.TreeDisconnectReq{Header: lifecycleWireHeader(smb.CommandTreeDisconnect, 101), StructureSize: 4}
				raw, err = req.MarshalBinary()
			} else {
				req := smb.LogoffReq{Header: lifecycleWireHeader(smb.CommandLogoff, 101), StructureSize: 4}
				raw, err = req.MarshalBinary()
			}
			if err != nil {
				t.Fatal(err)
			}
			writeLifecycleWire(t, client, raw)
			deadline := time.Now().Add(time.Second)
			for {
				current.tree.mu.Lock()
				closed := current.tree.closed
				current.tree.mu.Unlock()
				if (scope == "tree" && closed) || (scope == "session" && current.sess.closing.Load()) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("teardown did not begin while CREATE was paused")
				}
				time.Sleep(time.Millisecond)
			}
			release()
			seenCompound, seenControl := false, false
			for i := 0; i < 2; i++ {
				response, err = readPDU(client, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.UnmarshalBinary(response); err != nil {
					t.Fatal(err)
				}
				switch h.MessageID {
				case 10:
					seenCompound = true
					if h.Status == smb.StatusOk {
						t.Fatal("late CREATE succeeded after teardown")
					}
				case 101:
					seenControl = true
					if h.Status != smb.StatusOk {
						t.Fatalf("teardown status=0x%x", h.Status)
					}
				default:
					t.Fatalf("unexpected MessageID %d", h.MessageID)
				}
			}
			if !seenCompound || !seenControl {
				t.Fatal("missing compound or teardown reply")
			}
			if v.closed.Load() != 1 {
				t.Fatalf("late descriptor close count=%d want 1", v.closed.Load())
			}
			current.tree.mu.Lock()
			handles := len(current.tree.handles)
			current.tree.mu.Unlock()
			if handles != 0 {
				t.Fatalf("closed tree retained %d late handles", handles)
			}
			if scope == "session" {
				deadline = time.Now().Add(time.Second)
				for current.conn.session(current.sess.ID) != nil {
					if time.Now().After(deadline) {
						t.Fatal("LOGOFF retained session after reply")
					}
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

func TestReadCancellationIsScopedToOperationAndSession(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &readJob{ctx: root, cancel: cancel, done: make(chan struct{}), session: 7, canceled: make(map[uint64]bool)}
	c := &Conn{readJobs: map[uint64]*readJob{10: job, 11: job}}
	first, finishFirst := c.beginReadRequest(pduCtx{readJob: job}, &smb.Header{MessageID: 10, SessionID: 7, TreeID: 3})
	if c.cancelRead(10, 8) {
		t.Fatal("another session canceled the request")
	}
	select {
	case <-first.context().Done():
		t.Fatal("foreign cancellation reached request")
	default:
	}
	if !c.cancelRead(10, 7) {
		t.Fatal("own request was not found")
	}
	select {
	case <-first.context().Done():
	case <-time.After(time.Second):
		t.Fatal("own cancellation did not propagate")
	}
	finishFirst()
	second, finishSecond := c.beginReadRequest(pduCtx{readJob: job}, &smb.Header{MessageID: 11, SessionID: 7, TreeID: 3})
	defer finishSecond()
	if !c.cancelRead(10, 7) {
		t.Fatal("late cancellation was not recognized")
	}
	select {
	case <-second.context().Done():
		t.Fatal("canceling completed compound member canceled next member")
	default:
	}
	select {
	case <-root.Done():
		t.Fatal("operation cancellation canceled entire frame")
	default:
	}
}

func TestReadPacketLimitChecksHeaderBeforeAllocatingBody(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan error, 1)
	go func() { _, err := readPacketLimited(left, 256); done <- err }()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 257)
	if _, err := right.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != errInvalidNetBIOS {
			t.Fatalf("error=%v want invalid framing", err)
		}
	case <-time.After(time.Second):
		t.Fatal("oversized body was read")
	}
}

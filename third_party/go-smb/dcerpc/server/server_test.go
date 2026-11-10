// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid

package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfjallid/go-smb/dcerpc"
	"github.com/jfjallid/go-smb/smb"
)

// fakeService is a tiny Service used to verify that PipeHandler routes
// requests by abstract syntax UUID and opnum. Opnum 7 returns the input
// bytes back; opnum 99 returns an error to exercise the fault path.
type fakeService struct {
	uuid  string
	major uint16
	minor uint16

	// mu guards the call-recording fields: TestPipeHandlerConcurrentTransceive
	// dispatches from several goroutines at once.
	mu         sync.Mutex
	lastOpnum  uint16
	lastBuffer []byte
}

func (s *fakeService) InterfaceUUID() string              { return s.uuid }
func (s *fakeService) InterfaceVersion() (uint16, uint16) { return s.major, s.minor }

// last returns the opnum and stub of the most recent Dispatch call.
func (s *fakeService) last() (uint16, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOpnum, s.lastBuffer
}

func (s *fakeService) Dispatch(_ context.Context, op uint16, in []byte) ([]byte, error) {
	s.mu.Lock()
	s.lastOpnum = op
	s.lastBuffer = append([]byte(nil), in...)
	s.mu.Unlock()
	switch op {
	case 7:
		return append([]byte("ok:"), in...), nil
	default:
		return nil, errOpUnsupported
	}
}

var errOpUnsupported = &dispatchErr{msg: "unsupported op"}

type dispatchErr struct{ msg string }

func (e *dispatchErr) Error() string { return e.msg }

// helper: build a client Bind PDU offering one context for the given UUID.
func clientBind(t *testing.T, callID uint32, ifaceUUID string, major, minor uint16) []byte {
	t.Helper()
	req, err := newBindReqExported(callID, ifaceUUID, major, minor)
	if err != nil {
		t.Fatalf("newBindReq: %v", err)
	}
	buf, err := req.MarshalBinary()
	if err != nil {
		t.Fatalf("Bind marshal: %v", err)
	}
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(buf)))
	return buf
}

// newBindReqExported re-creates the package-private newBindReq from
// dcerpc.go for test use; we need the wire-format Bind to feed PipeHandler.
func newBindReqExported(callID uint32, ifaceUUID string, major, minor uint16) (*dcerpc.BindReq, error) {
	abstractUUID, err := dcerpc.UUIDToBin(ifaceUUID)
	if err != nil {
		return nil, err
	}
	transferUUID, err := dcerpc.UUIDToBin(dcerpc.MSRPCUuidNdr)
	if err != nil {
		return nil, err
	}
	return &dcerpc.BindReq{
		Header: dcerpc.Header{
			MajorVersion:   5,
			MinorVersion:   0,
			Type:           dcerpc.PacketTypeBind,
			Flags:          dcerpc.PfcFirstFrag | dcerpc.PfcLastFrag,
			Representation: 0x00000010,
			CallId:         callID,
		},
		MaxSendFragSize: 4280,
		MaxRecvFragSize: 4280,
		ContextList: dcerpc.ContextList{
			Items: []dcerpc.ContextItem{{
				Id:             0,
				AbstractSyntax: dcerpc.SyntaxId{UUID: abstractUUID, Version: (uint32(minor) << 16) | uint32(major)},
				TransferSyntax: []dcerpc.SyntaxId{{UUID: transferUUID, Version: 2}},
			}},
		},
	}, nil
}

// helper: build a client Request PDU on contextId 0 with given opnum/stub.
func clientRequest(t *testing.T, callID uint32, contextID, opnum uint16, stub []byte) []byte {
	t.Helper()
	req := dcerpc.RequestReq{
		Header: dcerpc.Header{
			MajorVersion:   5,
			MinorVersion:   0,
			Type:           dcerpc.PacketTypeRequest,
			Flags:          dcerpc.PfcFirstFrag | dcerpc.PfcLastFrag,
			Representation: 0x00000010,
			CallId:         callID,
		},
		AllocHint: uint32(len(stub)),
		ContextId: contextID,
		Opnum:     opnum,
		Buffer:    stub,
	}
	buf, err := req.MarshalBinary()
	if err != nil {
		t.Fatalf("Request marshal: %v", err)
	}
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(buf)))
	return buf
}

func TestPipeHandlerBindAndRequest(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	svc := &fakeService{uuid: ifaceUUID, major: 1, minor: 0}

	h := NewPipeHandler("testpipe", svc)

	// 1) Bind succeeds and returns a BindAck with one acceptance result.
	bindReq := clientBind(t, 1, ifaceUUID, 1, 0)
	resp, status, err := h.Transceive(context.Background(), bindReq)
	if err != nil {
		t.Fatalf("Bind Transceive: %v", err)
	}
	if status != 0 {
		t.Fatalf("Bind status = 0x%x", status)
	}

	var bindRes dcerpc.BindRes
	if err := bindRes.UnmarshalBinary(resp); err != nil {
		t.Fatalf("BindRes parse: %v", err)
	}
	if bindRes.Header.Type != dcerpc.PacketTypeBindAck {
		t.Fatalf("expected BindAck, got type %d", bindRes.Header.Type)
	}
	if bindRes.Header.CallId != 1 {
		t.Fatalf("CallId = %d", bindRes.Header.CallId)
	}
	if len(bindRes.ResultList.Items) != 1 || bindRes.ResultList.Items[0].Result != 0 {
		t.Fatalf("expected one acceptance, got %+v", bindRes.ResultList)
	}
	if !bytes.Contains(bindRes.SecAddr, []byte("testpipe")) {
		t.Fatalf("SecAddr should contain pipe name, got %q", bindRes.SecAddr)
	}

	// 2) Request opnum 7 reaches the service and the response stub is
	//    returned to the caller.
	reqStub := []byte("hello")
	reqPDU := clientRequest(t, 2, 0, 7, reqStub)
	resp, _, err = h.Transceive(context.Background(), reqPDU)
	if err != nil {
		t.Fatalf("Request Transceive: %v", err)
	}
	var reqRes dcerpc.RequestRes
	if err := reqRes.UnmarshalBinary(resp); err != nil {
		t.Fatalf("RequestRes parse: %v", err)
	}
	if reqRes.Header.Type != dcerpc.PacketTypeResponse {
		t.Fatalf("expected Response, got type %d", reqRes.Header.Type)
	}
	if reqRes.Header.CallId != 2 {
		t.Fatalf("CallId = %d", reqRes.Header.CallId)
	}
	if !bytes.Equal(reqRes.Buffer, append([]byte("ok:"), reqStub...)) {
		t.Fatalf("response stub = %q", reqRes.Buffer)
	}
	if gotOp, gotBuf := svc.last(); gotOp != 7 || !bytes.Equal(gotBuf, reqStub) {
		t.Fatalf("Service.Dispatch was not called as expected: op=%d buf=%q", gotOp, gotBuf)
	}

	// 3) Request on a context that was never bound -> fault PDU.
	reqPDU = clientRequest(t, 3, 99, 7, reqStub)
	resp, _, err = h.Transceive(context.Background(), reqPDU)
	if err != nil {
		t.Fatalf("Request (unbound ctx) Transceive: %v", err)
	}
	var hdr dcerpc.Header
	if err := hdr.UnmarshalBinary(resp[:16]); err != nil {
		t.Fatalf("fault header parse: %v", err)
	}
	if hdr.Type != dcerpc.PacketTypeFault {
		t.Fatalf("expected Fault, got type %d", hdr.Type)
	}
	gotStatus := binary.LittleEndian.Uint32(resp[24:28])
	if gotStatus != dcerpc.ErrorContextMismatch {
		t.Fatalf("fault status = 0x%x, want 0x%x", gotStatus, dcerpc.ErrorContextMismatch)
	}

	// 4) Request opnum 99 -> service errors -> fault PDU.
	reqPDU = clientRequest(t, 4, 0, 99, nil)
	resp, _, err = h.Transceive(context.Background(), reqPDU)
	if err != nil {
		t.Fatalf("Request (failing op) Transceive: %v", err)
	}
	if err := hdr.UnmarshalBinary(resp[:16]); err != nil {
		t.Fatalf("fault header parse: %v", err)
	}
	if hdr.Type != dcerpc.PacketTypeFault {
		t.Fatalf("expected Fault, got type %d", hdr.Type)
	}
}

func TestPipeHandlerBindRejectsUnknownInterface(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	svc := &fakeService{uuid: ifaceUUID, major: 1, minor: 0}
	h := NewPipeHandler("", svc)

	// Different UUID from what the service offers.
	bindReq := clientBind(t, 1, "00000000-0000-0000-0000-000000000000", 1, 0)
	resp, _, err := h.Transceive(context.Background(), bindReq)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var bindRes dcerpc.BindRes
	if err := bindRes.UnmarshalBinary(resp); err != nil {
		t.Fatalf("BindRes parse: %v", err)
	}
	if bindRes.Header.Type != dcerpc.PacketTypeBindAck {
		t.Fatalf("expected BindAck (with rejection), got %d", bindRes.Header.Type)
	}
	if len(bindRes.ResultList.Items) != 1 {
		t.Fatalf("expected one result item, got %d", len(bindRes.ResultList.Items))
	}
	// 1 = user_rejection
	if bindRes.ResultList.Items[0].Result != 1 {
		t.Fatalf("expected user_rejection, got %d", bindRes.ResultList.Items[0].Result)
	}
}

// Ensure interface-value indirection doesn't leak — exercising the lock
// path under concurrent Transceive calls (defensive smoke).
func TestPipeHandlerConcurrentTransceive(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	svc := &fakeService{uuid: ifaceUUID, major: 1, minor: 0}
	h := NewPipeHandler("p", svc)
	if _, _, err := h.Transceive(context.Background(), clientBind(t, 1, ifaceUUID, 1, 0)); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var counter atomic.Uint32
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if _, _, err := h.Transceive(context.Background(), clientRequest(t, counter.Add(1)+1, 0, 7, []byte{1, 2, 3})); err != nil {
					t.Errorf("Transceive: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// --- WRITE/READ stream transport (Apple smbx / impacket style) ---------------

// parseBindAck decodes a completed BindAck and requires the expected type and
// one accepted context.
func parseBindAck(t *testing.T, buf []byte) dcerpc.BindRes {
	t.Helper()
	var res dcerpc.BindRes
	if err := res.UnmarshalBinary(buf); err != nil {
		t.Fatalf("BindRes parse: %v", err)
	}
	if res.Header.Type != dcerpc.PacketTypeBindAck {
		t.Fatalf("expected BindAck, got PDU type %d", res.Header.Type)
	}
	if len(res.ResultList.Items) != 1 || res.ResultList.Items[0].Result != 0 {
		t.Fatalf("expected one accepted context, got %+v", res.ResultList)
	}
	return res
}

// TestPipeHandlerWriteReadBindAndRequest drives the stream path the way a
// WRITE+READ DCERPC client does: Bind via WRITE/READ, then a Request on the
// negotiated context via WRITE/READ.
func TestPipeHandlerWriteReadBindAndRequest(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	svc := &fakeService{uuid: ifaceUUID, major: 1, minor: 0}
	h := NewPipeHandler("testpipe", svc)
	ctx := context.Background()

	bindReq := clientBind(t, 7, ifaceUUID, 1, 0)
	n, status, err := h.Write(ctx, bindReq)
	if err != nil || status != 0 || n != len(bindReq) {
		t.Fatalf("Write(bind) = (%d, 0x%x, %v), want (%d, 0, nil)", n, status, err, len(bindReq))
	}
	out, status, err := h.Read(ctx, 4096)
	if err != nil || status != 0 {
		t.Fatalf("Read(BindAck) = (0x%x, %v)", status, err)
	}
	bindRes := parseBindAck(t, out)
	if bindRes.Header.CallId != 7 {
		t.Fatalf("BindAck CallId = %d, want 7", bindRes.Header.CallId)
	}

	reqStub := []byte("hello")
	reqPDU := clientRequest(t, 8, 0, 7, reqStub)
	if n, status, err := h.Write(ctx, reqPDU); err != nil || status != 0 || n != len(reqPDU) {
		t.Fatalf("Write(request) = (%d, 0x%x, %v)", n, status, err)
	}
	out, status, err = h.Read(ctx, 4096)
	if err != nil || status != 0 {
		t.Fatalf("Read(Response) = (0x%x, %v)", status, err)
	}
	var reqRes dcerpc.RequestRes
	if err := reqRes.UnmarshalBinary(out); err != nil {
		t.Fatalf("RequestRes parse: %v", err)
	}
	if reqRes.Header.Type != dcerpc.PacketTypeResponse || reqRes.Header.CallId != 8 {
		t.Fatalf("Response header = type %d call_id %d", reqRes.Header.Type, reqRes.Header.CallId)
	}
	if want := append([]byte("ok:"), reqStub...); !bytes.Equal(reqRes.Buffer, want) {
		t.Fatalf("response stub = %q, want %q", reqRes.Buffer, want)
	}
	if gotOp, gotBuf := svc.last(); gotOp != 7 || !bytes.Equal(gotBuf, reqStub) {
		t.Fatalf("Service.Dispatch = op=%d buf=%q", gotOp, gotBuf)
	}
}

// TestPipeHandlerWriteReadPartialPDUsAndFragmentedRead buffers a PDU that is
// split across WRITEs and reassembles the response from small READs, proving
// both directions fragment rather than being treated as whole-PDU messages.
func TestPipeHandlerWriteReadPartialPDUsAndFragmentedRead(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	ctx := context.Background()

	bindReq := clientBind(t, 3, ifaceUUID, 1, 0)
	// First WRITE carries only part of the common header; second carries the
	// rest of the PDU. Nothing may be dispatched before the PDU is complete.
	for _, chunk := range [][]byte{bindReq[:9], bindReq[9:]} {
		if n, status, err := h.Write(ctx, chunk); err != nil || status != 0 || n != len(chunk) {
			t.Fatalf("Write(chunk %d) = (%d, 0x%x, %v)", len(chunk), n, status, err)
		}
	}

	var got []byte
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatal("fragmented READ did not terminate")
		}
		out, status, err := h.Read(ctx, 7)
		if err != nil || status != 0 {
			t.Fatalf("Read #%d = (0x%x, %v)", i, status, err)
		}
		if len(out) == 0 || len(out) > 7 {
			t.Fatalf("Read #%d returned %d bytes (max 7)", i, len(out))
		}
		got = append(got, out...)
		if len(got) < dcerpc.PDUHeaderCommonSize {
			continue
		}
		var hdr dcerpc.Header
		if err := hdr.UnmarshalBinary(got[:dcerpc.PDUHeaderCommonSize]); err != nil {
			t.Fatalf("header parse after %d bytes: %v", len(got), err)
		}
		if len(got) > int(hdr.FragLength) {
			t.Fatalf("reassembled %d bytes beyond frag_length %d", len(got), hdr.FragLength)
		}
		if len(got) == int(hdr.FragLength) {
			break
		}
	}
	if res := parseBindAck(t, got); res.Header.CallId != 3 {
		t.Fatalf("BindAck CallId = %d, want 3", res.Header.CallId)
	}
}

// TestPipeHandlerWriteDispatchesMultiplePDUs verifies that one WRITE carrying
// back-to-back PDUs dispatches each in stream order (the Request after the
// Bind must see the just-bound context).
func TestPipeHandlerWriteDispatchesMultiplePDUs(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	ctx := context.Background()

	bindReq := clientBind(t, 11, ifaceUUID, 1, 0)
	reqPDU := clientRequest(t, 12, 0, 7, []byte("x"))
	stream := append(append([]byte(nil), bindReq...), reqPDU...)
	if n, status, err := h.Write(ctx, stream); err != nil || status != 0 || n != len(stream) {
		t.Fatalf("Write(stream) = (%d, 0x%x, %v)", n, status, err)
	}

	// A single READ may return both responses because the return value is
	// bounded by max, not by PDU boundaries (the client parses the stream).
	out, status, err := h.Read(ctx, 4096)
	if err != nil || status != 0 {
		t.Fatalf("Read = (0x%x, %v)", status, err)
	}
	pdus := splitResponsePDUs(t, out)
	if len(pdus) != 2 {
		t.Fatalf("READ returned %d PDUs, want 2", len(pdus))
	}
	if res := parseBindAck(t, pdus[0]); res.Header.CallId != 11 {
		t.Fatalf("first response CallId = %d, want 11 (BindAck)", res.Header.CallId)
	}
	var reqRes dcerpc.RequestRes
	if err := reqRes.UnmarshalBinary(pdus[1]); err != nil {
		t.Fatalf("RequestRes parse: %v", err)
	}
	if reqRes.Header.Type != dcerpc.PacketTypeResponse || reqRes.Header.CallId != 12 {
		t.Fatalf("second response = type %d call_id %d, want Response/12", reqRes.Header.Type, reqRes.Header.CallId)
	}
}

// splitResponsePDUs splits a byte stream into complete PDUs using each
// header's frag_length.
func splitResponsePDUs(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(stream) > 0 {
		if len(stream) < dcerpc.PDUHeaderCommonSize {
			t.Fatalf("truncated PDU header: %d trailing bytes", len(stream))
		}
		var hdr dcerpc.Header
		if err := hdr.UnmarshalBinary(stream[:dcerpc.PDUHeaderCommonSize]); err != nil {
			t.Fatalf("PDU header parse: %v", err)
		}
		if int(hdr.FragLength) > len(stream) {
			t.Fatalf("truncated PDU: frag_length %d, %d bytes left", hdr.FragLength, len(stream))
		}
		out = append(out, stream[:hdr.FragLength])
		stream = stream[hdr.FragLength:]
	}
	return out
}

type readResult struct {
	out    []byte
	status uint32
	err    error
}

// TestPipeHandlerReadBlocksUntilWrite ensures a READ issued before the client
// WRITEs (legal for a pipe) blocks and is then woken by the WRITE.
func TestPipeHandlerReadBlocksUntilWrite(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	started := make(chan struct{})
	done := make(chan readResult, 1)
	go func() {
		close(started)
		out, status, err := h.Read(context.Background(), 64)
		done <- readResult{out: out, status: status, err: err}
	}()
	<-started
	select {
	case r := <-done:
		t.Fatalf("Read returned before any WRITE: status=0x%x err=%v out=%d bytes", r.status, r.err, len(r.out))
	case <-time.After(20 * time.Millisecond):
	}

	bindReq := clientBind(t, 5, ifaceUUID, 1, 0)
	if _, _, err := h.Write(context.Background(), bindReq); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.status != 0 {
			t.Fatalf("blocked Read result = (0x%x, %v)", r.status, r.err)
		}
		if res := parseBindAck(t, r.out); res.Header.CallId != 5 {
			t.Fatalf("BindAck CallId = %d, want 5", res.Header.CallId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Read was not woken by WRITE")
	}
}

// TestPipeHandlerReadCancellation ensures a blocked READ returns the context
// error (mapped to STATUS_CANCELLED by smb/server/read.go) instead of hanging.
func TestPipeHandlerReadCancellation(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, _, err := h.Read(ctx, 64)
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("Read returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Read did not return")
	}
}

// TestPipeHandlerCloseWakesBlockedRead covers pipe teardown racing a pending
// READ: Close must wake it with STATUS_FILE_CLOSED, be idempotent, and later
// operations must not panic.
func TestPipeHandlerCloseWakesBlockedRead(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	started := make(chan struct{})
	done := make(chan readResult, 1)
	go func() {
		close(started)
		out, status, err := h.Read(context.Background(), 64)
		done <- readResult{out: out, status: status, err: err}
	}()
	<-started
	select {
	case r := <-done:
		t.Fatalf("Read returned before Close: status=0x%x err=%v out=%d bytes", r.status, r.err, len(r.out))
	case <-time.After(20 * time.Millisecond):
	}

	if err := h.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.status != smb.StatusFileClosed {
			t.Fatalf("woken Read = (0x%x, %v), want STATUS_FILE_CLOSED", r.status, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the blocked Read")
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if out, status, err := h.Read(context.Background(), 16); err != nil || status != smb.StatusFileClosed || out != nil {
		t.Fatalf("Read after Close = (%d bytes, 0x%x, %v)", len(out), status, err)
	}
	if n, status, err := h.Write(context.Background(), []byte{1, 2, 3}); err != nil || status != smb.StatusFileClosed || n != 0 {
		t.Fatalf("Write after Close = (%d, 0x%x, %v)", n, status, err)
	}
}

// TestPipeHandlerWriteRejectsInvalidFragmentLength covers malformed framing:
// frag_length shorter than the 16-byte common header cannot be a PDU.
func TestPipeHandlerWriteRejectsInvalidFragmentLength(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	bindReq := clientBind(t, 1, ifaceUUID, 1, 0)
	for _, fragLen := range []uint16{0, 4, uint16(dcerpc.PDUHeaderCommonSize - 1)} {
		bad := append([]byte(nil), bindReq...)
		binary.LittleEndian.PutUint16(bad[8:10], fragLen)
		n, status, err := h.Write(context.Background(), bad)
		if err != nil {
			t.Fatalf("frag_length=%d: unexpected error: %v", fragLen, err)
		}
		if status != smb.StatusInvalidParameter || n != 0 {
			t.Fatalf("frag_length=%d: Write = (%d, 0x%x), want (0, STATUS_INVALID_PARAMETER)", fragLen, n, status)
		}
	}
}

// TestPipeHandlerReadRejectsNonPositiveMax covers a zero-length SMB2 READ.
func TestPipeHandlerReadRejectsNonPositiveMax(t *testing.T) {
	const ifaceUUID = "12345678-1234-1234-1234-123456789012"
	h := NewPipeHandler("", &fakeService{uuid: ifaceUUID, major: 1, minor: 0})
	for _, max := range []int{0, -1} {
		out, status, err := h.Read(context.Background(), max)
		if err != nil || status != smb.StatusInvalidParameter || out != nil {
			t.Fatalf("Read(max=%d) = (%d bytes, 0x%x, %v)", max, len(out), status, err)
		}
	}
}

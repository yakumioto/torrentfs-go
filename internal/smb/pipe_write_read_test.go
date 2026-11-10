package smb

import (
	"encoding/binary"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	independent "github.com/hirochachacha/go-smb2"
	"github.com/jfjallid/go-smb/dcerpc"
	"github.com/jfjallid/go-smb/dcerpc/mssrvs"
)

// pipeBindPDU builds a client Bind PDU for the srvsvc abstract syntax, the
// same shape Apple's smbx RPC client sends over IPC$\srvsvc: SMB2 WRITE then
// SMB2 READ, with no RPC-level auth verifier.
func pipeBindPDU(t *testing.T, callID uint32) []byte {
	t.Helper()
	abstract, err := dcerpc.UUIDToBin(mssrvs.MSRPCUuidSrvSvc)
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := dcerpc.UUIDToBin(dcerpc.MSRPCUuidNdr)
	if err != nil {
		t.Fatal(err)
	}
	req := dcerpc.BindReq{
		Header: dcerpc.Header{
			MajorVersion: 5, Type: dcerpc.PacketTypeBind,
			Flags: dcerpc.PfcFirstFrag | dcerpc.PfcLastFrag, Representation: 0x00000010, CallId: callID,
		},
		MaxSendFragSize: 4280, MaxRecvFragSize: 4280,
		ContextList: dcerpc.ContextList{Items: []dcerpc.ContextItem{{
			Id:             0,
			AbstractSyntax: dcerpc.SyntaxId{UUID: abstract, Version: uint32(mssrvs.MSRPCSrvSvcMajorVersion)},
			TransferSyntax: []dcerpc.SyntaxId{{UUID: transfer, Version: 2}},
		}}},
	}
	buf, err := req.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(buf)))
	// The captured Apple Bind PDU was 72 bytes; this construction matches it
	// (one srvsvc context, NDR v2 transfer syntax, no RPC auth verifier).
	if len(buf) != 72 {
		t.Fatalf("bind PDU = %d bytes, want the 72-byte Apple/impacket form", len(buf))
	}
	return buf
}

// pipeNetShareEnumPDU builds the NetrShareEnumAll (opnum 15, level 1) Request
// PDU an Apple/impacket-style client sends after the BindAck.
func pipeNetShareEnumPDU(t *testing.T, callID uint32) []byte {
	t.Helper()
	stub, err := mssrvs.NewNetShareEnumAllRequest(`\\torrentfs`).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	req := dcerpc.RequestReq{
		Header: dcerpc.Header{
			MajorVersion: 5, Type: dcerpc.PacketTypeRequest,
			Flags: dcerpc.PfcFirstFrag | dcerpc.PfcLastFrag, Representation: 0x00000010, CallId: callID,
		},
		AllocHint: uint32(len(stub)),
		ContextId: 0,
		Opnum:     mssrvs.SrvSvcOpNetShareEnumAll,
		Buffer:    stub,
	}
	buf, err := req.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(buf)))
	return buf
}

// TestIndependentClientPipeWriteReadShareEnumeration reproduces the iOS Files
// "connect to server root" probe with an independent client that, like Apple's
// smbx and impacket, carries DCE/RPC over named-pipe WRITE+READ instead of
// FSCTL_PIPE_TRANSCEIVE: dial -> Mount("IPC$") -> Open("srvsvc") ->
// WRITE(bind) -> READ(BindAck) -> WRITE(NetrShareEnumAll) -> READ(Response).
// The share list must contain the virtual torrentfs share.
func TestIndependentClientPipeWriteReadShareEnumeration(t *testing.T) {
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
	share, err := sess.Mount("IPC$")
	if err != nil {
		t.Fatalf("Mount IPC$: %v", err)
	}
	defer func() {
		if err := share.Umount(); err != nil {
			t.Errorf("Umount: %v", err)
		}
	}()
	f, err := share.OpenFile("srvsvc", os.O_RDWR, 0o666)
	if err != nil {
		t.Fatalf("Open srvsvc: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close srvsvc: %v", err)
		}
	}()

	bindReq := pipeBindPDU(t, 1)
	n, err := f.Write(bindReq)
	if err != nil {
		t.Fatalf("pipe WRITE bind: %v", err)
	}
	if n != len(bindReq) {
		t.Fatalf("pipe WRITE bind: n=%d want %d", n, len(bindReq))
	}
	readBuf := make([]byte, 4280)
	n, err = f.Read(readBuf)
	if err != nil {
		t.Fatalf("pipe READ bind response: %v", err)
	}
	var bindRes dcerpc.BindRes
	if err := bindRes.UnmarshalBinary(readBuf[:n]); err != nil {
		t.Fatalf("parse BindAck: %v", err)
	}
	if bindRes.Header.Type != dcerpc.PacketTypeBindAck || bindRes.Header.CallId != 1 {
		t.Fatalf("BindAck header = type %d call_id %d", bindRes.Header.Type, bindRes.Header.CallId)
	}
	if len(bindRes.ResultList.Items) == 0 || bindRes.ResultList.Items[0].Result != 0 {
		t.Fatalf("srvsvc bind was not accepted: %+v", bindRes.ResultList)
	}

	enumReq := pipeNetShareEnumPDU(t, 2)
	if n, err = f.Write(enumReq); err != nil {
		t.Fatalf("pipe WRITE NetrShareEnumAll: %v", err)
	}
	if n != len(enumReq) {
		t.Fatalf("pipe WRITE NetrShareEnumAll: n=%d want %d", n, len(enumReq))
	}
	n, err = f.Read(readBuf)
	if err != nil {
		t.Fatalf("pipe READ NetrShareEnumAll response: %v", err)
	}
	var reqRes dcerpc.RequestRes
	if err := reqRes.UnmarshalBinary(readBuf[:n]); err != nil {
		t.Fatalf("parse Response: %v", err)
	}
	if reqRes.Header.Type != dcerpc.PacketTypeResponse || reqRes.Header.CallId != 2 {
		t.Fatalf("Response header = type %d call_id %d (fault status 0x%08x)",
			reqRes.Header.Type, reqRes.Header.CallId, reqRes.Buffer)
	}
	var enumRes mssrvs.NetShareEnumAllResponse
	if err := enumRes.Unmarshal(reqRes.Buffer); err != nil {
		t.Fatalf("parse NetShareEnumAllResponse: %v", err)
	}
	if enumRes.WindowsError != mssrvs.ErrorSuccess {
		t.Fatalf("NetrShareEnumAll win32 status = %d", enumRes.WindowsError)
	}
	if enumRes.InfoStruct.Level != 1 || enumRes.InfoStruct.Level1 == nil {
		t.Fatalf("share enum level = %d", enumRes.InfoStruct.Level)
	}
	names := make([]string, 0, len(enumRes.InfoStruct.Level1.Buffer))
	for _, entry := range enumRes.InfoStruct.Level1.Buffer {
		names = append(names, entry.Name)
	}
	if !slices.Contains(names, ShareName) {
		t.Fatalf("share enum %v does not contain %q", names, ShareName)
	}
	if b.opens.Load() != 0 {
		t.Fatal("share enumeration opened payload bytes")
	}
}

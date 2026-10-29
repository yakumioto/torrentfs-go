package smb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"testing"

	wire "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	smbunicode "github.com/jfjallid/go-smb/smb/unicode"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

func TestDefaultDataStreamMetadataAndReadOnlyAccess(t *testing.T) {
	backend := newTestBackend(0)
	adapter := &filesystem{tree: vfs.New(backend)}
	opened, status, err := adapter.Create(context.Background(), nil, server.CreateRequest{
		Path: `media\healthy.bin`, DesiredAccess: readAccess, CreateDisposition: fileOpen,
	})
	if err != nil || status != wire.StatusOk {
		t.Fatalf("create status=%x err=%v", status, err)
	}
	defer func() { _ = adapter.Close(context.Background(), opened.Handle) }()
	value, status, err := adapter.QueryFileInfo(context.Background(), opened.Handle, 0x16)
	if err != nil || status != wire.StatusOk {
		t.Fatalf("stream status=%x err=%v", status, err)
	}
	data := value.([]byte)
	if len(data) < 24 || binary.LittleEndian.Uint32(data[:4]) != 0 ||
		int(binary.LittleEndian.Uint32(data[4:])) != len(data)-24 ||
		int64(binary.LittleEndian.Uint64(data[8:])) != int64(len(backend.data["healthy.bin"])) ||
		!bytes.Equal(data[24:], smbunicode.ToUnicode("::$DATA")) {
		t.Fatalf("invalid default stream metadata: %x", data)
	}
	value, status, err = adapter.QueryFileInfo(context.Background(), opened.Handle, 0x08)
	if err != nil || status != wire.StatusOk || binary.LittleEndian.Uint32(value.([]byte))&writeAccess != 0 {
		t.Fatalf("access information status=%x err=%v", status, err)
	}
	if err := adapter.Close(context.Background(), opened.Handle); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := adapter.QueryFileInfo(context.Background(), opened.Handle, 0x16); status != wire.StatusFileClosed {
		t.Fatalf("closed stream query status=%x", status)
	}
}

func TestSMBAdapterRejectsMutatingOpens(t *testing.T) {
	adapter := &filesystem{tree: vfs.New(newTestBackend(0))}
	for _, access := range []uint32{0x02, 0x04, 0x10, 0x40, 0x100, 0x10000, 0x40000, 0x80000, 0x40000000, 0x10000000} {
		if _, status, _ := adapter.Create(context.Background(), nil, server.CreateRequest{
			Path: `media\healthy.bin`, DesiredAccess: access, CreateDisposition: fileOpen,
		}); status != wire.StatusAccessDenied {
			t.Errorf("write access %x accepted: status=%x", access, status)
		}
	}
	for _, disposition := range []uint32{0, 2, 4, 5} {
		if _, status, _ := adapter.Create(context.Background(), nil, server.CreateRequest{
			Path: `media\healthy.bin`, DesiredAccess: readAccess, CreateDisposition: disposition,
		}); status != wire.StatusAccessDenied {
			t.Errorf("disposition %d accepted: %x", disposition, status)
		}
	}
	if _, status, _ := adapter.Create(context.Background(), nil, server.CreateRequest{
		Path: `media\healthy.bin`, DesiredAccess: readAccess, CreateDisposition: fileOpen, CreateOptions: fileDeleteOnClose,
	}); status != wire.StatusAccessDenied {
		t.Fatal("delete-on-close accepted")
	}
	if _, status, _ := adapter.Write(context.Background(), nil, 0, nil); status != wire.StatusAccessDenied {
		t.Fatal("write accepted")
	}
	if status, _ := adapter.SetFileInfo(context.Background(), nil, 0, nil); status != wire.StatusAccessDenied {
		t.Fatal("metadata mutation accepted")
	}
}

func TestSMBAdapterErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status uint32
	}{
		{context.Canceled, wire.StatusCancelled}, {context.DeadlineExceeded, statusIOTimeout},
		{fs.ErrClosed, wire.StatusFileClosed}, {vfs.ErrClosed, wire.StatusFileClosed},
		{vfs.ErrNotFound, wire.StatusObjectNameNotFound}, {vfs.ErrReadOnly, wire.StatusAccessDenied},
		{vfs.ErrNotDir, statusNotADirectory}, {vfs.ErrIsDir, statusFileIsADirectory},
		{vfs.ErrInvalidName, statusInvalidObjectName}, {errors.New("storage failure"), statusIOError},
	} {
		if got := statusFor(tc.err); got != tc.status {
			t.Errorf("%v -> %x want %x", tc.err, got, tc.status)
		}
	}
}

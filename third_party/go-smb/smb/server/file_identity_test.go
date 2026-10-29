package server

import (
	"encoding/binary"
	"testing"

	"github.com/jfjallid/go-smb/smb"
)

func TestStableFileIDAgreesAcrossMetadataAndQFid(t *testing.T) {
	const id uint64 = 0x1122334455667788
	info := FileInfo{Name: "payload.bin", FileID: id, Size: 31, Attributes: FileAttributeReadonly}
	internal, status := serializeFileInfo(smb.FileInternalInformation, info, &lateLifecycleHandle{})
	if status != smb.StatusOk || len(internal) != 8 || binary.LittleEndian.Uint64(internal) != id {
		t.Fatalf("internal information status=0x%x bytes=%x", status, internal)
	}
	all, status := serializeFileInfo(smb.FileAllInformation, info, &lateLifecycleHandle{})
	if status != smb.StatusOk || len(all) < 72 || binary.LittleEndian.Uint64(all[64:]) != id {
		t.Fatal("FileAllInformation disagrees with FileInternalInformation")
	}
	for _, format := range []struct {
		class  byte
		offset int
	}{
		{smb.FileIdBothDirectoryInformation, 96}, {smb.FileIdFullDirectoryInformation, 72},
	} {
		entry := serializeDirEntry(format.class, DirEntry{FileInfo: info})
		if len(entry) < format.offset+8 || binary.LittleEndian.Uint64(entry[format.offset:]) != id {
			t.Fatalf("directory class %d disagrees with opened-file identity", format.class)
		}
	}
	context := queryOnDiskIDResponse(info, "torrentfs")
	if context.Name != "QFid" || len(context.Data) != 32 || binary.LittleEndian.Uint64(context.Data) != id {
		t.Fatal("QFid disagrees with metadata")
	}
	volume := binary.LittleEndian.Uint64(context.Data[8:])
	if volume == 0 || binary.LittleEndian.Uint64(queryOnDiskIDResponse(info, "TORRENTFS").Data[8:]) != volume {
		t.Fatal("volume identity is not stable under share-name case folding")
	}
	encoded := marshalCreateContexts([]createContext{context})
	decoded, err := parseCreateContexts(encoded)
	if err != nil || len(decoded) != 1 || binary.LittleEndian.Uint64(decoded[0].Data) != id {
		t.Fatalf("QFid wire round-trip: %v", err)
	}
}

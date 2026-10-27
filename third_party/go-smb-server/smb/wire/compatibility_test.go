package wire

import (
	"encoding/binary"
	"testing"
)

func TestFileAllInformationFixedFields(t *testing.T) {
	info := FileAllInformationAppend(nil, FileBasicInformation{FileAttributes: 1}, FileStandardInformation{EndOfFile: 123})
	if len(info) != 100 || binary.LittleEndian.Uint64(info[48:56]) != 123 || binary.LittleEndian.Uint32(info[96:100]) != 0 {
		t.Fatalf("FileAllInformation fixed layout: %x", info)
	}
}

func TestUTF16SurrogateRoundTrip(t *testing.T) {
	name := "电影😀.mkv"
	if got := UTF16FromBytes(UTF16ToBytes(name)); got != name {
		t.Fatalf("UTF-16: %q", got)
	}
}

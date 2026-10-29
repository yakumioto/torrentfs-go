// Copyright (c) 2026 torrentfs-go contributors. MIT licensed; see LICENSE.
package server

import (
	"encoding/binary"
	"hash/fnv"
	"strings"
)

const createContextQueryOnDiskID = "QFid"

// The on-disk ID is a filesystem identity, not an open handle's persistent or
// volatile FileId. All metadata paths must report the same VFS FileID.
func queryOnDiskIDResponse(info FileInfo, share string) createContext {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint64(data, info.FileID)
	volume := fnv.New64a()
	_, _ = volume.Write([]byte(strings.ToLower(share)))
	binary.LittleEndian.PutUint64(data[8:], volume.Sum64())
	return createContext{Name: createContextQueryOnDiskID, Data: data}
}

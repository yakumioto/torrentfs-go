package smb

import (
	"sort"

	srvsvc "github.com/jfjallid/go-smb/dcerpc/mssrvs/server"
	rpcserver "github.com/jfjallid/go-smb/dcerpc/server"
	"github.com/jfjallid/go-smb/smb/server"
)

// Only srvsvc's share-enumeration operation is registered. No host filesystem,
// registry, account-management or other administrative RPC service is exposed.
func sharePipeOpener(names []string) server.PipeOpener {
	sort.Strings(names)
	entries := make([]srvsvc.ShareEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, srvsvc.ShareEntry{Name: name, Comment: "Read-only torrent files"})
	}
	return &server.MapPipeOpener{Pipes: map[string]func(*server.Session) (server.PipeBackend, error){
		"srvsvc": func(*server.Session) (server.PipeBackend, error) {
			return rpcserver.NewPipeHandler("srvsvc", &srvsvc.Service{
				ServerName: "TORRENTFS", Shares: entries,
			}), nil
		},
	}}
}

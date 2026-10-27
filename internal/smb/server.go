package smb

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
	"github.com/sonroyaalmerol/go-smb-server/smb/server"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

// New configures authenticated read-only shares; it does not bind a listener.
func New(cfg config.Config, source filesystem.StreamURLBackend, logger *slog.Logger) (*server.Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.SMB.Username == "" || cfg.SMB.Password == "" {
		return nil, fmt.Errorf("smb credentials are required: %w", config.ErrInvalid)
	}
	if logger == nil {
		logger = logging.Discard()
	}
	credentials := ntlmssp.NewMemoryCredentials()
	credentials.Add("WORKGROUP", cfg.SMB.Username, cfg.SMB.Password)
	credentials.Add("", cfg.SMB.Username, cfg.SMB.Password)
	timeout := time.Duration(cfg.Mount.ReadTimeout)
	shares := []vfs.Share{vfs.NewDiskShare(cfg.SMB.ShareName, newBackend(source, timeout))}
	if cfg.SMB.STRMEnabled {
		shares = append(shares, vfs.NewDiskShare(cfg.SMB.ShareName+"-strm", newBackend(filesystem.NewSTRMBackend(source), timeout)))
	}
	return server.New(server.WithAddr(cfg.SMB.ListenAddr), server.WithShares(shares...),
		server.WithAuth(ntlmssp.NewServer(credentials, "WORKGROUP")), server.WithLogger(logger))
}

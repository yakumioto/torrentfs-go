// Package smb adapts the shared virtual tree to an authenticated, read-only
// SMB2/3 file server. It never mounts or exports a host directory.
package smb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
	"unicode"

	"github.com/jfjallid/go-smb/ntlmssp"
	wire "github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

const (
	ShareName     = "torrentfs"
	STRMShareName = "torrentfs-strm"
)

type Options struct {
	Username    string
	Password    string
	ReadTimeout time.Duration
	Logger      *slog.Logger
}

type Server struct {
	server *server.Server
	logger *slog.Logger
}

func New(shares map[string]*vfs.Tree, opts Options) (*Server, error) {
	if opts.Username == "" || opts.Password == "" || strings.ContainsFunc(opts.Username, unicode.IsControl) {
		return nil, errors.New("smb: a username and password are required")
	}
	if len(shares) == 0 {
		return nil, errors.New("smb: at least one virtual share is required")
	}
	// NTLMv2 uses the NT password hash too; MapAuthenticator does not accept
	// NTLMv1 authentication. Reuse the protocol library's key derivation.
	ntHash := ntlmssp.Ntowfv1(opts.Password)
	cfg := &server.ServerConfig{
		NetBIOSName: "TORRENTFS",
		MinDialect:  wire.DialectSmb_2_1,
		// Retain the 3.0.2 compatibility cap: an observed Infuse connection
		// closed after our 3.1.1 reply. This does not establish whether the
		// incompatibility is in the client or our 3.1.1 response.
		MaxDialect:          wire.DialectSmb_3_0_2,
		SigningRequired:     true,
		EncryptionSupported: true,
		ConcurrentReads:     true,
		MaxReadSize:         1 << 20,
		MaxWriteSize:        64 << 10,
		MaxTransactSize:     1 << 20,
		MaxRequestSize:      2 << 20,
		MaxConnections:      32,
		IdleTimeout:         5 * time.Minute,
		WriteTimeout:        10 * time.Second,
		Authenticator: &server.MapAuthenticator{Accounts: map[string]*server.Account{
			strings.ToLower(opts.Username): {NTHash: ntHash},
		}},
		// Upstream also serves credential-capture use cases. Its printf-style
		// diagnostics must never retain authentication blobs in application logs.
		Logger: protocolLogger{},
	}
	native := &server.Server{Config: cfg}
	names := make([]string, 0, len(shares))
	for name, tree := range shares {
		if name != ShareName && name != STRMShareName || tree == nil {
			return nil, errors.New("smb: unsupported virtual share")
		}
		native.RegisterShare(name, server.Share{Type: wire.ShareTypeDisk,
			VFS:           &filesystem{tree: tree, readTimeout: opts.ReadTimeout},
			MaximalAccess: readAccess, WritableUsers: map[string]bool{},
		})
		names = append(names, name)
	}
	cfg.PipeOpener = sharePipeOpener(names)
	return &Server{server: native, logger: opts.Logger}, nil
}

func (s *Server) Serve(listener net.Listener) error {
	if s.logger != nil {
		s.logger.Info("smb listening", "listen_addr", listener.Addr().String())
	}
	err := s.server.Serve(listener)
	if errors.Is(err, server.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("smb: serve: %w", err)
	}
	return nil
}

// Shutdown first interrupts network I/O and all read contexts, then waits for
// connection teardown. Idle players cannot hold session shutdown indefinitely.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.server.Close(); err != nil {
		return fmt.Errorf("smb: close: %w", err)
	}
	return s.server.Shutdown(ctx)
}

// Protocol diagnostics are deliberately not forwarded: credentials and raw
// packets can occur in upstream log arguments even when capture hooks are nil.
type protocolLogger struct{}

func (protocolLogger) Errorf(string, ...any)  {}
func (protocolLogger) Errorln(...any)         {}
func (protocolLogger) Noticef(string, ...any) {}
func (protocolLogger) Noticeln(...any)        {}
func (protocolLogger) Infof(string, ...any)   {}
func (protocolLogger) Infoln(...any)          {}
func (protocolLogger) Debugf(string, ...any)  {}
func (protocolLogger) Debugln(...any)         {}

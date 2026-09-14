// Command torrentfs mounts BitTorrent downloads as a FUSE filesystem.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const usageText = `torrentfs mounts BitTorrent downloads as a FUSE filesystem and serves a
torrent management HTTP API.

Usage:
  torrentfs -mountpoint <dir> [flags] <torrents-dir>

Flags:
  -mountpoint <dir>  directory to mount on (required unless the HTTP API is enabled)
  -config <file>     TOML configuration file
  -data-dir <dir>    override the configured torrent session data directory
  -h, --help         show this help and exit

<torrents-dir> must be an existing directory. Its direct regular, non-symlink
files whose names end in .torrent are scanned at startup and while running.
A single-file torrent is exposed under the mount point as a regular file
directly, e.g. <mount>/movie.mp4; a multi-file torrent is exposed as a
directory tree. A read-only stats/ control tree mirrors the data tree and
reports piece state. Existing metadata files are restored from
<torrents-dir>/.metadata, and complete .torrent files may also be written to
metadata/ while mounted.

When http.listen_addr is set the management API is served; unless a required
Bearer Token is configured it must bind loopback only. Omitting -mountpoint
then runs headless (HTTP only). Send SIGINT or SIGTERM to shut down.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run executes the CLI and returns the process exit code: 0 on success, 1 on
// runtime errors, 2 on usage or configuration errors.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("torrentfs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, usageText) }

	var (
		mountpoint = flags.String("mountpoint", "", "directory to mount on")
		configPath = flags.String("config", "", "TOML configuration file")
		dataDir    = flags.String("data-dir", "", "override the configured data directory")
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(flags.Args()) != 1 {
		_, _ = fmt.Fprintf(stderr, "torrentfs: exactly one existing torrents directory is required (got %d positional arguments)\n", len(flags.Args()))
		flags.Usage()
		return 2
	}
	torrentsDir := flags.Args()[0]
	if err := validateTorrentDir(torrentsDir); err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 2
	}

	dataDirSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "data-dir" {
			dataDirSet = true
		}
	})
	cfg, err := loadConfig(*configPath, *dataDir, dataDirSet)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 2
	}

	httpEnabled := cfg.HTTP.ListenAddr != ""
	if *mountpoint == "" && !httpEnabled {
		flags.Usage()
		return 2
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	sess, err := session.New(cfg, torrentsDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		if errors.Is(err, config.ErrInvalid) {
			return 2
		}
		return 1
	}

	var server *fuse.Server
	if *mountpoint != "" {
		server, err = filesystem.Mount(*mountpoint, sess, nil)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "torrentfs: mount %s: %v\n", *mountpoint, err)
			_ = sess.Close(context.Background())
			return 1
		}
	}

	var apiServer *api.Server
	serveErr := make(chan error, 1)
	if httpEnabled {
		apiServer = api.New(cfg, sess)
		go func() { serveErr <- apiServer.Serve(rootCtx, cfg.HTTP.ListenAddr) }()
	}

	var serveFailure error
	select {
	case <-signals:
	case serveFailure = <-serveErr:
	}

	var unmountErr error
	if server != nil {
		unmountErr = server.Unmount()
	}
	if apiServer != nil {
		if err := apiServer.Shutdown(context.Background()); err != nil && serveFailure == nil {
			serveFailure = err
		}
	}
	closeErr := sess.Close(rootCtx)
	cancel()
	if unmountErr != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: unmount %s: %v\n", *mountpoint, unmountErr)
	}
	if serveFailure != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: http server: %v\n", serveFailure)
	}
	if closeErr != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: close: %v\n", closeErr)
	}
	if unmountErr != nil || serveFailure != nil || closeErr != nil {
		return 1
	}
	return 0
}

func validateTorrentDir(path string) error {
	if path == "" {
		return errors.New("torrents directory is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("torrents directory %q must be an existing directory: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("torrents path %q must be an existing directory", path)
	}
	return nil
}

func loadConfig(path, dataDir string, dataDirSet bool) (config.Config, error) {
	cfg := config.Default()
	if path != "" {
		var err error
		cfg, err = config.Load(path)
		if err != nil {
			return config.Config{}, fmt.Errorf("load config %q: %w", path, err)
		}
	}
	if dataDirSet {
		cfg.Paths.DataDir = dataDir
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

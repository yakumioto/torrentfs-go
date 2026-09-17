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
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const usageText = `torrentfs mounts BitTorrent downloads as a read-only FUSE filesystem and
serves a torrent management HTTP API.

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
directory tree. The mount is data only and every path in it is read-only:
there is no metadata/ or stats/ control directory.

Managing torrents happens over the HTTP API: add by upload or magnet, list,
delete, and query per-torrent piece status. Durable managed metainfo and
pending magnet intents live in <torrents-dir>/.metadata, which is an
implementation detail and is never mounted.

When http.listen_addr is set the management API is served; an enabled
http.auth configuration is required for non-loopback listeners. Omitting
-mountpoint then runs headless (HTTP only). Send SIGINT or SIGTERM to shut down.
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

	var apiServer *api.Server
	if httpEnabled {
		apiServer, err = api.New(cfg, sess)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "torrentfs: initialize HTTP API: %v\n", err)
			_ = sess.Close(context.Background())
			if errors.Is(err, config.ErrInvalid) {
				return 2
			}
			return 1
		}
	}

	var server *fuse.Server
	if *mountpoint != "" {
		server, err = filesystem.Mount(*mountpoint, sess, nil)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "torrentfs: mount %s: %v\n", *mountpoint, err)
			if apiServer != nil {
				_ = apiServer.Shutdown(context.Background())
			}
			_ = sess.Close(context.Background())
			return 1
		}
	}

	serveErr := make(chan error, 1)
	if apiServer != nil {
		go func() { serveErr <- apiServer.Serve(rootCtx, cfg.HTTP.ListenAddr) }()
	}

	var serveFailure error
	select {
	case <-signals:
	case serveFailure = <-serveErr:
	}

	sequence := shutdownSequence{}
	if apiServer != nil {
		sequence.api = apiServer
	}
	if sess != nil {
		sequence.sess = sess
	}
	if server != nil {
		sequence.mount = server
	}
	shutdown := sequence.run()
	cancel()

	if shutdown.unmount != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: unmount %s: %v\n", *mountpoint, shutdown.unmount)
	}
	if shutdown.api != nil && serveFailure == nil {
		serveFailure = shutdown.api
	}
	if serveFailure != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: http server: %v\n", serveFailure)
	}
	if shutdown.close != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: close: %v\n", shutdown.close)
	}
	if shutdown.unmount != nil || serveFailure != nil || shutdown.close != nil {
		return 1
	}
	return 0
}

// httpShutdowner is the narrow API surface the shutdown sequence stops.
type httpShutdowner interface {
	Shutdown(context.Context) error
}

// sessionCloser is the narrow session surface the shutdown sequence closes.
type sessionCloser interface {
	Close(context.Context) error
}

// mountUnmounter is the narrow FUSE surface the shutdown sequence unmounts.
type mountUnmounter interface {
	Unmount() error
}

// defaultUnmountTimeout bounds the FUSE unmount stage. Unmount takes no
// context and no timeout, and it ends only when the FUSE connection reaches
// ENODEV. A peer mount namespace still holding a propagated copy of the mount
// keeps that connection alive, so Unmount would otherwise park in its
// event-loop Wait for as long as the peer lives.
const defaultUnmountTimeout = 30 * time.Second

// errUnmountTimeout reports that the unmount stage exceeded its deadline and
// was abandoned. The goroutine running Unmount is still parked in the
// event-loop Wait and cannot be cancelled; it ends only when the process does.
var errUnmountTimeout = errors.New("unmount did not complete")

// shutdownSequence stops a running torrentfs instance. The order is fixed and
// observable: stop the HTTP API, close the session, and only then unmount the
// FUSE server. Closing the session first cancels and drains outstanding reads,
// so Unmount does not block on FUSE requests still waiting for data.
type shutdownSequence struct {
	api   httpShutdowner
	sess  sessionCloser
	mount mountUnmounter

	// unmountTimeout overrides defaultUnmountTimeout; zero or negative uses
	// the default. It is the seam unit tests use to inject a short deadline.
	unmountTimeout time.Duration
}

// shutdownResult carries one error per stage so each can be reported
// separately; a failed stage never skips the stages after it.
type shutdownResult struct {
	api     error
	close   error
	unmount error
}

func (s shutdownSequence) run() shutdownResult {
	var result shutdownResult
	if s.api != nil {
		result.api = s.api.Shutdown(context.Background())
	}
	if s.sess != nil {
		result.close = s.sess.Close(context.Background())
	}
	if s.mount != nil {
		result.unmount = s.unmount()
	}
	return result
}

// unmount runs the mount stage under a bounded wait. Unmount has no context,
// so a deadline cannot cancel it: a peer mount namespace holding a propagated
// copy leaves the FUSE connection alive and the call blocked in its event-loop
// Wait. On expiry this returns a timeout error instead of hanging the shutdown
// forever, and the caller must let the process exit to release the goroutine.
func (s shutdownSequence) unmount() error {
	timeout := s.effectiveUnmountTimeout()
	done := make(chan error, 1)
	go func() { done <- s.mount.Unmount() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("unmount did not return within %s: another mount namespace may still hold a propagated copy of the FUSE mount; check `findmnt -T <mountpoint>` and `fuser -vm <mountpoint>` and see README: %w", timeout, errUnmountTimeout)
	}
}

func (s shutdownSequence) effectiveUnmountTimeout() time.Duration {
	if s.unmountTimeout > 0 {
		return s.unmountTimeout
	}
	return defaultUnmountTimeout
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

// Command torrentfs mounts BitTorrent downloads as a FUSE filesystem.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/logging"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const usageText = `torrentfs mounts BitTorrent downloads as a read-only FUSE filesystem and
serves a torrent management HTTP API.

Usage:
  torrentfs [-mountpoint <dir>] [-strm-mountpoint <dir>] [flags] <torrents-dir>

Flags:
  -mountpoint <dir>       original-file mount (required unless the HTTP API is enabled)
  -strm-mountpoint <dir>  optional STRM mount; requires strm.base_url and the HTTP API
  -config <file>          TOML configuration file
  -h, --help              show this help and exit

Configuration precedence is TORRENTFS_* environment variables, then the TOML
file, then built-in defaults. See the README for the complete environment
variable list.

<torrents-dir> must be an existing directory. Torrent tasks are created only
through the HTTP API; root .torrent files that are not registry-managed are
ignored. A single-file torrent is exposed under the mount point as a regular
file directly, e.g. <mount>/movie.mp4; a multi-file torrent is exposed as a
directory tree. The mount is data only and every path in it is read-only:
there is no metadata/ or stats/ control directory.

Managing torrents happens over the HTTP API: add by upload or magnet, list,
delete, and query per-torrent piece status. Canonical metainfo is stored only
at <torrents-dir>/<infohash>.torrent. The .metadata directory stores pending
magnet intents, registry state, peer identity, the layout marker, and the
instance lock; all of these paths are implementation details and are never
mounted.

The optional STRM mount replaces videos and ISO files with signed HTTP links;
other files retain their original contents. It can be mounted alone or beside
the original-file view. Its mountpoint and -mountpoint must not be the same or
nested, including symbolic-link aliases.

When http.listen_addr is set the management API is served; an enabled
http.auth configuration is required for non-loopback listeners. Omitting both
mountpoint flags then runs headless (HTTP only). Send SIGINT or SIGTERM to shut down.
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
		mountpoint     = flags.String("mountpoint", "", "directory to mount on")
		strmMountpoint = flags.String("strm-mountpoint", "", "directory for the STRM mount")
		configPath     = flags.String("config", "", "TOML configuration file")
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

	cfg, err := loadConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 2
	}

	*mountpoint, *strmMountpoint, err = validateMountpoints(*mountpoint, *strmMountpoint, cfg.STRM.BaseURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 2
	}
	httpEnabled := cfg.HTTP.ListenAddr != ""
	if *mountpoint == "" && *strmMountpoint == "" && !httpEnabled {
		flags.Usage()
		return 2
	}

	logger, _, err := logging.New(cfg.Log, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: initialize logging: %v\n", err)
		return 2
	}
	logLevel := cfg.Log.Level
	if logLevel == "" {
		logLevel = "info"
	}
	logFormat := cfg.Log.Format
	if logFormat == "" {
		logFormat = "text"
	}
	logger.Info("torrentfs starting",
		"version", "dev",
		"torrents_dir", torrentsDir,
		"cache_capacity_bytes", cfg.Cache.CapacityBytes,
		"mountpoint", *mountpoint,
		"strm_mountpoint", *strmMountpoint,
		"http_listen_addr", cfg.HTTP.ListenAddr,
		"listen_port", cfg.Connections.ListenPort,
		"mount_uid", os.Getuid(),
		"mount_gid", os.Getgid(),
		"mount_allow_other", cfg.Mount.AllowOther,
		"log_level", logLevel,
		"log_format", logFormat,
	)

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	sess, err := session.New(cfg, torrentsDir, session.WithLogger(logger))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		if errors.Is(err, config.ErrInvalid) {
			return 2
		}
		return 1
	}

	sequence := shutdownSequence{logger: logger, sess: sess}
	var apiServer *api.Server
	if httpEnabled {
		apiServer, err = api.New(cfg, sess, api.WithLogger(logger))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "torrentfs: initialize HTTP API: %v\n", err)
			sequence.run()
			if errors.Is(err, config.ErrInvalid) {
				return 2
			}
			return 1
		}
		sequence.api = apiServer
	}

	requests := []mountRequest{{mountpoint: *mountpoint, backend: sess}}
	if *strmMountpoint != "" {
		requests = append(requests, mountRequest{
			mountpoint: *strmMountpoint,
			backend:    filesystem.NewSTRMBackend(sess),
		})
	}
	sequence.mounts, err = mountFilesystems(requests, cfg.Mount.AllowOther,
		func(path string, backend filesystem.Backend, options *fs.Options) (mountUnmounter, error) {
			server, mountErr := filesystem.Mount(path, backend, options)
			if mountErr != nil {
				logger.Error("fuse mount failed", "mountpoint", path, "err", mountErr)
			}
			return server, mountErr
		})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		sequence.run()
		return 1
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

	shutdown := sequence.run()
	cancel()

	if shutdown.unmount != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", shutdown.unmount)
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

type mountRequest struct {
	mountpoint string
	backend    filesystem.Backend
}

type mountedFilesystem struct {
	mountpoint string
	server     mountUnmounter
}

// mountFilesystems keeps successful mounts even if a later mount fails, so the
// caller can clean them up after stopping HTTP and closing the session.
func mountFilesystems(requests []mountRequest, allowOther bool, mount func(string, filesystem.Backend, *fs.Options) (mountUnmounter, error)) ([]mountedFilesystem, error) {
	var mounts []mountedFilesystem
	for _, request := range requests {
		if request.mountpoint == "" {
			continue
		}
		// Mount modifies Options; each view needs its own options and inode space.
		options := &fs.Options{
			UID: uint32(os.Getuid()),
			GID: uint32(os.Getgid()),
		}
		options.MountOptions.AllowOther = allowOther
		server, err := mount(request.mountpoint, request.backend, options)
		if err != nil {
			return mounts, fmt.Errorf("mount %s: %w", request.mountpoint, err)
		}
		mounts = append(mounts, mountedFilesystem{mountpoint: request.mountpoint, server: server})
	}
	return mounts, nil
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
// FUSE servers. Closing the session first cancels and drains outstanding reads,
// so Unmount does not block on FUSE requests still waiting for data.
type shutdownSequence struct {
	logger *slog.Logger
	api    httpShutdowner
	sess   sessionCloser
	mounts []mountedFilesystem

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
	logger := s.logger
	if logger == nil {
		logger = logging.Discard()
	}
	logger.Info("shutting down")
	var result shutdownResult
	if s.api != nil {
		result.api = s.api.Shutdown(context.Background())
	}
	if s.sess != nil {
		result.close = s.sess.Close(context.Background())
	}
	result.unmount = s.unmount()

	var failures []error
	var failedStages []string
	if result.api != nil {
		failedStages = append(failedStages, "http-api")
		failures = append(failures, fmt.Errorf("http api: %w", result.api))
	}
	if result.close != nil {
		failedStages = append(failedStages, "session")
		failures = append(failures, fmt.Errorf("session: %w", result.close))
	}
	if result.unmount != nil {
		failedStages = append(failedStages, "fuse-unmount")
		failures = append(failures, fmt.Errorf("fuse unmount: %w", result.unmount))
	}
	if len(failures) == 0 {
		logger.Info("stopped")
	} else {
		logger.Error("shutdown incomplete", slog.Any("stages", failedStages), "err", errors.Join(failures...))
	}
	return result
}

// unmount starts every Unmount concurrently under one deadline. Unmount has no
// context: a peer namespace holding a propagated copy leaves the call blocked
// in its event-loop Wait. Expiry reports each pending path without waiting an
// additional timeout for the next mount; process exit releases those goroutines.
func (s shutdownSequence) unmount() error {
	if len(s.mounts) == 0 {
		return nil
	}
	type outcome struct {
		index int
		err   error
	}
	timeout := s.effectiveUnmountTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	done := make(chan outcome, len(s.mounts))
	pending := make(map[int]bool, len(s.mounts))
	failures := make([]error, len(s.mounts))
	for index, mount := range s.mounts {
		pending[index] = true
		go func() { done <- outcome{index: index, err: mount.server.Unmount()} }()
	}
	recordOutcome := func(completed outcome) {
		delete(pending, completed.index)
		if completed.err != nil {
			failures[completed.index] = fmt.Errorf("unmount %s: %w", s.mounts[completed.index].mountpoint, completed.err)
		}
	}
	for len(pending) != 0 {
		select {
		case completed := <-done:
			recordOutcome(completed)
		case <-timer.C:
			// Preserve results already queued when the shared deadline fired.
			for len(pending) != 0 {
				select {
				case completed := <-done:
					recordOutcome(completed)
				default:
					for index := range pending {
						failures[index] = fmt.Errorf("unmount %s did not return within %s: another mount namespace may still hold a propagated copy of the FUSE mount; check `findmnt -T <mountpoint>` and `fuser -vm <mountpoint>` and see README: %w", s.mounts[index].mountpoint, timeout, errUnmountTimeout)
					}
					return errors.Join(failures...)
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (s shutdownSequence) effectiveUnmountTimeout() time.Duration {
	if s.unmountTimeout > 0 {
		return s.unmountTimeout
	}
	return defaultUnmountTimeout
}

// validateMountpoints adds no checks to the original-file-only mode. STRM
// requires existing, canonical directories so aliases cannot hide either view.
func validateMountpoints(mountpoint, strmMountpoint, baseURL string) (string, string, error) {
	if strmMountpoint == "" {
		return mountpoint, strmMountpoint, nil
	}
	if baseURL == "" {
		return "", "", errors.New("strm.base_url is required with -strm-mountpoint")
	}
	canonical := func(name, path string) (string, error) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", name, path, err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", fmt.Errorf("%s %q must be an existing directory: %w", name, path, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", name, path, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s %q must be an existing directory", name, path)
		}
		return resolved, nil
	}
	strmPath, err := canonical("STRM mountpoint", strmMountpoint)
	if err != nil {
		return "", "", err
	}
	if mountpoint == "" {
		return "", strmPath, nil
	}
	filesPath, err := canonical("mountpoint", mountpoint)
	if err != nil {
		return "", "", err
	}
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
	}
	if contains(filesPath, strmPath) || contains(strmPath, filesPath) {
		return "", "", fmt.Errorf("mountpoints %q and %q must not be the same or nested", filesPath, strmPath)
	}
	return filesPath, strmPath, nil
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

func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if path == "" {
			return config.Config{}, fmt.Errorf("load config: %w", err)
		}
		return config.Config{}, fmt.Errorf("load config %q: %w", path, err)
	}
	return cfg, nil
}

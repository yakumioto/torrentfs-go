// Command torrentfs mounts BitTorrent downloads as a FUSE filesystem.
//
// M2 exposes the mounted torrent tree and a writable metadata/ control
// directory. Configuration loading from a TOML file lands in M4; for now paths
// are passed on the command line.
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

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const usageText = `torrentfs mounts BitTorrent downloads as a FUSE filesystem.

Usage:
  torrentfs -mountpoint <dir> [flags] <torrent-file>...

Flags:
  -mountpoint <dir>  directory to mount on (required)
  -data-dir <dir>    torrent session data directory
                     (default: <user cache dir>/torrentfs)
  -h, --help         show this help and exit

Each <torrent-file> is a .torrent file to expose under the mount point as a
directory. Complete .torrent files may also be written to metadata/ while
mounted. Send SIGINT or SIGTERM to unmount and exit.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run executes the CLI and returns the process exit code: 0 on success, 1 on
// runtime errors, 2 on usage errors.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("torrentfs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, usageText) }

	defaultDataDir := config.Default().Paths.DataDir
	var (
		mountpoint = flags.String("mountpoint", "", "directory to mount on")
		dataDir    = flags.String("data-dir", defaultDataDir, "torrent session data directory")
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *mountpoint == "" || flags.NArg() == 0 {
		flags.Usage()
		return 2
	}

	cfg := config.Default()
	cfg.Paths.DataDir = *dataDir

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	sess, err := session.New(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 1
	}
	for _, p := range flags.Args() {
		if err := sess.AddTorrent(rootCtx, session.Source{MetainfoPath: p}); err != nil {
			_, _ = fmt.Fprintf(stderr, "torrentfs: add torrent %s: %v\n", p, err)
			_ = sess.Close(context.Background())
			return 1
		}
	}

	server, err := filesystem.Mount(*mountpoint, sess, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: mount %s: %v\n", *mountpoint, err)
		_ = sess.Close(context.Background())
		return 1
	}

	<-signals
	unmountErr := server.Unmount()
	closeErr := sess.Close(rootCtx)
	cancel()
	if unmountErr != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: unmount %s: %v\n", *mountpoint, unmountErr)
	}
	if closeErr != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: close: %v\n", closeErr)
	}
	if unmountErr != nil || closeErr != nil {
		return 1
	}
	return 0
}

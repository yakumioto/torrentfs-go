// Command torrentfs mounts a BitTorrent download as a FUSE filesystem.
//
// M0 skeleton: only -h/--help works. No config loading or mounting is
// implemented yet.
package main

import (
	"errors"
	"flag"
	"io"
	"os"
)

const usageText = `torrentfs mounts a BitTorrent download as a FUSE filesystem.

Usage:
  torrentfs <config.toml>

Options:
  -h, --help  show this help and exit
`

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run returns the process exit code. Mounting is not implemented in this
// stage, so every invocation that is not -h/--help reports usage and exits 2
// rather than pretending a mount happened.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("torrentfs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, usageText) }

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// Mounting is not implemented in this stage, so a non-help invocation
	// reports usage and fails instead of silently succeeding.
	flags.Usage()
	return 2
}

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/lab"
)

const configUsageText = `torrentfs config works on the configuration alone.

Usage:
  torrentfs config check [--config <file>]
  torrentfs config print [--config <file>] [--format toml|json] [--sources]

Commands:
  check   Validate the configuration and report every problem. It prints
          nothing when the configuration is usable and exits with 2 otherwise.
  print   Print the effective configuration to standard output. A secret value
          is printed as <redacted>; the output is a report, not a
          configuration file.

Flags:
  --config <file>  TOML configuration file; the TORRENTFS_CONFIG variable is
                   used when the flag is absent
  --format <fmt>   The output format of print: toml (default) or json
  --sources        Name the source of every value: the built-in default, a
                   configuration file, or the environment

The check covers the configuration file, the environment, the unknown
variables, the value rules, and the rules of the upload statistics
experiment.
`

// runConfigCommand runs the configuration commands.
//
// The commands report on the configuration rather than starting a service, so
// they need no torrents directory and never open a listener. A successful
// report is data, so it goes to standard output; the diagnostics go to
// standard error.
func runConfigCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, configUsageText)
		return 2
	}
	switch args[0] {
	case "check":
		return runConfigCheck(args[1:], stderr)
	case "print":
		return runConfigPrint(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = io.WriteString(stderr, configUsageText)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "torrentfs config: unknown command %q\n", args[0])
	_, _ = io.WriteString(stderr, configUsageText)
	return 2
}

// runConfigCheck validates the configuration and reports every problem.
func runConfigCheck(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("torrentfs config check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "TOML configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "torrentfs config check: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	path := config.ResolveConfigPath(*configPath)
	if err := checkConfig(path); err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 2
	}
	return 0
}

// checkConfig validates the configuration the way a start would.
//
// The upload statistics experiment is checked too: its rules relate one
// setting to another, so a configuration that names the experiment wrongly
// is not usable even when every value has the right shape.
func checkConfig(path string) error {
	cfg, _, err := config.LoadWithSources(path)
	if err != nil {
		return err
	}
	if _, _, err := lab.SettingsFrom(cfg.Lab); err != nil {
		return fmt.Errorf("config: lab experiment: %w", err)
	}
	return cfg.ValidateOutput()
}

// runConfigPrint prints the effective configuration.
//
// The report is the result of the command, so it goes to standard output and
// can be redirected or piped. The command never prints a secret: the printed
// value of a secret key is the redacted placeholder.
func runConfigPrint(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("torrentfs config print", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "TOML configuration file")
	format := flags.String("format", "toml", "output format: toml or json")
	sources := flags.Bool("sources", false, "name the source of every value")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "torrentfs config print: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	path := config.ResolveConfigPath(*configPath)
	printed, err := config.Print(path, config.PrintOptions{Sources: *sources})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", err)
		return 2
	}
	switch *format {
	case "toml":
		_, _ = io.WriteString(stdout, printed.TOML())
	case "json":
		document, marshalErr := printed.JSON()
		if marshalErr != nil {
			_, _ = fmt.Fprintf(stderr, "torrentfs: %v\n", marshalErr)
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", document)
	default:
		_, _ = fmt.Fprintf(stderr, "torrentfs config print: unknown format %q (want toml or json)\n", *format)
		return 2
	}
	return 0
}

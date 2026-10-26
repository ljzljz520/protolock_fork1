package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nilslice/protolock"
)

const info = `Track your .proto files and prevent changes to messages and services which impact API compatibility.

Copyright Steve Manuel <nilslice@gmail.com>
Released under the BSD-3-Clause license.
`

const usage = `
Usage:
	protolock <command> [options]

Commands:
	-h, --help, help	display the usage information for protolock
	init			initialize a proto.lock file from current tree
	status			check for breaking changes and report conflicts
	commit			rewrite proto.lock file with current tree if no conflicts (--force to override)

Options:
	--strict [true]		enable strict mode and enforce all built-in rules
	--debug	[false]		enable debug mode and output debug messages
	--ignore 		comma-separated list of filepaths to ignore
	--force [false]		forces commit to rewrite proto.lock file and disregards warnings
	--plugins 		comma-separated list of executable protolock plugin names
	--lockdir [.]		directory of proto.lock file
	--protoroot [.]		root of directory tree containing proto files
	--uptodate [false]	enforce that proto.lock file is up-to-date with proto files
`

var (
	options   = flag.NewFlagSet("options", flag.ExitOnError)
	debug     = options.Bool("debug", false, "toggle debug mode for verbose output")
	strict    = options.Bool("strict", true, "enable strict mode and enforce all built-in rules")
	ignore    = options.String("ignore", "", "comma-separated list of filepaths to ignore")
	force     = options.Bool("force", false, "force commit to rewrite proto.lock file and disregard warnings")
	plugins   = options.String("plugins", "", "comma-separated list of executable protolock plugin names")
	lockDir   = options.String("lockdir", ".", "directory of proto.lock file")
	protoRoot = options.String("protoroot", ".", "root of directory tree containing proto files")
	upToDate  = options.Bool("uptodate", false, "enforce that proto.lock file is up-to-date with proto files")
)

func main() {
	// exit if no command (i.e. help, -h, --help, init, status, or commit)
	if len(os.Args) < 2 {
		fmt.Print(info + usage)
		os.Exit(0)
	}

	// parse and set options flags
	options.Parse(os.Args[2:])
	protolock.SetDebug(*debug)
	protolock.SetStrict(*strict)

	cfg, err := protolock.NewConfig(
		*lockDir,
		*protoRoot,
		*ignore,
		*upToDate,
		*debug,
	)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// switch through known commands
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)

	case "init":
		// InitToDisk captures one snapshot and publishes it exclusively and
		// atomically; a lock created by a concurrent process is never
		// overwritten.
		if err := protolock.InitToDisk(*cfg); err != nil {
			fmt.Println(logPrefix, "error:", err)
			os.Exit(1)
		}

	case "commit":
		// Rules and plugins validate the exact snapshot that gets serialized;
		// storage consistency is enforced even when --force is set.
		report, err := protolock.CommitToDisk(*cfg, protolock.CommitOptions{
			Force:   *force,
			Plugins: pluginValidator(),
		})
		if err == protolock.ErrWarningsFound {
			code, _ := protolock.HandleReport(report, os.Stdout, err)
			if code != 0 {
				os.Exit(code)
			}
			os.Exit(1)
		}
		if err != nil {
			fmt.Println(logPrefix, "error:", err)
			os.Exit(1)
		}

	case "status":
		status(cfg)

	default:
		os.Exit(0)
	}
}

func status(cfg *protolock.Config) {
	report, err := protolock.Status(*cfg)
	if err == protolock.ErrOutOfDate {
		fmt.Println(logPrefix, "error:", err, "run 'protolock commit'")
		// only exit if flag provided for backwards compatibility
		if cfg.UpToDate {
			os.Exit(2)
		}
		// don't report the error twice
		err = nil
	}
	if err != protolock.ErrWarningsFound && err != nil {
		fmt.Println(logPrefix, "error:", err)
		os.Exit(1)
	}
	// if plugins are provided, attempt to execute each as a executable
	// located in the user's OS executable path as reported by stdlib's
	// exec.LookPath func
	if *plugins != "" {
		report, err = runPlugins(*plugins, report, *debug)
		if err != nil {
			fmt.Println(logPrefix, "error:", err)
			os.Exit(1)
		}
	}

	code, err := protolock.HandleReport(report, os.Stdout, err)
	if err != protolock.ErrWarningsFound && err != nil {
		fmt.Println(logPrefix, "error:", err)
		os.Exit(1)
	}

	if code != 0 {
		os.Exit(code)
	}
}

// pluginValidator wires the external plugin runner into a library commit. The
// report's Current/Updated values come from the single snapshot captured by
// CommitToDisk, so plugins validate the exact bytes being published.
func pluginValidator() protolock.PluginValidator {
	if *plugins == "" {
		return nil
	}

	list := *plugins
	return func(report *protolock.Report) error {
		_, err := runPlugins(list, report, *debug)
		return err
	}
}

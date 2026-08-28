package cli

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
)

// Overridden at build time:
//
//	go build -ldflags "-X github.com/yourname/gecko/internal/cli.version=1.2.3 \
//	                   -X github.com/yourname/gecko/internal/cli.commit=abc1234 \
//	                   -X github.com/yourname/gecko/internal/cli.date=2026-01-01"
//
// -X only works on string variables in the main module's packages, and only
// on package-level vars, not constants.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Version resolves build metadata, preferring ldflags values and falling
// back to the VCS stamps the toolchain embeds automatically since Go 1.18.
func Version() (ver, rev, when string) {
	ver, rev, when = version, commit, date

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if ver == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		ver = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if rev == "" {
				rev = s.Value
			}
		case "vcs.time":
			if when == "" {
				when = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				ver += "-dirty"
			}
		}
	}
	return
}

func newVersionCommand() *Command {
	var short bool
	return &Command{
		Name:  "version",
		Short: "Print version information",
		Usage: "gecko version [--short]",
		Flags: func(fs *flag.FlagSet) { fs.BoolVar(&short, "short", false, "print only the version number") },
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			ver, rev, when := Version()
			if short {
				fmt.Fprintln(env.Stdout, ver)
				return nil
			}
			fmt.Fprintf(env.Stdout, "gecko %s (%s, %s/%s)\n",
				ver, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			if rev != "" {
				short := rev
				if len(short) > 12 {
					short = short[:12]
				}
				fmt.Fprintf(env.Stdout, "commit: %s\n", short)
			}
			if when != "" {
				fmt.Fprintf(env.Stdout, "built:  %s\n", when)
			}
			return nil
		},
	}
}

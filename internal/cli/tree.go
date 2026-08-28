package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CM-exe/gecko/internal/filesystem"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func newTreeCommand() *Command {
	var (
		depth    int
		all      bool
		dirsOnly bool
		showSize bool
		ascii    bool
		ignores  stringSlice
	)

	return &Command{
		Name:  "tree",
		Short: "Display a directory tree",
		Usage: "gecko tree [path] [flags]",
		Long: "Display the contents of a directory as a tree.\n" +
			"Hidden entries are excluded unless --all is given.",
		Flags: func(fs *flag.FlagSet) {
			fs.IntVar(&depth, "depth", 0, "maximum depth to descend (0 = unlimited)")
			fs.IntVar(&depth, "L", 0, "shorthand for --depth")
			fs.BoolVar(&all, "all", false, "include hidden entries")
			fs.BoolVar(&all, "a", false, "shorthand for --all")
			fs.BoolVar(&dirsOnly, "dirs-only", false, "list directories only")
			fs.BoolVar(&showSize, "size", false, "show file sizes")
			fs.BoolVar(&ascii, "ascii", false, "use ASCII drawing characters")
			fs.Var(&ignores, "ignore", "exclude matching entries (repeatable)")
		},
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			cfg, err := env.Config()
			if err != nil {
				return err
			}

			opts := filesystem.TreeOptions{
				MaxDepth: cfg.Tree.MaxDepth,
				Ignore:   cfg.Tree.Ignore,
			}

			// Flags win over config but only if actually set.
			if inv.Provided["depth"] || inv.Provided["L"] {
				opts.MaxDepth = depth
			}
			if inv.Provided["ignore"] {
				opts.Ignore = ignores
			}

			target := "."
			if len(inv.Args) > 0 {
				target = inv.Args[0]
			}
			if len(inv.Args) > 1 {
				fmt.Fprintf(env.Stderr, "gecko tree: too many arguments\n")
				return Quiet(ErrUsage)
			}

			// Resolve relative to the injected working directory, not the
			// process's, so tests can run from anywhere.
			if !filepath.IsAbs(target) {
				target = filepath.Join(env.WorkDir, target)
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return err
			}

			// os.DirFS roots the FS at the target, so every path inside is
			// "." or below. This is also our path-traversal boundary.
			fsys := os.DirFS(abs)

			stats, err := filesystem.WriteTree(ctx, env.Stdout, fsys, ".", filesystem.TreeOptions{
				MaxDepth: opts.MaxDepth,
				ShowAll:  all,
				DirsOnly: dirsOnly,
				ShowSize: showSize,
				ASCII:    ascii,
				Ignore:   opts.Ignore,
			})
			if err != nil {
				return fmt.Errorf("tree %s: %w", target, err)
			}

			fmt.Fprintf(env.Stdout, "\n%d directories, %d files", stats.Dirs, stats.Files)
			if showSize {
				fmt.Fprintf(env.Stdout, ", %s", filesystem.HumanSize(stats.TotalSize))
			}
			fmt.Fprintln(env.Stdout)
			return nil
		},
	}
}

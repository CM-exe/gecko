package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/CM-exe/gecko/internal/filesystem"
)

func newFindCommand() *Command {
	var (
		namePattern    string
		typeFilter     int
		minSize        int64
		maxSize        int64
		modifiedAfter  string
		modifiedBefore string
		includeHidden  bool
		ignore         string
		maxDepth       int
		followSymlinks bool
		caseSensitive  bool
		workers        int
	)

	return &Command{
		Name:  "find",
		Short: "Find files in a directory tree",
		Usage: "gecko find [flags] [path]",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&namePattern, "name", "", "match files by name pattern")
			fs.IntVar(&typeFilter, "type", 0, "filter by file type")
			fs.Int64Var(&minSize, "min-size", 0, "minimum file size in bytes; 0 = no bound")
			fs.Int64Var(&maxSize, "max-size", 0, "maximum file size in bytes; 0 = no bound")
			fs.StringVar(&modifiedAfter, "modified-after", "", "only files modified after this RFC3339 timestamp")
			fs.StringVar(&modifiedBefore, "modified-before", "", "only files modified before this RFC3339 timestamp")
			fs.BoolVar(&includeHidden, "include-hidden", false, "include hidden files and directories")
			fs.StringVar(&ignore, "ignore", "", "comma-separated directory names to prune entirely")
			fs.IntVar(&maxDepth, "max-depth", 0, "maximum directory depth to traverse; 0 = unlimited")
			fs.BoolVar(&followSymlinks, "follow-symlinks", false, "follow symlinks while searching")
			fs.BoolVar(&caseSensitive, "case-sensitive", true, "use case-sensitive matching for the name pattern")
			fs.IntVar(&workers, "workers", 0, "number of workers; 0 = auto")
		},
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			root := "."
			if len(inv.Args) > 0 {
				root = inv.Args[0]
			}
			if len(inv.Args) > 1 {
				return Quiet(ErrUsage)
			}

			var after time.Time
			if modifiedAfter != "" {
				v, err := time.Parse(time.RFC3339, modifiedAfter)
				if err != nil {
					return Quiet(fmt.Errorf("invalid --modified-after value %q: %w", modifiedAfter, err))
				}
				after = v
			}

			var before time.Time
			if modifiedBefore != "" {
				v, err := time.Parse(time.RFC3339, modifiedBefore)
				if err != nil {
					return Quiet(fmt.Errorf("invalid --modified-before value %q: %w", modifiedBefore, err))
				}
				before = v
			}

			var ignored []string
			if ignore != "" {
				for _, part := range strings.Split(ignore, ",") {
					part = strings.TrimSpace(part)
					if part != "" {
						ignored = append(ignored, part)
					}
				}
			}

			opts := filesystem.FindOptions{
				Pattern:        namePattern,
				Type:           filesystem.EntryType(typeFilter),
				MinSize:        minSize,
				MaxSize:        maxSize,
				ModifiedAfter:  after,
				ModifiedBefore: before,
				IncludeHidden:  includeHidden,
				Ignore:         ignored,
				MaxDepth:       maxDepth,
				FollowSymlinks: followSymlinks,
				CaseSensitive:  caseSensitive,
				Workers:        workers,
			}
			return runFind(root, opts, env.Stdout)
		},
	}
}

// runFind is the CLI wrapper around filesystem.Find.
//
// The producer writes matches to a channel that is consumed by a dedicated
// goroutine. If the consumer exits early (for example because stdout is piped to
// `head` and the pipe closes), we cancel the find context so the producer does
// not block forever when trying to send a result.
func runFind(root string, opts filesystem.FindOptions, w io.Writer) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	matches := make(chan filesystem.Match)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer cancel() // stop producer as soon as the consumer exits early

		for m := range matches {
			if _, err := fmt.Fprintf(w, "%s\n", m.RelPath); err != nil {
				return
			}
		}
	}()

	_, err := filesystem.Find(ctx, root, opts, matches)
	<-done

	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

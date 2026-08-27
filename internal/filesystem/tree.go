package filesystem

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// TreeOptions configures WriteTree. The zero value is a sensible default:
// unlimited depth, hidden files excluded, no sizes.
type TreeOptions struct {
	MaxDepth int  // 0 means unlimited
	ShowAll  bool // include entries whose name starts with "."
	DirsOnly bool
	ShowSize bool
	ASCII    bool
	Ignore   []string // exact directory names to skip entirely
}

// TreeStats summarises a completed walk.
type TreeStats struct {
	Dirs      int
	Files     int
	TotalSize int64
}

// glyphs holds the drawing characters for one rendering style.
type glyphs struct {
	tee    string // non-last child connector
	corner string // last child connector
	pipe   string // continuation under a non-last ancestor
	blank  string // continuation under a last ancestor
}

var (
	unicodeGlyphs = glyphs{"├── ", "└── ", "│   ", "    "}
	asciiGlyphs   = glyphs{"|-- ", "`-- ", "|   ", "    "}
)

// WriteTree renders the directory tree rooted at root within fsys.
//
// root must be a valid io/fs path: slash-separated, relative, with "."
// meaning the root of fsys.
func WriteTree(ctx context.Context, w io.Writer, fsys fs.FS, root string, opts TreeOptions) (TreeStats, error) {
	if !fs.ValidPath(root) {
		return TreeStats{}, fmt.Errorf("invalid path %q", root)
	}

	info, err := fs.Stat(fsys, root)
	if err != nil {
		return TreeStats{}, err
	}
	if !info.IsDir() {
		return TreeStats{}, fmt.Errorf("%s: not a directory", root)
	}

	bw := bufio.NewWriterSize(w, 64*1024)

	t := &treeWriter{
		w:      bw,
		fsys:   fsys,
		opts:   opts,
		glyphs: unicodeGlyphs,
	}
	if opts.ASCII {
		t.glyphs = asciiGlyphs
	}

	displayRoot := root
	if displayRoot == "." {
		displayRoot = "."
	}
	fmt.Fprintf(bw, "%s\n", displayRoot)

	err = t.walk(ctx, root, "", 1)

	// Flush even on error: partial output is more useful than none, and
	// the caller sees the error separately.
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	return t.stats, err
}

type treeWriter struct {
	w      *bufio.Writer
	fsys   fs.FS
	opts   TreeOptions
	glyphs glyphs
	stats  TreeStats
}

// walk renders the children of dir. prefix is the accumulated indentation
// string for this level; depth is 1-based.
func (t *treeWriter) walk(ctx context.Context, dir, prefix string, depth int) error {
	// Cancellation is checked once per directory rather than per entry:
	// select on a closed channel is cheap but not free, and directory
	// granularity is responsive enough for a human at a terminal.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if t.opts.MaxDepth > 0 && depth > t.opts.MaxDepth {
		return nil
	}

	entries, err := fs.ReadDir(t.fsys, dir)
	if err != nil {
		// An unreadable directory is a normal condition (permissions),
		// not a fatal one. Report it inline and continue.
		fmt.Fprintf(t.w, "%s%s[error: %v]\n", prefix, t.glyphs.corner, err)
		return nil
	}

	entries = t.filter(entries)

	// fs.ReadDir already sorts by filename, but directories-first reads
	// better, so re-sort with a compound key.
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})

	for i, e := range entries {
		last := i == len(entries)-1

		connector := t.glyphs.tee
		childPrefix := prefix + t.glyphs.pipe
		if last {
			connector = t.glyphs.corner
			childPrefix = prefix + t.glyphs.blank
		}

		name := e.Name()
		if e.IsDir() {
			name += "/"
			t.stats.Dirs++
		} else {
			t.stats.Files++
		}

		fmt.Fprintf(t.w, "%s%s%s", prefix, connector, name)

		if t.opts.ShowSize && !e.IsDir() {
			// Info() may cost a stat syscall; only pay it when asked.
			if info, err := e.Info(); err == nil {
				t.stats.TotalSize += info.Size()
				fmt.Fprintf(t.w, "  %s", HumanSize(info.Size()))
			}
		}
		fmt.Fprintln(t.w)

		if e.IsDir() {
			if err := t.walk(ctx, path.Join(dir, e.Name()), childPrefix, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *treeWriter) filter(entries []fs.DirEntry) []fs.DirEntry {
	out := entries[:0] // reuse the backing array: zero allocations
	for _, e := range entries {
		name := e.Name()
		if !t.opts.ShowAll && strings.HasPrefix(name, ".") {
			continue
		}
		if t.opts.DirsOnly && !e.IsDir() {
			continue
		}
		if isIngored(name, t.opts.Ignore) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func isIngored(name string, ignores []string) bool {
	for _, ignore := range ignores {
		if name == ignore {
			return true
		}
		// Try to convert into regex
		regex, err := path.Match(ignore, name)
		if err == nil && regex {
			return true
		}
	}
	return false
}

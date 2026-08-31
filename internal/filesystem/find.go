package filesystem

import (
	"context"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// EntryType restricts matches to files, directories or both.
type EntryType int

const (
	AnyType EntryType = iota
	FileType
	DirType
)

// FindOptions configures a search. The zero value matches everything.
type FindOptions struct {
	Pattern        string // glob against the base name; empty matches all
	Type           EntryType
	MinSize        int64     // bytes; 0 = no bound
	MaxSize        int64     // bytes; 0 = no bound
	ModifiedAfter  time.Time // zero = no bound
	ModifiedBefore time.Time
	IncludeHidden  bool
	Ignore         []string // directory names pruned entirely
	MaxDepth       int
	FollowSymlinks bool
	CaseSensitive  bool
	Workers        int // 0 = auto
}

// needsStat reports whether any configured filter requires file metadata
// beyond what fs.DirEntry provides for free. If false, the search runs
// entirely single-threaded with no extra syscalls, which is both faster
// and simpler than a worker pool would be.
func (o *FindOptions) needsStat() bool {
	return o.MinSize > 0 || o.MaxSize > 0 ||
		!o.ModifiedAfter.IsZero() || !o.ModifiedBefore.IsZero()
}

// Match is one search result.
type Match struct {
	Path    string
	RelPath string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// FindStats reports what a search did.
type FindStats struct {
	Scanned int64
	Matched int64
	Errors  int64
	Elapsed time.Duration
}

// Find walks root and sends matches to out, which it closes on return.
//
// The walk itself is sequential: filepath.WalkDir is inherently ordered
// and parallel directory traversal buys little on real hardware. When
// options require stat(2) per candidate, that work is fanned out to a
// bounded pool; when they do not, no goroutines are created at all.
func Find(ctx context.Context, root string, opts FindOptions, out chan<- Match) (FindStats, error) {
	defer close(out)

	start := time.Now()
	var stats FindStats
	var scanned, matched, errCount atomic.Int64

	workers := opts.Workers
	if workers <= 0 {
		// I/O-bound work benefits from more goroutines than cores,
		// because a goroutine blocked in a syscall releases its P.
		// 4x cores, capped, is a defensible default; see
		// BenchmarkFindWorkers for the measurements behind it.
		workers = runtime.NumCPU() * 4
		if workers > 64 {
			workers = 64
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	if opts.needsStat() {
		g.SetLimit(workers)
	} else {
		g.SetLimit(1)
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return stats, err
	}

	walkErr := (&Walker{
		Root:          rootAbs,
		IncludeHidden: opts.IncludeHidden,
		Ignore:        opts.Ignore,
		MaxDepth:      opts.MaxDepth,
		OnError: func(path string, err error) {
			// Unreadable directory or vanished file. Count it and keep
			// going: a permission-denied subdirectory must not abort a
			// scan of the other 40,000.
			errCount.Add(1)
		},
	}).Walk(gctx, func(path string, d fs.DirEntry) error {
		scanned.Add(1)
		name := d.Name()

		if !matchesType(d, opts.Type) {
			return nil
		}
		if !matchesName(name, opts.Pattern, opts.CaseSensitive) {
			return nil
		}

		// Cheap filters passed. If nothing else needs metadata, emit now.
		if !opts.needsStat() {
			matched.Add(1)
			select {
			case out <- Match{Path: path, RelPath: relOf(rootAbs, path), IsDir: d.IsDir()}:
			case <-gctx.Done():
				return gctx.Err()
			}
			return nil
		}

		// Otherwise fan the stat out. g.Go blocks once the limit is
		// reached, which applies backpressure to the walk itself —
		// exactly what we want, since an unbounded queue of pending
		// entries would grow to the size of the filesystem.
		entry := d
		p := path
		g.Go(func() error {
			info, err := entry.Info()
			if err != nil {
				errCount.Add(1)
				return nil // vanished between readdir and stat; not fatal
			}
			if !matchesSize(info.Size(), opts) || !matchesTime(info.ModTime(), opts) {
				return nil
			}
			matched.Add(1)
			select {
			case out <- Match{
				Path: p, RelPath: relOf(rootAbs, p), IsDir: entry.IsDir(),
				Size: info.Size(), ModTime: info.ModTime(),
			}:
				return nil
			case <-gctx.Done():
				return gctx.Err()
			}
		})
		return nil
	})

	waitErr := g.Wait()

	stats = FindStats{
		Scanned: scanned.Load(),
		Matched: matched.Load(),
		Errors:  errCount.Load(),
		Elapsed: time.Since(start),
	}

	// A cancellation surfaces from whichever path noticed first; prefer
	// the walk's error since it is closer to the cause.
	if walkErr != nil {
		return stats, walkErr
	}
	return stats, waitErr
}

func matchesType(d fs.DirEntry, t EntryType) bool {
	switch t {
	case FileType:
		return !d.IsDir()
	case DirType:
		return d.IsDir()
	default:
		return true
	}
}

func matchesName(name, pattern string, caseSensitive bool) bool {
	if pattern == "" {
		return true
	}
	if !caseSensitive {
		// Note: ToLower is not full Unicode case folding. Adequate for
		// filename globs; a correct implementation would need
		// golang.org/x/text/cases.
		name = strings.ToLower(name)
		pattern = strings.ToLower(pattern)
	}
	ok, err := filepath.Match(pattern, name)
	return err == nil && ok
}

func matchesSize(size int64, o FindOptions) bool {
	if o.MinSize > 0 && size < o.MinSize {
		return false
	}
	if o.MaxSize > 0 && size > o.MaxSize {
		return false
	}
	return true
}

func matchesTime(mt time.Time, o FindOptions) bool {
	if !o.ModifiedAfter.IsZero() && mt.Before(o.ModifiedAfter) {
		return false
	}
	if !o.ModifiedBefore.IsZero() && mt.After(o.ModifiedBefore) {
		return false
	}
	return true
}

func relOf(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

# Chapter 5 — `gecko find` and `gecko clean`: Concurrency and Destructive Operations

```
Difficulty:   Advanced
Est. time:    8–10 hours
Main concepts: filepath.WalkDir, bounded worker pools, the GMP scheduler, errgroup,
               context propagation, sync.Mutex vs channels, the race detector,
               path containment, confirmation UX, os.RemoveAll semantics
Prerequisites: Chapters 1–4
```

---

## A. Goal

```
$ gecko find "*.go" --max-size 10KB
internal/cli/help.go
internal/config/paths.go
...
42 matches in 1.2s (18,392 entries scanned)

$ gecko find --name "*.log" --older-than 30d --json

$ gecko clean
Scanning /home/you/projects...

  node_modules      12 dirs    2.4 GB
  target             3 dirs    812 MB
  .pytest_cache      8 dirs    391 MB
  dist               5 dirs    102 MB

Potentially recoverable: 3.7 GB

Delete these 28 directories? [y/N]
```

---

## B. Why this matters

Two very different lessons.

`find` is where concurrency becomes justified rather than decorative. A filesystem walk
is I/O-bound, matching is CPU-bound, and `stat`-ing for size or mtime is I/O-bound again.
That's a genuine pipeline, and it's the right place to learn why `go func` per directory
is a bug and a bounded pool is not.

`clean` deletes user data. It's the highest-stakes code in the entire project, and the
lesson is that **the hard part is not deletion, it's the design that makes an accidental
catastrophic deletion structurally impossible.** Most of this chapter's `clean` section
is about constraints, not code.

---

## C. Concepts

### The Go scheduler, concretely

You've used goroutines. Here's what actually happens, because the next section's design
depends on it.

Go's runtime implements an **M:N scheduler** with three entities:

- **G** — a goroutine. A stack (starting at 8 KB, growable), a program counter, and
  scheduling state. Cheap: allocation is roughly 2–4 µs and a few KB.
- **M** — an OS thread (machine). Expensive. Created by the runtime as needed.
- **P** — a processor: a scheduling context holding a local run queue of Gs. There are
  exactly `GOMAXPROCS` of them, defaulting to `runtime.NumCPU()`.

An M must hold a P to run Go code. The P's local run queue (256 entries) is drained
without locks; when empty, the P **steals** half the work from a random other P's queue,
or pulls from the global queue. Work stealing is what makes goroutine scheduling scale.

The critical part for us: **what happens on a blocking syscall.** When a goroutine makes
a blocking syscall (`read`, `openat`, `stat`), the runtime's `sysmon` thread notices the M
has been in the syscall for more than ~20 µs, detaches the P from that M, and hands the P
to another M so the other goroutines keep running. When the syscall returns, the original
M tries to reacquire a P; if it can't, its goroutine goes on the global queue and the M
parks.

Consequences that matter for `find`:

1. **Blocking on file I/O does not block other goroutines.** So a pool of 8 goroutines
   doing `stat` gets real parallelism even though `stat` is synchronous.
2. **But it does consume OS threads.** Each blocked syscall pins an M. Ten thousand
   concurrent `stat` calls means the runtime creates threads toward the 10,000 limit
   (`runtime/debug.SetMaxThreads`). Thread creation is ~10–100 µs and each costs ~8 KB
   of kernel stack plus scheduler pressure. This is the concrete reason unbounded
   goroutine spawning over a filesystem walk is a bug and not merely inelegant.
3. **`GOMAXPROCS` bounds CPU parallelism, not syscall parallelism.** For I/O-bound work
   the useful pool size is often larger than `NumCPU()`. For CPU-bound matching it is not.
   Our pipeline has both, which is why we'll benchmark rather than guess.

Preemption footnote: since Go 1.14 goroutines are **asynchronously preemptible** via
signals, so a tight loop with no function calls no longer starves its P. Before 1.14 it
did, which is why old advice about `runtime.Gosched()` exists and no longer applies.

### `filepath.WalkDir` vs `fs.WalkDir` vs `godirwalk`

`filepath.Walk` (old) calls `os.Lstat` on every entry — one extra syscall per file.
`filepath.WalkDir` (Go 1.16+) passes `fs.DirEntry`, so type checks are free and `stat` is
only paid when you ask. **Always use `WalkDir`.**

The callback signature is subtle:

```go
func(path string, d fs.DirEntry, err error) error
```

- `err != nil` means the entry could not be read. `d` may be non-nil.
- Returning `fs.SkipDir` from a directory's callback skips its contents. Returning it
  from a *file's* callback skips the rest of that file's parent directory — a genuinely
  surprising behaviour that's occasionally useful.
- Returning `fs.SkipAll` (Go 1.20+) stops the whole walk without an error.
- Returning any other non-nil error aborts the walk and `WalkDir` returns it.

Walk order is lexical and deterministic, which means it's also **single-threaded by
construction**. You cannot parallelise `WalkDir` itself; you parallelise what you do with
each entry.

### `errgroup`

```bash
go get golang.org/x/sync/errgroup
```

Dependency justification: `errgroup` is ~130 lines wrapping `sync.WaitGroup` + `context`.
We could write it. We won't, because it's maintained by the Go team under
`golang.org/x/`, it's the de facto standard, and the semantics (first error wins, context
cancelled on first error) are subtle enough that a home-grown version would be a liability.
It has one dependency, `golang.org/x/sync`, with no transitive deps.

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(8)              // bounded concurrency, Go 1.20+
for _, item := range items {
    item := item           // pre-Go 1.22 loop-variable capture
    g.Go(func() error {
        return process(ctx, item)
    })
}
err := g.Wait()            // first non-nil error, or nil
```

`errgroup.WithContext` returns a derived context that is **cancelled as soon as any
goroutine returns an error**. That's the propagation mechanism: the failing goroutine
doesn't have to tell the others; the context does.

`g.SetLimit(n)` makes `g.Go` block when n goroutines are already running. This is the
bounded-pool primitive and it removes the need for a semaphore channel in most cases.

Go 1.22 changed loop-variable semantics so each iteration gets a fresh variable; the
`item := item` line is a no-op there. Keep it if you support older toolchains, drop it
if your `go.mod` says `go 1.22` or later — and note that `go vet`'s `loopclosure` check
knows the difference.

### Channels vs mutexes for collecting results

Two ways to accumulate matches from N workers:

**Channel:** workers send to `results chan Match`; a single collector goroutine ranges
over it. Idiomatic, composable, and gives you streaming (results appear as found).
Costs a goroutine and channel send overhead (~50–100 ns per send under contention).

**Mutex:** workers append to a shared slice under `sync.Mutex`. Simpler to read, no extra
goroutine, but no streaming and lock contention scales badly past ~8 writers.

"Share memory by communicating" is the slogan, but the honest guidance is: **use a channel
when the data flows, use a mutex when the data is state.** Search results flow — we want
them printed as they're found, not after the walk completes. Channel.

Counters (`entriesScanned`) are state, and `sync/atomic` beats both:

```go
var scanned atomic.Int64   // Go 1.19+ typed atomics
scanned.Add(1)
```

`atomic.Int64` (not `atomic.AddInt64` on a raw `int64`) is the modern form: it's a struct
that can't be accidentally copied or accessed non-atomically, and it guarantees 64-bit
alignment on 32-bit platforms, which raw `int64` fields do not.

### The race detector

```bash
go test -race ./...
go build -race ./cmd/gecko   # for manual testing
```

It instruments every memory access and maintains a vector clock per goroutine, detecting
unsynchronised access to the same address where at least one access is a write. Costs
5–15× CPU and 5–10× memory, so it's a test-time tool.

Critical limitation: **it only detects races that actually occur during the run.** A race
on a code path your test doesn't exercise is invisible. So `-race` plus good coverage,
not `-race` alone. Run it in CI on every PR.

### `os.RemoveAll` semantics

- Removes recursively. Returns `nil` if the path doesn't exist (not an error).
- **Follows nothing:** it removes symlinks themselves, not their targets. Good.
- On Windows it fails on files held open by another process, and on read-only files
  (it does not clear the read-only attribute). Expect partial deletion.
- It is **not atomic**. A failure halfway leaves a partially deleted tree.

That last point drives a design decision in `clean`: we delete one target at a time and
report per-target status, rather than pretending the operation is transactional.

---

## D. Design

### `find`: the pipeline

Three stages with different characteristics:

```
walk (1 goroutine, I/O-bound, order-dependent)
   → filter by name/type (CPU-bound, cheap, no syscall)
      → stat for size/mtime (N goroutines, I/O-bound)  [only if needed]
         → emit (1 goroutine)
```

**Design question, answer before reading on:** where does the concurrency go, and how
many goroutines?

The naive answer is "walk in parallel". You can't — `WalkDir` is sequential and
parallelising directory traversal properly requires your own work queue, which is
possible but has poor returns because directory reads on a single spinning disk or even
an NVMe queue are already near the device's concurrency limit at low parallelism.

The correct answer: **the walk stays sequential; the expensive per-entry work goes into a
bounded pool.** And critically — **if no flag requires a `stat`, there is no pool at all.**
`gecko find "*.go"` matches on name only, needs zero extra syscalls, and running it
through a worker pool would be pure overhead.

That conditional is the whole lesson: *"use concurrency where it makes sense, not because
Go makes it easy."*

### `find`: matching

Name matching uses `filepath.Match` (glob: `*`, `?`, `[...]`, no `**`). Case sensitivity
should follow the platform by default — Windows and default macOS filesystems are
case-insensitive.

```go
if !caseSensitive {
    name = strings.ToLower(name)
    pattern = strings.ToLower(pattern)
}
```

`strings.ToLower` is not correct Unicode case folding (Turkish dotless ı, for one), but
`strings.EqualFold` doesn't work with globs. Note the limitation in a comment and move on;
correctness here would require a `golang.org/x/text` dependency for a marginal case.

Size specs (`>10MB`, `<1KB`, `10MB..1GB`) need a small parser. Duration specs (`30d`,
`2w`) too, because `time.ParseDuration` maxes out at hours and doesn't accept `d`.

### `clean`: the safety architecture

This is the important part of the chapter. Read it twice.

**Constraint 1: an allowlist, never a pattern.** `clean` recognises a fixed, compiled-in
set of directory names with known provenance:

```go
var cleanTargets = []Target{
    {Name: "node_modules", Marker: "package.json", Desc: "npm dependencies"},
    {Name: "target",       Marker: "Cargo.toml",   Desc: "Rust build output"},
    {Name: "dist",         Marker: "package.json", Desc: "JS build output"},
    ...
}
```

Note `Marker`. A directory called `target` is only a Rust build directory **if its parent
contains `Cargo.toml`**. Without that check, `gecko clean` in a photographer's folder
deletes `~/photos/target/`. The marker requirement turns a name match into evidence.

There is no `--pattern` flag. There will never be a `--pattern` flag. A user who wants to
delete arbitrary matches has `find` piped to `xargs rm`, where the danger is visible.

**Constraint 2: containment.** Every candidate must be provably inside the scan root:

```go
func contained(root, candidate string) (bool, error) {
    rootAbs, err := filepath.Abs(root)
    if err != nil { return false, err }
    rootAbs, err = filepath.EvalSymlinks(rootAbs)
    if err != nil { return false, err }

    candAbs, err := filepath.Abs(candidate)
    if err != nil { return false, err }
    candAbs, err = filepath.EvalSymlinks(candAbs)
    if err != nil { return false, err }

    rel, err := filepath.Rel(rootAbs, candAbs)
    if err != nil { return false, err }
    return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}
```

`EvalSymlinks` on both sides is what makes this real. Without it, a symlink
`./node_modules -> /usr/lib` passes a string-prefix check and destroys your system.
`filepath.Rel` after `EvalSymlinks` is the correct containment test; a raw
`strings.HasPrefix(cand, root)` is not (it accepts `/home/user2` for root `/home/user`).

**Constraint 3: refuse dangerous roots.**

```go
var forbiddenRoots = map[string]bool{"/": true, "/home": true, "/Users": true,
    "/usr": true, "/etc": true, "/var": true, "C:\\": true}

// Also refuse the user's home directory itself, and any root fewer than
// two path components deep.
```

Scanning `$HOME` recursively for `node_modules` is a legitimate use case, but it's also
how someone deletes a `node_modules` they were about to ship. Require `--force` for it.

**Constraint 4: dry run is the default.** `gecko clean` shows and asks. `gecko clean
--dry-run` shows and exits. Actual deletion requires either an interactive `y` or an
explicit `--yes`. And `--yes` is refused when stdin is not a TTY unless
`--i-know-what-im-doing`... no. That's cargo-culted paranoia. `--yes` means yes; the
guard rails are the allowlist and containment, not the prompt.

**Constraint 5: no shell, ever.** Deletion uses `os.RemoveAll` with a path. There is no
point at which a user-supplied string becomes part of a command line. This eliminates
command injection as a category, not as a bug to be patched.

Compare with the wrong implementation:

```go
// NEVER DO THIS
exec.Command("sh", "-c", "rm -rf "+path).Run()
```

A directory named `foo; rm -rf ~` is a valid directory name on Linux. `os/exec` with
separate arguments (`exec.Command("rm", "-rf", path)`) is already safe because it uses
`execve` directly with an argv array and no shell — but `os.RemoveAll` is safer still,
needing no subprocess at all.

### Size computation

Reporting "2.4 GB" requires walking each candidate directory and summing sizes. That's
expensive — `node_modules` can hold 200,000 files. This is the second genuine use for the
worker pool: size several candidate directories concurrently.

Subtlety: `info.Size()` is the apparent size (bytes in the file), not the disk usage
(blocks allocated). `du` reports the latter. For sparse files and small files on a 4 KB
block filesystem they differ substantially. We report apparent size and say so, because
disk usage requires `syscall.Stat_t.Blocks` on Unix with no Windows equivalent — a
portability cost not worth paying for a cleanup estimate.

---

## E. Implementation

### `internal/filesystem/find.go`

```go
package filesystem

import (
	"context"
	"fmt"
	"io/fs"
	"os"
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
	Pattern       string    // glob against the base name; empty matches all
	Type          EntryType
	MinSize       int64     // bytes; 0 = no bound
	MaxSize       int64     // bytes; 0 = no bound
	ModifiedAfter time.Time // zero = no bound
	ModifiedBefore time.Time
	IncludeHidden bool
	Ignore        []string // directory names pruned entirely
	MaxDepth      int
	FollowSymlinks bool
	CaseSensitive bool
	Workers       int // 0 = auto
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

	walkErr := filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, err error) error {
		// Cancellation: checked per entry because WalkDir gives us no
		// other hook, and the check is a few nanoseconds.
		select {
		case <-gctx.Done():
			return gctx.Err()
		default:
		}

		if err != nil {
			// Unreadable directory or vanished file. Count it and keep
			// going: a permission-denied subdirectory must not abort a
			// scan of the other 40,000.
			errCount.Add(1)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		scanned.Add(1)
		name := d.Name()

		if d.IsDir() && path != rootAbs {
			if !opts.IncludeHidden && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if containsString(opts.Ignore, name) {
				return fs.SkipDir
			}
			if opts.MaxDepth > 0 && depthOf(rootAbs, path) >= opts.MaxDepth {
				return fs.SkipDir
			}
		}

		if !opts.IncludeHidden && strings.HasPrefix(name, ".") && path != rootAbs {
			return nil
		}
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

func depthOf(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

func relOf(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
```

Read the `g.Go` block carefully. There are three non-obvious decisions:

1. **`g.SetLimit` provides backpressure to the walk.** When 64 stats are in flight,
   `g.Go` blocks, which blocks the `WalkDir` callback, which stops the walk. Without
   this, a walk over a million files queues a million goroutines before the first one
   finishes. This is the single most important line in the function.

2. **A failed `Info()` returns `nil`, not the error.** With `errgroup`, returning an error
   cancels the whole group. A file that vanished between `readdir` and `stat` is normal
   (it happens constantly in `/proc`, in build directories, in anything active) and must
   not abort the search. Only cancellation is fatal.

3. **Every channel send is in a `select` with `ctx.Done()`.** A bare `out <- match` when
   the consumer has stopped reading deadlocks the goroutine forever — a goroutine leak
   that `go test` won't catch and production will. **Every send on a channel you don't
   control must be selectable.**

### Size and duration parsing — `internal/filesystem/spec.go`

```go
package filesystem

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseSize parses a human size such as "10MB", "1.5GiB" or "512".
// Both decimal (KB=1000) and binary (KiB=1024) prefixes are accepted;
// a bare "KB" is treated as 1024 because that is what developers mean,
// even though it is technically wrong.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("size %q: no leading number", s)
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	unit := strings.ToUpper(strings.TrimSpace(s[i:]))

	var mult float64 = 1
	switch unit {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("size %q: unknown unit %q", s, unit)
	}
	v := num * mult
	if v < 0 || v > float64(1<<62) {
		return 0, fmt.Errorf("size %q out of range", s)
	}
	return int64(v), nil
}

// ParseAge parses a duration that may use day and week units, which
// time.ParseDuration does not support.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	last := s[len(s)-1]
	if last == 'd' || last == 'w' || last == 'y' {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		day := 24 * float64(time.Hour)
		switch last {
		case 'd':
			return time.Duration(n * day), nil
		case 'w':
			return time.Duration(n * 7 * day), nil
		case 'y':
			// 365 days, not a calendar year. Documented, not clever.
			return time.Duration(n * 365 * day), nil
		}
	}
	return time.ParseDuration(s)
}
```

### `internal/filesystem/clean.go`

```go
package filesystem

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Target describes a directory that is safe to delete when its marker
// file is present in the parent directory.
//
// The marker requirement is the core safety property: a directory named
// "target" is only a Rust build directory if its parent has Cargo.toml.
// Without that evidence, "target" is just a directory someone named
// "target", and deleting it is data loss.
type Target struct {
	Name        string
	Markers     []string // any one of these in the parent qualifies
	Description string
	Regenerable string // the command that recreates it
}

// cleanTargets is a fixed allowlist. There is deliberately no way for a
// user to extend it with a pattern: an arbitrary-pattern delete is a
// different, more dangerous tool, and Gecko does not provide one.
var cleanTargets = []Target{
	{"node_modules", []string{"package.json"}, "npm/yarn/pnpm dependencies", "npm install"},
	{"target", []string{"Cargo.toml"}, "Rust build artifacts", "cargo build"},
	{"dist", []string{"package.json"}, "JavaScript build output", "npm run build"},
	{"build", []string{"CMakeLists.txt", "Makefile"}, "C/C++ build output", "make"},
	{".pytest_cache", []string{"pyproject.toml", "setup.py", "setup.cfg"}, "pytest cache", "(regenerated automatically)"},
	{"__pycache__", []string{}, "Python bytecode cache", "(regenerated automatically)"},
	{".mypy_cache", []string{"pyproject.toml", "setup.py", "mypy.ini"}, "mypy cache", "(regenerated automatically)"},
	{".next", []string{"package.json"}, "Next.js build cache", "npm run build"},
	{".nuxt", []string{"package.json"}, "Nuxt build cache", "npm run build"},
	{"vendor", []string{"composer.json"}, "Composer dependencies", "composer install"},
	{".gradle", []string{"build.gradle", "build.gradle.kts"}, "Gradle cache", "(regenerated automatically)"},
}

// Candidate is one directory Gecko proposes to delete.
type Candidate struct {
	Path        string
	RelPath     string
	Target      Target
	Size        int64
	FileCount   int
	SizeErr     error
}

// ScanForCleanup finds deletable directories under root.
//
// It never deletes anything. Deletion is a separate call so that the
// caller is structurally required to confirm in between.
func ScanForCleanup(ctx context.Context, root string) ([]Candidate, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := checkSafeRoot(rootAbs); err != nil {
		return nil, err
	}

	byName := make(map[string]Target, len(cleanTargets))
	for _, t := range cleanTargets {
		byName[t.Name] = t
	}

	var candidates []Candidate

	err = filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() || path == rootAbs {
			return nil
		}

		// Never descend into version control metadata.
		if d.Name() == ".git" || d.Name() == ".hg" || d.Name() == ".svn" {
			return fs.SkipDir
		}

		t, ok := byName[d.Name()]
		if !ok {
			return nil
		}
		if !hasMarker(filepath.Dir(path), t.Markers) {
			return nil // named right, but no evidence: leave it alone
		}
		if ok, err := contained(rootAbs, path); err != nil || !ok {
			// Symlinked outside the scan root, or unresolvable.
			return fs.SkipDir
		}

		candidates = append(candidates, Candidate{
			Path: path, RelPath: relOf(rootAbs, path), Target: t,
		})
		// Do not descend: a node_modules inside a node_modules is
		// already accounted for by the parent.
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}

	if err := sizeCandidates(ctx, candidates); err != nil {
		return candidates, err
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Size > candidates[j].Size })
	return candidates, nil
}

func hasMarker(dir string, markers []string) bool {
	if len(markers) == 0 {
		return true // e.g. __pycache__ is unambiguous by name alone
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// sizeCandidates computes each candidate's total size concurrently.
// This is the second legitimate use of a worker pool in this package:
// each candidate is an independent, syscall-heavy subtree walk.
func sizeCandidates(ctx context.Context, cands []Candidate) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.NumCPU())

	for i := range cands {
		i := i
		g.Go(func() error {
			size, count, err := dirSize(gctx, cands[i].Path)
			cands[i].Size, cands[i].FileCount, cands[i].SizeErr = size, count, err
			// A sizing failure is informational, not fatal: we can
			// still offer to delete a directory we could not measure.
			if gctx.Err() != nil {
				return gctx.Err()
			}
			return nil
		})
	}
	return g.Wait()
}
```

Writing to `cands[i]` from multiple goroutines is safe **because each goroutine owns a
distinct index** and slice elements at distinct indices are distinct memory. This is a
legitimate lock-free pattern and `-race` confirms it. What would *not* be safe is
`append`ing to a shared slice, because `append` may reallocate.

```go
func dirSize(ctx context.Context, root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// Apparent size (bytes in file), not disk usage (blocks
		// allocated). du reports the latter; getting it portably would
		// require syscall.Stat_t on Unix with no Windows equivalent.
		total += info.Size()
		count++
		return nil
	})
	return total, count, err
}

// contained reports whether candidate resolves to a location inside root.
// Both paths are fully symlink-resolved first: without that, a symlink
// such as ./node_modules -> /usr/lib would pass a string prefix test.
func contained(root, candidate string) (bool, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	candReal, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootReal, candReal)
	if err != nil {
		return false, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// checkSafeRoot refuses to scan locations where a mistake would be
// catastrophic.
func checkSafeRoot(root string) error {
	clean := filepath.Clean(root)

	if clean == string(filepath.Separator) || clean == filepath.VolumeName(clean)+string(filepath.Separator) {
		return fmt.Errorf("refusing to scan the filesystem root %q", clean)
	}
	for _, bad := range []string{"/usr", "/etc", "/var", "/bin", "/sbin", "/lib", "/System", "/Library",
		"C:\\Windows", "C:\\Program Files"} {
		if strings.EqualFold(clean, filepath.Clean(bad)) {
			return fmt.Errorf("refusing to scan system directory %q", clean)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == clean {
		return fmt.Errorf("refusing to scan your home directory directly; " +
			"scan a project directory, or pass --force if you are sure")
	}
	return nil
}

// Delete removes a candidate. It re-verifies containment immediately
// before deleting: the scan and the confirmation are separated by an
// unbounded amount of time during which the filesystem can change, and
// a TOCTOU window here means deleting the wrong thing.
func Delete(root string, c Candidate) error {
	ok, err := contained(root, c.Path)
	if err != nil {
		return fmt.Errorf("verify %s: %w", c.RelPath, err)
	}
	if !ok {
		return fmt.Errorf("refusing to delete %s: no longer inside %s", c.Path, root)
	}
	if filepath.Base(c.Path) != c.Target.Name {
		return fmt.Errorf("refusing to delete %s: name changed since scan", c.Path)
	}
	return os.RemoveAll(c.Path)
}
```

The re-verification in `Delete` addresses a **TOCTOU** (time-of-check to time-of-use)
race. Between the scan and the user typing `y`, an attacker with write access to the tree
could replace `./project/node_modules` with a symlink to `/`. Re-checking closes most of
that window. It cannot close all of it — the filesystem could change between our check
and `RemoveAll` — but combined with `RemoveAll` not following symlinks, the remaining
exposure is very small. Naming the residual risk honestly is part of security work.

### The CLI layer

`internal/cli/clean.go` handles the prompt. Key points:

```go
// Confirm reads a yes/no answer. It returns false on EOF, so a
// non-interactive invocation without --yes declines rather than hangs.
func confirm(env *Env, prompt string) (bool, error) {
	fmt.Fprintf(env.Stdout, "%s [y/N] ", prompt)
	r := bufio.NewReader(env.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
```

Defaulting to no on EOF matters: `gecko clean < /dev/null` in a CI script must not delete
anything. Default-yes prompts are how accidents happen.

---

## F. Exercise

1. `Find` sends on `out` and the caller must range over it in another goroutine. Write
   the CLI side. Watch out: if the consumer returns early (broken pipe when piping to
   `head`), the producer must not leak. Use `context.WithCancel` and `defer cancel()`.

2. Benchmark `find` with `Workers` set to 1, 2, 4, 8, 16, 32, 64 over a large real tree.
   Plot throughput. Where does it plateau, and does it differ between an SSD and a
   network mount? Use the result to justify (or change) the `NumCPU()*4` default.

3. Add `gecko clean --json` and think about what a machine-readable output should contain
   that the human one doesn't (absolute paths, sizes in bytes, the marker that qualified
   each candidate).

4. **Adversarial exercise.** Write a test that constructs a directory tree designed to
   trick `clean` into deleting something outside the root. Try: a symlinked
   `node_modules`, a `node_modules` whose parent's `package.json` is itself a symlink, a
   directory literally named `../../etc`. Verify each is refused.

---

## G. Testing

### Race detection is mandatory here

```go
func TestFindConcurrentSafety(t *testing.T) {
	dir := buildTree(t, 500) // helper creating 500 files across 50 dirs

	for i := 0; i < 5; i++ { // repeat: races are probabilistic
		out := make(chan Match, 16)
		var got int
		done := make(chan struct{})
		go func() {
			for range out {
				got++
			}
			close(done)
		}()
		stats, err := Find(context.Background(), dir,
			FindOptions{Pattern: "*.txt", MinSize: 1, Workers: 32}, out)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if int64(got) != stats.Matched {
			t.Errorf("received %d matches, stats say %d", got, stats.Matched)
		}
	}
}
```

```bash
go test ./internal/filesystem -race -run TestFind -count=5
```

`-count=5` re-runs and increases the chance of exposing a timing-dependent bug. Races are
probabilistic; a single passing run proves little.

### Goroutine leak detection

```go
func TestFindNoGoroutineLeak(t *testing.T) {
	dir := buildTree(t, 200)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Match) // unbuffered: producer blocks immediately
	go func() {
		<-out    // consume exactly one, then abandon
		cancel() // simulate a consumer that gave up
	}()
	Find(ctx, dir, FindOptions{MinSize: 1}, out)

	// Give the runtime a moment to reap.
	for i := 0; i < 50 && runtime.NumGoroutine() > before; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Errorf("leaked goroutines: before=%d after=%d", before, after)
	}
}
```

This test only passes because every send is inside a `select` with `ctx.Done()`. Remove
one and watch it fail. For production projects, `go.uber.org/goleak` does this more
rigorously; for learning, the manual version shows the mechanism.

### Safety tests for `clean`

```go
func TestCleanRefusesUnmarkedDirectory(t *testing.T) {
	dir := t.TempDir()
	// A "target" directory with no Cargo.toml: someone's photos.
	os.MkdirAll(filepath.Join(dir, "photos", "target"), 0o755)
	os.WriteFile(filepath.Join(dir, "photos", "target", "img.jpg"), []byte("x"), 0o644)

	cands, err := ScanForCleanup(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("proposed deleting %d unmarked directories: %+v", len(cands), cands)
	}
}

func TestCleanRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "precious.txt"), []byte("do not delete"), 0o644)

	proj := filepath.Join(root, "proj")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "package.json"), []byte("{}"), 0o644)
	if err := os.Symlink(outside, filepath.Join(proj, "node_modules")); err != nil {
		t.Skip(err)
	}

	cands, _ := ScanForCleanup(context.Background(), root)
	for _, c := range cands {
		if err := Delete(root, c); err == nil {
			t.Errorf("deleted %s, which escapes the root", c.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious.txt")); err != nil {
		t.Fatal("the file outside the root was destroyed")
	}
}

func TestCleanRefusesDangerousRoots(t *testing.T) {
	for _, root := range []string{"/", "/usr", "/etc"} {
		if _, err := ScanForCleanup(context.Background(), root); err == nil {
			t.Errorf("ScanForCleanup(%q) should have refused", root)
		}
	}
}
```

Note `TestCleanRefusesSymlinkEscape` asserts on the *outcome* (the file still exists), not
just on the error. Testing that an error was returned proves the code took a branch;
testing that the data survived proves the code was correct.

The Windows skip is real: `os.Symlink` on Windows requires either Developer Mode or
`SeCreateSymbolicLinkPrivilege`. Skipping with a reason is right; silently passing is not.

---

## H. Review

- The GMP model, work stealing, and specifically what happens to a P during a blocking
  syscall — and why that makes 4×`NumCPU()` a defensible pool size for I/O.
- Why `WalkDir` is sequential and why parallelising it is usually not worth it.
- `errgroup.WithContext` + `SetLimit` as a bounded pool with backpressure and
  first-error cancellation.
- Why every channel send needs a `select` on `ctx.Done()`.
- Why a failed `stat` returns `nil` inside an errgroup but a cancellation returns the error.
- Writing to distinct slice indices from multiple goroutines is safe; `append` is not.
- `atomic.Int64` over `atomic.AddInt64` and why.
- The four safety constraints of `clean`: allowlist, marker evidence, containment via
  `EvalSymlinks`+`Rel`, and re-verification against TOCTOU.
- Why `os.RemoveAll` beats any subprocess, and why `exec.Command("rm", ...)` is safe but
  `sh -c "rm "+path` is not.

---

## I. Refactoring

Look at `find.go` and `clean.go` together. Both do a `WalkDir` with cancellation checks,
hidden-file filtering and ignore-list pruning. That's duplicated logic and it will
duplicate again in `project` (chapter 6) and `watch` (chapter 9).

Extract a `Walker`:

```go
// Walker performs a cancellable, filtered directory walk. It exists
// because find, clean, project and watch all need the same pruning
// rules, and four copies would drift.
type Walker struct {
	Root          string
	IncludeHidden bool
	Ignore        []string
	MaxDepth      int
	OnError       func(path string, err error) // nil = ignore and continue
}

func (w *Walker) Walk(ctx context.Context, fn func(path string, d fs.DirEntry) error) error
```

**Now compare this to chapter 2's decision not to extract an emitter interface.** The
difference is that there we had one implementation and a hypothetical second; here we
have two real callers and two more confirmed. *Three uses is a pattern; one use is a
guess.* That's the extraction heuristic, and it's why we waited until chapter 5.

Do the extraction. Then verify: does `find`'s test suite still pass unchanged? If the
abstraction is right, the tests shouldn't need to know it happened.

---

## Commit

```
feat: add find command with concurrent metadata filtering
feat: add clean command with allowlist-based cleanup detection
test: add adversarial safety tests for cleanup path containment
refactor: extract shared Walker from find and clean
```

Four commits. The safety tests get their own commit deliberately: in a real project that
commit is the one a security reviewer reads, and burying it inside a feature commit hides
it.

Next: `06-doctor-project.md`.

# Chapter 2 — `gecko tree`: Filesystem Traversal

```
Difficulty:   Intermediate
Est. time:    4–5 hours
Main concepts: io/fs, fs.FS, fs.DirEntry, fs.WalkDir vs manual recursion,
               filepath vs path, symlink semantics, bufio.Writer, Unicode box
               drawing, testing/fstest, golden files
Prerequisites: Chapter 1
```

---

## A. Goal

```
$ gecko tree --depth 2 --size
gecko/
├── cmd/
│   └── gecko/
│       └── main.go            412 B
├── internal/
│   └── cli/
│       ├── cli.go             6.1 KB
│       ├── help.go            2.3 KB
│       └── version.go         1.8 KB
├── go.mod                      54 B
└── README.md                  1.2 KB

4 directories, 6 files
```

Flags: `--depth`, `--all` (hidden files), `--dirs-only`, `--size`, `--no-color`,
`--ascii`, `--follow`.

---

## B. Why this matters

`tree` looks trivial and isn't. It forces four decisions you'll re-encounter in `find`,
`clean`, `project` and `watch`:

- **How do I model "a filesystem"?** If the answer is "call `os.ReadDir` directly", your
  traversal is untestable without creating real directories on disk.
- **Do I recurse or iterate?** A 40,000-directory monorepo is not hypothetical.
- **What do I do about symlinks?** A symlink to `..` is an infinite loop, and every
  naive traversal has one.
- **How do I write output fast?** Unbuffered `fmt.Fprintf` per line syscalls per line.

Get these right once here and reuse them four times.

---

## C. Concepts

### `io/fs` and why `fs.FS` changes everything

`fs.FS` is a one-method interface:

```go
type FS interface {
    Open(name string) (File, error)
}
```

with extension interfaces (`ReadDirFS`, `StatFS`, `SubFS`, `ReadFileFS`) that
implementations may optionally provide. `os.DirFS("/some/root")` adapts the real
filesystem to it. `fstest.MapFS` gives you an in-memory one built from a map literal.

The consequence: if your traversal takes an `fs.FS`, you can test it against a
15-line map instead of a temp directory, and the test runs in microseconds with no
cleanup and no OS-dependent behaviour.

Constraints of `fs.FS` paths, which trip everyone up once:

- Always forward slashes, even on Windows. Use `path`, **not** `path/filepath`.
- Always relative to the FS root. Never leading `/`.
- The root itself is `"."`, never `""`.
- No `..` elements at all.

`os.DirFS` handles the translation. This is also, incidentally, a free path-traversal
defence — which chapter 7 depends on.

### `fs.DirEntry` vs `fs.FileInfo`

`ReadDir` returns `[]DirEntry`, not `[]FileInfo`. This matters for performance.

On Linux, `getdents64` returns the name **and** the file type for each entry in one
syscall. `DirEntry.IsDir()` reads that cached type — zero extra syscalls.
`DirEntry.Info()` may need a `stat` per entry.

So: filtering by directory-ness is free; showing sizes costs one `stat` per file.
That's precisely why `--size` is a flag and not the default.

```go
entries, err := fs.ReadDir(fsys, dir)   // 1 syscall (amortised)
for _, e := range entries {
    e.IsDir()          // free
    info, _ := e.Info() // possibly 1 syscall each
}
```

### `filepath` vs `path`

| | `path` | `path/filepath` |
|---|---|---|
| Separator | always `/` | OS-specific (`\` on Windows) |
| Use for | `fs.FS` paths, URLs | real OS paths |

Mixing them produces bugs that only appear on Windows. Rule: **the moment a path
enters `fs.FS`, it's a `path`; the moment it comes back out for display or `os.*`
calls, it's a `filepath`.**

Also on Windows: paths are case-insensitive but case-preserving, `C:` and `\\server\share`
are volume prefixes (`filepath.VolumeName`), and `filepath.Clean` will not collapse `..`
across a volume boundary. `filepath.Separator` and `filepath.ToSlash`/`FromSlash` are
your conversion tools.

### `fs.WalkDir` vs writing your own

`fs.WalkDir(fsys, root, fn)` does a lexical depth-first walk, calling `fn` for every
entry. It's the right tool for `find` (chapter 5) where order barely matters.

For `tree` it's the wrong tool, because rendering needs to know **whether an entry is
the last child of its parent** in order to draw `└──` instead of `├──`. `WalkDir`'s
callback doesn't tell you that, and reconstructing it means buffering siblings anyway.
So we do our own recursion over `ReadDir`. That's a legitimate reason to hand-roll —
contrast with hand-rolling because you didn't know `WalkDir` existed.

### Recursion depth

Go goroutine stacks start at 8 KB and grow dynamically up to `runtime/debug.SetMaxStack`
(1 GB on 64-bit by default). Recursion depth equals directory nesting depth, which on a
real filesystem is bounded by `PATH_MAX` (4096 on Linux) divided by minimum component
length — call it a few hundred. **Recursion is safe here.** Stack growth is a copying
operation the runtime performs transparently; you will not blow it with directory nesting.

Where recursion *would* be wrong: traversing a symlinked cycle, which is unbounded. Hence
the next section.

### Symlinks

`fs.FS` does not follow symlinks by default and `os.DirFS` reports them via
`fs.ModeSymlink`. If we add `--follow`, we need cycle detection, and the standard
technique is a visited set keyed on `(device, inode)` — which `fs.FS` cannot give you
portably. Our decision: **`--follow` operates on real OS paths and uses
`filepath.EvalSymlinks` plus a visited-path set.** We accept that `--follow` is not
available over an arbitrary `fs.FS`.

### `bufio.Writer`

Every `fmt.Fprintf(os.Stdout, ...)` on an unbuffered writer is a `write(2)` syscall.
For 10,000 lines that's 10,000 syscalls at roughly 1µs each. Wrapping in a
`bufio.Writer` (default 4 KB, we'll use 64 KB) collapses that to a few hundred.

```go
bw := bufio.NewWriterSize(env.Stdout, 64*1024)
defer bw.Flush()   // MUST happen, or output is silently lost
```

We'll measure this in section G.

---

## D. Design

### Package boundary

This is the first extraction. `tree` has real logic — walking, filtering, formatting —
that deserves testing without going through the CLI. So:

```
internal/
  cli/
    tree.go        # flag wiring only, ~50 lines
  filesystem/
    tree.go        # Tree walker and renderer
    tree_test.go
```

Dependency direction: `cli` imports `filesystem`. `filesystem` imports nothing of ours.
If you ever feel the urge to import `cli` from `filesystem` (say, to print a warning),
that's the signal to return a value instead.

### API shape

Before reading on: what should `filesystem`'s exported surface be?

The instinct is `func Tree(root string, depth int, all bool, size bool, w io.Writer) error`.
Seven parameters and growing. Two better options:

**Options struct:**
```go
type TreeOptions struct {
    MaxDepth  int
    ShowAll   bool
    DirsOnly  bool
    ShowSize  bool
    ASCII     bool
    Follow    bool
}
func WriteTree(w io.Writer, fsys fs.FS, root string, opts TreeOptions) (Stats, error)
```

**Functional options:**
```go
func WriteTree(w io.Writer, fsys fs.FS, root string, opts ...TreeOption) (Stats, error)
```

Functional options are idiomatic for libraries with a long-lived, backwards-compatible
API and many optional knobs (`grpc.Dial`, `http.Server` middleware chains). They cost a
constructor function per option and indirection when reading.

**Decision: options struct.** This is an internal package with one caller that already
has all the values as flag variables. Functional options would be ceremony. We'll revisit
in chapter 14 when `sdk/` becomes public — *there* they earn their keep, because we can't
add a struct field without... actually we can add struct fields compatibly. We'll discuss
the real distinction (zero-value meaningfulness) then.

### Separating walk from render

Two responsibilities:

1. Produce a tree of nodes (filtered, depth-limited).
2. Render nodes as text.

Fusing them is tempting and costs you `--json` output later, plus makes the render
logic untestable in isolation. But building the entire node tree in memory before
rendering costs memory proportional to the whole tree, which for a 500k-file scan is
significant.

**Decision: streaming render with a prefix stack.** We recurse, and at each level we
carry the prefix string built so far. Memory is O(depth), not O(nodes). We keep the
rendering in a small method so a future `--json` mode can swap the emitter.

```go
type emitter interface {
    entry(depth int, prefix string, name string, e fs.DirEntry, last bool) error
}
```

Declared here, in the consumer. Implemented by `textEmitter` now and `jsonEmitter` later.
That's the "interfaces where consumed" invariant in action — and note we only introduce
it because we have a *concrete* second implementation planned. If we didn't, a plain
struct would be correct.

Actually — we don't have that second implementation yet, and chapter 1's rules say don't
abstract prematurely. **Revised decision: concrete `treeWriter` struct now.** We'll
extract the interface in chapter 5 when `find --json` gives us a real second case. This
is deliberate: I want you to see the abstraction be *rejected* on the grounds that one
implementation isn't a pattern.

### Box drawing

```
├── prefix for a non-last child
└── prefix for the last child
│   continuation for a non-last ancestor
    continuation for a last ancestor (3 spaces)
```

Unicode: `U+251C ├`, `U+2514 └`, `U+2500 ─`, `U+2502 │`. ASCII fallback: `|--`, `` `-- ``,
`|  `. Windows `cmd.exe` with a non-UTF-8 code page mangles the Unicode; Windows Terminal
and PowerShell 7 are fine. We'll detect properly in chapter 12 and offer `--ascii` now.

---

## E. Implementation

### `internal/filesystem/tree.go`

```go
// Package filesystem implements Gecko's file and directory operations:
// tree rendering, search, hashing and cleanup analysis. Everything here
// operates on io/fs abstractions where practical so it can be tested
// against in-memory filesystems.
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
		if e.IsDir() && contains(t.opts.Ignore, name) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
```

`out := entries[:0]` is the standard Go filter-in-place idiom. It reuses the slice's
backing array, so filtering allocates nothing. It's safe here because we only ever write
to index `i` after reading index `i`.

Go 1.21+ has `slices.Contains`; use it instead of `contains` if you're on a recent
toolchain. I've written it out so the mechanism is visible.

### `internal/filesystem/size.go`

```go
package filesystem

import "fmt"

// HumanSize formats a byte count using binary (1024-based) units, matching
// what `ls -lh` and `du -h` report. Decimal (1000-based) units are what
// disk manufacturers use; we pick binary because developers comparing
// against other CLI tools expect it.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
```

### `internal/cli/tree.go`

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yourname/gecko/internal/filesystem"
)

func newTreeCommand() *Command {
	var (
		depth    int
		all      bool
		dirsOnly bool
		showSize bool
		ascii    bool
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
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			target := "."
			if len(args) > 0 {
				target = args[0]
			}
			if len(args) > 1 {
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
				MaxDepth: depth,
				ShowAll:  all,
				DirsOnly: dirsOnly,
				ShowSize: showSize,
				ASCII:    ascii,
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
```

Register it in `New()`:

```go
a.Register(newTreeCommand)
```

Note the two `IntVar` calls binding `--depth` and `-L` to the same variable. That's the
standard-library way to do flag aliases; the downside is both appear in help output.
Cobra/pflag handle this better, which is one small point in their favour for chapter 6.

---

## F. Exercise

1. Add `--ignore` accepting a repeatable flag (`--ignore node_modules --ignore .git`).
   The `flag` package has no built-in repeatable flag; you'll need to implement
   `flag.Value`:

   ```go
   type stringSlice []string
   func (s *stringSlice) String() string { return strings.Join(*s, ",") }
   func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }
   ```

   Wire it with `fs.Var(&ignores, "ignore", "...")`.

2. Currently `--dirs-only` still counts files in `stats.Files` if... actually check
   whether it does. Read `filter` and `walk` carefully and decide whether the stats are
   correct. Fix if not.

3. Predict, then measure: how much slower is `tree` on a large directory without the
   `bufio.Writer`? Remove it, run against `/usr` or your `node_modules`, and time both.

---

## G. Testing

### Unit tests with `fstest.MapFS`

`internal/filesystem/tree_test.go`:

```go
package filesystem

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fs.FS {
	return fstest.MapFS{
		"cmd/gecko/main.go":        {Data: []byte("package main\n")},
		"internal/cli/cli.go":      {Data: []byte("package cli\n")},
		"internal/cli/help.go":     {Data: []byte("package cli\n")},
		"internal/filesystem/t.go": {Data: []byte("package filesystem\n")},
		".hidden/secret.txt":       {Data: []byte("shh\n")},
		".gitignore":               {Data: []byte("/gecko\n")},
		"go.mod":                   {Data: []byte("module x\n")},
		"README.md":                {Data: []byte("# x\n")},
	}
}

func TestWriteTree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     TreeOptions
		contains []string
		excludes []string
		wantDirs int
	}{
		{
			name:     "default hides dotfiles",
			opts:     TreeOptions{ASCII: true},
			contains: []string{"cmd/", "go.mod", "README.md"},
			excludes: []string{".gitignore", ".hidden"},
			wantDirs: 5,
		},
		{
			name:     "all shows dotfiles",
			opts:     TreeOptions{ASCII: true, ShowAll: true},
			contains: []string{".gitignore", ".hidden/"},
		},
		{
			name:     "depth 1 stops at top level",
			opts:     TreeOptions{ASCII: true, MaxDepth: 1},
			contains: []string{"cmd/", "internal/"},
			excludes: []string{"main.go", "cli.go"},
		},
		{
			name:     "dirs only",
			opts:     TreeOptions{ASCII: true, DirsOnly: true},
			contains: []string{"cmd/", "internal/"},
			excludes: []string{"go.mod", "README.md"},
		},
		{
			name:     "ignore skips directories",
			opts:     TreeOptions{ASCII: true, Ignore: []string{"internal"}},
			contains: []string{"cmd/"},
			excludes: []string{"internal/", "cli.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			stats, err := WriteTree(context.Background(), &buf, testFS(), ".", tt.opts)
			if err != nil {
				t.Fatalf("WriteTree: %v", err)
			}
			got := buf.String()
			for _, s := range tt.contains {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q:\n%s", s, got)
				}
			}
			for _, s := range tt.excludes {
				if strings.Contains(got, s) {
					t.Errorf("output should not contain %q:\n%s", s, got)
				}
			}
			if tt.wantDirs > 0 && stats.Dirs != tt.wantDirs {
				t.Errorf("Dirs = %d, want %d", stats.Dirs, tt.wantDirs)
			}
		})
	}
}

func TestWriteTreeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, root string }{
		{"nonexistent", "nope"},
		{"a file, not a directory", "go.mod"},
		{"absolute path is invalid for fs.FS", "/etc"},
		{"parent traversal is invalid", "../x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := WriteTree(context.Background(), &buf, testFS(), c.root, TreeOptions{}); err == nil {
				t.Errorf("expected error for root %q", c.root)
			}
		})
	}
}

func TestWriteTreeCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	var buf bytes.Buffer
	_, err := WriteTree(ctx, &buf, testFS(), ".", TreeOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
```

That last test is why `walk` checks `ctx.Done()` before doing work rather than after.

### Golden files

Substring assertions miss layout regressions — a wrong `│` alignment passes every check
above. Golden files catch it:

```go
func TestWriteTreeGolden(t *testing.T) {
	var buf bytes.Buffer
	_, err := WriteTree(context.Background(), &buf, testFS(), ".", TreeOptions{ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "tree_all.golden")

	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != string(want) {
		t.Errorf("output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

var update = flag.Bool("update", false, "update golden files")
```

Then `go test ./internal/filesystem -update` regenerates, and you review the diff in Git
like any other change. This is the standard Go pattern (the toolchain itself uses it).

**Warning:** golden files and Windows disagree about line endings. Either normalise with
`strings.ReplaceAll(got, "\r\n", "\n")` or set `* text=auto eol=lf` in `.gitattributes`.
Do the latter; do it now.

### Benchmark: does buffering matter?

```go
func BenchmarkWriteTree(b *testing.B) {
	fsys := largeFS(1000) // build a MapFS with 1000 files across 100 dirs
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		WriteTree(context.Background(), io.Discard, fsys, ".", TreeOptions{})
	}
}
```

`io.Discard` isolates the walk from the terminal. To measure buffering specifically,
benchmark against a `slowWriter` that sleeps 1µs per `Write` — that simulates a syscall
and makes the difference obvious.

```bash
go test ./internal/filesystem -bench=. -benchmem -run=^$
```

### End-to-end CLI test

`internal/cli/tree_test.go`:

```go
func TestTreeCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // automatically removed after the test
	mustWrite(t, filepath.Join(dir, "a.txt"), "hello")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	mustWrite(t, filepath.Join(dir, "sub", "b.txt"), "world")

	env, out, _ := testEnv(nil)
	env.WorkDir = dir

	code := Main(context.Background(), []string{"tree", "--ascii"}, env)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "1 directories, 2 files") {
		t.Errorf("bad summary:\n%s", out)
	}
}
```

`t.TempDir()` creates a per-test directory and registers cleanup, including on Windows
where it retries removal to work around file-locking. Never use `os.TempDir()` + manual
cleanup.

---

## H. Review

You should be able to explain:

- What `fs.FS` buys you and the four rules its paths obey.
- Why `DirEntry.IsDir()` is free and `DirEntry.Info()` isn't.
- When `fs.WalkDir` is right and when hand-rolling is justified.
- Why recursion depth is safe here but symlink following isn't.
- The `entries[:0]` filter-in-place idiom and why it's allocation-free.
- Why we *declined* to introduce an emitter interface, and what would change that.
- How `fstest.MapFS`, golden files and `t.TempDir()` each cover a different failure class.

---

## I. Refactoring

`WriteTree` currently owns three things: validation, buffering and the summary
formatting is in the CLI. That split is slightly wrong — the caller shouldn't have to
know to print the summary. But moving the summary into `WriteTree` couples the library
to a specific output format.

The right resolution: `WriteTree` returns `TreeStats` (it does), and the CLI decides
presentation (it does). That's correct — leave it. **Recognising that something is
already right is a refactoring skill.**

One thing that *is* wrong: `contains()` will duplicate across `find` and `clean`. Do not
create `internal/utils`. When `find` needs it, either use `slices.Contains` from the
standard library, or move it to a named concept (`internal/filesystem/match.go` with a
`Matcher` type) — never to a bag of helpers.

---

## Commit

```
feat: add tree command with depth, size and filtering options
test: cover tree rendering against in-memory filesystems
```

Two commits, not one, because the golden-file infrastructure is reusable and reviewing
it separately is easier.

Next: `03-config-platform.md`.

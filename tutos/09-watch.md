# Chapter 9 — `gecko watch`: File Events and Process Lifecycle

```
Difficulty:   Advanced+
Est. time:    10–12 hours
Main concepts: inotify/kqueue/ReadDirectoryChangesW, polling vs event-driven,
               fsnotify, debouncing and coalescing, time.Timer reuse, process groups,
               Job Objects, signal escalation, stdout/stderr streaming, restart loops
Prerequisites: Chapters 1–8
```

---

## A. Goal

```
$ gecko watch --ignore 'testdata/**' -- go test ./...

  Gecko Watch

  Watching   . (147 files, 23 directories)
  Command    go test ./...
  Debounce   150ms

  [14:02:11] running…
  ok  github.com/yourname/gecko/internal/cli    0.412s
  ok  github.com/yourname/gecko/internal/config 0.108s
  [14:02:13] ✓ passed in 1.8s

  [14:02:41] internal/cli/tree.go changed
  [14:02:41] restarting…
  [14:02:43] ✗ failed (exit 1) in 1.9s

  Press Ctrl+C to stop
```

---

## B. Why this matters

This is the hardest chapter so far, and the difficulty is not the file watching. It's the
process lifecycle.

`gecko watch -- go run .` spawns `go run`, which compiles to a temp binary and spawns
*that*. Killing `go run` leaves your server running and holding port 8080. The next
restart fails with "address in use". Every naive file-watcher has this bug, and fixing it
requires process groups on Unix and Job Objects on Windows — genuinely different
mechanisms with no shared abstraction in the standard library.

Second: this is where you learn what an OS filesystem event API actually gives you, and
why every watcher library in every language has the same set of caveats.

---

## C. Concepts

### What the OS actually provides

**Linux: `inotify`.** A file descriptor you `read()` for a stream of
`struct inotify_event`. You add *watches* with `inotify_add_watch(fd, path, mask)`.

Critical properties:
- **Watches are per-directory, not recursive.** Watching a tree means adding a watch for
  every directory, and adding new ones as directories are created. A large monorepo can
  mean 50,000 watches.
- There's a per-user limit: `/proc/sys/fs/inotify/max_user_watches`, historically 8192,
  now often 65536 or higher. Exceeding it returns `ENOSPC` — an error message that says
  "no space left on device" while your disk is fine. This confuses everyone once.
- Events carry a watch descriptor and a name, not a full path. You maintain the mapping.
- **Events can be dropped.** If the kernel buffer overflows you get `IN_Q_OVERFLOW` and
  you've lost events with no way to know which. Robust watchers rescan on overflow.

**macOS: two options.** `kqueue` with `EVFILT_VNODE` requires an **open file descriptor
per watched file**, which hits `RLIMIT_NOFILE` fast on a big tree. The alternative,
`FSEvents`, is directory-granular, coalesces aggressively, and has a historical latency of
up to a second by default. Most Go tools use kqueue with directory-only watches plus
directory rescans on change.

**Windows: `ReadDirectoryChangesW`.** Genuinely recursive with one call
(`bWatchSubtree = TRUE`), which is nicer than both Unix APIs. Uses overlapped (async) I/O
with completion ports. Its buffer can also overflow, signalled by a zero-length result.

**BSD: kqueue.** Same as macOS.

The consequence: a cross-platform watcher is three completely different implementations
with three different failure modes, unified behind a common event type. That is
substantial, ongoing work.

### Polling: the alternative you should try first

```go
for {
    snapshot := scan(root)           // WalkDir, record path → (mtime, size)
    diff := compare(previous, snapshot)
    emit(diff)
    previous = snapshot
    time.Sleep(interval)
}
```

Costs: O(files) `stat` calls per interval. For 10,000 files at 500 ms that's 20,000
syscalls per second — measurable CPU and, on a laptop, measurable battery.

Benefits, which are real: dead simple, identical on every platform, no watch limits, no
dropped events, works over NFS and SMB where inotify does not, and works on filesystems
that don't support events at all.

**Build the polling version first.** It's ~80 lines, it teaches you what the event
semantics need to be, and it becomes the fallback for network filesystems.

### Why we then adopt `fsnotify`

```bash
go get github.com/fsnotify/fsnotify
```

Against the dependency checklist:

1. **Why we need it.** Polling burns CPU proportional to tree size, and for a watch
   command that runs all day on a developer's laptop, that matters.
2. **Alternatives.** Write it ourselves (three platform backends, `x/sys` syscall
   wrapping, months of edge cases), or `rjeczalik/notify` (supports recursive watches
   natively on Windows/macOS but is less maintained), or polling only.
3. **Tradeoffs.** `fsnotify` does *not* do recursive watching — you add each directory
   yourself and handle newly-created directories. It doesn't deduplicate. It exposes
   platform quirks rather than hiding them. It depends on `golang.org/x/sys`.
4. **Could we implement it?** Yes, and it would take longer than the rest of this course
   combined to reach parity. This is the clearest "use the library" case in the project —
   and unlike a CLI framework, the thing it hides is *syscall plumbing*, not
   *design*. Nothing about Go architecture is learned by writing the third inotify
   binding.
5. **Minimal?** Two dependencies, both `golang.org/x/`.

**But** — and this is why we build polling first — you will not understand fsnotify's
caveats unless you understand what it's wrapping. Its docs list "events are not
deduplicated", "some editors write files in ways that produce multiple events", and
"watches are not recursive" as known behaviours, and all three only make sense once you
know the underlying APIs.

### The editor problem

Save a file in vim. You get, typically:

```
CREATE  main.go~
WRITE   main.go~
CREATE  main.go.swp
RENAME  main.go
CREATE  main.go
WRITE   main.go
CHMOD   main.go
REMOVE  main.go~
```

Seven events for one save. VS Code uses atomic writes (write temp, rename over), which
means the watched inode is *replaced* — on Linux, your inotify watch is now on a deleted
inode and you stop receiving events for that file entirely unless you watch the directory
rather than the file.

**Two lessons:** watch directories, never individual files. And debounce.

### Debouncing vs throttling

**Debounce:** wait until events stop for N ms, then fire once. A burst of seven events
produces one run.
**Throttle:** fire at most once per N ms regardless.

We want debounce with a **maximum wait**, otherwise a continuously-writing process (a
build tool, a log) starves the command forever.

```go
type Debouncer struct {
    delay   time.Duration
    maxWait time.Duration
    timer   *time.Timer
    first   time.Time
}
```

Implementation detail that bites people: `timer.Reset` on a timer that has already fired
and whose channel hasn't been drained leaves a stale value in the channel. The correct
reset sequence pre-Go 1.23:

```go
if !t.Stop() {
    select {
    case <-t.C:
    default:
    }
}
t.Reset(d)
```

**Go 1.23 changed this.** Timer channels are now unbuffered and `Stop`/`Reset` no longer
leave stale values, so the dance is unnecessary. It's also harmless. Since our `go.mod`
may target older versions, and since you'll read a lot of code containing it, know both.

### Killing a process tree

`cmd.Process.Kill()` sends SIGKILL to one PID. Children survive and are reparented to
init. For `go run .`, the compiled binary is the child of `go run` and survives.

**Unix: process groups.**

```go
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
// ...
syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)   // negative PID = process group
```

`Setpgid: true` makes the child the leader of a new process group with PGID == its PID.
Its descendants inherit that group. `kill(-pgid, sig)` signals the entire group.

The negative-PID convention is a real POSIX feature, not a Go trick: `kill(2)` with a
negative pid means "the process group whose ID is the absolute value".

**Signal escalation** is the civilised approach:

```go
syscall.Kill(-pgid, syscall.SIGTERM)   // ask nicely
select {
case <-done:                            // it exited
case <-time.After(gracePeriod):
    syscall.Kill(-pgid, syscall.SIGKILL) // insist
}
```

SIGTERM is catchable — a well-behaved server closes connections and flushes. SIGKILL is
not catchable and leaves temp files behind. Always try TERM first.

**Windows: Job Objects.** There are no process groups and no signals. `TerminateProcess`
kills one process.

The mechanism is a **Job Object**: a kernel container. Assign a process to a job, set
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, and every process in the job dies when the job
handle closes — including descendants, because child processes inherit job membership by
default.

```go
job, _ := windows.CreateJobObject(nil, nil)
info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
    BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
        LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
    },
}
windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
    uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
windows.AssignProcessToJobObject(job, processHandle)
// later: windows.CloseHandle(job) kills everything
```

There's a graceful-shutdown gap on Windows: there's no SIGTERM. `GenerateConsoleCtrlEvent`
with `CTRL_BREAK_EVENT` can signal a process group *if* the child was created with
`CREATE_NEW_PROCESS_GROUP`, and console applications can handle it. It's unreliable for
GUI and non-console processes. Most Go tools accept that Windows restarts are hard kills.
Document it.

**This is the canonical case for build-tag file splitting**: different imports
(`golang.org/x/sys/windows` vs `syscall`), different algorithms, no shared shape.

### Streaming child output

```go
cmd.Stdout = os.Stdout   // simple; child writes directly to our fd
```

That's the fastest option — the child inherits the file descriptor and we're not in the
data path at all. But we can't prefix lines with timestamps, colour stderr, or detect
"server started" patterns.

```go
stdout, _ := cmd.StdoutPipe()
go scanLines(stdout, prefix)
```

That costs a goroutine and a copy per line but gives control. **You must drain the pipes
before `Wait()`** (chapter 6's deadlock), and you must handle the pipe closing when the
process dies.

Subtlety: a child that detects a non-TTY stdout will disable colour and switch to
line-buffered or block-buffered output. `go test` does this — piped output arrives in
bursts. Solutions: set the child's environment (`FORCE_COLOR=1`, `CLICOLOR_FORCE=1`) or
allocate a pty. A pty is the real fix and needs `creack/pty`, which has no Windows
support. We'll inherit the fds by default and offer `--pipe` for prefixed output.

---

## D. Design

### Two watcher backends behind one interface

```go
// Watcher reports filesystem changes under a set of roots.
//
// Two implementations exist because the event-based backend is
// unavailable or unreliable in several real situations: network
// filesystems, containers with low inotify limits, and very large trees.
type Watcher interface {
    Events() <-chan Event
    Errors() <-chan error
    Close() error
}
```

Here an interface is justified: two real implementations, both used in production paths,
selected at runtime by `--poll` or by automatic fallback when `fsnotify.NewWatcher`
fails with `ENOSPC`.

### The pipeline

```
Watcher → filter (ignore patterns, temp files) → debouncer → runner
                                                     ↑
                                              cancel previous run
```

Each stage is a goroutine connected by channels. The runner holds at most one live
process; a new trigger cancels the current one before starting the next.

### Restart semantics

Two distinct modes and it's important to get the naming right:

- **Task mode** (`go test ./...`): the command is expected to exit. A change while it's
  running should kill and rerun.
- **Server mode** (`go run .`): the command is expected to run forever. Same behaviour,
  but the exit code display differs — an exit is abnormal, not a result.

Detecting this automatically is guesswork. Add `--restart` to mark server mode explicitly;
default to task semantics.

### Ignore patterns

Must ignore by default: `.git`, `node_modules`, `vendor`, `target`, `dist`, `.idea`,
`__pycache__`, and — critically — **the build output of the command itself**. Running
`gecko watch -- go build -o app .` where `app` is in the watched tree is an infinite
loop. There's no clever fix; document it and make the ignore list easy to extend.

Editor temp files: `*~`, `*.swp`, `*.swx`, `4913` (vim's probe file), `.#*` (emacs),
`*.tmp`, and anything starting with `.` by default.

Pattern syntax: `filepath.Match` has no `**`. For `testdata/**` to work you need either
`doublestar` (a dependency) or your own split-on-`/` matcher. Write your own — it's 40
lines and the semantics are worth understanding.

---

## E. Implementation

### `internal/watch/poll.go` — build this first

```go
// Package watch reports filesystem changes and reruns commands.
package watch

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"
)

type Op uint8

const (
	Create Op = 1 << iota
	Write
	Remove
	Rename
	Chmod
)

type Event struct {
	Path string
	Op   Op
}

// snapshot maps a path to the metadata used to detect change. Size is
// included because mtime granularity is one second on some filesystems
// (ext3, HFS+, FAT), so a fast rewrite of the same length can otherwise
// go unnoticed.
type snapshot map[string]fileState

type fileState struct {
	modTime time.Time
	size    int64
	isDir   bool
}

// pollWatcher detects changes by rescanning.
//
// It exists as a fallback for environments where event-based watching
// is unavailable or unreliable: NFS and SMB mounts (inotify does not
// see remote writes), containers with a low fs.inotify.max_user_watches,
// and trees large enough to exhaust the watch limit.
type pollWatcher struct {
	roots    []string
	interval time.Duration
	filter   *Filter
	events   chan Event
	errs     chan error
	done     chan struct{}
}

func NewPollWatcher(roots []string, interval time.Duration, f *Filter) (Watcher, error) {
	w := &pollWatcher{
		roots:    roots,
		interval: interval,
		filter:   f,
		events:   make(chan Event, 64),
		errs:     make(chan error, 8),
		done:     make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

func (w *pollWatcher) Events() <-chan Event { return w.events }
func (w *pollWatcher) Errors() <-chan error { return w.errs }

func (w *pollWatcher) Close() error {
	close(w.done)
	return nil
}

func (w *pollWatcher) loop() {
	defer close(w.events)

	prev, err := w.scan()
	if err != nil {
		w.sendErr(err)
	}

	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-t.C:
		}

		cur, err := w.scan()
		if err != nil {
			w.sendErr(err)
			continue
		}
		w.diff(prev, cur)
		prev = cur
	}
}

func (w *pollWatcher) scan() (snapshot, error) {
	snap := make(snapshot, 1024)
	for _, root := range w.roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // vanished or unreadable; not fatal for a scan
			}
			if w.filter.SkipDir(path, d) {
				return fs.SkipDir
			}
			if w.filter.Skip(path, d) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			snap[path] = fileState{modTime: info.ModTime(), size: info.Size(), isDir: d.IsDir()}
			return nil
		})
		if err != nil {
			return snap, err
		}
	}
	return snap, nil
}

func (w *pollWatcher) diff(old, new snapshot) {
	for path, ns := range new {
		os, existed := old[path]
		switch {
		case !existed:
			w.send(Event{Path: path, Op: Create})
		case !ns.modTime.Equal(os.modTime) || ns.size != os.size:
			w.send(Event{Path: path, Op: Write})
		}
	}
	for path := range old {
		if _, ok := new[path]; !ok {
			w.send(Event{Path: path, Op: Remove})
		}
	}
}

func (w *pollWatcher) send(e Event) {
	select {
	case w.events <- e:
	case <-w.done:
	default:
		// Consumer is behind. Dropping is correct here: the debouncer
		// coalesces anyway, and blocking would stall the scan loop.
	}
}

func (w *pollWatcher) sendErr(err error) {
	select {
	case w.errs <- err:
	default:
	}
}
```

Benchmark this against a real tree before moving on:

```go
func BenchmarkPollScan(b *testing.B) { /* 10k files */ }
```

On my machine, scanning 10,000 files takes ~28 ms. At a 500 ms interval that's ~6% of one
core, continuously. That number is the justification for the next section.

### `internal/watch/fsnotify.go`

```go
package watch

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// notifyWatcher wraps fsnotify with recursive directory registration.
//
// fsnotify deliberately does not do recursion: inotify watches are
// per-directory, and kqueue needs a file descriptor per watched object.
// Adding subtrees, and adding newly-created directories as they appear,
// is the caller's job.
type notifyWatcher struct {
	fsw    *fsnotify.Watcher
	filter *Filter
	events chan Event
	errs   chan error
	done   chan struct{}

	mu      sync.Mutex
	watched map[string]bool
}

func NewNotifyWatcher(roots []string, f *Filter) (Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &notifyWatcher{
		fsw: fsw, filter: f,
		events:  make(chan Event, 256),
		errs:    make(chan error, 8),
		done:    make(chan struct{}),
		watched: make(map[string]bool),
	}
	for _, root := range roots {
		if err := w.addTree(root); err != nil {
			fsw.Close()
			return nil, err
		}
	}
	go w.loop()
	return w, nil
}

// addTree registers every directory under root.
func (w *notifyWatcher) addTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if w.filter.SkipDir(path, d) {
			return fs.SkipDir
		}
		return w.addDir(path)
	})
}

func (w *notifyWatcher) addDir(path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.watched[path] {
		return nil
	}
	if err := w.fsw.Add(path); err != nil {
		// ENOSPC on Linux means fs.inotify.max_user_watches is
		// exhausted. The message ("no space left on device") is
		// notoriously misleading, so we translate it.
		if errors.Is(err, syscall.ENOSPC) {
			return fmt.Errorf("inotify watch limit reached while adding %s; "+
				"raise fs.inotify.max_user_watches or use --poll: %w", path, err)
		}
		return err
	}
	w.watched[path] = true
	return nil
}

func (w *notifyWatcher) loop() {
	defer close(w.events)
	defer w.fsw.Close()

	for {
		select {
		case <-w.done:
			return

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.sendErr(err)

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// A newly created directory has no watch yet. Register it
			// immediately; files created inside it before we do are
			// missed, which is an inherent race in inotify and is why
			// robust tools also rescan periodically.
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.addTree(ev.Name)
				}
			}
			if w.filter.SkipPath(ev.Name) {
				continue
			}
			w.send(Event{Path: ev.Name, Op: convertOp(ev.Op)})
		}
	}
}
```

The comment about the create-race is important and honest: **there is no way to eliminate
it with inotify.** Between a directory being created and your watch being added,
files can appear. Every watcher in every language has this. Tools that care combine
events with a low-frequency rescan.

### `internal/watch/debounce.go`

```go
package watch

import "time"

// Debouncer coalesces a burst of events into a single trigger.
//
// A single editor save typically produces five to eight events: a temp
// file created and written, the original renamed away, the new file
// renamed into place, and a chmod. Without coalescing, one save runs the
// command seven times.
//
// maxWait bounds the delay so that a process writing continuously (a
// build tool, a log) cannot starve the trigger indefinitely.
type Debouncer struct {
	delay   time.Duration
	maxWait time.Duration

	in  <-chan Event
	out chan []Event
}

func NewDebouncer(in <-chan Event, delay, maxWait time.Duration) *Debouncer {
	d := &Debouncer{delay: delay, maxWait: maxWait, in: in, out: make(chan []Event, 1)}
	go d.run()
	return d
}

func (d *Debouncer) C() <-chan []Event { return d.out }

func (d *Debouncer) run() {
	defer close(d.out)

	var pending []Event
	seen := make(map[string]bool)

	// A stopped timer with a drained channel; we only ever use it while
	// pending is non-empty.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var deadline <-chan time.Time

	maxTimer := time.NewTimer(time.Hour)
	if !maxTimer.Stop() {
		<-maxTimer.C
	}
	var maxDeadline <-chan time.Time

	for {
		select {
		case ev, ok := <-d.in:
			if !ok {
				if len(pending) > 0 {
					d.emit(pending)
				}
				return
			}
			// Deduplicate by path: five writes to one file is one change.
			if !seen[ev.Path] {
				seen[ev.Path] = true
				pending = append(pending, ev)
			}

			// Reset the quiet-period timer.
			//
			// Before Go 1.23, Reset on a timer that already fired and
			// whose channel was not drained leaves a stale value in the
			// channel, firing immediately on the next select. The
			// Stop-and-drain dance below is the portable fix; from Go
			// 1.23 timer channels are unbuffered and it is unnecessary
			// but harmless.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d.delay)
			deadline = timer.C

			if maxDeadline == nil {
				maxTimer.Reset(d.maxWait)
				maxDeadline = maxTimer.C
			}

		case <-deadline:
			d.emit(pending)
			pending, seen = nil, make(map[string]bool)
			deadline = nil
			if !maxTimer.Stop() {
				select {
				case <-maxTimer.C:
				default:
				}
			}
			maxDeadline = nil

		case <-maxDeadline:
			// The quiet period never arrived; fire anyway.
			d.emit(pending)
			pending, seen = nil, make(map[string]bool)
			maxDeadline = nil
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			deadline = nil
		}
	}
}

func (d *Debouncer) emit(evs []Event) {
	if len(evs) == 0 {
		return
	}
	select {
	case d.out <- evs:
	default:
		// A run is already pending; the newer batch supersedes it in
		// effect, since the runner restarts from scratch anyway.
	}
}
```

Setting a channel variable to `nil` to disable a `select` case is a core Go idiom worth
naming explicitly: **a receive from a nil channel blocks forever, so `case <-nilChan:` is
never selected.** That's how you conditionally enable branches without duplicating the
select.

### Process groups — the platform split

Now we finally have a case that justifies build tags: different imports, different
mechanisms, no shared shape.

`internal/process/group_unix.go`:

```go
//go:build !windows

package process

import (
	"os/exec"
	"syscall"
	"time"
)

// configureGroup makes the child the leader of a new process group so
// that its descendants can be signalled together.
//
// Without this, killing "go run ." kills the go tool but leaves the
// compiled binary running — still holding whatever port it bound.
func configureGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup signals the whole process group, escalating from
// SIGTERM to SIGKILL after grace.
//
// A negative pid means "the process group with this ID" — a POSIX
// feature of kill(2), not a Go convention. Because Setpgid gave the
// child a group whose ID equals its PID, -pid addresses the whole tree.
func terminateGroup(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid

	// SIGTERM first: a well-behaved server closes listeners, drains
	// connections and removes its pidfile. SIGKILL cannot be caught,
	// so it skips all of that.
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		// ESRCH means it already exited between our check and here.
		if err != syscall.ESRCH {
			return err
		}
		return nil
	}

	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	return syscall.Kill(pgid, syscall.SIGKILL)
}
```

`internal/process/group_windows.go`:

```go
//go:build windows

package process

import (
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has neither process groups nor signals. The equivalent
// mechanism is a Job Object: a kernel container that child processes
// inherit membership of. Closing the job handle with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE terminates every process in it.
type jobHandle struct {
	h windows.Handle
}

func configureGroup(cmd *exec.Cmd) {
	// CREATE_NEW_PROCESS_GROUP allows GenerateConsoleCtrlEvent to
	// deliver CTRL_BREAK_EVENT to the child, which is the closest
	// Windows equivalent of SIGTERM for console applications.
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// assignJob places a started process into a kill-on-close job object.
// It must be called after Start, because the process handle does not
// exist before then — which introduces a small race where a very fast
// grandchild could escape. Windows offers no way to close it fully
// without CREATE_SUSPENDED and a manual resume.
func assignJob(cmd *exec.Cmd) (*jobHandle, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	defer windows.CloseHandle(ph)

	if err := windows.AssignProcessToJobObject(h, ph); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &jobHandle{h: h}, nil
}

func terminateGroup(cmd *exec.Cmd, job *jobHandle, done <-chan struct{}, grace time.Duration) error {
	if job == nil || cmd.Process == nil {
		return nil
	}
	// Try a graceful CTRL_BREAK first. This only reaches console
	// applications that installed a handler; GUI and many runtimes
	// ignore it, so the grace period is short.
	_ = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))

	select {
	case <-done:
		windows.CloseHandle(job.h)
		return nil
	case <-time.After(grace):
	}
	// Closing the job handle kills every process in it.
	return windows.CloseHandle(job.h)
}
```

Note the honest comments about the two races: the assign-after-Start gap on Windows, and
the unreliability of `CTRL_BREAK_EVENT`. **Documenting a limitation you can't fix is
engineering; omitting it is not.**

The shared declaration lives in `internal/process/runner.go` with no build tag, and the
per-platform files supply the implementation. That's the classic pattern: **one API, N
files, the compiler picks.**

### Filename-based build constraints

`_windows.go`, `_linux.go`, `_darwin.go`, `_unix.go` (Go 1.19+ recognises `unix` as a
build tag meaning any Unix-like GOOS) are recognised **by filename suffix alone** — no
comment needed. But writing `//go:build windows` explicitly at the top is good practice:
it survives a rename and it's visible when reading the file.

Also: `_test.go` interacts with these. `foo_windows_test.go` only compiles on Windows.

---

## F. Exercise

1. Implement the `**` glob matcher. Signature: `func MatchPattern(pattern, path string) bool`
   where `**` matches any number of path segments and `*` matches within one segment.
   Table-test it hard: `a/**/b` should match `a/b`, `a/x/b`, `a/x/y/b` but not `a/b/c`.

2. **The instrumentation exercise.** Add a `--stats` flag that reports events received,
   events after filtering, batches emitted, and runs triggered. Then save a file in your
   editor and look at the numbers. Whatever your editor does will surprise you.

3. Verify the process-group kill actually works. Write a test that runs a shell script
   spawning a background `sleep 300`, kill the group, and assert the sleep is gone
   (`ps -p PID`). Then remove `Setpgid` and watch the test fail. On Windows, do the
   equivalent with `cmd /c start`.

4. Measure poll vs fsnotify CPU on a 10,000-file tree over 60 seconds. Use
   `/usr/bin/time -v` or read `/proc/self/stat`. Decide what the default should be and
   what the automatic-fallback trigger should be.

---

## G. Testing

### Filesystem events are timing-dependent — design around it

```go
// waitFor polls a condition rather than sleeping a fixed duration.
// Fixed sleeps make tests either slow or flaky, and usually both.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
```

Use this everywhere in this package. A `time.Sleep(100 * time.Millisecond)` in a
filesystem test is a future flake on a loaded CI machine.

### Debouncer tests, no filesystem needed

```go
func TestDebouncerCoalescesBurst(t *testing.T) {
	t.Parallel()
	in := make(chan Event)
	d := NewDebouncer(in, 50*time.Millisecond, time.Second)

	// Simulate an editor save: seven events in quick succession.
	go func() {
		for _, p := range []string{"a.go~", "a.go~", "a.go.swp", "a.go", "a.go", "a.go", "a.go~"} {
			in <- Event{Path: p, Op: Write}
			time.Sleep(2 * time.Millisecond)
		}
		close(in)
	}()

	var batches [][]Event
	for b := range d.C() {
		batches = append(batches, b)
	}
	if len(batches) != 1 {
		t.Fatalf("got %d batches, want 1 (coalescing failed)", len(batches))
	}
	// Four distinct paths, deduplicated from seven events.
	if len(batches[0]) != 4 {
		t.Errorf("batch has %d events, want 4 unique paths", len(batches[0]))
	}
}

func TestDebouncerMaxWaitPreventsStarvation(t *testing.T) {
	t.Parallel()
	in := make(chan Event)
	d := NewDebouncer(in, 100*time.Millisecond, 250*time.Millisecond)

	stop := make(chan struct{})
	go func() {
		// Continuous writes: the quiet period never arrives.
		for i := 0; ; i++ {
			select {
			case <-stop:
				close(in)
				return
			case in <- Event{Path: fmt.Sprintf("f%d.go", i), Op: Write}:
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case <-d.C():
		// Fired despite no quiet period: maxWait works.
	case <-time.After(2 * time.Second):
		t.Fatal("debouncer starved: maxWait did not fire")
	}
	close(stop)
}
```

The debouncer being testable without touching a filesystem is the payoff for making it a
channel-to-channel transform rather than a method on the watcher.

### Watcher conformance suite

Both backends must behave identically. Write the tests once and run them against both:

```go
func TestWatcherBackends(t *testing.T) {
	backends := map[string]func([]string, *Filter) (Watcher, error){
		"poll": func(roots []string, f *Filter) (Watcher, error) {
			return NewPollWatcher(roots, 20*time.Millisecond, f)
		},
		"fsnotify": NewNotifyWatcher,
	}

	for name, ctor := range backends {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := ctor([]string{dir}, DefaultFilter())
			if err != nil {
				t.Skipf("backend unavailable: %v", err)
			}
			defer w.Close()

			collected := collectEvents(w)

			t.Run("create", func(t *testing.T) {
				os.WriteFile(filepath.Join(dir, "new.go"), []byte("x"), 0o644)
				waitFor(t, 2*time.Second, func() bool { return collected.has("new.go") })
			})
			t.Run("write", func(t *testing.T) { ... })
			t.Run("remove", func(t *testing.T) { ... })
			t.Run("new subdirectory is watched", func(t *testing.T) {
				sub := filepath.Join(dir, "sub")
				os.Mkdir(sub, 0o755)
				time.Sleep(50 * time.Millisecond) // let the watch register
				os.WriteFile(filepath.Join(sub, "x.go"), []byte("x"), 0o644)
				waitFor(t, 2*time.Second, func() bool { return collected.has("sub/x.go") })
			})
			t.Run("ignored files produce no events", func(t *testing.T) { ... })
		})
	}
}
```

A shared conformance suite across implementations is the strongest argument that an
interface was the right choice. If you can't write one, the two implementations aren't
really substitutable and the interface is a lie.

### Process tree kill

```go
//go:build !windows

func TestTerminateKillsGrandchildren(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	// A shell that spawns a background sleep and prints its PID.
	script := `sleep 300 & echo $!; wait`
	cmd := exec.Command("sh", "-c", script)
	configureGroup(cmd)

	out, err := cmd.StdoutPipe()
	if err != nil { t.Fatal(err) }
	if err := cmd.Start(); err != nil { t.Fatal(err) }

	var grandchildPID int
	fmt.Fscan(bufio.NewReader(out), &grandchildPID)

	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()

	if err := terminateGroup(cmd, done, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-done

	// Signal 0 tests for existence without sending anything.
	waitFor(t, 2*time.Second, func() bool {
		return syscall.Kill(grandchildPID, 0) != nil
	})
}
```

`kill(pid, 0)` is the standard existence check: it performs permission checks and returns
`ESRCH` if no such process, without delivering a signal.

Remove `configureGroup(cmd)` and this test fails, leaving a `sleep 300` running. That
failure is the whole chapter in one test.

---

## H. Review

- inotify is per-directory with a user watch limit; kqueue needs an fd per object;
  `ReadDirectoryChangesW` is natively recursive. Three APIs, three failure modes.
- `ENOSPC` from inotify means the watch limit, not disk space.
- Events can be dropped (queue overflow), so robust watchers also rescan.
- Editors produce 5–8 events per save and often replace the inode — hence watch
  directories, not files, and debounce.
- Debounce with `maxWait`; the timer Stop-drain-Reset dance and what Go 1.23 changed.
- A nil channel in a `select` disables that case — the idiom for conditional branches.
- `Setpgid` + negative PID kills a process tree on Unix; Job Objects do it on Windows.
- SIGTERM before SIGKILL, and why Windows has no clean equivalent.
- Build-tag file splitting is justified by *different imports and different algorithms*,
  which is finally true here and wasn't in chapters 3 or 7.
- Conformance suites as the test that an interface abstraction is honest.

---

## I. Refactoring

Look at `internal/process`. It now has `Output` (chapter 6), `configureGroup`,
`terminateGroup`, and the streaming runner from this chapter. Chapter 10 will add task
execution on top. That's cohesive — a package about running processes — so no split
needed.

But the *watch* command now contains an orchestration loop (watch → debounce → run) that
chapter 10's `run --watch` will want. Extract it:

```go
// Supervisor runs a command, restarting it when triggered.
type Supervisor struct {
    Cmd     []string
    Dir     string
    Env     []string
    Grace   time.Duration
    OnStart func()
    OnExit  func(code int, dur time.Duration)
}

func (s *Supervisor) Run(ctx context.Context, trigger <-chan struct{}) error
```

Taking `trigger <-chan struct{}` rather than a `Watcher` is the important design choice:
the supervisor doesn't know or care that the trigger comes from the filesystem. Chapter
10 can drive it from a task dependency completing; a future `gecko watch --http` could
drive it from a webhook. **Depend on the narrowest thing that works.**

---

## Commit

```
feat: add polling file watcher
feat: add fsnotify backend with recursive directory registration
feat: add event debouncing with max-wait starvation guard
feat: kill process trees via process groups and job objects
test: add cross-backend watcher conformance suite
refactor: extract Supervisor from watch command
```

Six commits. The polling watcher gets its own commit even though fsnotify supersedes it
as the default, because it remains the documented fallback and its history should be
readable.

Next: `10-task-runner.md`.

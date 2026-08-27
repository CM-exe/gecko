# Chapter 6 — `gecko doctor` and `gecko project`: Subprocesses, Logging, and the Cobra Decision

```
Difficulty:   Advanced
Est. time:    6–8 hours
Main concepts: os/exec, exec.LookPath, CommandContext, pipes and deadlocks, per-task
               timeouts, log/slog, structured vs user-facing output, Cobra internals
Prerequisites: Chapters 1–5
```

---

## A. Goal

```
$ gecko doctor

Gecko Doctor

System
  OS         macOS 15.2
  Arch       arm64
  Shell      zsh
  Terminal   Ghostty

Development
  Go         ✓  1.24.0        /opt/homebrew/bin/go
  Git        ✓  2.47.1        /usr/bin/git
  Docker     ✓  27.4.0        /usr/local/bin/docker
  Node       ✓  22.12.0       /opt/homebrew/bin/node
  Python     ✗  not found
  Rust       ⚠  1.70.0        /usr/bin/rustc  (1.75+ recommended)

Gecko
  Config     /Users/you/.config/gecko/config.yaml
  Plugins    2 installed

Issues: 1 warning, 1 missing

$ gecko project info
Project
───────
Name        gecko
Language    Go
Go version  1.24
Module      github.com/yourname/gecko

Files       143
Lines       18,391
Tests       47 (32.9% of files)

Git
  Branch    main
  Changes   3 modified, 1 untracked
  Commit    a3f9c21 (2 hours ago)
```

---

## B. Why this matters

`doctor` is the first command that runs other programs, and running programs correctly
is harder than it looks. The failure modes — deadlocking on a full pipe, hanging forever
on a tool that waits for input, leaking zombie processes, orphaning children on
cancellation — are all real and all reachable from a naive implementation.

It's also the command that forces the question: **how do you probe twelve tools quickly?**
Sequentially, at ~30 ms per subprocess, that's 400 ms of dead time. Concurrently it's 40
ms. This is a case where concurrency is unambiguously justified, and it makes a nice
contrast with chapter 5 where it partly wasn't.

Finally: at nine commands with nesting, the Cobra question is no longer theoretical.

---

## C. Concepts

### `os/exec` fundamentals and the things that bite

```go
cmd := exec.CommandContext(ctx, "go", "version")
out, err := cmd.Output()
```

**`exec.Command` does not use a shell.** It calls `execve` (or `CreateProcess`) with an
argv array. `exec.Command("echo", "a; rm -rf /")` prints the literal string. This is the
default and it is why command injection is not a concern in Go unless you deliberately
opt in with `sh -c`.

**`LookPath` resolution happens at `Command` time, not `Start` time.** `exec.Command`
stores the resolution error in `cmd.Err` (Go 1.19+; previously `cmd.Path` was left as the
bare name) and `Start` returns it. Since Go 1.19, `LookPath` also **refuses to resolve
relative to the current directory on Windows**, closing a long-standing vulnerability
where running `gecko` in a directory containing a malicious `git.exe` would execute it.
If you need the old behaviour you must ask for it explicitly — don't.

**The pipe deadlock.** This is the classic:

```go
cmd.Stdout = os.Stdout        // fine
// but:
stdout, _ := cmd.StdoutPipe()
cmd.Start()
cmd.Wait()                     // DEADLOCK if the child writes > 64 KB
io.ReadAll(stdout)
```

An OS pipe has a fixed buffer (64 KB on Linux by default). If the child fills it and
nobody reads, the child blocks in `write`. `Wait` blocks for the child. Deadlock.

Rules that avoid it entirely:
- Use `cmd.Output()` or `cmd.CombinedOutput()` — they set up buffers and read
  concurrently for you.
- If you use `StdoutPipe`, read it to completion **before** calling `Wait`.
- Never call `Wait` before draining every pipe you created.

Also: `cmd.Output()` populates `(*exec.ExitError).Stderr` with up to 32 KB of the child's
stderr, which makes error messages dramatically more useful. Use `Output`, not
`CombinedOutput`, when you need to distinguish the streams.

**`CommandContext` kills, it does not clean up.** When the context is cancelled,
`CommandContext` calls `cmd.Process.Kill()` — SIGKILL on Unix, `TerminateProcess` on
Windows. That kills *the child* but not its grandchildren, because `Kill` targets a single
PID. A `make` that spawned a compiler leaves the compiler running. Chapter 9 solves this
properly with process groups; for `doctor`, whose children are all short-lived
`--version` calls, single-PID kill is adequate. Note the limitation.

Go 1.20 added `cmd.Cancel` and `cmd.WaitDelay`, which are worth knowing:

```go
cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
cmd.WaitDelay = 5 * time.Second  // then force-kill
```

That gives a graceful signal first and a hard kill after a grace period, and it also
bounds how long `Wait` will block on a child that has exited but whose pipes are still
held open by a grandchild. That last case — `Wait` hanging forever because a grandchild
inherited stdout — is a real production hang, and `WaitDelay` is the fix.

**Zombies.** On Unix, a child that has exited but not been reaped stays in the process
table. `cmd.Wait()` reaps it. **Every `Start` needs a matching `Wait`**, including on
error paths. `Output()` and `Run()` do this for you.

### Parsing version output

`go version` prints `go version go1.24.0 darwin/arm64`. `git --version` prints
`git version 2.47.1`. `docker --version` prints `Docker version 27.4.0, build bde2b89`.
`node --version` prints `v22.12.0`. `python3 --version` printed to **stderr** before
Python 3.4.

There's no standard. A regex for a semver-ish token is the pragmatic answer:

```go
var versionRe = regexp.MustCompile(`\b(\d+)\.(\d+)(?:\.(\d+))?\b`)
```

Combined with per-tool overrides where the generic regex picks the wrong number (e.g.
`openssl version` → `OpenSSL 3.4.0 22 Oct 2024`, where the year could match).

### `log/slog` and the logs-vs-output distinction

Go 1.21 added `log/slog` to the standard library. It gives you structured, levelled
logging with pluggable handlers and no dependency.

```go
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
logger.Debug("probing tool", "name", "go", "path", path)
logger.Error("probe failed", "name", "docker", "err", err)
```

**The rule that matters more than the API:**

| | Goes to | Audience | Format |
|---|---|---|---|
| **Command output** | stdout | the user and their pipes | stable, parseable, documented |
| **Diagnostics** | stderr | the user when debugging | human-readable, may change |
| **Logs** | stderr, gated by `-v`/`--debug` | you, when they file a bug | structured, verbose |

Concretely: `gecko doctor` writes its report to stdout. If Docker's probe times out, the
*fact* that Docker is unavailable goes to stdout (it's part of the report). The
*reason* — "context deadline exceeded after 2s running /usr/bin/docker --version" — goes
to a debug log that only appears with `-v`.

The failure mode to avoid: logging to stdout. `gecko find "*.go" | wc -l` breaks the
moment a log line lands in the pipe. **Never log to stdout from a CLI.**

Add a logger to `Env`:

```go
type Env struct {
    ...
    Log *slog.Logger
}
```

with a default of `slog.New(discardHandler{})` — or in Go 1.24+, `slog.DiscardHandler`.

### The Cobra decision

Nine commands, nesting two deep, plugins coming. Time to decide honestly.

**What Cobra actually provides:**
- Command tree with automatic help and usage generation.
- `pflag`: POSIX/GNU-style flags — `--flag`, `-f`, `-abc` combining, `--flag=value`,
  and crucially proper long/short aliasing (our `--depth`/`-L` hack disappears).
- Shell completion generation for bash, zsh, fish and PowerShell. This is genuinely
  hard to write yourself and is worth real money in UX terms.
- Command suggestion on typos ("did you mean `serve`?") via Levenshtein distance.
- Hooks: `PersistentPreRun`, `PreRun`, `PostRun` at each level.
- `Args` validators: `cobra.ExactArgs(1)`, `MinimumNArgs`, etc.

**What Cobra costs:**
- ~14 direct + transitive dependencies (`spf13/pflag`, `inconshreveable/mousetrap`,
  and for completions a chunk more).
- A `*cobra.Command` has ~60 fields. Understanding failure modes means reading its source.
- It owns your `main`. `rootCmd.Execute()` calls `os.Exit` internally unless you're
  careful with `SilenceErrors`/`SilenceUsage` — the exact pattern chapter 1 warned about.
- Its `RunE` signature is `func(*cobra.Command, []string) error`, with no `context.Context`
  parameter (you get it via `cmd.Context()`, set by `ExecuteContext`). Fine, but a
  footgun for anyone who doesn't know.

**What Cobra is doing internally**, since the brief asks: `Find(args)` walks the command
tree matching `args[0]` against each level's children (including `Aliases`), returning the
deepest match plus remaining args — essentially our `runCommand` loop. Flag parsing is
`pflag.FlagSet.Parse`, which differs from `flag` in accepting `--name=value`,
`-abc` as `-a -b -c`, and interspersing flags with positional arguments (our stdlib
version stops at the first positional). Help is a `text/template` executed against the
command, which is why you customise it with `SetHelpTemplate`.

**Our decision: keep the hand-rolled dispatcher.**

Reasoning: the dispatcher is 150 lines we understand completely, it already satisfies the
plugin requirement (which Cobra would also satisfy, via `AddCommand` at runtime), and its
one real deficiency — flag syntax — is fixable by adopting `spf13/pflag` alone, without
Cobra. `pflag` is a drop-in `flag` replacement with three dependencies.

**What would change the decision:** shell completion. Writing correct zsh completion for
a dynamic command tree is a week of work and Cobra does it in one line. If you want
completion, take Cobra. We'll implement a simplified completion in chapter 12 and you can
compare.

This is the honest answer, not the "libraries are bad" answer. Note that the value of
having built it ourselves isn't that our version is better — it's that we can now read
Cobra's source and recognise every piece.

Adopt `pflag`:

```bash
go get github.com/spf13/pflag
```

The migration is mechanical: `flag.NewFlagSet` → `pflag.NewFlagSet`, and
`fs.IntVar(&d, "depth", 0, "")` + `fs.IntVar(&d, "L", 0, "")` becomes
`fs.IntVarP(&d, "depth", "L", 0, "")`. One line instead of two, and help output stops
listing the alias separately.

---

## D. Design

### `doctor`: a probe is a value

```go
// Probe describes one thing doctor knows how to check.
type Probe struct {
    Name       string
    Binary     string        // executable to find on PATH
    Args       []string      // typically {"--version"}
    Parse      func(stdout, stderr string) (string, error)
    MinVersion string        // semver-ish; empty = no minimum
    Optional   bool          // missing is fine, not an issue
}
```

Data, not code. Adding a tool is a struct literal. The `Parse` function is the escape
hatch for tools with unusual output, and `nil` means "use the generic regex".

### Concurrency and timeouts

Every probe runs in its own goroutine with its own `context.WithTimeout`. Two-second
budget per probe; the whole set is bounded by that plus scheduling.

Why a per-probe timeout rather than one global one? Because a hung `docker` (very common
when the daemon isn't running — `docker --version` is usually fine but `docker info`
hangs for 30s) must not consume the budget for `git`. Independent failure domains get
independent deadlines.

```go
g, gctx := errgroup.WithContext(ctx)
g.SetLimit(runtime.NumCPU() * 2)
results := make([]Result, len(probes))
for i, p := range probes {
    i, p := i, p
    g.Go(func() error {
        pctx, cancel := context.WithTimeout(gctx, 2*time.Second)
        defer cancel()
        results[i] = runProbe(pctx, p)
        return nil   // a failed probe is a result, not an error
    })
}
g.Wait()
```

`return nil` always. A probe failure is data the report displays, not a reason to abort.
`errgroup`'s error path is reserved for cancellation. Getting this backwards is the most
common `errgroup` misuse.

### `project`: detection by evidence

```go
type Detector struct {
    Language string
    Files    []string   // any one present in the root
    Weight   int        // higher wins ties
}
```

`go.mod` → Go. `Cargo.toml` → Rust. `package.json` → JavaScript/TypeScript (refine by
looking for `tsconfig.json`). `pyproject.toml`/`setup.py`/`requirements.txt` → Python.

A repository can be several things at once. Report the primary (highest weight with a
match) and list secondaries. Don't over-engineer: the brief says so explicitly, and a
project detector that tries to be a language classifier is a rabbit hole.

Line counting: count lines in files with known source extensions, skipping the ignore
list. This is the third caller of chapter 5's `Walker` — good, that's the extraction
earning its keep.

Git information: shell out to `git`, or parse `.git/HEAD` directly? Parsing is faster and
dependency-free for the branch name (`.git/HEAD` contains `ref: refs/heads/main`), but
`git status --porcelain` for changes is not reasonably reimplementable. **Decision: parse
`.git/HEAD` for the branch, exec `git` for status, degrade gracefully if `git` is absent.**
A hybrid, chosen because the cheap half is genuinely cheap.

---

## E. Implementation

### `internal/process/run.go`

This is a new package. `doctor`, `project`, `watch` and `run` all execute subprocesses,
so the shared mechanics belong in one place.

```go
// Package process wraps os/exec with the safety properties Gecko needs:
// bounded execution time, guaranteed reaping, and output capture that
// cannot deadlock.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Result is the outcome of running a command to completion.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	TimedOut bool
}

// Output runs name with args and captures both streams.
//
// It uses cmd.Output rather than pipes because Output arranges
// concurrent reads internally; a hand-rolled StdoutPipe + Wait
// deadlocks as soon as the child writes more than the pipe buffer.
func Output(ctx context.Context, name string, args ...string) (Result, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, name, args...)

	// WaitDelay bounds how long Wait blocks after the context is
	// cancelled. Without it, a child that exited but left a grandchild
	// holding stdout open makes Wait hang indefinitely.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Deliberately not inheriting the parent's environment wholesale
	// here would break tools that need PATH and HOME; we inherit, but
	// callers that care can set cmd.Env themselves via RunOptions.
	err := cmd.Run()

	res := Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	switch {
	case err == nil:
		res.ExitCode = 0
		return res, nil

	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = -1
		return res, fmt.Errorf("%s: timed out after %s", name, res.Duration.Round(time.Millisecond))

	case errors.Is(ctx.Err(), context.Canceled):
		res.ExitCode = -1
		return res, ctx.Err()

	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The process ran and failed. That is a result, not an
			// error condition for our purposes; the caller inspects
			// ExitCode. We still return err so callers that only
			// check err behave sensibly.
			res.ExitCode = ee.ExitCode()
			return res, fmt.Errorf("%s exited %d: %s", name, res.ExitCode, firstLine(res.Stderr))
		}
		// Could not start at all: not found, not executable, etc.
		res.ExitCode = -1
		return res, fmt.Errorf("%s: %w", name, err)
	}
}

// Exists reports whether name resolves to an executable on PATH.
//
// Since Go 1.19 LookPath refuses to resolve a bare name relative to the
// current directory on Windows, which closes a privilege-escalation
// path where running a tool inside an untrusted directory would execute
// a planted binary of the same name.
func Exists(name string) (string, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return path, true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}
```

### `internal/doctor/doctor.go`

```go
package doctor

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yourname/gecko/internal/process"
)

type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusMissing
	StatusError
)

// Probe describes a tool doctor knows how to check. Probes are data so
// that adding a tool is a struct literal rather than code.
type Probe struct {
	Name       string
	Binary     string
	Args       []string
	MinVersion string
	Optional   bool
	// Parse extracts a version from the tool's output. nil uses the
	// generic semver-ish regex, which handles most tools.
	Parse func(stdout, stderr string) (string, error)
}

type Result struct {
	Probe    Probe
	Status   Status
	Version  string
	Path     string
	Detail   string
	Duration time.Duration
}

// DefaultProbes is the built-in tool list.
var DefaultProbes = []Probe{
	{Name: "Go", Binary: "go", Args: []string{"version"}, MinVersion: "1.22"},
	{Name: "Git", Binary: "git", Args: []string{"--version"}, MinVersion: "2.30"},
	{Name: "Docker", Binary: "docker", Args: []string{"--version"}, Optional: true},
	{Name: "Node", Binary: "node", Args: []string{"--version"}, Optional: true},
	{Name: "Python", Binary: pythonBinary(), Args: []string{"--version"}, Optional: true},
	{Name: "Rust", Binary: "rustc", Args: []string{"--version"}, Optional: true},
	{Name: "Make", Binary: "make", Args: []string{"--version"}, Optional: true},
	{Name: "curl", Binary: "curl", Args: []string{"--version"}, Optional: true},
}

func pythonBinary() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}

// probeTimeout bounds each probe independently. A hung docker daemon
// must not consume the budget available to git.
const probeTimeout = 2 * time.Second

// Run executes every probe concurrently and returns results in the
// original order.
func Run(ctx context.Context, probes []Probe) []Result {
	results := make([]Result, len(probes))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.NumCPU() * 2)

	for i, p := range probes {
		i, p := i, p
		g.Go(func() error {
			results[i] = runProbe(gctx, p)
			// Always nil: a failed probe is a reportable result, not a
			// reason to abandon the other probes. errgroup's error
			// channel is reserved for cancellation.
			return nil
		})
	}
	_ = g.Wait()
	return results
}

func runProbe(ctx context.Context, p Probe) Result {
	res := Result{Probe: p}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	path, ok := process.Exists(p.Binary)
	if !ok {
		res.Status = StatusMissing
		if p.Optional {
			res.Detail = "not installed"
		} else {
			res.Detail = "not found on PATH"
		}
		return res
	}
	res.Path = path

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := process.Output(pctx, path, p.Args...)
	if err != nil && out.ExitCode != 0 {
		res.Status = StatusError
		res.Detail = err.Error()
		return res
	}

	parse := p.Parse
	if parse == nil {
		parse = genericVersion
	}
	ver, perr := parse(out.Stdout, out.Stderr)
	if perr != nil {
		res.Status = StatusWarn
		res.Detail = "installed, version unrecognised"
		return res
	}
	res.Version = ver

	if p.MinVersion != "" && compareVersions(ver, p.MinVersion) < 0 {
		res.Status = StatusWarn
		res.Detail = fmt.Sprintf("%s+ recommended", p.MinVersion)
		return res
	}
	res.Status = StatusOK
	return res
}

// versionRe matches a semver-ish token. Tools have no common format:
//
//	go version go1.24.0 darwin/arm64
//	git version 2.47.1
//	Docker version 27.4.0, build bde2b89
//	v22.12.0
//
// so we take the first N.N[.N] we find. Python <3.4 printed to stderr,
// which is why both streams are searched.
var versionRe = regexp.MustCompile(`\b(\d+)\.(\d+)(?:\.(\d+))?\b`)

func genericVersion(stdout, stderr string) (string, error) {
	for _, s := range []string{stdout, stderr} {
		if m := versionRe.FindString(s); m != "" {
			return m, nil
		}
	}
	return "", fmt.Errorf("no version found")
}

// compareVersions compares dotted numeric versions. It is not a full
// semver implementation: no pre-release tags, no build metadata. That is
// sufficient for tool version checks and avoids a dependency. Chapter 14
// needs real semver and will take one.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		ai, bi := 0, 0
		if i < len(as) {
			ai, _ = strconv.Atoi(numericPrefix(as[i]))
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(numericPrefix(bs[i]))
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

func numericPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
```

The `compareVersions` comment is doing real work: it tells the next reader that this is a
deliberate simplification with a known scope, and where the real thing lives. A
simplification you've documented is engineering; one you haven't is a bug waiting.

### Rendering

Keep rendering out of the doctor package. `internal/cli/doctor.go` formats results with
`tabwriter` and status glyphs, falling back to ASCII when the terminal can't do Unicode
(chapter 12 makes that detection real; for now, a `--ascii` flag).

```go
func glyph(s doctor.Status, ascii bool) string {
	switch s {
	case doctor.StatusOK:
		if ascii { return "+" }
		return "✓"
	case doctor.StatusWarn:
		if ascii { return "!" }
		return "⚠"
	default:
		if ascii { return "-" }
		return "✗"
	}
}
```

### Exit code policy for `doctor`

`gecko doctor` in CI should fail if a required tool is missing. But warnings shouldn't
fail a normal interactive run.

**Decision:** exit 0 always by default; `--strict` exits 1 on any warning or missing
required tool. That keeps the default friendly and makes the CI use explicit.

---

## F. Exercise

1. Add a `Parse` override for a tool the generic regex gets wrong. `openssl version`
   outputs `OpenSSL 3.4.0 22 Oct 2024` — verify whether the generic regex picks `3.4.0`
   or something else, and write the override if needed.

2. Implement `gecko project stats`: file count, line count, test-file count, breakdown by
   extension. Use the `Walker` from chapter 5. Then benchmark it against a large repo and
   decide whether line counting should be concurrent. (Hint: it's read-heavy and the
   files are small. Measure before you decide.)

3. Migrate the codebase from `flag` to `pflag`. It's mechanical, but do it as its own
   commit, and note which commands' help output improves.

4. Add `-v`/`--verbose` as a persistent flag that sets `Env.Log` to a `slog.Logger` at
   debug level writing to stderr. The hard part is "persistent": our dispatcher parses
   flags per-level. Where does a global flag get parsed?

---

## G. Testing

### Testing code that runs subprocesses

Three strategies, in increasing order of realism and cost.

**1. Inject a fake runner.** Define the interface at the consumer:

```go
// runner exists so doctor's logic can be tested without subprocesses.
// Declared here rather than in package process, per the "interfaces
// where consumed" rule.
type runner interface {
	Output(ctx context.Context, name string, args ...string) (process.Result, error)
	Exists(name string) (string, bool)
}
```

Then `Run(ctx, probes, r runner)`. Tests pass a map-backed fake. Fast, deterministic,
covers all the parsing logic. Doesn't prove `os/exec` is used correctly.

**2. `TestMain` re-execution.** The trick the standard library uses: the test binary
re-executes itself with a marker environment variable and behaves as the fake subprocess.

```go
func TestMain(m *testing.M) {
	if os.Getenv("GECKO_TEST_HELPER") != "" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

func helperMain() {
	switch os.Getenv("GECKO_TEST_HELPER") {
	case "version":
		fmt.Println("faketool version 1.2.3")
	case "hang":
		select {} // never returns; used to test timeouts
	case "bigoutput":
		// 1 MiB, far more than a pipe buffer: proves no deadlock
		buf := bytes.Repeat([]byte("x"), 1<<20)
		os.Stdout.Write(buf)
	case "fail":
		fmt.Fprintln(os.Stderr, "something broke")
		os.Exit(3)
	}
	os.Exit(0)
}

func helperCommand(t *testing.T, mode string) (string, []string, []string) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe, []string{"-test.run=TestHelperNoop"}, []string{"GECKO_TEST_HELPER=" + mode}
}
```

This gives you a **real subprocess** whose behaviour you fully control, on every platform,
with no fixtures to install. It is the correct way to test `os/exec` usage and it's worth
learning properly.

**3. Real tools with skips.** `if _, ok := process.Exists("git"); !ok { t.Skip(...) }`.
Use sparingly — a test that skips in CI provides no signal.

### The tests that matter

```go
func TestOutputNoDeadlockOnLargeOutput(t *testing.T) {
	exe, args, env := helperCommand(t, "bigoutput")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 1<<20 {
		t.Errorf("got %d bytes, want %d", buf.Len(), 1<<20)
	}
}

func TestOutputTimesOut(t *testing.T) {
	exe, args, env := helperCommand(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %s; the timeout did not work", elapsed)
	}
}

func TestOutputCapturesExitCode(t *testing.T) { /* mode "fail" → ExitCode 3, Stderr set */ }
```

The large-output test is the one that catches the pipe deadlock. Write it once and it
protects you forever.

### Doctor logic tests

```go
func TestGenericVersion(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, stdout, stderr, want string }{
		{"go", "go version go1.24.0 darwin/arm64\n", "", "1.24.0"},
		{"git", "git version 2.47.1\n", "", "2.47.1"},
		{"docker", "Docker version 27.4.0, build bde2b89\n", "", "27.4.0"},
		{"node", "v22.12.0\n", "", "22.12.0"},
		{"python legacy on stderr", "", "Python 2.7.18\n", "2.7.18"},
		{"two-part", "make 4.4\n", "", "4.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := genericVersion(tt.stdout, tt.stderr)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("= %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct{ a, b string; want int }{
		{"1.24.0", "1.22", 1},
		{"1.22", "1.24.0", -1},
		{"1.22.0", "1.22", 0},
		{"1.9", "1.10", -1},   // the classic string-comparison trap
		{"2.0", "1.99.99", 1},
	}
	...
}
```

The `1.9` vs `1.10` case is there deliberately: string comparison says `"1.9" > "1.10"`,
numeric comparison says the opposite. Every version comparator needs this test.

---

## H. Review

- `exec.Command` uses `execve` with argv, so no shell and no injection — and what
  changes if you invoke `sh -c`.
- The pipe deadlock: why `StdoutPipe` + `Wait` before reading hangs at 64 KB.
- `CommandContext` kills one PID, not a process tree; `Cancel` and `WaitDelay` and what
  they solve.
- Why every `Start` needs a `Wait` (zombie reaping).
- Go 1.19's Windows `LookPath` change and the vulnerability it closed.
- Per-task timeouts as independent failure domains.
- `errgroup`: return `nil` for expected failures, reserve errors for cancellation.
- stdout vs stderr vs logs, and why logging to stdout breaks pipes.
- What Cobra does internally, what it costs, and a defensible reason to decline it.
- `TestMain` self-re-execution for testing subprocess code portably.

---

## I. Refactoring

`doctor.Run` takes `[]Probe` and calls `process.Output` directly, which makes the
exercise-1 fake impossible. Introduce the `runner` interface **now that there is a
concrete second implementation** (the test fake).

Note the pattern repeating: chapter 2 rejected an interface (one implementation),
chapter 5 accepted an extraction (three callers), chapter 6 accepts an interface (a real
test double). The rule isn't "avoid interfaces" — it's **an interface needs at least two
real implementations, and a test fake counts.**

Second: `internal/doctor` is a new top-level package containing one exported function.
Is that justified, or should it live in `internal/cli`? Argue both sides.

My view: justified, because it has meaningful logic (probing, parsing, comparison)
testable without the CLI, and because chapter 13's plugins will want to contribute
probes. But it's genuinely marginal, and if you'd rather keep it in `cli` until the
plugin need materialises, that's defensible too. **Being able to argue both sides of a
package-boundary question is the skill; there isn't always a right answer.**

---

## Commit

```
feat: add process package with safe subprocess execution
feat: add doctor command with concurrent tool probing
feat: add project command with language detection and stats
refactor: migrate from flag to pflag for POSIX-style flags
feat: add structured logging with --verbose
```

Next: `07-serve.md`.

# Chapter 1 — Command Dispatch

```
Difficulty:   Intermediate
Est. time:    3–4 hours
Main concepts: flag.FlagSet, dependency injection via io.Writer, sentinel and typed
               errors, errors.Is/As, exit-code policy, text/tabwriter, ldflags
               version stamping, debug.ReadBuildInfo
Prerequisites: Go fundamentals
```

---

## A. Goal

```
$ gecko
Gecko — a developer toolbox

Usage:  gecko <command> [arguments] [flags]

Commands:
  help      Show help for a command
  version   Print version information

Run "gecko help <command>" for more information.

$ gecko version
gecko 0.1.0-dev (go1.24.0, linux/amd64)

$ gecko nope
gecko: unknown command "nope"
Run "gecko help" for available commands.
$ echo $?
2
```

Small output. Large amount of design.

---

## B. Why this matters

The dispatcher is the single component every future feature touches. Three future
requirements constrain today's design, and none of them are visible yet:

1. **Testability.** By chapter 7 you'll want to assert on the exact bytes `gecko serve`
   prints, in a test that runs `t.Parallel()`. If commands write to `os.Stdout`, that
   test is impossible without global mutation.
2. **Runtime command injection.** By chapter 13, `gecko-docker` found on `$PATH` must
   appear in `gecko help` and route `gecko docker ps`. The dispatcher cannot know that
   command at compile time, and must not special-case plugins.
3. **Exit-code semantics.** Shell scripts will branch on Gecko's exit status. "Bad
   usage" and "port in use" deserve different codes, and the dispatcher must derive
   that without importing every command's error types.

Get the `Command` shape wrong and you fight it for fourteen chapters.

---

## C. Concepts

### `flag.FlagSet` vs the package-level `flag` functions

`flag.String("port", ...)` registers on `flag.CommandLine`, a package-level singleton.
That's fine for a single-purpose tool and disqualifying for us:

- A singleton can't hold two commands' flags without collision.
- Tests that parse flags mutate global state, so they can't run in parallel.
- `flag.CommandLine` calls `os.Exit(2)` on parse error by default, which kills your
  test binary.

We use `flag.NewFlagSet(name, flag.ContinueOnError)` per invocation. `ContinueOnError`
makes `Parse` return an error instead of exiting. We also set `fs.SetOutput(io.Discard)`
and print usage ourselves, because the default error text goes to stderr uncontrollably.

### Why `os.Exit` must appear exactly once

`os.Exit` does not run deferred functions. Not the ones in the calling function, not
the ones anywhere up the stack. A command that does:

```go
f, _ := os.Create(path)
defer f.Close()
...
os.Exit(1)   // f is never flushed or closed
```

silently truncates. The discipline: **only `main` calls `os.Exit`, and it is the last
statement.** Everything else returns an `error`.

### Sentinel errors vs typed errors

Two kinds of error, two jobs.

A **sentinel** is a package-level comparable value:

```go
var ErrUsage = errors.New("invalid usage")
```

Use it when the only thing a caller needs is "which category is this?" Compare with
`errors.Is`, never `==` — `errors.Is` unwraps wrapped errors, `==` does not.

A **typed error** carries data:

```go
type ExitError struct {
    Code int
    Err  error
}
```

Extract with `errors.As`. Our dispatcher will use `errors.As` to pull an explicit exit
code out of an error chain when a command wants one, and fall back to `errors.Is`
category matching otherwise. This is how the dispatcher stays ignorant of command
internals.

### Wrapping

`fmt.Errorf("read config: %w", err)` preserves the chain; `%v` severs it. Rule of thumb
for this project: **wrap when you add context, sever when you're deliberately hiding an
implementation detail from callers.**

### `text/tabwriter`

Aligning help output with `%-10s` breaks the moment a command name exceeds your guess.
`tabwriter` computes column widths from the data:

```go
w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
fmt.Fprintf(w, "  %s\t%s\n", name, short)
w.Flush()   // nothing is written until Flush
```

The `\t` is the column separator, not a literal tab in the output.

---

## D. Design

### The `Command` shape: struct or interface?

Both work. Compare against requirement 2 (runtime injection).

**Interface:**
```go
type Command interface {
    Name() string
    Short() string
    Run(ctx context.Context, env *Env, args []string) error
}
```
Every command needs a concrete type with four methods. A plugin command discovered at
runtime needs a `pluginCommand` struct — fine, but each core command becomes ~20 lines
of boilerplate before it does anything.

**Struct with function fields:**
```go
type Command struct {
    Name  string
    Short string
    Run   func(ctx context.Context, env *Env, args []string) error
}
```
A plugin command is just a struct literal with a closure. A core command is a struct
literal with a closure. There is no difference between them, which is exactly what
requirement 2 asks for.

**Decision: struct with function fields.** The interface would be the right call if
commands had rich, varied behaviour beyond "run". They don't.

### The factory problem

Naive registration stores a `*Command` built once at init:

```go
func newTreeCommand() *Command {
    var depth int                       // captured by both closures below
    return &Command{
        Flags: func(fs *flag.FlagSet) { fs.IntVar(&depth, "depth", 0, "") },
        Run:   func(...) error { _ = depth; ... },
    }
}
```

`depth` is shared by every invocation of that command. In a test running two
invocations in parallel, that is a data race — and `go test -race` will say so.

**Fix: register factories, not commands.**

```go
type CommandFunc func() *Command
```

The registry holds `map[string]CommandFunc`. Every dispatch calls the factory, getting
fresh closure variables. Help output calls the factories too — they're trivially cheap.

This also solves plugins for free: a plugin's factory closes over the discovered binary
path.

### Injecting the environment

```go
type Env struct {
    Stdin   io.Reader
    Stdout  io.Writer
    Stderr  io.Writer
    Getenv  func(string) string
    WorkDir string
}
```

Not just writers — `Getenv` and `WorkDir` too, because chapter 3's config resolution
and chapter 6's `doctor` both need to be tested against a fake environment. Injecting
a function rather than a `map[string]string` keeps the production case zero-cost
(`os.Getenv` directly).

### Where flags are parsed

**In the dispatcher, using the command's registration.** The command declares its flags
via `Flags func(*flag.FlagSet)`; the dispatcher constructs the `FlagSet`, calls that
hook, parses, and hands `fs.Args()` to `Run`. Why not inside `Run`?

- Uniform `--help` handling: the dispatcher can intercept `-h` before `Run` executes.
- Uniform error mapping: a parse failure becomes `ErrUsage` in one place.
- The help renderer needs the `FlagSet` to print flag documentation *without* running
  the command.

### Nesting and argument slicing

```go
type Command struct {
    ...
    Sub map[string]CommandFunc
}
```

Resolution is a loop, not recursion: parse this level's flags, take `args[0]` as the
next subcommand name if it exists in `Sub`, descend with `args[1:]`. **The dispatcher
owns all slicing.** A command never inspects `os.Args`.

One subtlety: `gecko run test -v` — is `-v` Gecko's or the task's? Standard Go flag
parsing stops at the first non-flag argument, so `-v` after `test` lands in the
subcommand's flags, which is what you want. For `gecko watch -- go test ./...`, the
bare `--` terminates flag parsing and everything after is a positional argument.
`flag` handles this natively.

### Exit codes

```
0    success
1    generic runtime failure
2    usage error (unknown command, bad flag, missing argument)
3-125 command-specific (rare; must be documented)
130  interrupted (SIGINT) — 128 + 2, by shell convention
```

Mapping logic, in priority order:
1. `nil` → 0
2. `errors.Is(err, context.Canceled)` → 130
3. `errors.As(err, &ExitError{})` → its `Code`
4. `errors.Is(err, ErrUsage)` → 2
5. otherwise → 1

Commands only need `ErrUsage` or a plain error in 95% of cases.

---

## E. Implementation

### `internal/cli/cli.go`

```go
// Package cli implements Gecko's command registry, argument dispatch and
// help rendering. It is intentionally free of any feature logic: commands
// live in their own files and depend on domain packages, never the reverse.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// ErrUsage marks an error as the user's fault: a bad flag, an unknown
// command, a missing argument. The dispatcher maps it to exit code 2 and
// prints command usage alongside the message.
var ErrUsage = errors.New("invalid usage")

// ExitError lets a command choose its own process exit status. Use it
// sparingly and document any code you introduce.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Env is the ambient environment a command runs in. Everything a command
// might otherwise reach for as a global lives here, which is what makes
// commands testable in parallel.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	WorkDir string
}

// OSEnv returns an Env bound to the real process.
func OSEnv() *Env {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return &Env{
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		WorkDir: wd,
	}
}

// Command is a single node in the command tree. It is a plain struct with
// function fields rather than an interface so that commands discovered at
// runtime (plugins, chapter 13) are indistinguishable from compiled-in ones.
type Command struct {
	Name  string // single word, as typed by the user
	Short string // one line, shown in command lists
	Long  string // optional paragraph, shown by "gecko help <cmd>"
	Usage string // argument spec, e.g. "gecko tree [path]"

	// Flags registers this command's flags. It is called once per
	// invocation with a fresh FlagSet.
	Flags func(fs *flag.FlagSet)

	// Run executes the command. args are the positional arguments left
	// after flag parsing.
	Run func(ctx context.Context, env *Env, args []string) error

	// Sub holds child commands, keyed by name.
	Sub map[string]CommandFunc

	Hidden bool
}

// CommandFunc constructs a Command. Registering factories rather than
// values gives every invocation its own closure variables; sharing them
// across parallel invocations would be a data race.
type CommandFunc func() *Command

// App is the root of the command tree.
type App struct {
	Name     string
	Short    string
	commands map[string]CommandFunc
}

func New() *App {
	a := &App{
		Name:     "gecko",
		Short:    "a developer toolbox",
		commands: make(map[string]CommandFunc),
	}
	a.Register(newHelpCommand(a))
	a.Register(newVersionCommand)
	return a
}

// Register adds a top-level command. It may be called after construction,
// which is how plugin commands are injected in chapter 13.
func (a *App) Register(f CommandFunc) {
	c := f()
	a.commands[c.Name] = f
}

// names returns visible top-level command names, sorted.
func (a *App) names() []string {
	out := make([]string, 0, len(a.commands))
	for name, f := range a.commands {
		if f().Hidden {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Execute resolves args against the command tree and runs the result.
// args excludes the program name.
func (a *App) Execute(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return a.printRootHelp(env.Stdout)
	}

	switch args[0] {
	case "-h", "--help":
		return a.printRootHelp(env.Stdout)
	case "-v", "--version":
		args = []string{"version"}
	}

	name := args[0]
	f, ok := a.commands[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "gecko: unknown command %q\n", name)
		fmt.Fprintf(env.Stderr, "Run \"gecko help\" for available commands.\n")
		return ErrUsage
	}

	return runCommand(ctx, env, f(), []string{a.Name}, args[1:])
}

// runCommand parses one level's flags, descends into a subcommand if the
// first positional argument names one, and otherwise invokes Run.
// path is the chain of names consumed so far, used for help text.
func runCommand(ctx context.Context, env *Env, c *Command, path []string, args []string) error {
	path = append(path, c.Name)
	full := strings.Join(path, " ")

	fs := flag.NewFlagSet(full, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we render all diagnostics ourselves
	fs.Usage = func() {}

	if c.Flags != nil {
		c.Flags(fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandHelp(env.Stdout, c, full, fs)
			return nil
		}
		fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
		printCommandHelp(env.Stderr, c, full, fs)
		return ErrUsage
	}

	rest := fs.Args()

	// Descend if the next positional argument names a subcommand.
	if len(rest) > 0 && c.Sub != nil {
		if sub, ok := c.Sub[rest[0]]; ok {
			return runCommand(ctx, env, sub(), path, rest[1:])
		}
	}

	if c.Run == nil {
		// A pure grouping command such as "gecko plugin".
		if len(rest) > 0 {
			fmt.Fprintf(env.Stderr, "gecko: unknown subcommand %q for %q\n", rest[0], full)
			return ErrUsage
		}
		printCommandHelp(env.Stdout, c, full, fs)
		return ErrUsage
	}

	return c.Run(ctx, env, rest)
}

// Main is the single entry point that translates errors into exit codes.
// It never panics out and never calls os.Exit itself.
func Main(ctx context.Context, args []string, env *Env) int {
	app := New()
	err := app.Execute(ctx, env, args)
	return ExitCode(err, env)
}

// ExitCode maps an error to a process exit status. The ordering matters:
// an explicit ExitError beats a category match.
func ExitCode(err error, env *Env) int {
	switch {
	case err == nil:
		return 0

	case errors.Is(err, context.Canceled):
		return 130 // 128 + SIGINT

	default:
		var ee *ExitError
		if errors.As(err, &ee) {
			if ee.Err != nil {
				fmt.Fprintf(env.Stderr, "gecko: %v\n", ee.Err)
			}
			return ee.Code
		}
		if errors.Is(err, ErrUsage) {
			// The message was already printed at the point of failure;
			// ErrUsage itself carries no user-facing text.
			if !errors.Is(err, ErrUsage) || err.Error() != ErrUsage.Error() {
				fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
			}
			return 2
		}
		fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
		return 1
	}
}
```

Two details worth pausing on.

`fs.SetOutput(io.Discard)` plus `fs.Usage = func() {}` disables everything the `flag`
package would print on its own. We want every byte of output to come from our code so
tests can assert on it and so `--help` goes to stdout while errors go to stderr.

The `ErrUsage` branch in `ExitCode` has an awkward guard. Sometimes a command returns
bare `ErrUsage` after printing its own message (don't print again); sometimes it returns
`fmt.Errorf("tree: %w", ErrUsage)` with real text (do print). We'll clean this up in
section I.

### `internal/cli/help.go`

```go
package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

func (a *App) printRootHelp(out io.Writer) error {
	fmt.Fprintf(out, "Gecko — %s\n\n", a.Short)
	fmt.Fprintf(out, "Usage:  gecko <command> [arguments] [flags]\n\n")
	fmt.Fprintf(out, "Commands:\n")

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	for _, name := range a.names() {
		c := a.commands[name]()
		fmt.Fprintf(w, "  %s\t%s\n", c.Name, c.Short)
	}
	w.Flush()

	fmt.Fprintf(out, "\nRun \"gecko help <command>\" for more information.\n")
	return nil
}

func printCommandHelp(out io.Writer, c *Command, full string, fs *flag.FlagSet) {
	if c.Long != "" {
		fmt.Fprintf(out, "%s\n\n", c.Long)
	} else if c.Short != "" {
		fmt.Fprintf(out, "%s\n\n", c.Short)
	}

	usage := c.Usage
	if usage == "" {
		usage = full
	}
	fmt.Fprintf(out, "Usage:  %s\n", usage)

	if len(c.Sub) > 0 {
		fmt.Fprintf(out, "\nSubcommands:\n")
		names := make([]string, 0, len(c.Sub))
		for n := range c.Sub {
			names = append(names, n)
		}
		sort.Strings(names)

		w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
		for _, n := range names {
			sc := c.Sub[n]()
			fmt.Fprintf(w, "  %s\t%s\n", sc.Name, sc.Short)
		}
		w.Flush()
	}

	// Only print a Flags section if the command actually declares flags.
	var count int
	fs.VisitAll(func(*flag.Flag) { count++ })
	if count > 0 {
		fmt.Fprintf(out, "\nFlags:\n")
		w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
		fs.VisitAll(func(f *flag.Flag) {
			name := "--" + f.Name
			if len(f.Name) == 1 {
				name = "-" + f.Name
			}
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = fmt.Sprintf(" (default %s)", f.DefValue)
			}
			fmt.Fprintf(w, "  %s\t%s%s\n", name, f.Usage, def)
		})
		w.Flush()
	}
}

func newHelpCommand(a *App) CommandFunc {
	return func() *Command {
		return &Command{
			Name:  "help",
			Short: "Show help for a command",
			Usage: "gecko help [command]",
			Run: func(ctx context.Context, env *Env, args []string) error {
				if len(args) == 0 {
					return a.printRootHelp(env.Stdout)
				}
				f, ok := a.commands[args[0]]
				if !ok {
					fmt.Fprintf(env.Stderr, "gecko: unknown command %q\n", args[0])
					return ErrUsage
				}
				c := f()
				fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				if c.Flags != nil {
					c.Flags(fs)
				}
				printCommandHelp(env.Stdout, c, "gecko "+c.Name, fs)
				return nil
			},
		}
	}
}
```

Note `newHelpCommand` takes the `*App` and returns a `CommandFunc` — a closure over the
app. That's a small circularity (`App` holds a command that holds `App`) but it's
contained, and the alternative (a global registry) is worse.

Add `"context"` to that file's imports.

### `internal/cli/version.go`

Version information should be stamped at build time by the release pipeline, but must
still be useful for `go install`ed builds and `go run`. Both paths:

```go
package cli

import (
	"context"
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
		Flags: func(fs *flagSet) { fs.BoolVar(&short, "short", false, "print only the version number") },
		Run: func(ctx context.Context, env *Env, args []string) error {
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
```

`flagSet` there is a deliberate typo-trap — replace it with `*flag.FlagSet` and add the
`flag` import. (If you pasted without reading, the compiler just taught you something
about reading.)

`debug.ReadBuildInfo` is genuinely useful: since Go 1.18 the toolchain embeds
`vcs.revision`, `vcs.time` and `vcs.modified` automatically when building from a Git
work tree. That means even a plain `go build` gives you commit provenance for free.

### `cmd/gecko/main.go`

```go
// Command gecko is a cross-platform developer toolbox.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/yourname/gecko/internal/cli"
)

func main() {
	// NotifyContext cancels ctx on the first signal and restores default
	// handling on the second, so a wedged command can still be killed with
	// a second Ctrl-C. SIGTERM is defined on Windows in Go's syscall
	// package, so this compiles everywhere.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Main(ctx, os.Args[1:], cli.OSEnv()))
}
```

Ten lines, and it will not grow. Note the tension: `defer stop()` never runs because
`os.Exit` is on the same line. That's acceptable here — `stop` only unregisters signal
handlers and the process is ending — but it's exactly the trap described in section C,
so be deliberate about it. The clean form:

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], cli.OSEnv())
	stop()
	os.Exit(code)
}
```

Build and try it:

```bash
go build -o gecko ./cmd/gecko
./gecko
./gecko version
./gecko nope; echo "exit=$?"
```

---

## F. Exercise

Before reading chapter 2, add a `gecko config` grouping command with no `Run` and two
subcommands, `path` and `show`, both of which just print a placeholder. You should need
to touch nothing in `cli.go`. If you do need to, the design has a hole — find it.

Second, harder: `gecko help version` currently constructs a `FlagSet` in two places
(`runCommand` and `newHelpCommand`). Extract that duplication without introducing a
global.

---

## G. Testing

Create `internal/cli/cli_test.go`.

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// testEnv returns an Env writing into buffers, plus the buffers.
func testEnv(env map[string]string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &out,
		Stderr:  &errb,
		Getenv:  func(k string) string { return env[k] },
		WorkDir: "/tmp",
	}, &out, &errb
}

func TestExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring
		wantStderr string // substring
	}{
		{
			name:       "no args prints root help",
			args:       nil,
			wantCode:   0,
			wantStdout: "Usage:  gecko <command>",
		},
		{
			name:       "version",
			args:       []string{"version"},
			wantCode:   0,
			wantStdout: "gecko ",
		},
		{
			name:       "version short",
			args:       []string{"version", "--short"},
			wantCode:   0,
			wantStdout: "dev",
		},
		{
			name:       "unknown command is a usage error",
			args:       []string{"nope"},
			wantCode:   2,
			wantStderr: `unknown command "nope"`,
		},
		{
			name:       "unknown flag is a usage error",
			args:       []string{"version", "--nope"},
			wantCode:   2,
			wantStderr: "flag provided but not defined",
		},
		{
			name:       "help for a command",
			args:       []string{"help", "version"},
			wantCode:   0,
			wantStdout: "--short",
		},
		{
			name:       "help for unknown command",
			args:       []string{"help", "nope"},
			wantCode:   2,
			wantStderr: "unknown command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel() // safe only because Env is injected

			env, out, errb := testEnv(nil)
			code := Main(context.Background(), tt.args, env)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tt.wantCode, errb)
			}
			if tt.wantStdout != "" && !strings.Contains(out.String(), tt.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tt.wantStdout, out)
			}
			if tt.wantStderr != "" && !strings.Contains(errb.String(), tt.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tt.wantStderr, errb)
			}
		})
	}
}
```

The `t.Parallel()` inside each subtest is the payoff for injecting `Env`. Try changing
one command to write to `os.Stdout` and rerun — the test still passes, which is worse
than failing, because it proves the assertion has silently stopped checking anything.

Also test the exit-code mapper directly:

```go
func TestExitCode(t *testing.T) {
	t.Parallel()
	env, _, _ := testEnv(nil)

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"generic", errors.New("boom"), 1},
		{"usage", ErrUsage, 2},
		{"wrapped usage", fmt.Errorf("tree: %w", ErrUsage), 2},
		{"canceled", context.Canceled, 130},
		{"wrapped canceled", fmt.Errorf("serve: %w", context.Canceled), 130},
		{"explicit", &ExitError{Code: 42, Err: errors.New("nope")}, 42},
		{"explicit beats usage", fmt.Errorf("%w", &ExitError{Code: 7, Err: ErrUsage}), 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCode(c.err, env); got != c.want {
				t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}
```

Run:

```bash
go test ./... -race
go vet ./...
gofmt -l .
```

---

## H. Review

You should now be able to explain:

- Why `flag.CommandLine` is unusable for a multi-command tool, and what
  `flag.ContinueOnError` changes.
- Why `os.Exit` appears exactly once in the whole program.
- The difference between `errors.Is` and `errors.As`, and when each is the right tool.
- Why the registry holds factories rather than command values, in terms of data races.
- Why a struct with function fields beat an interface for `Command`, and what would
  change that verdict.
- How `-ldflags -X` works and what `debug.ReadBuildInfo` gives you without it.

---

## I. Refactoring

Two things are already wrong.

**1. The `ErrUsage` double-print guard is nonsense.** That `if !errors.Is(...) ||
err.Error() != ...` condition is unreadable and fragile. The real problem is that
"usage error" is conflating two concerns: the exit code, and whether a message has
already been shown. Split them:

```go
// usageError signals a user error whose message has already been rendered.
type quietError struct{ err error }

func (q quietError) Error() string { return q.err.Error() }
func (q quietError) Unwrap() error { return q.err }

// Quiet marks err as already reported to the user.
func Quiet(err error) error { return quietError{err} }
```

Then in `ExitCode`:

```go
var q quietError
reported := errors.As(err, &q)
...
if !reported {
    fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
}
```

and `runCommand` returns `Quiet(ErrUsage)` after printing help. Now the guard is one
boolean and the semantics are explicit.

**2. `App.names()` calls every factory to check `Hidden`.** Harmless today, wasteful
once plugin factories do real work. Store a small `commandInfo{name, short, hidden}`
alongside the factory at registration time.

```go
type entry struct {
	f      CommandFunc
	name   string
	short  string
	hidden bool
}
```

Do that now; chapter 13 will thank you.

---

## Commit

```
feat: add CLI dispatcher with version and help commands
test: cover command dispatch and exit-code mapping
```

Why this boundary: the dispatcher plus two commands is the smallest thing that is
independently useful and independently testable. Committing the dispatcher alone would
leave a package with no caller; committing it together with `tree` would mix
infrastructure with a feature.

Next: `02-tree.md`.

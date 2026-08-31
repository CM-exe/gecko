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
	"sync"

	"github.com/CM-exe/gecko/internal/config"
)

// ErrUsage marks an error as the user's fault: a bad flag, an unknown
// command, a missing argument. The dispatcher maps it to exit code 2 and
// prints command usage alongside the message.
var ErrUsage = errors.New("invalid usage")

// usageError signals a user error whose message has already been rendered.
type quietError struct{ err error }

func (q quietError) Error() string { return q.err.Error() }
func (q quietError) Unwrap() error { return q.err }

// Quiet marks err as already reported to the user.
func Quiet(err error) error { return quietError{err} }

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

	configOnce sync.Once
	config     *config.Config
	configErr  error
}

// Config loads Gecko's configuration on first use. Subsequent calls
// return the cached result, including a cached error.
func (e *Env) Config() (*config.Config, error) {
	e.configOnce.Do(func() {
		e.config, e.configErr = config.Load(e.Getenv)
	})
	return e.config, e.configErr
}

// SetConfig overrides the configuration, for tests.
func (e *Env) SetConfig(c *config.Config) {
	e.configOnce.Do(func() {}) // burn the Once so Load never runs
	e.config, e.configErr = c, nil
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

// Invocation carries per-run state that Run needs but that isn't a
// positional argument.
type Invocation struct {
	Args     []string
	Provided map[string]bool // flags the user explicitly set
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
	Run func(ctx context.Context, env *Env, inv *Invocation) error

	// Sub holds child commands, keyed by name.
	Sub map[string]CommandFunc

	Hidden bool
}

// CommandFunc constructs a Command. Registering factories rather than
// values gives every invocation its own closure variables; sharing them
// across parallel invocations would be a data race.
type CommandFunc func() *Command

type entry struct {
	f      CommandFunc
	name   string
	short  string
	hidden bool
}

// App is the root of the command tree.
type App struct {
	Name     string
	Short    string
	commands map[string]entry
}

func New() *App {
	a := &App{
		Name:     "gecko",
		Short:    "a developer toolbox",
		commands: make(map[string]entry),
	}
	a.Register(newHelpCommand(a))
	a.Register(newVersionCommand)
	a.Register(newTreeCommand)
	a.Register(newConfigCommand)
	a.Register(newHashCommand)
	a.Register(newFindCommand)
	a.Register(newCleanCommand)
	return a
}

// Register adds a top-level command. It may be called after construction,
// which is how plugin commands are injected in chapter 13.
func (a *App) Register(f CommandFunc) {
	c := f()
	a.commands[c.Name] = entry{f: f, name: c.Name, short: c.Short, hidden: c.Hidden}
}

// names returns visible top-level command names, sorted.
func (a *App) names() []string {
	out := make([]string, 0, len(a.commands))
	for name, e := range a.commands {
		if e.hidden {
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
		return a.printHelp(env.Stdout)
	case "-v", "--version":
		args = []string{"version"}
	}

	name := args[0]
	e, ok := a.commands[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "gecko: unknown command %q\n", name)
		fmt.Fprintf(env.Stderr, "Run \"gecko help\" for available commands.\n")
		return Quiet(ErrUsage)
	}

	return runCommand(ctx, env, e.f(), []string{a.Name}, args[1:])
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
		return Quiet(ErrUsage)
	}

	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	_ = provided

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
			return Quiet(ErrUsage)
		}
		printCommandHelp(env.Stdout, c, full, fs)
		return Quiet(ErrUsage)
	}

	return c.Run(ctx, env, &Invocation{Args: rest, Provided: provided})
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
		var q quietError
		reported := errors.As(err, &q)
		var ee *ExitError
		if errors.As(err, &ee) {
			if ee.Err != nil {
				fmt.Fprintf(env.Stderr, "gecko: %v\n", ee.Err)
			}
			return ee.Code
		}
		if errors.Is(err, ErrUsage) {
			if !reported {
				fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
			}
			return 2
		}
		fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
		return 1
	}
}

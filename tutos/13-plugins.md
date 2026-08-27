# Chapter 13 — The Plugin System

```
Difficulty:   Expert
Est. time:    10–12 hours
Main concepts: executable plugins vs plugin.so vs RPC, PATH discovery, JSON
               handshake protocols, protocol versioning, stdio passthrough, exit-code
               transparency, timeouts on subprocess metadata, PATH poisoning,
               trust boundaries, graceful degradation
Prerequisites: Chapters 1–12
```

---

## A. Goal

```
$ gecko
Gecko — a developer toolbox

Core Commands
  clean      Find and remove disposable development files
  doctor     Inspect the developer environment
  serve      Serve a directory over HTTP
  ...

Plugins
  docker     Docker utilities                    (gecko-docker v1.2.0)
  postgres   PostgreSQL utilities                (gecko-postgres v0.4.1)

$ gecko docker ps
CONTAINER ID   IMAGE      STATUS
a3f9c21b8e     postgres   Up 2 hours

$ gecko plugin list --verbose
docker    v1.2.0  /usr/local/bin/gecko-docker    protocol 1  ok      12ms
postgres  v0.4.1  ~/.local/share/gecko/plugins/  protocol 1  ok       8ms
broken    —       /usr/local/bin/gecko-broken    —           timeout  2.0s
```

---

## B. Why this matters

A plugin system is where a tool becomes a platform, and the architectural choice you make
here is essentially irreversible — it defines the contract every third-party developer
writes against.

It's also the chapter where chapter 1's design gets validated. If `Command` was designed
correctly, plugin commands drop into the registry with no special-casing anywhere in the
dispatcher. If it wasn't, you're refactoring now.

Finally, this is the largest attack surface in the project: Gecko will locate an
executable by name and run it. Every decision here has a security dimension.

---

## C. Concepts

### Three plugin architectures

**1. `plugin.so` — Go's native dynamic loading.**

```go
p, _ := plugin.Open("docker.so")
sym, _ := p.Lookup("Commands")
```

Disqualifying problems, and they're severe:
- **Linux and macOS only.** No Windows support, and none planned. That alone ends it for
  a cross-platform tool.
- The plugin must be built with the **exact same Go version**, the **exact same versions
  of every shared dependency**, and the same build flags. A patch-version mismatch fails
  at load with an unhelpful error.
- Plugins **cannot be unloaded**. Memory grows monotonically.
- A plugin panic kills the host process.
- No isolation whatsoever: the plugin has full access to the host's memory.

**2. RPC over a subprocess — the HashiCorp go-plugin model.**

Terraform, Vault and Packer use this. The plugin is a separate process; the host talks to
it over gRPC or net/rpc on a local socket, with a handshake and version negotiation.

Genuinely excellent for its use case: bidirectional calls, streaming, typed interfaces,
plugin crashes are contained. The cost is a protobuf/gRPC dependency, a much heavier
plugin authoring experience, and complexity that only pays off when the host needs to
call *back* into the plugin repeatedly.

**3. Standalone executables — the git/kubectl model.**

`git foo` runs `git-foo` from PATH. `kubectl foo` runs `kubectl-foo`. Docker's CLI plugins
work the same way.

- Works identically on all three platforms.
- Plugins can be written in **any language**. A shell script is a valid plugin.
- Complete isolation: a plugin crash is an exit code.
- No version coupling to the host's Go toolchain.
- Trivial to author, trivial to debug (run it directly).

Cost: process spawn per invocation (~5–20 ms), and communication is limited to argv,
environment, stdio and exit codes.

**Decision: standalone executables**, as the brief specifies — and the reasoning is worth
being able to reproduce. The 10 ms spawn cost is irrelevant for a CLI. The
language-agnosticism and crash isolation are worth a great deal. And critically: **the
model is already understood by every developer who has used `git` or `kubectl`.**

### The discovery protocol

`gecko docker ps` → find `gecko-docker` → run it with `["ps"]`.

Where to look, in order:
1. `$GECKO_PLUGIN_DIR` if set.
2. The configured plugin directory (chapter 3's `Paths.Plugins`).
3. `$PATH`.

Later entries do not shadow earlier ones: the first match wins, and `gecko plugin list`
reports shadowed duplicates so the user can see what's happening.

Executable detection differs by platform:
- **Unix**: the file must have an execute bit set for someone (`mode&0111 != 0`).
- **Windows**: extension must be in `%PATHEXT%` (`.COM;.EXE;.BAT;.CMD;...`). There is no
  execute bit. So `gecko-docker.exe` is the plugin, and the command name is `docker`.

### The metadata handshake

Gecko needs, before running anything: does this plugin exist, what's its version, what
protocol does it speak, and what subcommands does it offer (for help output)?

Options:
- **A sidecar file** (`gecko-docker.json`). Fast, no subprocess. But two files to install
  and they can desynchronise.
- **A well-known argument** (`gecko-docker --gecko-metadata`). One artifact, always
  consistent. Costs a subprocess spawn.
- **An environment variable** (`GECKO_PLUGIN_PROTOCOL=1 gecko-docker`). Same cost;
  slightly less discoverable when debugging by hand.

**Decision: a well-known argument, `__gecko_meta`**, plus aggressive caching.

Why a double-underscore prefix rather than `--gecko-metadata`: it cannot collide with a
plugin's own flags, and it's obviously internal. Why not an env var: a developer
debugging a plugin can type `gecko-docker __gecko_meta` and see the output immediately.

The protocol:

```
$ gecko-docker __gecko_meta
{
  "protocol": 1,
  "name": "docker",
  "version": "1.2.0",
  "description": "Docker utilities",
  "commands": [
    {"name": "ps", "description": "Show containers"},
    {"name": "cleanup", "description": "Remove unused resources"}
  ]
}
exit 0
```

Rules, and each exists for a reason:

- **JSON on stdout, nothing else.** Any diagnostic goes to stderr. A plugin that prints a
  banner before its JSON is broken, and we report that clearly rather than producing a
  confusing parse error.
- **Exit 0 on success.** Non-zero means the plugin doesn't support metadata; treat it as
  a legacy plugin with no known subcommands rather than as an error.
- **A timeout, hard.** 2 seconds. A plugin that hangs must not hang `gecko help`.
- **`protocol` is an integer, checked before anything else is trusted.** Unknown protocol
  → the plugin is listed as incompatible, not run.

### Protocol versioning

```go
const (
    ProtocolVersion    = 1  // what we speak
    MinProtocolVersion = 1  // oldest we accept
)
```

The compatibility rule must be decided now, because it's a promise:

- **Additive changes** (new optional metadata fields) do not bump the version. Plugins
  built against v1 keep working; Gecko ignores unknown fields.
- **Breaking changes** bump it. Gecko then supports a range and adapts.

Gecko also passes its own protocol version to plugins:

```
GECKO_PROTOCOL_VERSION=1
GECKO_VERSION=0.9.0
```

so a plugin can adapt to an older host. Bidirectional negotiation from day one costs
nothing and is impossible to retrofit.

### Execution: stdio passthrough

```go
cmd := exec.CommandContext(ctx, pluginPath, args...)
cmd.Stdin  = env.Stdin
cmd.Stdout = env.Stdout
cmd.Stderr = env.Stderr
cmd.Env    = pluginEnv(env)
err := cmd.Run()
```

The plugin inherits the file descriptors directly. Consequences, all desirable:
- The plugin's own TTY detection works, so it can colour its output.
- Interactive plugins (prompts, pagers) work.
- No buffering, no deadlock, no copying.

**Do not** capture and re-emit. It breaks colour, breaks interactivity, and adds a
deadlock risk for nothing.

Exit code must be transparent:

```go
var ee *exec.ExitError
if errors.As(err, &ee) {
    return &cli.ExitError{Code: ee.ExitCode()}
}
```

`gecko docker ps` in a shell script must have the same exit status as `gecko-docker ps`.
This is exactly why chapter 1 built `ExitError`.

### Security

**The trust model, stated plainly: a plugin runs with the user's full privileges. There
is no sandbox.** Same as `git`, `kubectl` and every shell tool. Pretending otherwise
would be worse than being explicit, because a fake sandbox invites risky behaviour.

Given that, the threats we *can* address:

**PATH poisoning.** If `.` is on `$PATH` (a bad but real configuration), running `gecko`
in a downloaded repository containing `gecko-docker` executes attacker code. Go 1.19's
`exec.LookPath` refuses to resolve a bare name relative to the current directory on
Windows, but on Unix a `.` entry in `$PATH` is still honoured. Defence: when scanning
`$PATH`, **skip `.`, empty entries (which mean `.`), and any relative path.**

**Name confusion.** A plugin named `gecko-serve` would shadow a core command. Defence:
core commands always win, and `gecko plugin list` flags the conflict.

**Metadata as untrusted input.** The JSON comes from an arbitrary executable. Bound the
read (1 MB), bound the time (2 s), validate name characters (`[a-z0-9][a-z0-9-]*`), and
cap the number of subcommands. A plugin returning a 4 GB metadata document must not OOM
`gecko help`.

**Environment leakage.** The plugin inherits the environment, including `AWS_SECRET_ACCESS_KEY`.
That's expected — plugins need credentials. But strip Gecko's own internals and add
explicit context.

**Argument injection: not applicable, by construction.** Arguments pass through `argv`
directly. There is no shell. `gecko docker "; rm -rf /"` passes one literal argument.
Worth stating in the security policy so nobody later "helpfully" adds a shell.

### Failure handling

A broken plugin must never break Gecko. Specifically:

| Failure | Behaviour |
|---|---|
| Metadata times out | List as unavailable, don't block help |
| Metadata is malformed | List as broken with the parse error under `-v` |
| Protocol too new | List as incompatible with the required version |
| Plugin not executable | Skip silently, mention under `-v` |
| Plugin crashes on run | Propagate its exit code |
| Plugin name collides with core | Core wins, warn |

The theme: **`gecko help` must always work, no matter what is installed.** If plugin
discovery can break the help output, users have no way to diagnose the problem.

### Caching

Spawning N plugins on every invocation adds N × ~10 ms to *every command*, including
`gecko version`. Unacceptable.

Cache metadata in `$CACHE/gecko/plugins.json`, keyed by path, with the binary's size and
mtime as the invalidation key. Re-probe only when those change.

Not a content hash: hashing a 20 MB plugin binary costs more than spawning it. Size+mtime
is what `make` uses and it's the right trade here.

**And: don't probe at all unless needed.** `gecko version` doesn't need plugins.
`gecko docker ps` needs only `gecko-docker`. Only `gecko help` and `gecko plugin list`
need all of them. Make discovery lazy.

---

## D. Design

### Package layout

```
internal/plugin/
  discover.go   # find candidates on disk
  meta.go       # the metadata protocol
  cache.go      # metadata cache
  command.go    # adapt a plugin to cli.Command
  manager.go    # install/remove (chapter 14)
```

`plugin` imports `cli`? **No** — that would invert the dependency. Instead, `plugin`
exposes plain data and `cli` adapts it:

```go
// in package plugin
type Plugin struct {
    Name     string
    Path     string
    Meta     Metadata
    Err      error
}

// in package cli
func pluginCommand(p *plugin.Plugin) CommandFunc { ... }
```

`cli` imports `plugin`; `plugin` knows nothing about commands. This is the dependency
direction invariant, and the plugin system is where it's most tempting to break.

### Lazy discovery on `Env`

```go
func (e *Env) Plugins() *plugin.Set {
    e.pluginOnce.Do(func() {
        e.plugins = plugin.Discover(e.Getenv, e.pluginDirs())
    })
    return e.plugins
}
```

`Discover` only *finds* executables (cheap: a few `ReadDir` calls). Metadata is fetched
lazily per plugin, and in parallel when all are needed.

### Dispatcher integration — the payoff

```go
func (a *App) Execute(ctx context.Context, env *Env, args []string) error {
    name := args[0]
    if f, ok := a.commands[name]; ok {
        return runCommand(ctx, env, f(), []string{a.Name}, args[1:])
    }
    // Not a core command: try plugins.
    if p := env.Plugins().Lookup(name); p != nil {
        return runCommand(ctx, env, pluginCommand(p)(), []string{a.Name}, args[1:])
    }
    return unknownCommand(env, name)
}
```

Six lines. The plugin path uses the same `runCommand`, gets the same error handling, the
same exit-code mapping. **That's chapter 1's design being validated** — and if you'd used
an interface with four methods per command, you'd be writing a `pluginCommand` type with
four method implementations instead of a struct literal.

One necessary change: plugin commands must **not** have their flags parsed by Gecko. If
Gecko parsed `--tail 50` in `gecko docker logs --tail 50`, it would reject an unknown
flag. Add:

```go
type Command struct {
    ...
    // RawArgs disables flag parsing: everything after the command name
    // is passed through untouched. Used by plugin commands, whose flags
    // belong to the plugin and are unknown to us.
    RawArgs bool
}
```

and in `runCommand`:

```go
if c.RawArgs {
    return c.Run(ctx, env, &Invocation{Args: args})
}
```

---

## E. Implementation

### `internal/plugin/discover.go`

```go
// Package plugin implements Gecko's executable-plugin system.
//
// A plugin is a standalone executable named "gecko-<name>" found in the
// plugin directory or on PATH. Gecko discovers it, queries its metadata
// over a small JSON protocol, and delegates matching commands to it.
//
// This design was chosen over Go's plugin package (Linux/macOS only,
// requires identical toolchain and dependency versions, cannot unload,
// no crash isolation) and over an RPC model (heavier to author, needs a
// gRPC dependency, and unnecessary when the host never calls back into
// the plugin).
package plugin

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const prefix = "gecko-"

type Plugin struct {
	Name    string    // "docker"
	Path    string    // absolute path to the executable
	Source  string    // "plugin-dir" or "path", for diagnostics
	Size    int64
	ModTime time.Time

	meta     *Metadata
	metaErr  error
	metaOnce sync.Once
}

type Set struct {
	plugins  []*Plugin
	byName   map[string]*Plugin
	shadowed []*Plugin // later duplicates, reported but not used
}

// Discover finds plugin executables. It does not query metadata:
// spawning every plugin on every invocation would add tens of
// milliseconds to commands that never touch plugins.
func Discover(getenv func(string) string, pluginDirs []string) *Set {
	s := &Set{byName: make(map[string]*Plugin)}

	dirs := append([]string(nil), pluginDirs...)
	dirs = append(dirs, pathDirs(getenv)...)

	for _, dir := range dirs {
		source := "path"
		if len(pluginDirs) > 0 && containsPath(pluginDirs, dir) {
			source = "plugin-dir"
		}
		for _, p := range scanDir(dir, source) {
			if existing, ok := s.byName[p.Name]; ok {
				_ = existing
				s.shadowed = append(s.shadowed, p)
				continue // first match wins
			}
			s.byName[p.Name] = p
			s.plugins = append(s.plugins, p)
		}
	}

	sort.Slice(s.plugins, func(i, j int) bool { return s.plugins[i].Name < s.plugins[j].Name })
	return s
}

// pathDirs splits PATH, skipping entries that would allow an executable
// in the current directory to be picked up.
//
// A "." entry (or an empty entry, which POSIX defines as meaning ".")
// on PATH means running gecko inside a downloaded repository would
// execute a gecko-docker file placed there by whoever wrote it. Go 1.19
// closed this for LookPath on Windows; on Unix a literal "." in PATH is
// still honoured, so we filter it ourselves.
func pathDirs(getenv func(string) string) []string {
	raw := getenv("PATH")
	if raw == "" {
		return nil
	}
	var out []string
	for _, d := range filepath.SplitList(raw) {
		if d == "" || d == "." {
			continue
		}
		if !filepath.IsAbs(d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func scanDir(dir, source string) []*Plugin {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // missing or unreadable directories are normal
	}

	var out []*Plugin
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := pluginName(e.Name())
		if !ok {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !isExecutable(info, e.Name()) {
			continue
		}
		out = append(out, &Plugin{
			Name: name, Path: full, Source: source,
			Size: info.Size(), ModTime: info.ModTime(),
		})
	}
	return out
}

// pluginName extracts "docker" from "gecko-docker" or "gecko-docker.exe".
//
// The name must be a lowercase identifier: this is validated here rather
// than trusting the metadata, because the name determines which command
// the plugin claims and a name containing path separators or spaces
// would produce confusing behaviour at best.
func pluginName(filename string) (string, bool) {
	base := filename
	if runtime.GOOS == "windows" {
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if !strings.HasPrefix(base, prefix) {
		return "", false
	}
	name := base[len(prefix):]
	if !validName(name) {
		return "", false
	}
	return name, true
}

func validName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// isExecutable applies the platform's notion of "runnable".
//
// Unix uses the execute permission bits. Windows has none; whether a
// file is executable is determined entirely by its extension appearing
// in PATHEXT.
func isExecutable(info fs.FileInfo, filename string) bool {
	if runtime.GOOS == "windows" {
		ext := strings.ToLower(filepath.Ext(filename))
		for _, e := range pathExt() {
			if ext == e {
				return true
			}
		}
		return false
	}
	return info.Mode()&0o111 != 0
}

func pathExt() []string {
	raw := os.Getenv("PATHEXT")
	if raw == "" {
		raw = ".COM;.EXE;.BAT;.CMD"
	}
	parts := strings.Split(strings.ToLower(raw), ";")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func (s *Set) Lookup(name string) *Plugin { return s.byName[name] }
func (s *Set) All() []*Plugin             { return s.plugins }
func (s *Set) Shadowed() []*Plugin        { return s.shadowed }
```

### `internal/plugin/meta.go`

```go
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	// ProtocolVersion is the protocol Gecko speaks.
	//
	// Compatibility rule: additive changes (new optional fields) do not
	// bump this, because unknown fields are ignored on both sides.
	// Breaking changes do, and Gecko then supports a range.
	ProtocolVersion = 1

	// MinProtocolVersion is the oldest protocol Gecko still accepts.
	MinProtocolVersion = 1

	// metaArg is the argument that requests metadata. The double
	// underscore prevents any collision with a plugin's own flags and
	// marks it as internal, while still being typeable by a developer
	// debugging their plugin by hand.
	metaArg = "__gecko_meta"

	// metaTimeout bounds the probe. A plugin that hangs must not hang
	// "gecko help".
	metaTimeout = 2 * time.Second

	// maxMetaSize bounds the response. The metadata comes from an
	// arbitrary executable and must be treated as untrusted input; a
	// plugin emitting gigabytes must not exhaust our memory.
	maxMetaSize = 1 << 20

	maxCommands = 256
)

type Metadata struct {
	Protocol    int             `json:"protocol"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Commands    []CommandMeta   `json:"commands"`
	Homepage    string          `json:"homepage,omitempty"`
}

type CommandMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Usage       string `json:"usage,omitempty"`
}

// IncompatibleError reports a protocol mismatch.
type IncompatibleError struct {
	Got, Min, Max int
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("plugin speaks protocol %d; this Gecko supports %d–%d",
		e.Got, e.Min, e.Max)
}

// Meta returns the plugin's metadata, querying it at most once.
func (p *Plugin) Meta(ctx context.Context) (*Metadata, error) {
	p.metaOnce.Do(func() {
		p.meta, p.metaErr = p.queryMeta(ctx)
	})
	return p.meta, p.metaErr
}

func (p *Plugin) queryMeta(ctx context.Context) (*Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.Path, metaArg)
	cmd.Env = protocolEnv(nil)
	cmd.Stdin = nil // a plugin must not read stdin during a metadata probe

	var stdout, stderr strings.Builder
	cmd.Stdout = &limitedWriter{w: &stdout, n: maxMetaSize}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	cmd.WaitDelay = time.Second

	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("metadata probe timed out after %s", metaTimeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// A non-zero exit means the plugin does not implement the
			// metadata protocol. That is not fatal: it can still be
			// invoked, it just contributes no subcommands to help.
			return nil, fmt.Errorf("does not support the metadata protocol (exit %d)", ee.ExitCode())
		}
		return nil, err
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		return nil, errors.New("metadata probe produced no output")
	}

	var m Metadata
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		// Give the developer something actionable. A plugin printing a
		// banner before its JSON is the most common mistake, and
		// "invalid character 'D'" alone does not suggest the fix.
		preview := raw
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
		return nil, fmt.Errorf("invalid metadata JSON (%w); output began: %q "+
			"(metadata must be JSON on stdout only; diagnostics belong on stderr)", err, preview)
	}

	if err := validateMeta(&m, p.Name); err != nil {
		return nil, err
	}
	return &m, nil
}

// validateMeta checks untrusted plugin output before Gecko relies on it.
func validateMeta(m *Metadata, discoveredName string) error {
	if m.Protocol < MinProtocolVersion || m.Protocol > ProtocolVersion {
		return &IncompatibleError{Got: m.Protocol, Min: MinProtocolVersion, Max: ProtocolVersion}
	}
	if m.Name == "" {
		m.Name = discoveredName
	}
	if m.Name != discoveredName {
		// The filename determines which command the plugin serves. A
		// metadata name that disagrees is either a packaging error or
		// an attempt to impersonate another plugin.
		return fmt.Errorf("metadata name %q does not match executable name %q", m.Name, discoveredName)
	}
	if !validName(m.Name) {
		return fmt.Errorf("invalid plugin name %q", m.Name)
	}
	if len(m.Commands) > maxCommands {
		return fmt.Errorf("plugin declares %d commands, limit is %d", len(m.Commands), maxCommands)
	}
	for i, c := range m.Commands {
		if !validName(c.Name) {
			return fmt.Errorf("command %d has invalid name %q", i, c.Name)
		}
		if len(c.Description) > 200 {
			m.Commands[i].Description = c.Description[:200]
		}
	}
	return nil
}

// limitedWriter discards output beyond n bytes rather than erroring, so
// a chatty plugin fails on validation rather than on I/O.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	return l.w.Write(p)
}

func protocolEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	return append(base,
		"GECKO_PROTOCOL_VERSION="+strconv.Itoa(ProtocolVersion),
		"GECKO_VERSION="+geckoVersion,
	)
}

// MetaAll fetches metadata for every plugin concurrently.
//
// Sequential probing at ~10ms each makes "gecko help" visibly slow once
// a handful of plugins are installed; the probes are independent and
// I/O-bound, so fanning them out is straightforwardly correct here.
func MetaAll(ctx context.Context, plugins []*Plugin) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, p := range plugins {
		p := p
		g.Go(func() error {
			_, _ = p.Meta(gctx) // errors are stored on the plugin
			return nil
		})
	}
	_ = g.Wait()
}
```

### `internal/cli/plugin_command.go`

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/yourname/gecko/internal/plugin"
)

// pluginCommand adapts a discovered plugin to the Command type.
//
// Note that this is an ordinary Command struct literal with a closure —
// exactly what a compiled-in command is. The dispatcher has no plugin
// special case, which was the requirement that drove chapter 1's choice
// of a struct with function fields over an interface.
func pluginCommand(p *plugin.Plugin) CommandFunc {
	return func() *Command {
		c := &Command{
			Name: p.Name,
			// RawArgs: the plugin owns its own flags. If Gecko parsed
			// them, "gecko docker logs --tail 50" would fail on an
			// unknown flag.
			RawArgs: true,
			Run: func(ctx context.Context, env *Env, inv *Invocation) error {
				return execPlugin(ctx, env, p, inv.Args)
			},
		}
		// Metadata is best-effort: a plugin that cannot describe itself
		// is still runnable.
		if m, err := p.Meta(context.Background()); err == nil {
			c.Short = m.Description
			c.Long = m.Description
			c.Usage = fmt.Sprintf("gecko %s <command> [flags]", p.Name)
		} else {
			c.Short = fmt.Sprintf("(plugin at %s)", p.Path)
		}
		return c
	}
}

// execPlugin runs the plugin, passing our standard streams straight
// through.
//
// The plugin inherits the file descriptors rather than having its output
// captured and re-emitted. That keeps its own TTY detection working (so
// it can colour output), keeps interactive prompts and pagers working,
// and avoids both a copy and a pipe-deadlock risk.
func execPlugin(ctx context.Context, env *Env, p *plugin.Plugin, args []string) error {
	cmd := exec.CommandContext(ctx, p.Path, args...)
	cmd.Stdin = env.Stdin
	cmd.Stdout = env.Stdout
	cmd.Stderr = env.Stderr
	cmd.Dir = env.WorkDir
	cmd.Env = plugin.ExecEnv(env.Getenv, env.WorkDir)

	// A plugin that spawns children must not leave them behind when the
	// user interrupts Gecko.
	process.ConfigureGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	env.Log.Debug("running plugin", "name", p.Name, "path", p.Path, "args", args)

	err := cmd.Run()
	if err == nil {
		return nil
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// Exit-code transparency: a shell script wrapping
		// "gecko docker ps" must see the same status it would get from
		// running gecko-docker directly. Quiet, because the plugin has
		// already written its own error message to stderr.
		return Quiet(&ExitError{Code: ee.ExitCode()})
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("plugin %s: %w", p.Name, err)
}
```

### Help integration

```go
func (a *App) printRootHelp(ctx context.Context, env *Env, out io.Writer) error {
	// ... core commands as before ...

	// Plugin discovery must never break help. Any failure here degrades
	// to "no plugins listed" rather than an error, because a user whose
	// help output is broken has no way to diagnose the plugin that
	// broke it.
	set := env.Plugins()
	if plugins := set.All(); len(plugins) > 0 {
		mctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		plugin.MetaAll(mctx, plugins)

		fmt.Fprintf(out, "\nPlugins\n")
		w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
		for _, p := range plugins {
			desc := ""
			if m, err := p.Meta(mctx); err == nil {
				desc = m.Description
			} else {
				desc = env.Style.Dim("(unavailable)")
			}
			fmt.Fprintf(w, "  %s\t%s\n", p.Name, desc)
		}
		w.Flush()
	}
	return nil
}
```

### A reference plugin

`examples/gecko-hello/main.go`, deliberately with no Gecko dependency at all — it's just
a program that speaks the protocol:

```go
// Command gecko-hello is a minimal reference plugin.
//
// It depends on nothing from Gecko: the protocol is small enough to
// implement directly, which is the point of the executable model. A
// plugin can be written in any language, including a shell script.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const metadata = `{
  "protocol": 1,
  "name": "hello",
  "version": "1.0.0",
  "description": "A minimal example plugin",
  "commands": [
    {"name": "greet", "description": "Print a greeting"},
    {"name": "env",   "description": "Show the environment Gecko provided"}
  ]
}`

func main() {
	args := os.Args[1:]

	// The metadata probe. Note: JSON to stdout, nothing else, exit 0.
	if len(args) == 1 && args[0] == "__gecko_meta" {
		fmt.Println(metadata)
		return
	}

	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch args[0] {
	case "greet":
		name := "world"
		if len(args) > 1 {
			name = args[1]
		}
		fmt.Printf("Hello, %s!\n", name)

	case "env":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]string{
			"GECKO_VERSION":          os.Getenv("GECKO_VERSION"),
			"GECKO_PROTOCOL_VERSION": os.Getenv("GECKO_PROTOCOL_VERSION"),
			"GECKO_PLUGIN_NAME":      os.Getenv("GECKO_PLUGIN_NAME"),
		})

	default:
		// Diagnostics go to stderr, always.
		fmt.Fprintf(os.Stderr, "gecko-hello: unknown command %q\n", args[0])
		usage()
		os.Exit(2)
	}
}
```

And the same thing as a shell script, to prove the point:

```sh
#!/bin/sh
# gecko-shellplugin — a complete plugin in 15 lines.
case "$1" in
  __gecko_meta)
    cat <<'EOF'
{"protocol":1,"name":"shellplugin","version":"0.1.0",
 "description":"Proof that plugins need not be Go",
 "commands":[{"name":"pwd","description":"Print the working directory"}]}
EOF
    ;;
  pwd) pwd ;;
  *)   echo "unknown command: $1" >&2; exit 2 ;;
esac
```

---

## F. Exercise

1. Implement the metadata cache. Key on path; invalidate on size or mtime change; store
   in `$CACHE/gecko/plugins.json`. Then measure `gecko help` with 10 plugins, cached and
   uncached.

2. Implement `gecko plugin list --verbose` showing path, version, protocol, status and
   probe duration — including the failure cases. Then deliberately install a plugin that
   hangs, one that prints garbage, and one claiming protocol 99, and check the output is
   useful for each.

3. **Subcommand completion.** With metadata, `gecko docker <TAB>` could complete `ps` and
   `cleanup`. Implement `gecko completion bash|zsh` that includes plugin subcommands.

4. **The adversarial exercise.** Write a plugin that: emits 1 GB of metadata; hangs
   forever; exits 0 with empty output; returns metadata claiming to be named `serve`;
   returns 100,000 commands. Verify each is handled without crashing Gecko or breaking
   `gecko help`.

---

## G. Testing

### Build test plugins with `go build` in `TestMain`

```go
// buildTestPlugin compiles a plugin into a temp directory and returns
// the directory. Building real executables rather than faking the
// protocol is what makes these tests meaningful: they exercise process
// spawning, stdio inheritance and exit codes for real.
func buildTestPlugin(t *testing.T, dir, name, source string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "gecko-"+name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, src)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building plugin %s: %v\n%s", name, err, b)
	}
}
```

Building is slow (~1 s each), so build once per package in `TestMain` and share.

### Protocol conformance

```go
func TestPluginMetadata(t *testing.T) {
	dir := t.TempDir()
	buildTestPlugin(t, dir, "good", goodPluginSource)

	set := Discover(func(string) string { return "" }, []string{dir})
	p := set.Lookup("good")
	if p == nil {
		t.Fatal("plugin not discovered")
	}

	m, err := p.Meta(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.0.0" {
		t.Errorf("Version = %q", m.Version)
	}
	if len(m.Commands) != 2 {
		t.Errorf("got %d commands, want 2", len(m.Commands))
	}
}
```

### The failure matrix — the important tests

```go
func TestPluginFailureModes(t *testing.T) {
	dir := t.TempDir()

	sources := map[string]string{
		"hangs":     `package main
                      func main() { select {} }`,
		"garbage":   `package main
                      import "fmt"
                      func main() { fmt.Println("not json at all") }`,
		"banner":    `package main
                      import "fmt"
                      func main() { fmt.Println("Loading..."); fmt.Println(` + "`" + `{"protocol":1,"name":"banner"}` + "`" + `) }`,
		"empty":     `package main
                      func main() {}`,
		"future":    `package main
                      import "fmt"
                      func main() { fmt.Println(` + "`" + `{"protocol":99,"name":"future"}` + "`" + `) }`,
		"wrongname": `package main
                      import "fmt"
                      func main() { fmt.Println(` + "`" + `{"protocol":1,"name":"serve"}` + "`" + `) }`,
		"huge":      `package main
                      import ("fmt";"strings")
                      func main() { fmt.Println(strings.Repeat("x", 5<<20)) }`,
	}
	for name, src := range sources {
		buildTestPlugin(t, dir, name, src)
	}

	set := Discover(func(string) string { return "" }, []string{dir})

	tests := []struct {
		name      string
		wantErrIs func(error) bool
		wantMsg   string
	}{
		{"hangs", nil, "timed out"},
		{"garbage", nil, "invalid metadata JSON"},
		{"banner", nil, "invalid metadata JSON"},
		{"empty", nil, "no output"},
		{"future", func(e error) bool {
			var ie *IncompatibleError
			return errors.As(e, &ie)
		}, ""},
		{"wrongname", nil, "does not match executable name"},
		{"huge", nil, "invalid metadata JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := set.Lookup(tt.name)
			if p == nil { t.Fatalf("%s not discovered", tt.name) }

			start := time.Now()
			_, err := p.Meta(context.Background())
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected an error")
			}
			if elapsed > 5*time.Second {
				t.Errorf("probe took %s; the timeout is not bounding it", elapsed)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantMsg)
			}
			if tt.wantErrIs != nil && !tt.wantErrIs(err) {
				t.Errorf("error has the wrong type: %v", err)
			}
		})
	}
}
```

That elapsed-time assertion appears in every case and is the one that matters most: a
plugin must not be able to make Gecko slow.

### Help must survive anything

```go
func TestHelpWorksWithBrokenPlugins(t *testing.T) {
	dir := t.TempDir()
	buildTestPlugin(t, dir, "hangs", hangsSource)
	buildTestPlugin(t, dir, "garbage", garbageSource)

	env, out, _ := testEnv(map[string]string{"GECKO_PLUGIN_DIR": dir})

	start := time.Now()
	code := Main(context.Background(), []string{"help"}, env)
	elapsed := time.Since(start)

	if code != 0 {
		t.Errorf("help exited %d with broken plugins installed", code)
	}
	if !strings.Contains(out.String(), "Core Commands") {
		t.Error("core commands missing from help")
	}
	if elapsed > 6*time.Second {
		t.Errorf("help took %s; a hanging plugin is blocking it", elapsed)
	}
}
```

### Exit-code transparency

```go
func TestPluginExitCodePropagates(t *testing.T) {
	dir := t.TempDir()
	buildTestPlugin(t, dir, "exiter", `package main
        import ("os";"strconv")
        func main() {
            if len(os.Args) > 1 && os.Args[1] == "__gecko_meta" {
                os.Stdout.WriteString(`+"`"+`{"protocol":1,"name":"exiter","version":"1"}`+"`"+`)
                return
            }
            n, _ := strconv.Atoi(os.Args[1])
            os.Exit(n)
        }`)

	for _, want := range []int{0, 1, 2, 42, 127} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			env, _, _ := testEnv(map[string]string{"GECKO_PLUGIN_DIR": dir})
			got := Main(context.Background(), []string{"exiter", strconv.Itoa(want)}, env)
			if got != want {
				t.Errorf("gecko returned %d, plugin exited %d", got, want)
			}
		})
	}
}
```

### Security tests

```go
func TestPathDirsSkipsCurrentDirectory(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == "PATH" {
			return strings.Join([]string{".", "", "relative/dir", "/usr/bin"},
				string(filepath.ListSeparator))
		}
		return ""
	}
	dirs := pathDirs(getenv)
	for _, d := range dirs {
		if d == "." || d == "" || !filepath.IsAbs(d) {
			t.Errorf("pathDirs returned unsafe entry %q", d)
		}
	}
	if len(dirs) != 1 || dirs[0] != "/usr/bin" {
		t.Errorf("dirs = %v, want only /usr/bin", dirs)
	}
}

func TestCoreCommandsCannotBeShadowed(t *testing.T) {
	dir := t.TempDir()
	buildTestPlugin(t, dir, "serve", impostorSource) // gecko-serve

	env, out, _ := testEnv(map[string]string{"GECKO_PLUGIN_DIR": dir})
	Main(context.Background(), []string{"serve", "--help"}, env)

	if strings.Contains(out.String(), "IMPOSTOR") {
		t.Fatal("a plugin shadowed the core serve command")
	}
}

func TestArgumentsAreNotShellInterpreted(t *testing.T) {
	dir := t.TempDir()
	// A plugin that echoes its argv as JSON.
	buildTestPlugin(t, dir, "echoer", echoerSource)

	env, out, _ := testEnv(map[string]string{"GECKO_PLUGIN_DIR": dir})
	Main(context.Background(), []string{"echoer", "; rm -rf /", "$(whoami)", "`id`"}, env)

	var argv []string
	json.Unmarshal(out.Bytes(), &argv)
	if len(argv) != 3 || argv[0] != "; rm -rf /" {
		t.Errorf("arguments were transformed: %q", argv)
	}
}
```

---

## H. Review

- The three plugin architectures and the specific disqualifying facts about `plugin.so`.
- Why executables win for a cross-platform CLI, and what the ~10 ms costs you.
- The discovery order, and why `.` and relative entries are stripped from `$PATH`.
- Windows executability via `PATHEXT`, not permission bits.
- The metadata protocol: JSON-only on stdout, exit 0, bounded time and size.
- Additive vs breaking protocol changes, and bidirectional version advertisement.
- stdio passthrough preserves colour and interactivity; capturing breaks both.
- Exit-code transparency, and why chapter 1's `ExitError` existed for this.
- Treating plugin output as untrusted input: bound size, bound time, validate names.
- The trust model — no sandbox — stated honestly rather than implied away.
- Lazy discovery and metadata caching, so plugins cost nothing when unused.
- **`gecko help` must work no matter what is installed.**

---

## I. Refactoring

`Env` now has `configOnce`, `pluginOnce`, and their fields. The pattern is repeating.
Generalise with Go 1.21's `sync.OnceValues`:

```go
type Env struct {
    ...
    config  func() (*config.Config, error)  // memoised
    plugins func() *plugin.Set              // memoised
}

func OSEnv() *Env {
    e := &Env{...}
    e.config  = sync.OnceValues(func() (*config.Config, error) { return config.Load(e.Getenv) })
    e.plugins = sync.OnceValue(func() *plugin.Set { return plugin.Discover(e.Getenv, e.pluginDirs()) })
    return e
}

func (e *Env) Config() (*config.Config, error) { return e.config() }
func (e *Env) Plugins() *plugin.Set            { return e.plugins() }
```

Cleaner, and overriding for tests becomes assigning a function rather than the `Once`
trick from chapter 3:

```go
func (e *Env) SetConfig(c *config.Config) {
    e.config = func() (*config.Config, error) { return c, nil }
}
```

Notice that this also removes the `sync.Once` from the struct, which removes the
`copylocks` constraint. A small refactor that eliminates a whole category of `go vet`
finding is a good refactor.

---

## Commit

```
feat: add plugin discovery for executable plugins
feat: add plugin metadata protocol with version negotiation
feat: route unknown commands to plugins with exit-code transparency
feat: include plugins in help output with graceful degradation
test: add plugin failure-mode and security test suites
docs: add plugin protocol specification
refactor: memoise Env accessors with sync.OnceValues
```

The `docs:` commit is not optional here. The moment third parties can write plugins, the
protocol is a public contract and needs a written specification independent of the
implementation.

Next: `14-plugin-ecosystem.md`.

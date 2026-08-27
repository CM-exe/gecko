# Chapter 3 — Configuration and Platform Paths

```
Difficulty:   Intermediate
Est. time:    4–5 hours
Main concepts: os.UserConfigDir, XDG/Known Folders, precedence chains, YAML vs TOML
               vs JSON, first third-party dependency, atomic file writes, file
               permissions, reflection-free config merging
Prerequisites: Chapters 1–2
```

---

## A. Goal

```
$ gecko config path
/home/you/.config/gecko/config.yaml

$ gecko config show
theme: auto
server:
  default_port: 8080
  cors: false
plugins:
  directory: /home/you/.local/share/gecko/plugins

$ gecko config set server.default_port 3000
$ gecko config get server.default_port
3000
```

And every future command reads defaults from this.

---

## B. Why this matters

Two reasons, one obvious and one not.

The obvious one: hard-coding `~/.config/gecko` breaks on Windows and violates Apple's
conventions on macOS. Getting this right is table stakes for a tool that claims
cross-platform support.

The non-obvious one: **the precedence chain is the actual design problem.** A user
expects `gecko serve --port 3000` to beat `GECKO_PORT=8080` to beat the config file to
beat the built-in default. Implementing that without either a reflection-heavy library
or a mountain of `if` statements is a real exercise in Go API design, and it's where
most home-grown config code becomes unmaintainable.

---

## C. Concepts

### Where configuration actually goes

There are three distinct directories and conflating them is the standard mistake.

| Purpose | Linux (XDG) | macOS | Windows |
|---|---|---|---|
| **Config** (user-edited, backed up) | `$XDG_CONFIG_HOME` or `~/.config` | `~/Library/Application Support` | `%AppData%` |
| **Data** (installed plugins, assets) | `$XDG_DATA_HOME` or `~/.local/share` | `~/Library/Application Support` | `%LocalAppData%` |
| **Cache** (regenerable) | `$XDG_CACHE_HOME` or `~/.cache` | `~/Library/Caches` | `%LocalAppData%\Temp` |

Go's standard library gives you two of these:

- `os.UserConfigDir()` — `$XDG_CONFIG_HOME` / `~/Library/Application Support` / `%AppData%`
- `os.UserCacheDir()` — `$XDG_CACHE_HOME` / `~/Library/Caches` / `%LocalAppData%`
- `os.UserHomeDir()` — `$HOME` / `%USERPROFILE%`

There is **no** `os.UserDataDir()`. You must write it. On Linux the correct answer is
`$XDG_DATA_HOME` defaulting to `~/.local/share`; on macOS and Windows most tools
collapse data into the config or local-app-data location.

A subtlety on macOS: Go's `os.UserConfigDir()` returns `~/Library/Application Support`,
which is the Apple-sanctioned answer. But a large fraction of developer CLI tools on
macOS use `~/.config` anyway, because developers expect their dotfiles in one place.
**Our decision: honour `$XDG_CONFIG_HOME` if set on any platform, then fall back to
`os.UserConfigDir()`.** This gives XDG-preferring macOS users what they want without
violating the platform default for everyone else. We'll document it.

### Windows specifics

`%AppData%` is roaming (synced across domain machines); `%LocalAppData%` is not.
Config → roaming. Plugin binaries → local, because a Linux-built binary syncing to a
Windows machine is nonsense. Go's `os.UserConfigDir()` returns roaming; `os.UserCacheDir()`
returns local.

Also: Windows has no `0600` file mode in the Unix sense. `os.WriteFile(path, data, 0o600)`
compiles and runs on Windows but the permission bits are largely ignored — ACLs govern
access, inherited from the parent directory. Since `%AppData%` is already user-scoped,
this is acceptable. Do not *rely* on Unix modes for security on Windows; note it in a
comment so the next reader knows it was considered.

### The precedence chain

Four layers, lowest to highest:

```
1. Compiled-in defaults
2. Config file
3. Environment variables (GECKO_*)
4. Command-line flags
```

The mechanically hard part is layer 4: how do you know whether a flag was *set* or is
merely at its zero value? `--port 0` is indistinguishable from "not given" if you only
inspect the variable.

`flag.FlagSet` answers this with `fs.Visit(fn)`, which visits **only flags that were
actually set**, as opposed to `fs.VisitAll` which visits all registered flags. That is
the whole trick.

```go
set := map[string]bool{}
fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
if set["port"] { cfg.Server.Port = port }
```

### Config format: the first dependency decision

We need a format. The candidates:

**JSON** — standard library, zero dependencies. No comments, trailing-comma-hostile,
verbose for humans. Users *will* want to comment out a setting.

**TOML** — `github.com/BurntSushi/toml` or `github.com/pelletier/go-toml/v2`. Excellent
for flat-to-medium config, unambiguous, comments supported. Nested structures get noisy
(`[server.tls.client]`).

**YAML** — `gopkg.in/yaml.v3` or `sigs.k8s.io/yaml`. What the project spec asked for, what
most developer tools use, comments supported, nesting is natural. Downsides are real:
the Norway problem (`no` parses as `false`), significant whitespace, and a large,
historically CVE-prone spec.

**Decision: YAML via `gopkg.in/yaml.v3`.**

Justifying it against the dependency checklist from the brief:

1. *Why we need it.* Nested config with comments. JSON can't do comments; writing a YAML
   parser is a multi-week project with a security surface.
2. *Alternatives.* TOML is the strongest alternative and genuinely better-specified.
   YAML wins on ecosystem familiarity — the spec's example config is YAML, and chapter
   14's plugin manifests will be YAML too.
3. *Tradeoffs.* `yaml.v3` is in maintenance mode. `goccy/go-yaml` is faster and actively
   developed. `yaml.v3` is more widely vendored and battle-tested. We accept slower
   parsing of a 20-line file.
4. *Could we implement it?* A YAML subset, yes — perhaps 400 lines for scalars, maps and
   sequences. A useful exercise, but it would be a strictly worse parser and the lesson
   (recursive-descent parsing) isn't a Go lesson.
5. *Keeping it minimal.* `yaml.v3` has zero transitive dependencies. Check with
   `go mod graph | grep yaml`.

```bash
go get gopkg.in/yaml.v3
```

Note the Norway problem in practice: `theme: no` becomes the boolean `false`, not the
string `"no"`. yaml.v3 implements YAML 1.1 semantics here. Since our fields are typed
structs, a bool landing in a `string` field produces a clear unmarshal error rather than
silent corruption. **Typed unmarshalling into structs is the mitigation.** Never
unmarshal user config into `map[string]any` if you can avoid it.

### Atomic writes

`gecko config set` rewrites the file. If the process dies mid-write, the user's config
is truncated. The fix is write-then-rename:

```go
tmp := path + ".tmp"
os.WriteFile(tmp, data, 0o600)
os.Rename(tmp, path)
```

`rename(2)` is atomic within a filesystem on POSIX. On Windows, `os.Rename` maps to
`MoveFileEx` with `MOVEFILE_REPLACE_EXISTING`, which is atomic enough for our purposes
but *fails if the destination is open by another process*. That's a real difference and
we'll handle the error rather than pretend it can't happen.

For true durability you'd also `f.Sync()` before rename and fsync the parent directory.
For a config file that's overkill; for chapter 14's plugin installs it isn't, and we'll
do it properly there.

---

## D. Design

### Package layout

```
internal/
  config/
    config.go      # the Config struct, defaults, Load/Save
    paths.go       # platform directory resolution
    paths_test.go
    config_test.go
  cli/
    config.go      # the "gecko config" command tree
```

`config` imports nothing of ours. `cli` imports `config`. Later, `plugin` will import
`config` too — that's fine, dependencies still point inward.

### The Config struct

```go
type Config struct {
    Theme   string        `yaml:"theme"`
    Server  ServerConfig  `yaml:"server"`
    Plugins PluginsConfig `yaml:"plugins"`
}
```

**Design question before you read on:** should `Config` be passed to every command
explicitly, stored on `Env`, or fetched from a package-level singleton?

The singleton is out — it defeats parallel tests, exactly as `os.Stdout` did in chapter 1.

Between the other two: putting it on `Env` means every command gets it for free but
`Env` becomes a grab bag. Passing it explicitly means threading it through
`Run(ctx, env, cfg, args)` — changing the signature every command already uses.

**Decision: a lazily-loaded accessor on `Env`.**

```go
type Env struct {
    ...
    configOnce sync.Once
    config     *config.Config
    configErr  error
}

func (e *Env) Config() (*config.Config, error) {
    e.configOnce.Do(func() {
        e.config, e.configErr = config.Load(e.Getenv)
    })
    return e.config, e.configErr
}
```

Rationale: most commands don't need config, and reading and parsing a file on every
`gecko version` invocation is waste. `sync.Once` gives us "load at most once, safely
even if two goroutines ask" without a mutex on the read path after the first call.

Cost: `Env` can no longer be copied by value (it contains a `sync.Once`, which contains a
`sync.Mutex`, which must not be copied). We already pass `*Env` everywhere. Add
`go vet` to CI — the `copylocks` check catches this class of bug automatically.

But wait — this makes `Env` import `config`, and `config` is in `internal/`. Fine. Does
`config` import `cli`? No. Direction holds.

### Precedence implementation without reflection

Reflection-based merging (Viper's approach) is powerful and opaque. For a config with
about a dozen fields, explicit merging is shorter to read and impossible to get subtly
wrong:

```go
func (c *Config) applyEnv(getenv func(string) string) {
    if v := getenv("GECKO_THEME"); v != "" {
        c.Theme = v
    }
    if v := getenv("GECKO_PORT"); v != "" {
        if n, err := strconv.Atoi(v); err == nil {
            c.Server.Port = n
        }
    }
    ...
}
```

Twelve fields is twelve `if`s. When it reaches fifty, revisit. That threshold — "explicit
until it's genuinely unwieldy" — is the actual lesson.

---

## E. Implementation

### `internal/config/paths.go`

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// appName is the directory name used under each platform's base directory.
const appName = "gecko"

// Paths holds the resolved per-user directories Gecko uses.
type Paths struct {
	Config  string // user-editable configuration
	Data    string // installed plugins and other persistent state
	Cache   string // regenerable; safe to delete at any time
	Plugins string // convenience: Data/plugins
}

// Getenv matches os.Getenv's signature so tests can inject a fake
// environment. Passing a function rather than a map keeps the production
// path allocation-free.
type Getenv func(string) string

// ResolvePaths determines Gecko's directories for the current platform.
//
// Precedence for each directory:
//  1. GECKO_CONFIG_DIR / GECKO_DATA_DIR / GECKO_CACHE_DIR (explicit override)
//  2. XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_CACHE_HOME, honoured on every
//     platform so that developers who standardise on XDG get what they
//     expect even on macOS
//  3. The platform default, via os.UserConfigDir / os.UserCacheDir
//
// It does not create anything; see EnsureDirs.
func ResolvePaths(getenv Getenv) (Paths, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg, err := resolveOne(getenv, "GECKO_CONFIG_DIR", "XDG_CONFIG_HOME", os.UserConfigDir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve config dir: %w", err)
	}
	cache, err := resolveOne(getenv, "GECKO_CACHE_DIR", "XDG_CACHE_HOME", os.UserCacheDir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve cache dir: %w", err)
	}
	data, err := resolveOne(getenv, "GECKO_DATA_DIR", "XDG_DATA_HOME", userDataDir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data dir: %w", err)
	}

	return Paths{
		Config:  cfg,
		Data:    data,
		Cache:   cache,
		Plugins: filepath.Join(data, "plugins"),
	}, nil
}

func resolveOne(getenv Getenv, override, xdg string, fallback func() (string, error)) (string, error) {
	if v := getenv(override); v != "" {
		return v, nil // used verbatim: the user asked for exactly this
	}
	if v := getenv(xdg); v != "" && filepath.IsAbs(v) {
		// The XDG spec says relative values must be ignored, not resolved.
		return filepath.Join(v, appName), nil
	}
	base, err := fallback()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// userDataDir is the missing counterpart to os.UserConfigDir. The standard
// library does not provide one, so we implement the platform conventions
// ourselves.
func userDataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		// LOCALAPPDATA rather than APPDATA: plugin binaries are
		// machine-specific and must not roam to other machines.
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v, nil
		}
		return os.UserCacheDir()

	case "darwin", "ios":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil

	default: // linux, freebsd, openbsd, netbsd, ...
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// ConfigFile returns the path to the main configuration file.
func (p Paths) ConfigFile() string {
	return filepath.Join(p.Config, "config.yaml")
}

// EnsureDirs creates the directories Gecko needs, with permissions that
// exclude other users on Unix. On Windows the mode bits are largely
// ignored and access is governed by inherited ACLs; since the base
// directories are already per-user, that is acceptable.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.Config, p.Data, p.Cache, p.Plugins} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}
```

Note `runtime.GOOS` in a `switch` rather than build-tagged files. This is deliberate and
it's the first instance of the four-way classification from the overview:

- The logic is small and shares its shape across platforms.
- No platform-specific *imports* are required.
- Having all three branches visible in one file is easier to review for correctness than
  three files you must open separately.

Build tags become correct when platforms need **different imports** (e.g. `golang.org/x/sys/windows`)
or **genuinely different algorithms**. Chapter 11 has both. This doesn't.

### `internal/config/config.go`

```go
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config is Gecko's user configuration. Every field must have a
// meaningful zero value or a default supplied by Defaults(), because a
// partially-populated YAML file leaves the rest zeroed.
type Config struct {
	Theme   string        `yaml:"theme"`
	Server  ServerConfig  `yaml:"server"`
	Plugins PluginsConfig `yaml:"plugins"`
	Tree    TreeConfig    `yaml:"tree"`

	// path records where this config was loaded from; empty means
	// defaults only. Unexported so it never round-trips into the file.
	path string
}

type ServerConfig struct {
	DefaultPort int    `yaml:"default_port"`
	Host        string `yaml:"host"`
	CORS        bool   `yaml:"cors"`
	Gzip        bool   `yaml:"gzip"`
}

type PluginsConfig struct {
	Directory string   `yaml:"directory"`
	Disabled  []string `yaml:"disabled"`
}

type TreeConfig struct {
	MaxDepth int      `yaml:"max_depth"`
	Ignore   []string `yaml:"ignore"`
}

// Defaults returns the compiled-in configuration: layer 1 of the
// precedence chain.
func Defaults() *Config {
	return &Config{
		Theme: "auto",
		Server: ServerConfig{
			DefaultPort: 8080,
			Host:        "localhost",
			Gzip:        true,
		},
		Tree: TreeConfig{
			Ignore: []string{".git", "node_modules", "vendor"},
		},
	}
}

// Load applies the full precedence chain except command-line flags,
// which the caller applies afterwards with ApplyFlags.
//
// A missing config file is not an error: Gecko works with zero
// configuration, which is the point of having defaults.
func Load(getenv Getenv) (*Config, error) {
	paths, err := ResolvePaths(getenv)
	if err != nil {
		return nil, err
	}

	cfg := Defaults()
	cfg.path = paths.ConfigFile()
	if cfg.Plugins.Directory == "" {
		cfg.Plugins.Directory = paths.Plugins
	}

	if err := cfg.loadFile(cfg.path); err != nil {
		return nil, err
	}
	cfg.applyEnv(getenv)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.path, err)
	}
	return cfg, nil
}

func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // absent config is the normal case
		}
		return fmt.Errorf("read config: %w", err)
	}

	// KnownFields makes a typo like "por: 3000" an error rather than a
	// setting that silently does nothing. This is the single most
	// valuable thing you can turn on in a config loader.
	dec := yaml.NewDecoder(newReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func (c *Config) applyEnv(getenv Getenv) {
	if getenv == nil {
		return
	}
	if v := getenv("GECKO_THEME"); v != "" {
		c.Theme = v
	}
	if v := getenv("GECKO_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := getenv("GECKO_PORT"); v != "" {
		// A malformed environment variable is silently ignored rather
		// than fatal: the user can still fix it with a flag, and
		// failing every command because of a stale shell export is
		// hostile. It is surfaced by "gecko doctor" in chapter 6.
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.DefaultPort = n
		}
	}
	if v := getenv("GECKO_PLUGIN_DIR"); v != "" {
		c.Plugins.Directory = v
	}
}

// Validate rejects configurations that would fail confusingly later.
func (c *Config) Validate() error {
	if c.Server.DefaultPort < 0 || c.Server.DefaultPort > 65535 {
		return fmt.Errorf("server.default_port: %d out of range 0-65535", c.Server.DefaultPort)
	}
	switch c.Theme {
	case "auto", "light", "dark", "none":
	default:
		return fmt.Errorf("theme: %q is not one of auto, light, dark, none", c.Theme)
	}
	if c.Plugins.Directory != "" && !filepath.IsAbs(c.Plugins.Directory) {
		return fmt.Errorf("plugins.directory: %q must be an absolute path", c.Plugins.Directory)
	}
	return nil
}

// Path reports where this configuration was loaded from.
func (c *Config) Path() string { return c.path }

// Save writes the configuration atomically: a temporary file in the same
// directory, then a rename. Same-directory matters because rename is only
// atomic within a filesystem.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config has no path")
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "config-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, c.path); err != nil {
		// On Windows this fails if another process holds the target
		// open. Report it plainly rather than silently losing the write.
		return fmt.Errorf("replace %s: %w", c.path, err)
	}
	return nil
}
```

Add `"bytes"` and a tiny helper (`func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }`),
or just inline `bytes.NewReader(data)`. And import `"runtime"`.

`dec.KnownFields(true)` deserves emphasis. Without it, `serevr: {port: 3000}` in a config
file does nothing and the user spends twenty minutes wondering why. With it, they get
`field serevr not found in type config.Config`. Turn it on in every config loader you
ever write.

### Env integration — `internal/cli/cli.go` additions

```go
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
```

`e.configOnce.Do(func() {})` marking the `Once` as done is a slightly sneaky but correct
way to pre-empt lazy loading in tests.

### `internal/cli/config.go`

```go
package cli

import (
	"context"
	"fmt"

	"github.com/yourname/gecko/internal/config"
	"gopkg.in/yaml.v3"
)

func newConfigCommand() *Command {
	return &Command{
		Name:  "config",
		Short: "Inspect and modify Gecko's configuration",
		Usage: "gecko config <subcommand>",
		Sub: map[string]CommandFunc{
			"path": newConfigPathCommand,
			"show": newConfigShowCommand,
			"init": newConfigInitCommand,
		},
		// No Run: this is a grouping command. The dispatcher prints help.
	}
}

func newConfigPathCommand() *Command {
	var all bool
	return &Command{
		Name:  "path",
		Short: "Print configuration and data directory paths",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&all, "all", false, "print every Gecko directory")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			p, err := config.ResolvePaths(env.Getenv)
			if err != nil {
				return err
			}
			if !all {
				fmt.Fprintln(env.Stdout, p.ConfigFile())
				return nil
			}
			w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "config\t%s\n", p.Config)
			fmt.Fprintf(w, "data\t%s\n", p.Data)
			fmt.Fprintf(w, "cache\t%s\n", p.Cache)
			fmt.Fprintf(w, "plugins\t%s\n", p.Plugins)
			return w.Flush()
		},
	}
}

func newConfigShowCommand() *Command {
	return &Command{
		Name:  "show",
		Short: "Print the effective configuration",
		Long: "Print the configuration after applying defaults, the config\n" +
			"file and environment variables.",
		Run: func(ctx context.Context, env *Env, args []string) error {
			cfg, err := env.Config()
			if err != nil {
				return err
			}
			enc := yaml.NewEncoder(env.Stdout)
			enc.SetIndent(2)
			if err := enc.Encode(cfg); err != nil {
				return err
			}
			return enc.Close()
		},
	}
}

func newConfigInitCommand() *Command {
	var force bool
	return &Command{
		Name:  "init",
		Short: "Write a default configuration file",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&force, "force", false, "overwrite an existing file")
		},
		Run: func(ctx context.Context, env *Env, args []string) error {
			paths, err := config.ResolvePaths(env.Getenv)
			if err != nil {
				return err
			}
			target := paths.ConfigFile()
			if _, err := os.Stat(target); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", target)
			}
			cfg := config.Defaults()
			cfg.SetPath(target) // add this setter to config
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "wrote %s\n", target)
			return nil
		},
	}
}
```

Imports needed: `flag`, `os`, `text/tabwriter`.

Register: `a.Register(newConfigCommand)`.

---

## F. Exercise

1. Implement `gecko config get <key>` and `gecko config set <key> <value>` with
   dotted keys (`server.default_port`). Two approaches: a hand-written switch over
   known keys, or marshal to `map[string]any`, navigate, and unmarshal back. Try the
   second; discover why it loses type information and comments; then decide which you'd
   ship. The answer is not obvious and both are defensible.

2. `Load` returns an error if the file is malformed, which means a broken config file
   makes *every* command fail — including `gecko config path`, which the user needs in
   order to fix it. Restructure so that `config path` and `config init` still work with a
   broken file.

3. Write a table test for `ResolvePaths` that asserts the correct directory on all three
   platforms. You can't call `runtime.GOOS` differently per test case — so how do you
   test the Windows branch from Linux? (Hint: the answer involves either refactoring
   `userDataDir` to take GOOS as a parameter, or accepting that this branch is only
   covered in CI. Both are legitimate; argue for one.)

---

## G. Testing

```go
package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func fakeEnv(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestResolvePathsOverride(t *testing.T) {
	t.Parallel()
	got, err := ResolvePaths(fakeEnv(map[string]string{
		"GECKO_CONFIG_DIR": "/custom/cfg",
		"GECKO_DATA_DIR":   "/custom/data",
		"GECKO_CACHE_DIR":  "/custom/cache",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Config != "/custom/cfg" {
		t.Errorf("Config = %q", got.Config)
	}
	if want := filepath.Join("/custom/data", "plugins"); got.Plugins != want {
		t.Errorf("Plugins = %q, want %q", got.Plugins, want)
	}
}

func TestResolvePathsXDG(t *testing.T) {
	t.Parallel()
	got, err := ResolvePaths(fakeEnv(map[string]string{
		"XDG_CONFIG_HOME": "/xdg/config",
		"XDG_DATA_HOME":   "/xdg/data",
		"XDG_CACHE_HOME":  "/xdg/cache",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/xdg/config", "gecko"); got.Config != want {
		t.Errorf("Config = %q, want %q", got.Config, want)
	}
}

func TestResolvePathsIgnoresRelativeXDG(t *testing.T) {
	t.Parallel()
	// The XDG spec requires relative values to be ignored entirely.
	got, err := ResolvePaths(fakeEnv(map[string]string{"XDG_CONFIG_HOME": "relative/path"}))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs("relative/path") {
		t.Skip("unreachable")
	}
	if got.Config == filepath.Join("relative/path", "gecko") {
		t.Error("relative XDG_CONFIG_HOME should have been ignored")
	}
}
```

Note the `ResolvePaths` calls do **not** touch the real environment, because we inject
`Getenv`. Compare with the alternative — `t.Setenv("XDG_CONFIG_HOME", ...)` — which works
but forbids `t.Parallel()` (Go's testing package panics if you combine them, deliberately).
Injection beats environment mutation.

Config loading round-trip:

```go
func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "gecko", "config.yaml")
	os.MkdirAll(filepath.Dir(cfgFile), 0o700)
	os.WriteFile(cfgFile, []byte("theme: dark\nserver:\n  default_port: 9000\n"), 0o600)

	env := fakeEnv(map[string]string{"XDG_CONFIG_HOME": dir})

	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "dark" {
		t.Errorf("file did not override default: %q", cfg.Theme)
	}
	if cfg.Server.DefaultPort != 9000 {
		t.Errorf("port = %d", cfg.Server.DefaultPort)
	}
	if !cfg.Server.Gzip {
		t.Error("unspecified field lost its default")
	}

	// Now environment beats file.
	env2 := fakeEnv(map[string]string{"XDG_CONFIG_HOME": dir, "GECKO_PORT": "7777"})
	cfg2, _ := Load(env2)
	if cfg2.Server.DefaultPort != 7777 {
		t.Errorf("env did not override file: %d", cfg2.Server.DefaultPort)
	}
}
```

That third assertion — `!cfg.Server.Gzip` — is the one that catches the classic config
bug: unmarshalling into a fresh struct rather than one pre-populated with defaults,
zeroing every field the file omitted. Test for it explicitly.

Error paths:

```go
func TestLoadRejectsUnknownFields(t *testing.T) { /* "serevr: {}" → error mentioning serevr */ }
func TestLoadRejectsBadPort(t *testing.T)       { /* default_port: 99999 → range error */ }
func TestSaveIsAtomic(t *testing.T)             { /* no *.tmp left behind after Save */ }
```

---

## H. Review

- The three directory kinds (config/data/cache) and why conflating them is wrong.
- What `os.UserConfigDir` gives you and what it doesn't (`UserDataDir` doesn't exist).
- The four-layer precedence chain and why `fs.Visit` (not `VisitAll`) is the key to
  layer 4.
- Why `runtime.GOOS` switching is correct here and build tags aren't — yet.
- `yaml.Decoder.KnownFields(true)` and why it's non-negotiable.
- Write-temp-then-rename, and how Windows differs.
- Why injecting `Getenv` beats `t.Setenv` for testability.
- `sync.Once` for lazy initialisation, and the `copylocks` consequence.

---

## I. Refactoring

Look back at chapter 2's `tree` command. It has a `--depth` flag with a hard-coded
default of 0 and an `Ignore` option nothing ever populates. Now that config exists,
wire it up — and in doing so, implement layer 4 properly:

```go
Run: func(ctx context.Context, env *Env, args []string) error {
    cfg, err := env.Config()
    if err != nil {
        return err
    }

    opts := filesystem.TreeOptions{
        MaxDepth: cfg.Tree.MaxDepth,
        Ignore:   cfg.Tree.Ignore,
    }
    // Flags win over config, but only if actually set.
    if flagSet["depth"] || flagSet["L"] {
        opts.MaxDepth = depth
    }
    ...
}
```

Which requires the dispatcher to hand the command the set of explicitly-provided flags.
Add to `runCommand`, after `fs.Parse`:

```go
provided := make(map[string]bool)
fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
```

and thread it through. Where? Adding a fourth parameter to every `Run` is invasive. Put
it on a per-invocation value instead:

```go
type Command struct {
    ...
    Run func(ctx context.Context, env *Env, args []string) error
}
```

becomes, cleanly:

```go
// Invocation carries per-run state that Run needs but that isn't a
// positional argument.
type Invocation struct {
    Args     []string
    Provided map[string]bool  // flags the user explicitly set
}

Run func(ctx context.Context, env *Env, inv *Invocation) error
```

Yes, this touches every command. There are three of them. **This is why we refactor early
— the cost of a signature change grows linearly with command count, and we have the
information to make the change now.** Do it.

---

## Commit

```
feat: add cross-platform configuration with XDG support
feat: add config command for path, show and init
refactor: thread explicit-flag information through command invocations
```

The refactor is a separate commit from the feature because it's mechanical and touches
many files; reviewers should be able to skip it quickly after checking one example.

Next: `04-hash.md`.

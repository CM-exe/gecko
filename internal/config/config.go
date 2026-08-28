package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

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

	// A configuration file is user-editable input.  Do not make its
	// contents a prerequisite for commands that only need the config path
	// (notably `config path` and `config init`): those commands must remain
	// usable so the user can inspect or replace a broken file.  loadFile
	// reports filesystem errors, but deliberately treats YAML errors as an
	// unusable file and leaves the compiled-in defaults in place.
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
		// Keep path-oriented and recovery commands operational when the file
		// is malformed.  The next Save will replace it atomically.
		return nil
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

func (c *Config) SetPath(path string) { c.path = path }

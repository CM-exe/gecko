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

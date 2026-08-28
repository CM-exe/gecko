package config

import (
	"os"
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

// runtime.GOOS is fixed for a process, so the Windows and macOS branches
// cannot be forced from a Linux test. The table documents all conventions;
// each case runs on its native CI platform.
func TestResolvePathsPlatformDataDirectory(t *testing.T) {
	tests := []struct {
		name string
		goos string
		want func(home, localAppData string) string
	}{
		{
			name: "windows",
			goos: "windows",
			want: func(_, localAppData string) string { return localAppData },
		},
		{
			name: "darwin",
			goos: "darwin",
			want: func(home, _ string) string {
				return filepath.Join(home, "Library", "Application Support")
			},
		},
		{
			name: "linux",
			goos: "linux",
			want: func(home, _ string) string {
				return filepath.Join(home, ".local", "share")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS != tt.goos {
				t.Skipf("GOOS=%s; this case runs on %s CI", runtime.GOOS, tt.goos)
			}

			home := t.TempDir()
			localAppData := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("GECKO_DATA_DIR", "")
			t.Setenv("XDG_DATA_HOME", "")
			t.Setenv("LOCALAPPDATA", localAppData)

			got, err := ResolvePaths(os.Getenv)
			if err != nil {
				t.Fatalf("ResolvePaths() error = %v", err)
			}
			want := filepath.Join(tt.want(home, localAppData), appName)
			if got.Data != want {
				t.Errorf("Data = %q, want %q", got.Data, want)
			}
			if got.Plugins != filepath.Join(want, "plugins") {
				t.Errorf("Plugins = %q, want %q", got.Plugins, filepath.Join(want, "plugins"))
			}
		})
	}
}

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

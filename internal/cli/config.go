package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"

	"github.com/CM-exe/gecko/internal/config"
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
			"get":  newConfigGetCommand,
			"set":  newConfigSetCommand,
		},
		// No Run: this is a grouping command. The dispatcher prints help.
	}
}

func newConfigGetCommand() *Command {
	return &Command{
		Name:  "get",
		Short: "Get a configuration value",
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			if len(inv.Args) != 1 {
				return fmt.Errorf("usage: gecko config get <key>")
			}
			cfg, err := env.Config()
			if err != nil {
				return err
			}
			values, err := configAsMap(cfg)
			if err != nil {
				return err
			}
			value, err := configMapValue(values, inv.Args[0])
			if err != nil {
				return err
			}
			return yaml.NewEncoder(env.Stdout).Encode(value)
		},
	}
}

func newConfigSetCommand() *Command {
	return &Command{
		Name:  "set",
		Short: "Set a configuration value",
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			if len(inv.Args) != 2 {
				return fmt.Errorf("usage: gecko config set <key> <value>")
			}
			cfg, err := env.Config()
			if err != nil {
				return err
			}
			values, err := configAsMap(cfg)
			if err != nil {
				return err
			}
			var value any
			if err := yaml.Unmarshal([]byte(inv.Args[1]), &value); err != nil {
				return fmt.Errorf("invalid value: %w", err)
			}
			if err := setConfigMapValue(values, inv.Args[0], value); err != nil {
				return err
			}
			data, err := yaml.Marshal(values)
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return fmt.Errorf("invalid value for %s: %w", inv.Args[0], err)
			}
			return cfg.Save()
		},
	}
}

// This map round-trip is concise, but it loses YAML comments and can lose
// scalar type information in the intermediate map. Typed accessors would be
// preferable if preserving comments or strict input types were required.
func configAsMap(cfg any) (map[string]any, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func configMapValue(values map[string]any, key string) (any, error) {
	if key == "" {
		return nil, fmt.Errorf("configuration key cannot be empty")
	}
	var current any = values
	for _, part := range strings.Split(key, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("configuration key %q is not an object", key)
		}
		var exists bool
		current, exists = object[part]
		if !exists {
			return nil, fmt.Errorf("unknown configuration key %q", key)
		}
	}
	return current, nil
}

func setConfigMapValue(values map[string]any, key string, value any) error {
	if key == "" {
		return fmt.Errorf("configuration key cannot be empty")
	}
	parts := strings.Split(key, ".")
	object := values
	for _, part := range parts[:len(parts)-1] {
		next, ok := object[part].(map[string]any)
		if !ok {
			return fmt.Errorf("unknown configuration key %q", key)
		}
		object = next
	}
	last := parts[len(parts)-1]
	if _, ok := object[last]; !ok {
		return fmt.Errorf("unknown configuration key %q", key)
	}
	object[last] = value
	return nil
}

func newConfigPathCommand() *Command {
	var all bool
	return &Command{
		Name:  "path",
		Short: "Print configuration and data directory paths",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&all, "all", false, "print every Gecko directory")
		},
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
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
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
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
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			paths, err := config.ResolvePaths(env.Getenv)
			if err != nil {
				return err
			}
			target := paths.ConfigFile()
			if _, err := os.Stat(target); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", target)
			}
			cfg := config.Defaults()
			cfg.SetPath(target)
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "wrote %s\n", target)
			return nil
		},
	}
}

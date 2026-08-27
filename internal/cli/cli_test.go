package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

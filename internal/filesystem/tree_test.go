package filesystem

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fs.FS {
	return fstest.MapFS{
		"cmd/gecko/main.go":        {Data: []byte("package main\n")},
		"internal/cli/cli.go":      {Data: []byte("package cli\n")},
		"internal/cli/help.go":     {Data: []byte("package cli\n")},
		"internal/filesystem/t.go": {Data: []byte("package filesystem\n")},
		".hidden/secret.txt":       {Data: []byte("shh\n")},
		".gitignore":               {Data: []byte("/gecko\n")},
		"go.mod":                   {Data: []byte("module x\n")},
		"README.md":                {Data: []byte("# x\n")},
	}
}

func TestWriteTree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     TreeOptions
		contains []string
		excludes []string
		wantDirs int
	}{
		{
			name:     "default hides dotfiles",
			opts:     TreeOptions{ASCII: true},
			contains: []string{"cmd/", "go.mod", "README.md"},
			excludes: []string{".gitignore", ".hidden"},
			wantDirs: 5,
		},
		{
			name:     "all shows dotfiles",
			opts:     TreeOptions{ASCII: true, ShowAll: true},
			contains: []string{".gitignore", ".hidden/"},
		},
		{
			name:     "depth 1 stops at top level",
			opts:     TreeOptions{ASCII: true, MaxDepth: 1},
			contains: []string{"cmd/", "internal/"},
			excludes: []string{"main.go", "cli.go"},
		},
		{
			name:     "dirs only",
			opts:     TreeOptions{ASCII: true, DirsOnly: true},
			contains: []string{"cmd/", "internal/"},
			excludes: []string{"go.mod", "README.md"},
		},
		{
			name:     "ignore skips directories",
			opts:     TreeOptions{ASCII: true, Ignore: []string{"internal"}},
			contains: []string{"cmd/"},
			excludes: []string{"internal/", "cli.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			stats, err := WriteTree(context.Background(), &buf, testFS(), ".", tt.opts)
			if err != nil {
				t.Fatalf("WriteTree: %v", err)
			}
			got := buf.String()
			for _, s := range tt.contains {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q:\n%s", s, got)
				}
			}
			for _, s := range tt.excludes {
				if strings.Contains(got, s) {
					t.Errorf("output should not contain %q:\n%s", s, got)
				}
			}
			if tt.wantDirs > 0 && stats.Dirs != tt.wantDirs {
				t.Errorf("Dirs = %d, want %d", stats.Dirs, tt.wantDirs)
			}
		})
	}
}

func TestWriteTreeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, root string }{
		{"nonexistent", "nope"},
		{"a file, not a directory", "go.mod"},
		{"absolute path is invalid for fs.FS", "/etc"},
		{"parent traversal is invalid", "../x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if _, err := WriteTree(context.Background(), &buf, testFS(), c.root, TreeOptions{}); err == nil {
				t.Errorf("expected error for root %q", c.root)
			}
		})
	}
}

func TestWriteTreeCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	var buf bytes.Buffer
	_, err := WriteTree(ctx, &buf, testFS(), ".", TreeOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestWriteTreeGolden(t *testing.T) {
	var buf bytes.Buffer
	_, err := WriteTree(context.Background(), &buf, testFS(), ".", TreeOptions{ShowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "tree_all.golden")

	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != string(want) {
		t.Errorf("output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

var update = flag.Bool("update", false, "update golden files")

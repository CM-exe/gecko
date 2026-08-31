package filesystem

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
)

// Walker performs a cancellable, filtered directory walk. It exists
// because find, clean, project and watch all need the same pruning
// rules, and four copies would drift.
type Walker struct {
	Root          string
	IncludeHidden bool
	Ignore        []string
	MaxDepth      int
	OnError       func(path string, err error) // nil = ignore and continue
}

func (w *Walker) Walk(ctx context.Context, fn func(path string, d fs.DirEntry) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return nil
	}

	root := w.Root
	if root == "" {
		root = "."
	}

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			if w.OnError != nil {
				w.OnError(path, err)
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d == nil {
			return nil
		}

		name := d.Name()
		isRoot := path == root

		if d.IsDir() && !isRoot {
			if !w.IncludeHidden && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if containsString(w.Ignore, name) {
				return fs.SkipDir
			}
			if w.MaxDepth > 0 && depthOf(root, path) >= w.MaxDepth {
				return fs.SkipDir
			}
		}

		if !w.IncludeHidden && strings.HasPrefix(name, ".") && !isRoot {
			return nil
		}

		if err := fn(path, d); err != nil {
			if err == fs.SkipDir {
				return nil
			}
			return err
		}
		return nil
	})
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func depthOf(root, path string) int {
	if root == "" {
		root = "."
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return 0
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

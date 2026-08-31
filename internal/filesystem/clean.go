package filesystem

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Target describes a directory that is safe to delete when its marker
// file is present in the parent directory.
//
// The marker requirement is the core safety property: a directory named
// "target" is only a Rust build directory if its parent has Cargo.toml.
// Without that evidence, "target" is just a directory someone named
// "target", and deleting it is data loss.
type Target struct {
	Name        string
	Markers     []string // any one of these in the parent qualifies
	Description string
	Regenerable string // the command that recreates it
}

// cleanTargets is a fixed allowlist. There is deliberately no way for a
// user to extend it with a pattern: an arbitrary-pattern delete is a
// different, more dangerous tool, and Gecko does not provide one.
var cleanTargets = []Target{
	{"node_modules", []string{"package.json"}, "npm/yarn/pnpm dependencies", "npm install"},
	{"target", []string{"Cargo.toml"}, "Rust build artifacts", "cargo build"},
	{"dist", []string{"package.json"}, "JavaScript build output", "npm run build"},
	{"build", []string{"CMakeLists.txt", "Makefile"}, "C/C++ build output", "make"},
	{".pytest_cache", []string{"pyproject.toml", "setup.py", "setup.cfg"}, "pytest cache", "(regenerated automatically)"},
	{"__pycache__", []string{}, "Python bytecode cache", "(regenerated automatically)"},
	{".mypy_cache", []string{"pyproject.toml", "setup.py", "mypy.ini"}, "mypy cache", "(regenerated automatically)"},
	{".next", []string{"package.json"}, "Next.js build cache", "npm run build"},
	{".nuxt", []string{"package.json"}, "Nuxt build cache", "npm run build"},
	{"vendor", []string{"composer.json"}, "Composer dependencies", "composer install"},
	{".gradle", []string{"build.gradle", "build.gradle.kts"}, "Gradle cache", "(regenerated automatically)"},
}

// Candidate is one directory Gecko proposes to delete.
type Candidate struct {
	Path      string `json:"path"`
	RelPath   string `json:"rel_path"`
	Target    Target `json:"target"`
	Size      int64  `json:"size_bytes"`
	FileCount int    `json:"file_count"`
	SizeErr   error  `json:"size_error,omitempty"`
}

// ScanForCleanup finds deletable directories under root.
//
// It never deletes anything. Deletion is a separate call so that the
// caller is structurally required to confirm in between.
func ScanForCleanup(ctx context.Context, root string) ([]Candidate, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := checkSafeRoot(rootAbs); err != nil {
		return nil, err
	}

	byName := make(map[string]Target, len(cleanTargets))
	for _, t := range cleanTargets {
		byName[t.Name] = t
	}

	var candidates []Candidate

	walker := &Walker{
		Root:          rootAbs,
		IncludeHidden: true,
		Ignore:        []string{".git", ".hg", ".svn"},
		OnError: func(path string, err error) {
			// Ignore per-entry failures; parent traversal will continue.
		},
	}

	err = walker.Walk(ctx, func(path string, d fs.DirEntry) error {
		if !d.IsDir() || path == rootAbs {
			return nil
		}

		t, ok := byName[d.Name()]
		if !ok {
			return nil
		}
		if !hasMarker(filepath.Dir(path), t.Markers) {
			return nil // named right, but no evidence: leave it alone
		}
		if ok, err := contained(rootAbs, path); err != nil || !ok {
			// Symlinked outside the scan root, or unresolvable.
			return fs.SkipDir
		}

		candidates = append(candidates, Candidate{
			Path: path, RelPath: relOf(rootAbs, path), Target: t,
		})
		// Do not descend: a node_modules inside a node_modules is
		// already accounted for by the parent.
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}

	if err := sizeCandidates(ctx, candidates); err != nil {
		return candidates, err
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Size > candidates[j].Size })
	return candidates, nil
}

func hasMarker(dir string, markers []string) bool {
	if len(markers) == 0 {
		return true // e.g. __pycache__ is unambiguous by name alone
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// sizeCandidates computes each candidate's total size concurrently.
// This is the second legitimate use of a worker pool in this package:
// each candidate is an independent, syscall-heavy subtree walk.
func sizeCandidates(ctx context.Context, cands []Candidate) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.NumCPU())

	for i := range cands {
		i := i
		g.Go(func() error {
			size, count, err := dirSize(gctx, cands[i].Path)
			cands[i].Size, cands[i].FileCount, cands[i].SizeErr = size, count, err
			// A sizing failure is informational, not fatal: we can
			// still offer to delete a directory we could not measure.
			if gctx.Err() != nil {
				return gctx.Err()
			}
			return nil
		})
	}
	return g.Wait()
}

func dirSize(ctx context.Context, root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// Apparent size (bytes in file), not disk usage (blocks
		// allocated). du reports the latter; getting it portably would
		// require syscall.Stat_t on Unix with no Windows equivalent.
		total += info.Size()
		count++
		return nil
	})
	return total, count, err
}

// contained reports whether candidate resolves to a location inside root.
// Both paths are fully symlink-resolved first: without that, a symlink
// such as ./node_modules -> /usr/lib would pass a string prefix test.
func contained(root, candidate string) (bool, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	candReal, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(rootReal, candReal)
	if err != nil {
		return false, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// checkSafeRoot refuses to scan locations where a mistake would be
// catastrophic.
func checkSafeRoot(root string) error {
	clean := filepath.Clean(root)

	if clean == string(filepath.Separator) || clean == filepath.VolumeName(clean)+string(filepath.Separator) {
		return fmt.Errorf("refusing to scan the filesystem root %q", clean)
	}
	for _, bad := range []string{"/usr", "/etc", "/var", "/bin", "/sbin", "/lib", "/System", "/Library",
		"C:\\Windows", "C:\\Program Files"} {
		if strings.EqualFold(clean, filepath.Clean(bad)) {
			return fmt.Errorf("refusing to scan system directory %q", clean)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == clean {
		return fmt.Errorf("refusing to scan your home directory directly; " +
			"scan a project directory, or pass --force if you are sure")
	}
	return nil
}

// Delete removes a candidate. It re-verifies containment immediately
// before deleting: the scan and the confirmation are separated by an
// unbounded amount of time during which the filesystem can change, and
// a TOCTOU window here means deleting the wrong thing.
func Delete(root string, c Candidate) error {
	ok, err := contained(root, c.Path)
	if err != nil {
		return fmt.Errorf("verify %s: %w", c.RelPath, err)
	}
	if !ok {
		return fmt.Errorf("refusing to delete %s: no longer inside %s", c.Path, root)
	}
	if filepath.Base(c.Path) != c.Target.Name {
		return fmt.Errorf("refusing to delete %s: name changed since scan", c.Path)
	}
	return os.RemoveAll(c.Path)
}

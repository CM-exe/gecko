package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCleanRefusesUnsafeDirectoryTrees(t *testing.T) {
	t.Run("symlinked node_modules", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "app")
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}
		if err := os.WriteFile(filepath.Join(project, "package.json"), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write package.json: %v", err)
		}

		outside := filepath.Join(t.TempDir(), "external")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatalf("mkdir outside: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(project, "node_modules")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		cands, err := ScanForCleanup(context.Background(), root)
		if err != nil {
			t.Fatalf("scan returned error: %v", err)
		}
		for _, c := range cands {
			if filepath.Base(c.Path) == "node_modules" {
				t.Fatalf("unsafe symlinked node_modules was accepted: %#v", c)
			}
		}
	})

	t.Run("parent package.json is itself a symlink", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "app")
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatalf("mkdir project: %v", err)
		}

		outsidePkgDir := filepath.Join(t.TempDir(), "external")
		if err := os.MkdirAll(outsidePkgDir, 0o755); err != nil {
			t.Fatalf("mkdir outside pkg dir: %v", err)
		}
		pkgTarget := filepath.Join(outsidePkgDir, "package.json")
		if err := os.WriteFile(pkgTarget, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write external package.json: %v", err)
		}
		if err := os.Symlink(pkgTarget, filepath.Join(project, "package.json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Mkdir(filepath.Join(project, "node_modules"), 0o755); err != nil {
			t.Fatalf("mkdir node_modules: %v", err)
		}

		cands, err := ScanForCleanup(context.Background(), root)
		if err != nil {
			t.Fatalf("scan returned error: %v", err)
		}
		for _, c := range cands {
			if filepath.Base(c.Path) == "node_modules" {
				t.Fatalf("unsafe package.json symlink was accepted: %#v", c)
			}
		}
	})

	t.Run("directory named ../../etc", func(t *testing.T) {
		root := t.TempDir()
		bad := filepath.Clean(filepath.Join(root, "..", "..", "etc"))
		if err := os.MkdirAll(bad, 0o755); err != nil {
			t.Fatalf("mkdir outside dir: %v", err)
		}

		err := Delete(root, Candidate{
			Path:    bad,
			RelPath: "../../etc",
			Target:  Target{Name: "node_modules"},
		})
		if err == nil {
			t.Fatal("Delete unexpectedly succeeded for a path outside the root")
		}
		if !strings.Contains(err.Error(), "refusing to delete") && !strings.Contains(err.Error(), "verify") {
			t.Fatalf("unexpected error for outside path: %v", err)
		}
	})
}

func TestCleanRefusesUnmarkedDirectory(t *testing.T) {
	dir := t.TempDir()
	// A "target" directory with no Cargo.toml: someone's photos.
	os.MkdirAll(filepath.Join(dir, "photos", "target"), 0o755)
	os.WriteFile(filepath.Join(dir, "photos", "target", "img.jpg"), []byte("x"), 0o644)

	cands, err := ScanForCleanup(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("proposed deleting %d unmarked directories: %+v", len(cands), cands)
	}
}

func TestCleanRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "precious.txt"), []byte("do not delete"), 0o644)

	proj := filepath.Join(root, "proj")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "package.json"), []byte("{}"), 0o644)
	if err := os.Symlink(outside, filepath.Join(proj, "node_modules")); err != nil {
		t.Skip(err)
	}

	cands, _ := ScanForCleanup(context.Background(), root)
	for _, c := range cands {
		if err := Delete(root, c); err == nil {
			t.Errorf("deleted %s, which escapes the root", c.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious.txt")); err != nil {
		t.Fatal("the file outside the root was destroyed")
	}
}

func TestCleanRefusesDangerousRoots(t *testing.T) {
	for _, root := range []string{"/", "/usr", "/etc"} {
		if _, err := ScanForCleanup(context.Background(), root); err == nil {
			t.Errorf("ScanForCleanup(%q) should have refused", root)
		}
	}
}

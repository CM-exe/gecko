package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // automatically removed after the test
	mustWrite(t, filepath.Join(dir, "a.txt"), "hello")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	mustWrite(t, filepath.Join(dir, "sub", "b.txt"), "world")

	env, out, _ := testEnv(nil)
	env.WorkDir = dir

	code := Main(context.Background(), []string{"tree", "--ascii"}, env)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "1 directories, 2 files") {
		t.Errorf("bad summary:\n%s", out)
	}
}

func mustWrite(t *testing.T, s1, s2 string) {
	t.Helper()
	if err := os.WriteFile(s1, []byte(s2), 0o644); err != nil {
		t.Fatalf("write %s: %v", s1, err)
	}
}

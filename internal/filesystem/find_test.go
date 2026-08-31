package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func buildTree(t *testing.T, files int) string {
	t.Helper()
	root := t.TempDir()

	for i := 0; i < files; i++ {
		dir := i % 50
		path := filepath.Join(root, "dir"+strconv.Itoa(dir), "file"+strconv.Itoa(i)+".txt")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
	}
	return root
}

func TestFindConcurrentSafety(t *testing.T) {
	dir := buildTree(t, 500) // helper creating 500 files across 50 dirs

	for i := 0; i < 5; i++ { // repeat: races are probabilistic
		out := make(chan Match, 16)
		var got int
		done := make(chan struct{})
		go func() {
			for range out {
				got++
			}
			close(done)
		}()
		stats, err := Find(context.Background(), dir,
			FindOptions{Pattern: "*.txt", MinSize: 1, Workers: 32}, out)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if int64(got) != stats.Matched {
			t.Errorf("received %d matches, stats say %d", got, stats.Matched)
		}
	}
}

func TestFindNoGoroutineLeak(t *testing.T) {
	dir := buildTree(t, 200)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Match) // unbuffered: producer blocks immediately
	go func() {
		<-out    // consume exactly one, then abandon
		cancel() // simulate a consumer that gave up
	}()
	Find(ctx, dir, FindOptions{MinSize: 1}, out)

	// Give the runtime a moment to reap.
	for i := 0; i < 50 && runtime.NumGoroutine() > before; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Errorf("leaked goroutines: before=%d after=%d", before, after)
	}
}

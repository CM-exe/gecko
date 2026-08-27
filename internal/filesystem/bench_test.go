package filesystem

import (
	"context"
	"fmt"
	"io"
	"testing"
	"testing/fstest"
)

func largeFS(files int) fstest.MapFS {
	fsys := make(fstest.MapFS, files)
	for i := 0; i < files; i++ {
		dir := i % 100
		fsys[fmt.Sprintf("dir%03d/file%04d", dir, i)] = &fstest.MapFile{}
	}
	return fsys
}

func BenchmarkWriteTree(b *testing.B) {
	fsys := largeFS(1000) // build a MapFS with 1000 files across 100 dirs
	b.ReportAllocs()

	for b.Loop() {
		WriteTree(context.Background(), io.Discard, fsys, ".", TreeOptions{})
	}
}

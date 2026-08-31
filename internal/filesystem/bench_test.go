package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
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

func hashParallel(r io.Reader, algos []Algorithm) {
	if len(algos) == 0 {
		return
	}

	data, err := io.ReadAll(r)
	if err != nil || len(data) == 0 {
		return
	}

	chunks := 1
	if len(data) > 0 {
		chunks = len(algos)
	}
	if chunks <= 0 {
		chunks = 1
	}

	chunkSize := (len(data) + chunks - 1) / chunks
	for start := 0; start < len(data); start += chunkSize {
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}

		chunk := data[start:end]
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			HashReader(context.Background(), bytes.NewReader(chunk), algos)
		}()
		wg.Wait()
	}
}

func BenchmarkWriteTree(b *testing.B) {
	fsys := largeFS(1000) // build a MapFS with 1000 files across 100 dirs
	b.ReportAllocs()

	for b.Loop() {
		WriteTree(context.Background(), io.Discard, fsys, ".", TreeOptions{})
	}
}

func BenchmarkBufferSize(b *testing.B) {
	data := make([]byte, 32<<20) // 32 MiB
	rand.Read(data)

	for _, size := range []int{4 << 10, 16 << 10, 32 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20} {
		b.Run(fmt.Sprintf("buf=%dKiB", size>>10), func(b *testing.B) {
			buf := make([]byte, size)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h := sha256.New()
				r := bytes.NewReader(data)
				io.CopyBuffer(struct{ io.Writer }{h}, r, buf)
				// The struct wrapper defeats ReaderFrom/WriterTo so we
				// measure the copy loop rather than a fast path.
			}
		})
	}
}

func BenchmarkMultiAlgo(b *testing.B) {
	data := make([]byte, 16<<20)
	algos := []Algorithm{MD5, SHA1, SHA256}

	b.Run("MultiWriter", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			HashReader(context.Background(), bytes.NewReader(data), algos)
		}
	})

	b.Run("Parallel", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			hashParallel(bytes.NewReader(data), algos)
		}
	})
}

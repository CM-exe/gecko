package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// BenchmarkFindWorkers measures throughput for the metadata-filtered walk
// path, where the worker pool is actually exercised. Use a real tree on an SSD
// or network mount by setting BENCH_FIND_ROOT, or let the benchmark build a
// representative tree under the temp dir.
//
// The result is usually a throughput curve that climbs quickly from 1 to 4 or
// 8 workers, then flattens as the storage layer becomes the bottleneck. On SSDs
// the plateau is typically in the 8-16 worker range; on network mounts it is
// often lower and noisier because the syscall / read latency dominates. This is
// a good fit for a default of NumCPU()*4, capped at 64: it gives enough
// parallelism to hide I/O latency without creating a large queue of blocked
// goroutines on slower mounts.
func BenchmarkFindWorkers(b *testing.B) {
	root, err := benchFindRoot(b)
	if err != nil {
		b.Fatal(err)
	}

	const (
		dirs      = 200
		filesEach = 128
		fileSize  = 4 << 10
	)
	b.SetBytes(int64(dirs * filesEach * fileSize))
	b.ReportAllocs()

	for _, workers := range []int{1, 2, 4, 8, 16, 32, 64} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				out := make(chan Match, 1024)
				go func() {
					for range out {
					}
				}()

				stats, err := Find(context.Background(), root, FindOptions{
					Pattern: "*.txt",
					MinSize: 1,
					Workers: workers,
				}, out)
				if err != nil {
					b.Fatal(err)
				}
				if stats.Scanned == 0 {
					b.Fatal("Find scanned zero files")
				}
			}
		})
	}
}

func benchFindRoot(b *testing.B) (string, error) {
	if root := os.Getenv("BENCH_FIND_ROOT"); root != "" {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			return root, nil
		}
	}

	root := b.TempDir()
	const (
		dirs      = 200
		filesEach = 128
		fileSize  = 4 << 10
	)
	payload := bytes.Repeat([]byte("x"), fileSize)

	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("dir%03d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		for f := 0; f < filesEach; f++ {
			name := filepath.Join(dir, fmt.Sprintf("file%04d.txt", f))
			if err := os.WriteFile(name, payload, 0o644); err != nil {
				return "", err
			}
		}
	}
	return root, nil
}

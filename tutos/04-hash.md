# Chapter 4 — `gecko hash`: Streaming I/O and Measurement

```
Difficulty:   Intermediate
Est. time:    3–4 hours
Main concepts: hash.Hash, io.Copy / CopyBuffer, io.MultiWriter, buffer sizing,
               escape analysis, sync.Pool, testing.B, benchmem, pprof, benchstat,
               crypto/subtle for constant-time comparison
Prerequisites: Chapters 1–3
```

---

## A. Goal

```
$ gecko hash go.mod
sha256  d2a84f4b8b650937ec8f73cd8be2c74add5a911ba64df27458ed8229da804a26  go.mod

$ gecko hash --algo md5,sha1,sha256 large.iso
md5     8f14e45fceea167a5a36dedd4bea2543  large.iso
sha1    b6589fc6ab0dc82cf12099d1c2d40ab994e8410c  large.iso
sha256  4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce  large.iso

$ gecko hash --check checksums.txt
go.mod: OK
main.go: FAILED
gecko hash: 1 of 2 files failed verification

$ cat file | gecko hash -
sha256  ...  -
```

---

## B. Why this matters

Three lessons packed into a small command.

**Streaming.** `os.ReadFile` on a 4 GB ISO allocates 4 GB. That's the entire reason
`io.Reader` exists, and hashing is the cleanest demonstration: the algorithm is
inherently incremental, so there is no excuse for buffering the whole input.

**Measurement discipline.** This is the first place we benchmark. The temptation to
"optimise" hashing with goroutines is strong and, as we'll prove with numbers, mostly
wrong. Learning to reach for `testing.B` before `go func` is the point.

**Chapter 14 depends on it.** Plugin installation verifies SHA-256 checksums. The
constant-time comparison and the streaming verifier we write here get reused there,
with real security consequences.

---

## C. Concepts

### `hash.Hash` is just an `io.Writer`

```go
type Hash interface {
    io.Writer
    Sum(b []byte) []byte
    Reset()
    Size() int
    BlockSize() int
}
```

Because it embeds `io.Writer`, hashing a file is one line:

```go
h := sha256.New()
io.Copy(h, f)
sum := h.Sum(nil)
```

`Sum(b)` **appends** to `b` and returns the result; it does not reset the hash. Passing
`nil` gets a fresh slice. Passing a pre-allocated `buf[:0]` avoids an allocation, which
matters only in a loop over millions of files (chapter 5 will care).

`BlockSize()` is the algorithm's internal block size (64 bytes for SHA-256, 128 for
SHA-512). Feeding it data in multiples of the block size avoids internal buffering work.
Our buffer will be a multiple of 64 KB, so this is satisfied incidentally.

### `io.Copy` and its hidden fast paths

`io.Copy(dst, src)` is not a naive loop. It checks, in order:

1. Does `src` implement `io.WriterTo`? Then call `src.WriteTo(dst)`.
2. Does `dst` implement `io.ReaderFrom`? Then call `dst.ReadFrom(src)`.
3. Otherwise allocate a 32 KB buffer and loop.

This is why `io.Copy(netConn, file)` can end up using `sendfile(2)` on Linux — `net.TCPConn`
implements `ReaderFrom` and dispatches to the zero-copy syscall. Chapter 7's file server
benefits from exactly this.

For hashing, neither fast path applies (`hash.Hash` implements neither interface), so
you get the 32 KB path with a fresh allocation per call. `io.CopyBuffer(dst, src, buf)`
lets you supply the buffer and pick its size.

**Is 32 KB the right size?** We'll measure. The intuition: too small means more syscalls;
too large means more cache pressure and a bigger allocation. The typical sweet spot for
sequential file reads on modern hardware is 64 KB to 1 MB, but it's hardware-dependent,
which is precisely why you measure rather than guess.

### `io.MultiWriter` for multiple algorithms

Computing MD5, SHA-1 and SHA-256 by reading the file three times costs three times the
I/O. One read, three writers:

```go
h1, h2, h3 := md5.New(), sha1.New(), sha256.New()
w := io.MultiWriter(h1, h2, h3)
io.CopyBuffer(w, f, buf)
```

`MultiWriter.Write` calls each writer sequentially and returns early on the first error.
The CPU cost is additive (each algorithm still hashes every byte) but the I/O cost is
paid once. For a file larger than the page cache, that's a 3× improvement.

Could we parallelise the three hashes across goroutines? Yes, and it's the natural
follow-up question. We'll benchmark it in section G. Preview: for I/O-bound workloads it
gains nothing; for cached files it gains close to 3× on a multicore machine. Which means
the honest answer is "it depends on your working set", and that's a legitimate reason to
keep the simple version.

### Escape analysis, briefly and concretely

Go decides at compile time whether a value lives on the stack (free to allocate, freed on
return) or the heap (GC-managed). A value escapes if the compiler cannot prove its
lifetime is bounded by the function.

```bash
go build -gcflags='-m' ./internal/filesystem 2>&1 | grep escapes
```

Typical output you'll see here:

```
./hash.go:42:15: make([]byte, 65536) escapes to heap
```

Why does a local buffer escape? Because we pass it to `io.CopyBuffer`, which stores it in
an interface-typed call chain the compiler can't see through. A 64 KB heap allocation per
call is fine for one file and terrible for a million, which is what motivates `sync.Pool`
in section I.

Escape analysis rules of thumb worth internalising:
- Returning a pointer to a local → escapes.
- Storing in an interface → usually escapes (the interface holds a pointer).
- Passing to a function the compiler can't inline and analyse → escapes.
- Slices whose size isn't a compile-time constant → heap.

### Constant-time comparison

Verifying a checksum with `==` or `bytes.Equal` leaks timing information: the comparison
short-circuits at the first differing byte, so an attacker who can measure verification
time can recover the expected hash byte by byte.

For file checksums this threat is largely theoretical — the expected hash is usually
public. For chapter 14's signature verification it is **not** theoretical. Build the
habit now:

```go
import "crypto/subtle"

if subtle.ConstantTimeCompare(got, want) != 1 {
    return ErrMismatch
}
```

`ConstantTimeCompare` returns 1 for equal, 0 otherwise, and always examines every byte.
Note it returns 0 immediately if the lengths differ — length is not secret.

---

## D. Design

### API

```go
package filesystem

// Algorithm identifies a supported hash function.
type Algorithm string

const (
    MD5    Algorithm = "md5"
    SHA1   Algorithm = "sha1"
    SHA256 Algorithm = "sha256"
    SHA512 Algorithm = "sha512"
)

// Digest is one algorithm's result for one input.
type Digest struct {
    Algorithm Algorithm
    Sum       []byte
}

func (d Digest) Hex() string

// HashReader computes the requested digests in a single pass over r.
func HashReader(ctx context.Context, r io.Reader, algos []Algorithm) ([]Digest, error)
```

Note `HashReader` takes an `io.Reader`, not a path. That single choice gives us:
- stdin support for free (`gecko hash -`),
- testability without touching the disk,
- reuse from chapter 8's HTTP client (hash a response body) and chapter 14's downloader.

A `HashFile(path)` convenience wrapper can exist on top. **Always design the
`io.Reader`-shaped function first and the path-shaped one second.** This is one of the
most consistently useful heuristics in Go.

### Why `Algorithm` is a string type and not an int enum

`type Algorithm string` lets us parse from the command line, print in output, and use as
a map key with no conversion table. The cost is that invalid values are constructible
(`Algorithm("nonsense")`), so parsing must validate. A `map[Algorithm]func() hash.Hash`
registry handles both:

```go
var registry = map[Algorithm]func() hash.Hash{
    MD5:    md5.New,
    SHA1:   sha1.New,
    SHA256: sha256.New,
    SHA512: sha512.New,
}
```

Adding an algorithm is one map entry. Compare with a `switch`, which needs edits in three
places (parse, construct, list).

### Security note on offering MD5 and SHA-1

Both are cryptographically broken for collision resistance. MD5 collisions are trivial;
SHA-1 collisions were demonstrated in 2017 (SHAttered) and chosen-prefix collisions in
2020. They remain useful for non-adversarial integrity checks and for verifying against
legacy checksum files.

**Our policy:** offer them, default to SHA-256, and print a warning to *stderr* (not
stdout, so piping stays clean) when a broken algorithm is used for `--check`. Do not
silently allow a security-relevant verification with MD5. This is the kind of judgement
a real tool has to make explicitly rather than by omission.

---

## E. Implementation

### `internal/filesystem/hash.go`

```go
package filesystem

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
)

type Algorithm string

const (
	MD5    Algorithm = "md5"
	SHA1   Algorithm = "sha1"
	SHA256 Algorithm = "sha256"
	SHA512 Algorithm = "sha512"
)

// registry maps an algorithm name to its constructor. Adding support for
// a new algorithm is a single entry here.
var registry = map[Algorithm]func() hash.Hash{
	MD5:    md5.New,
	SHA1:   sha1.New,
	SHA256: sha256.New,
	SHA512: sha512.New,
}

// weak lists algorithms that are broken for collision resistance. They
// remain available for compatibility with existing checksum files but
// callers should warn when using them for verification.
var weak = map[Algorithm]bool{MD5: true, SHA1: true}

func (a Algorithm) Valid() bool { return registry[a] != nil }
func (a Algorithm) Weak() bool  { return weak[a] }

// Algorithms returns the supported algorithm names, sorted.
func Algorithms() []Algorithm {
	out := make([]Algorithm, 0, len(registry))
	for a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ParseAlgorithms parses a comma-separated list such as "sha256,md5".
func ParseAlgorithms(s string) ([]Algorithm, error) {
	parts := strings.Split(s, ",")
	out := make([]Algorithm, 0, len(parts))
	seen := make(map[Algorithm]bool, len(parts))

	for _, p := range parts {
		a := Algorithm(strings.ToLower(strings.TrimSpace(p)))
		if a == "" {
			continue
		}
		if !a.Valid() {
			return nil, fmt.Errorf("unknown algorithm %q (supported: %s)", a, joinAlgos(Algorithms()))
		}
		if seen[a] {
			continue // requesting sha256 twice is a typo, not an error
		}
		seen[a] = true
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no algorithms specified")
	}
	return out, nil
}

func joinAlgos(as []Algorithm) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = string(a)
	}
	return strings.Join(s, ", ")
}

// Digest is one algorithm's output for one input.
type Digest struct {
	Algorithm Algorithm
	Sum       []byte
}

func (d Digest) Hex() string { return hex.EncodeToString(d.Sum) }

func (d Digest) String() string { return string(d.Algorithm) + ":" + d.Hex() }

// hashBufSize is the read buffer used when streaming input into the hash
// functions. 64 KiB is a multiple of every supported algorithm's block
// size and, per BenchmarkBufferSize, sits at the point where larger
// buffers stop paying for themselves on typical hardware.
const hashBufSize = 64 * 1024

// HashReader computes every requested digest in a single pass over r.
//
// It never buffers the whole input, so it is safe on inputs larger than
// available memory. Cancellation is checked once per buffer, giving a
// worst-case latency of one read.
func HashReader(ctx context.Context, r io.Reader, algos []Algorithm) ([]Digest, error) {
	if len(algos) == 0 {
		return nil, fmt.Errorf("no algorithms specified")
	}

	hashes := make([]hash.Hash, len(algos))
	writers := make([]io.Writer, len(algos))
	for i, a := range algos {
		ctor := registry[a]
		if ctor == nil {
			return nil, fmt.Errorf("unknown algorithm %q", a)
		}
		hashes[i] = ctor()
		writers[i] = hashes[i]
	}

	var dst io.Writer
	if len(writers) == 1 {
		dst = writers[0] // avoid MultiWriter's indirection in the common case
	} else {
		dst = io.MultiWriter(writers...)
	}

	buf := make([]byte, hashBufSize)

	// We loop manually rather than calling io.CopyBuffer so that
	// cancellation is checked between reads. io.Copy has no way to
	// observe a context.
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return nil, werr // a hash.Hash never errors, but MultiWriter can
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	out := make([]Digest, len(algos))
	for i, h := range hashes {
		out[i] = Digest{Algorithm: algos[i], Sum: h.Sum(nil)}
	}
	return out, nil
}
```

The manual read loop replaces `io.CopyBuffer` for one reason: **context cancellation**.
`io.Copy` cannot be interrupted. Hashing a 50 GB file with no way to Ctrl-C it is a bug.
The cost is losing `io.Copy`'s `ReaderFrom`/`WriterTo` fast paths, which don't apply to
hashes anyway.

There's a subtlety in the loop: we process `n > 0` bytes **before** checking `err`. The
`io.Reader` contract explicitly permits returning `n > 0` together with `io.EOF` in the
same call, and readers that do this are legal. Checking the error first silently drops
the last chunk. This is one of the most common bugs in hand-written Go read loops.

### Checksum files

```go
// ChecksumEntry is one line of a checksum file, in the format written by
// sha256sum and friends: "<hex>  <filename>" (two spaces for binary mode,
// " *" for the same thing on some tools).
type ChecksumEntry struct {
	Sum  string
	Name string
	Line int
}

// ParseChecksumFile reads coreutils-style checksum lines from r.
func ParseChecksumFile(r io.Reader) ([]ChecksumEntry, error) {
	var out []ChecksumEntry
	sc := bufio.NewScanner(r)
	// Default Scanner token limit is 64 KiB; filenames can be long but
	// not that long. Left at the default deliberately: a longer line
	// means a malformed file, and we want the error.
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		// Split on the first run of whitespace; the filename may itself
		// contain spaces, so we must not use strings.Fields.
		i := strings.IndexAny(text, " \t")
		if i < 0 {
			return nil, fmt.Errorf("line %d: malformed entry", line)
		}
		sum := text[:i]
		name := strings.TrimLeft(text[i:], " \t")
		name = strings.TrimPrefix(name, "*") // binary-mode marker

		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("line %d: %q is not a hex digest", line, sum)
		}
		out = append(out, ChecksumEntry{Sum: sum, Name: name, Line: line})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AlgorithmForDigestLength guesses the algorithm from a hex digest's
// length. Checksum files rarely record which algorithm produced them.
func AlgorithmForDigestLength(hexLen int) (Algorithm, bool) {
	switch hexLen {
	case 32:
		return MD5, true
	case 40:
		return SHA1, true
	case 64:
		return SHA256, true
	case 128:
		return SHA512, true
	}
	return "", false
}

// VerifyReader checks r against an expected hex digest in constant time.
func VerifyReader(ctx context.Context, r io.Reader, algo Algorithm, wantHex string) error {
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		return fmt.Errorf("expected digest is not valid hex: %w", err)
	}
	digests, err := HashReader(ctx, r, []Algorithm{algo})
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(digests[0].Sum, want) != 1 {
		return &MismatchError{Algorithm: algo, Want: wantHex, Got: digests[0].Hex()}
	}
	return nil
}

// MismatchError reports a failed verification.
type MismatchError struct {
	Algorithm Algorithm
	Want, Got string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s mismatch: want %s, got %s", e.Algorithm, e.Want, e.Got)
}
```

Add imports: `bufio`, `crypto/subtle`.

### `internal/cli/hash.go`

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/yourname/gecko/internal/filesystem"
)

func newHashCommand() *Command {
	var (
		algoFlag  string
		checkFile string
		quiet     bool
	)

	return &Command{
		Name:  "hash",
		Short: "Compute or verify file checksums",
		Usage: "gecko hash [flags] <file>... | gecko hash --check <file>",
		Long: "Compute cryptographic digests of files, streaming so that\n" +
			"inputs larger than memory are handled correctly.\n" +
			"Use \"-\" to read from standard input.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&algoFlag, "algo", "sha256", "comma-separated algorithms")
			fs.StringVar(&algoFlag, "a", "sha256", "shorthand for --algo")
			fs.StringVar(&checkFile, "check", "", "verify against a checksum file")
			fs.BoolVar(&quiet, "quiet", false, "with --check, print only failures")
		},
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			algos, err := filesystem.ParseAlgorithms(algoFlag)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrUsage, err)
			}
			if checkFile != "" {
				return runCheck(ctx, env, checkFile, quiet)
			}
			if len(inv.Args) == 0 {
				fmt.Fprintln(env.Stderr, "gecko hash: no files given")
				return Quiet(ErrUsage)
			}
			return runHash(ctx, env, inv.Args, algos)
		},
	}
}

func runHash(ctx context.Context, env *Env, files []string, algos []filesystem.Algorithm) error {
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	var failed int
	for _, name := range files {
		digests, err := hashOne(ctx, env, name, algos)
		if err != nil {
			// One unreadable file must not abort the rest; report and
			// continue, then exit non-zero at the end. This matches the
			// behaviour of sha256sum and of every good Unix tool.
			fmt.Fprintf(env.Stderr, "gecko hash: %v\n", err)
			failed++
			continue
		}
		for _, d := range digests {
			fmt.Fprintf(w, "%s\t%s\t%s\n", d.Algorithm, d.Hex(), name)
		}
	}
	if failed > 0 {
		w.Flush()
		return Quiet(&ExitError{Code: 1, Err: fmt.Errorf("%d file(s) could not be hashed", failed)})
	}
	return nil
}

func hashOne(ctx context.Context, env *Env, name string, algos []filesystem.Algorithm) ([]filesystem.Digest, error) {
	if name == "-" {
		return filesystem.HashReader(ctx, env.Stdin, algos)
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(env.WorkDir, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Reject directories explicitly: reading a directory returns EISDIR
	// on Linux but succeeds with garbage on some systems.
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return nil, fmt.Errorf("%s: is a directory", name)
	}
	return filesystem.HashReader(ctx, f, algos)
}
```

`runCheck` is left as part of the exercise.

---

## F. Exercise

1. Implement `--check`. Requirements: infer the algorithm from digest length, resolve
   filenames relative to the checksum file's directory (not the CWD — this is what
   `sha256sum -c` does and getting it wrong is a classic bug), print `NAME: OK` or
   `NAME: FAILED`, exit 1 if any failed, and warn on stderr if the inferred algorithm
   is weak.

2. **Path traversal.** A malicious checksum file could contain
   `../../../../etc/passwd` or an absolute path. What should `--check` do? Decide, then
   implement. (There's no single right answer, but "silently read whatever it says" is
   wrong.)

3. Before benchmarking: predict whether hashing three algorithms in three goroutines
   beats `io.MultiWriter`. Write down your prediction and the reasoning. Then measure.

---

## G. Testing and measurement

### Correctness against known vectors

```go
func TestHashReaderKnownVectors(t *testing.T) {
	t.Parallel()
	// Empty string and "abc" are the canonical test vectors for these
	// algorithms; they appear in the FIPS and RFC specifications.
	tests := []struct {
		input string
		algo  Algorithm
		want  string
	}{
		{"", MD5, "d41d8cd98f00b204e9800998ecf8427e"},
		{"", SHA1, "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
		{"", SHA256, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", SHA256, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"abc", SHA1, "a9993e364706816aba3e25717850c26c9cd0d89d"},
	}
	for _, tt := range tests {
		t.Run(string(tt.algo)+"/"+tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := HashReader(context.Background(), strings.NewReader(tt.input), []Algorithm{tt.algo})
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Hex() != tt.want {
				t.Errorf("= %s, want %s", got[0].Hex(), tt.want)
			}
		})
	}
}
```

Using published vectors rather than "whatever my code produced" is the difference between
a test and a tautology.

### The awkward-reader test

Real readers return short reads, and network readers return `(n>0, io.EOF)`. Both break
naive loops.

```go
// awkwardReader returns one byte at a time and delivers io.EOF together
// with the final byte, which the io.Reader contract permits.
type awkwardReader struct {
	data []byte
	pos  int
}

func (a *awkwardReader) Read(p []byte) (int, error) {
	if a.pos >= len(a.data) {
		return 0, io.EOF
	}
	p[0] = a.data[a.pos]
	a.pos++
	if a.pos == len(a.data) {
		return 1, io.EOF // the legal-but-unloved case
	}
	return 1, nil
}

func TestHashReaderHandlesShortReadsWithEOF(t *testing.T) {
	t.Parallel()
	got, err := HashReader(context.Background(), &awkwardReader{data: []byte("abc")}, []Algorithm{SHA256})
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got[0].Hex() != want {
		t.Errorf("= %s, want %s (last byte likely dropped)", got[0].Hex(), want)
	}
}
```

Delete the `if n > 0` guard from `HashReader` and watch this test fail. That's the value
of the test.

### Cancellation

```go
func TestHashReaderCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An infinite reader: without cancellation this test would hang.
	_, err := HashReader(ctx, rand.Reader, []Algorithm{SHA256})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
```

`crypto/rand.Reader` never returns EOF, which makes it a good stand-in for an endless
stream. A test that hangs on regression is bad; add `-timeout 30s` to your CI test
invocation so a hang fails rather than blocking forever.

### Fuzzing the checksum parser

Parsers get fuzzed. This one takes untrusted input from a downloaded file.

```go
func FuzzParseChecksumFile(f *testing.F) {
	f.Add("d41d8cd98f00b204e9800998ecf8427e  file.txt\n")
	f.Add("# comment\n\n")
	f.Add("abc *bin ary name.iso\n")

	f.Fuzz(func(t *testing.T, in string) {
		entries, err := ParseChecksumFile(strings.NewReader(in))
		if err != nil {
			return // rejecting malformed input is correct
		}
		for _, e := range entries {
			if e.Name == "" {
				t.Errorf("accepted an entry with an empty name from %q", in)
			}
			if _, derr := hex.DecodeString(e.Sum); derr != nil {
				t.Errorf("accepted non-hex sum %q", e.Sum)
			}
		}
	})
}
```

```bash
go test ./internal/filesystem -fuzz=FuzzParseChecksumFile -fuzztime=60s
```

Failing inputs are written to `testdata/fuzz/` and become permanent regression tests
automatically. Commit them.

### Benchmarks: buffer size

```go
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
```

`b.SetBytes` makes the output report MB/s, which is far more interpretable than ns/op
for throughput work.

```bash
go test ./internal/filesystem -bench=BufferSize -benchmem -run=^$ -benchtime=3x
```

Expected shape of the result: throughput climbs steeply from 4 KB to about 64 KB, then
flattens. Beyond ~256 KB you may see it *degrade* as the buffer stops fitting in L2 cache.
Your numbers will differ from mine; that's the point.

### Benchmarks: is parallel hashing worth it?

```go
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
```

Where `hashParallel` fans the buffer out to N goroutines with a `sync.WaitGroup` per
chunk. Write it. Then think carefully about what the benchmark is actually measuring:
`bytes.NewReader` is memory, so this measures the **CPU-bound** case, which is the
best case for parallelism. To measure the realistic case you need a file bigger than
your page cache, which a benchmark can't easily arrange.

That difficulty is itself the lesson: **microbenchmarks measure what you set up, not
what your users experience.** The honest conclusion is usually "keep the simple version
unless profiling on real workloads says otherwise".

### Comparing benchmark runs

```bash
go install golang.org/x/perf/cmd/benchstat@latest

go test ./internal/filesystem -bench=. -count=10 -run=^$ > old.txt
# make your change
go test ./internal/filesystem -bench=. -count=10 -run=^$ > new.txt
benchstat old.txt new.txt
```

`-count=10` is not optional. A single run has enough variance to show a 15% "improvement"
that's pure noise. `benchstat` reports a p-value and will tell you when a difference is
statistically insignificant. **A benchmark result without a variance estimate is not
evidence.**

### Profiling

```bash
go test ./internal/filesystem -bench=BenchmarkHashLarge -run=^$ \
    -cpuprofile=cpu.prof -memprofile=mem.prof
go tool pprof -http=:8080 cpu.prof
```

In the web UI, the flame graph for SHA-256 will be dominated by
`crypto/sha256.blockAVX2` or `block` — assembly. That's the correct and expected shape:
the hash is doing the work and your Go code is not the bottleneck. **Seeing a profile
where you're *not* the problem is as instructive as seeing one where you are.**

For allocations:

```bash
go tool pprof -sample_index=alloc_space mem.prof
(pprof) top10
(pprof) list HashReader
```

`list` annotates the source line by line with allocation counts. You'll see the
`make([]byte, hashBufSize)` line light up.

---

## H. Review

- `hash.Hash` is an `io.Writer`, and why that makes streaming trivial.
- `io.Copy`'s three-way dispatch and when the fast paths engage.
- Why we hand-rolled the read loop (cancellation) and what it cost us.
- The `n > 0` before `err` rule in read loops, and the reader contract behind it.
- Buffer sizing as an empirical question, with a measured answer.
- `b.SetBytes`, `-count=N`, and `benchstat` — what makes a benchmark result evidence.
- Escape analysis via `-gcflags=-m`, and why a local buffer heap-allocates.
- Constant-time comparison and the threat model that motivates it.
- Fuzzing a parser, and why the corpus belongs in Git.

---

## I. Refactoring

The 64 KB buffer allocates per call. For `gecko hash *.go` across 200 files that's
12.8 MB of garbage. Chapter 5's `find` will hash thousands of files. Fix it with
`sync.Pool`:

```go
var bufPool = sync.Pool{
	New: func() any {
		// Pool stores interface values; returning a *[]byte rather than
		// a []byte avoids an allocation on every Put, because converting
		// a slice header to an interface allocates but a pointer does not.
		b := make([]byte, hashBufSize)
		return &b
	},
}

func HashReader(...) ([]Digest, error) {
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp
	...
}
```

The `*[]byte` detail is a real, measurable thing — `go vet` even has a check
(`SA6002` in staticcheck) for putting non-pointer slices into a pool. Verify the win:

```bash
go test -bench=HashMany -benchmem -count=10 -run=^$ > before.txt
# apply the pool
go test -bench=HashMany -benchmem -count=10 -run=^$ > after.txt
benchstat before.txt after.txt
```

**But before you keep it:** `sync.Pool` entries are cleared at every GC cycle, it adds
per-CPU shard bookkeeping, and it makes the code harder to reason about. If `benchstat`
says the difference is insignificant for realistic workloads, **revert it**. Adding a
pool because it feels faster is exactly the anti-pattern this chapter exists to prevent.

My own measurement on a typical laptop: for single-file hashing, no measurable
difference. For 1000-file batches, allocations drop from ~64 MB to ~64 KB and wall time
improves 3–8%. That's enough to keep it — but note that "enough to keep it" is a
conclusion drawn from a number, not from an instinct.

---

## Commit

```
feat: add hash command with streaming multi-algorithm digests
test: add known-vector, fuzz and cancellation tests for hashing
perf: pool hash buffers (allocations -99% on batch workloads)
```

The `perf:` commit message states the measured effect. A performance commit without a
number in its message is not reviewable.

Next: `05-find-clean.md`.

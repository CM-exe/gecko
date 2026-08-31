package filesystem

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"sync"
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

var bufPool = sync.Pool{
	New: func() any {
		// Pool stores interface values; returning a *[]byte rather than
		// a []byte avoids an allocation on every Put, because converting
		// a slice header to an interface allocates but a pointer does not.
		b := make([]byte, hashBufSize)
		return &b
	},
}

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

	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp

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

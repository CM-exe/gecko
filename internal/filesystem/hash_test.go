package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

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

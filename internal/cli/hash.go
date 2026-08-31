package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/CM-exe/gecko/internal/filesystem"
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

func checkPathWithinDir(baseDir, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%s: absolute paths are not allowed in checksum files", name)
	}

	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(baseDir, filepath.Clean(name))
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(baseAbs, candidateAbs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: path escapes checksum file directory", name)
	}
	return candidateAbs, nil
}

func runCheck(ctx context.Context, env *Env, checkFile string, quiet bool) error {
	checkPath := checkFile
	if !filepath.IsAbs(checkPath) {
		checkPath = filepath.Join(env.WorkDir, checkPath)
	}
	f, err := os.Open(checkPath)
	if err != nil {
		return err
	}
	defer f.Close()

	algorithms := map[int]string{32: "md5", 40: "sha1", 64: "sha256", 96: "sha384", 128: "sha512"}
	warned := make(map[string]bool)
	failed := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	checkDir := filepath.Dir(checkPath)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			fmt.Fprintf(env.Stdout, "%s: FAILED\n", line)
			failed++
			continue
		}
		sum, name := fields[0], strings.TrimLeft(line[len(fields[0]):], " \t")
		algoName := algorithms[len(sum)]
		ok := algoName != ""
		if ok {
			_, err = hex.DecodeString(sum)
			ok = err == nil
		}
		if ok && (algoName == "md5" || algoName == "sha1") && !warned[algoName] {
			fmt.Fprintf(env.Stderr, "gecko hash: warning: inferred algorithm %s is weak\n", algoName)
			warned[algoName] = true
		}
		if ok {
			algos, e := filesystem.ParseAlgorithms(algoName)
			if e != nil {
				ok = false
			} else if name == "-" {
				digests, e := filesystem.HashReader(ctx, env.Stdin, algos)
				ok = e == nil && len(digests) == 1 && strings.EqualFold(digests[0].Hex(), sum)
			} else {
				path, e := checkPathWithinDir(checkDir, name)
				if e != nil {
					ok = false
				} else {
					var input *os.File
					input, e = os.Open(path)
					if e == nil {
						digests, hashErr := filesystem.HashReader(ctx, input, algos)
						input.Close()
						ok = hashErr == nil && len(digests) == 1 && strings.EqualFold(digests[0].Hex(), sum)
					} else {
						ok = false
					}
				}
			}
		}
		if ok {
			if !quiet {
				fmt.Fprintf(env.Stdout, "%s: OK\n", name)
			}
		} else {
			fmt.Fprintf(env.Stdout, "%s: FAILED\n", name)
			failed++
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if failed != 0 {
		return Quiet(&ExitError{Code: 1, Err: fmt.Errorf("%d checksum(s) failed", failed)})
	}
	return nil
}

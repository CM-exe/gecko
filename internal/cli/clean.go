package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/CM-exe/gecko/internal/filesystem"
)

func newCleanCommand() *Command {
	var (
		yes     bool
		jsonOut bool
	)

	return &Command{
		Name:  "clean",
		Short: "Remove known temporary/build directories safely",
		Usage: "gecko clean [flags] [path]",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&yes, "yes", false, "do not prompt for confirmation")
			fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON output")
		},
		Run: func(ctx context.Context, env *Env, inv *Invocation) error {
			root := "."
			switch len(inv.Args) {
			case 0:
				// default to the current working directory
			case 1:
				root = inv.Args[0]
			default:
				fmt.Fprintln(env.Stderr, "gecko clean: too many arguments")
				return Quiet(ErrUsage)
			}

			return runClean(ctx, root, env.Stdout, env.Stdin, yes, jsonOut)
		},
	}
}

// Confirm reads a yes/no answer. It returns false on EOF, so a
// non-interactive invocation without --yes declines rather than hangs.
func confirm(env *Env, prompt string) (bool, error) {
	fmt.Fprintf(env.Stdout, "%s [y/N] ", prompt)
	r := bufio.NewReader(env.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func runClean(ctx context.Context, root string, w io.Writer, stdin io.Reader, yes bool, jsonOut bool) error {
	candidates, err := filesystem.ScanForCleanup(ctx, root)
	if err != nil {
		return err
	}

	if jsonOut {
		return runCleanJSON(w, root, candidates, yes, stdin)
	}

	if len(candidates) == 0 {
		fmt.Fprintln(w, "No cleanup candidates found.")
		return nil
	}

	for _, c := range candidates {
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.RelPath, c.Target.Description, formatBytes(c.Size))
	}

	if !yes {
		ok, err := confirm(&Env{Stdout: w, Stdin: stdin}, fmt.Sprintf("Delete %d candidate(s) under %q?", len(candidates), root))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(w, "Aborted.")
			return nil
		}
	}

	for _, c := range candidates {
		if err := filesystem.Delete(root, c); err != nil {
			fmt.Fprintf(w, "failed to delete %s: %v\n", c.RelPath, err)
			return err
		}
		fmt.Fprintf(w, "deleted %s\n", c.RelPath)
	}
	return nil
}

type cleanCandidate struct {
	filesystem.Candidate
	Deleted bool   `json:"deleted,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

func runCleanJSON(w io.Writer, root string, candidates []filesystem.Candidate, yes bool, stdin io.Reader) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	out := struct {
		Root       string           `json:"root"`
		Candidates []cleanCandidate `json:"candidates"`
		Count      int              `json:"count"`
	}{
		Root:  absRoot,
		Count: len(candidates),
	}
	for _, c := range candidates {
		path := filepath.Join(absRoot, c.RelPath)
		item := cleanCandidate{
			Path:      path,
			RelPath:   c.RelPath,
			Target:    c.Target,
			Size:      c.Size,
			FileCount: c.FileCount,
			SizeErr:   c.SizeErr,
		}
		out.Candidates = append(out.Candidates, item)
	}

	if len(candidates) == 0 {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if !yes {
		ok, err := confirm(&Env{Stdout: w, Stdin: stdin}, fmt.Sprintf("Delete %d candidate(s) under %q?", len(candidates), root))
		if err != nil {
			return err
		}
		if !ok {
			out := struct {
				Root       string           `json:"root"`
				Aborted    bool             `json:"aborted"`
				Count      int              `json:"count"`
				Candidates []cleanCandidate `json:"candidates"`
			}{
				Root:    absRoot,
				Aborted: true,
				Count:   len(candidates),
			}
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
	}

	for i, c := range candidates {
		if err := filesystem.Delete(root, c); err != nil {
			out.Candidates[i].Deleted = false
			out.Candidates[i].Reason = err.Error()
			out.Candidates[i].Message = "delete failed"
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			if err2 := enc.Encode(out); err2 != nil {
				return err2
			}
			return err
		}
		out.Candidates[i].Deleted = true
		out.Candidates[i].Message = "deleted"
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	const units = "KMGTPE"
	for i, unit := range units {
		if n < 1024<<uint(10*(i+1)) {
			divisor := 1 << uint(10*(i+1))
			return fmt.Sprintf("%.1f %ciB", float64(n)/float64(divisor), unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", float64(n)/(1<<50))
}

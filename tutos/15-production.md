# Chapter 15 — Production: Profiling, Security Review, CI and Release

```
Difficulty:   Advanced
Est. time:    10–12 hours
Main concepts: pprof in depth (CPU, heap, block, mutex, trace), benchstat, startup
               latency, binary size, staticcheck/govulncheck/gosec, GitHub Actions,
               cross-compilation, reproducible builds, GoReleaser internals, SLSA
               provenance, packaging, documentation as a deliverable
Prerequisites: Chapters 1–14
```

---

## A. Goal

A repository someone else would trust: green CI on three platforms, signed release
binaries for six targets, a README that explains itself, and a security policy that's
honest.

```
$ curl -fsSL https://gecko.dev/install.sh | sh
$ brew install yourname/tap/gecko
$ scoop install gecko
$ go install github.com/yourname/gecko/cmd/gecko@latest
```

---

## B. Why this matters

Everything so far has been about making Gecko work. This chapter is about making it
*trustworthy* — which is a different property, established by artifacts other people can
inspect: reproducible builds, a test matrix they can read, a changelog, a vulnerability
policy.

It's also where you find out what your performance actually is, as opposed to what you
assumed in chapters 4 and 5.

---

## C. Concepts

### The profiling toolkit, and what each profile answers

| Profile | Question it answers | How to get it |
|---|---|---|
| **CPU** | Where is time spent executing? | `-cpuprofile`, `pprof.StartCPUProfile` |
| **Heap** | What is allocating, and what is retained? | `-memprofile`, `/debug/pprof/heap` |
| **Block** | Where do goroutines wait on synchronisation? | `runtime.SetBlockProfileRate` |
| **Mutex** | Where is lock contention? | `runtime.SetMutexProfileFraction` |
| **Goroutine** | What are all goroutines doing right now? | `/debug/pprof/goroutine?debug=2` |
| **Trace** | What happened, in order, including scheduling? | `runtime/trace` |

The CPU profile samples at 100 Hz by default, so anything under ~10 ms of total CPU is
invisible. For a CLI that runs for 50 ms, **the CPU profile is nearly useless** — you'll
sample five times. Either loop the operation thousands of times in a benchmark, or use a
trace instead.

Heap profiles are the subtle ones. There are four sample types:

- `alloc_space` — total bytes allocated since start. **This is what you want for
  reducing GC pressure.**
- `alloc_objects` — total allocation count. Useful when many small allocations dominate.
- `inuse_space` — bytes live at the moment of sampling. This is what you want for finding
  a leak.
- `inuse_objects` — live object count.

`go tool pprof mem.prof` defaults to `inuse_space`. If you're chasing allocation churn
rather than a leak, you must pass `-sample_index=alloc_space` or you'll be looking at the
wrong number and concluding nothing is wrong.

Heap profiles also sample: one sample per 512 KB allocated by default
(`runtime.MemProfileRate`). Set it to 1 for exact profiling in a benchmark; leave it
alone in production, where it would be ruinous.

### Reading a profile

```bash
go tool pprof -http=:8080 cpu.prof
```

The views that matter:

- **Flame graph** — width is cumulative time. Look for wide plateaus.
- **Top** — flat vs cumulative. *Flat* is time in that function itself; *cumulative*
  includes callees. A function with high cumulative and near-zero flat is just a caller.
- **Peek** — callers and callees of one function, with percentages.
- **Source** — line-by-line annotation. This is where you actually find the problem.

The single most common misreading: seeing `runtime.mallocgc` at the top and concluding
"the GC is slow". `mallocgc` is *allocation*, and its presence means your code allocates
too much. Follow the callers.

Second most common: profiling a debug build, or profiling with the race detector on. Both
distort results beyond usefulness.

### Execution traces — the right tool for a CLI

```go
trace.Start(f)
defer trace.Stop()
```
```bash
go tool trace trace.out
```

A trace records **every** scheduling event: goroutine creation, blocking, syscalls, GC
phases, and the actual timeline across processors. For a 50 ms CLI invocation this tells
you what a sampling profiler cannot: that you spent 30 ms blocked on a single `stat`, or
that your eight workers were serialised behind a mutex.

The "Goroutine analysis" and "Synchronization blocking profile" views are where the
answers usually are.

### Startup latency

For a CLI, startup *is* the performance story. `gecko version` should be under 20 ms.
What costs time before your code runs:

- **Dynamic linking.** A `CGO_ENABLED=0` static binary skips the dynamic loader
  entirely — worth several milliseconds on some systems.
- **Package `init()` functions.** Every `init` in every imported package runs before
  `main`. A regex compiled at package level (`regexp.MustCompile` in a `var`) costs
  microseconds; a few hundred of them cost milliseconds. Measure with
  `GODEBUG=inittrace=1`:

  ```bash
  GODEBUG=inittrace=1 ./gecko version
  init internal/bytealg @0.008 ms, 0 ms clock, 0 bytes, 0 allocs
  init regexp @1.2 ms, 0.15 ms clock, 8192 bytes, 12 allocs
  ```

- **Runtime initialisation.** Heap arena setup, scheduler start. Roughly 1 ms, not
  reducible.

The actionable item is almost always `init` work and eager package-level state. Chapter
13's lazy plugin discovery was exactly this concern applied in advance.

### Binary size

```bash
go build -ldflags="-s -w" -trimpath ./cmd/gecko
```

`-s` strips the symbol table, `-w` the DWARF debug info. Together, typically 25–30%
smaller. The cost: no useful stack traces from a panic in the field, and no debugging with
delve. **For a CLI that's an acceptable trade; for a server it usually isn't.**

`-trimpath` removes absolute paths from the binary, which matters for reproducibility and
also avoids leaking your home directory path to users.

To see where the bytes went:

```bash
go install github.com/jondot/goweight@latest
goweight ./cmd/gecko
```

Common surprises: `net/http` pulls in TLS and its certificate handling (~2 MB);
`regexp` is ~500 KB; `time`'s embedded zoneinfo appears if you import `time/tzdata`.

### Static analysis

```bash
go vet ./...                                    # in the toolchain, always run it
staticcheck ./...                               # honnef.co/go/tools
govulncheck ./...                               # golang.org/x/vuln
gosec ./...                                     # securego/gosec
gofumpt -l .                                    # stricter gofmt
```

**`govulncheck` deserves special mention.** Unlike a generic dependency scanner, it uses
call-graph analysis: it reports a vulnerability only if your code can actually *reach* the
affected function. That eliminates most of the noise that makes teams ignore scanners.
Run it in CI on a schedule as well as on PRs — new CVEs appear against unchanged code.

`staticcheck` catches things `vet` doesn't: unused struct fields, inefficient string
concatenation in loops, incorrect `sync.Pool` usage (SA6002 — the `*[]byte` issue from
chapter 4), impossible type assertions, and deprecated API use.

`gosec` produces false positives at a rate that requires curation. Configure it, don't
just run it: `#nosec G304 -- path is validated by fs.FS above` with a reason. A blanket
`#nosec` with no explanation is worse than no scanner.

### CI design

Structure matters as much as coverage. Principles:

- **Fast feedback first.** Lint and unit tests on Linux in one job (~1 min); the full
  matrix in parallel jobs.
- **`fail-fast: false`.** Otherwise one platform's failure hides the other two.
- **Cache the module and build caches.** `actions/setup-go` with `cache: true` does both.
- **Race detector on Linux only** if time is tight — it's 5–15× slower and races are
  rarely platform-specific. But run it *somewhere* on every PR.
- **Pin action versions to a SHA**, not a tag. A tag can be moved; that's a supply-chain
  attack vector, and it has happened.
- **Least-privilege tokens.** `permissions: contents: read` by default, elevated only in
  the release job.

### Reproducible builds

Two builds of the same commit should produce byte-identical binaries. Requirements:

1. `-trimpath` — removes build-machine paths.
2. Same Go version — encoded in the binary.
3. `CGO_ENABLED=0` — otherwise the system C toolchain leaks in.
4. No timestamps in `-ldflags` — or a deterministic one from the commit
   (`git show -s --format=%cI`), not `date`.
5. Same dependency versions — `go.sum` guarantees this.

Verify:

```bash
go build -trimpath -o a ./cmd/gecko && go build -trimpath -o b ./cmd/gecko
cmp a b && echo reproducible
```

Reproducibility lets a third party verify that your published binary matches your
published source. That's a meaningful security property, not a purity exercise.

### What GoReleaser actually does

Rather than treating it as magic:

1. Reads `.goreleaser.yaml`.
2. Verifies a clean git tree at a tag.
3. Runs `go build` once per GOOS/GOARCH combination with your `ldflags`, in parallel.
4. Packages each into `.tar.gz` (Unix) or `.zip` (Windows) with the README and LICENSE.
5. Computes SHA-256 for everything into `checksums.txt`.
6. Optionally signs the checksums file (cosign, GPG).
7. Generates a changelog from commit messages since the last tag.
8. Creates a GitHub release and uploads the artifacts.
9. Optionally publishes a Homebrew formula, Scoop manifest, Linux packages, Docker
   images.

Every step is something you could script in 200 lines of bash. GoReleaser's value is that
those 200 lines are already debugged across thousands of projects, and that steps 9's
package-manager formats are fiddly and change.

**Decision: use it, and keep `gecko run release` as a wrapper**, so the entrypoint is
consistent with the rest of the project's tooling.

### Provenance and SLSA

SLSA is a framework for build-integrity levels. Level 3 requires a non-falsifiable
provenance attestation from a hardened build service — GitHub's OIDC-signed attestations
qualify:

```yaml
- uses: actions/attest-build-provenance@v1
  with:
    subject-path: 'dist/*'
```

This produces a signed statement of what was built, from which commit, by which workflow.
Users verify with `gh attestation verify`. Keyless signing via cosign and Sigstore's
transparency log means no key management, which is the part everyone gets wrong.

Given chapter 14's honest admission that the plugin signing key can't be rotated,
adopting Sigstore for Gecko's *own* releases is worth doing — it's a few lines and it
sidesteps key management entirely.

---

## E. Implementation

### Profiling flags in the binary

```go
// internal/cli/profile.go

// Profiling is exposed through hidden global flags rather than a
// separate build tag, so a user reporting "gecko find is slow on my
// repo" can produce a profile without rebuilding anything.
type profileSession struct {
	cpuFile   *os.File
	traceFile *os.File
	memPath   string
}

func startProfiling(cpuPath, memPath, tracePath string) (*profileSession, error) {
	s := &profileSession{memPath: memPath}

	if cpuPath != "" {
		f, err := os.Create(cpuPath)
		if err != nil {
			return nil, err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return nil, err
		}
		s.cpuFile = f
	}

	if tracePath != "" {
		f, err := os.Create(tracePath)
		if err != nil {
			s.Stop()
			return nil, err
		}
		if err := trace.Start(f); err != nil {
			f.Close()
			s.Stop()
			return nil, err
		}
		s.traceFile = f
	}
	return s, nil
}

// Stop finalises every profile. It must run before the process exits,
// which is why cli.Main and not a defer in a command is responsible for
// calling it: os.Exit skips defers, and an unfinalised CPU profile is
// an empty file.
func (s *profileSession) Stop() {
	if s == nil {
		return
	}
	if s.traceFile != nil {
		trace.Stop()
		s.traceFile.Close()
	}
	if s.cpuFile != nil {
		pprof.StopCPUProfile()
		s.cpuFile.Close()
	}
	if s.memPath != "" {
		f, err := os.Create(s.memPath)
		if err == nil {
			// A GC before writing gives inuse_* numbers that reflect
			// what is actually retained rather than what has not yet
			// been collected.
			runtime.GC()
			pprof.Lookup("heap").WriteTo(f, 0)
			f.Close()
		}
	}
}
```

Wire into `Main`:

```go
func Main(ctx context.Context, args []string, env *Env) int {
	args, prof, err := extractProfileFlags(args)
	if err != nil {
		fmt.Fprintf(env.Stderr, "gecko: %v\n", err)
		return 2
	}
	defer prof.Stop()   // safe: Main returns, only main() calls os.Exit

	app := New()
	return ExitCode(app.Execute(ctx, env, args), env)
}
```

The comment matters: this only works because chapter 1 established that `Main` returns an
int and `main` alone calls `os.Exit`. A design decision from the first hour paying off in
the last chapter.

### The benchmark suite

```go
// internal/cli/bench_test.go

// BenchmarkStartup measures the full cost of a trivial invocation:
// runtime init, package init, dispatch and output. This is the number
// users feel on every single command, so it is the one worth guarding
// against regression.
func BenchmarkStartup(b *testing.B) {
	env := &Env{
		Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader(""),
		Getenv: func(string) string { return "" }, WorkDir: ".",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Main(context.Background(), []string{"version"}, env)
	}
}

func BenchmarkTreeLargeRepo(b *testing.B) {
	dir := buildSyntheticTree(b, 5000)
	// ... measure WriteTree over it
}

func BenchmarkFindByName(b *testing.B)      { /* no stat: single-threaded path */ }
func BenchmarkFindBySize(b *testing.B)      { /* stat: worker pool path */ }
func BenchmarkHashLargeFile(b *testing.B)   { /* 100 MB, b.SetBytes */ }
func BenchmarkServeStaticFile(b *testing.B) { /* httptest + real client */ }
func BenchmarkPluginDiscovery(b *testing.B) { /* 20 plugins, cached and cold */ }
```

Track them over time:

```bash
go test ./... -bench=. -benchmem -count=10 -run=^$ > bench-$(git rev-parse --short HEAD).txt
benchstat bench-abc1234.txt bench-def5678.txt
```

Commit the baselines to a `benchmarks/` directory. Then a regression is a diff, not a
vague memory of "it used to feel faster".

### `.github/workflows/ci.yml`

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
  schedule:
    # govulncheck against unchanged code: new CVEs appear without commits.
    - cron: '0 6 * * 1'

# Least privilege by default; the release workflow elevates explicitly.
permissions:
  contents: read

concurrency:
  # Cancel superseded runs on the same branch, but never on main.
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.ref != 'refs/heads/main' }}

jobs:
  # Fast feedback: fails in about a minute on the common mistakes.
  quick:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
          cache: true

      - name: gofmt
        run: |
          unformatted=$(gofmt -l .)
          if [ -n "$unformatted" ]; then
            echo "::error::These files are not gofmt'd:"
            echo "$unformatted"
            exit 1
          fi

      - run: go vet ./...

      - uses: dominikh/staticcheck-action@v1
        with: {version: latest, install-go: false}

      - name: govulncheck
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck ./...

      - name: go.mod is tidy
        run: |
          go mod tidy
          git diff --exit-code go.mod go.sum

  test:
    needs: quick
    strategy:
      fail-fast: false   # see all platforms, not just the first to fail
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
        go: ['1.23', '1.24']   # oldest supported and current
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: '${{ matrix.go }}', cache: true}

      - run: go build ./...

      - name: Test
        run: go test ./... -timeout 10m -coverprofile=coverage.out

      # The race detector is 5-15x slower, so it runs on one platform.
      # Races are almost never GOOS-specific.
      - name: Test with race detector
        if: matrix.os == 'ubuntu-latest' && matrix.go == '1.24'
        run: go test ./... -race -timeout 15m

      - name: Upload coverage
        if: matrix.os == 'ubuntu-latest' && matrix.go == '1.24'
        uses: codecov/codecov-action@v4
        with: {files: ./coverage.out}

  # Compilation alone catches the most common build-tag error: a missing
  # implementation for one GOOS, which surfaces as an undefined symbol.
  crosscompile:
    needs: quick
    runs-on: ubuntu-latest
    strategy:
      matrix:
        target:
          - {goos: linux,   goarch: amd64}
          - {goos: linux,   goarch: arm64}
          - {goos: linux,   goarch: arm}
          - {goos: darwin,  goarch: amd64}
          - {goos: darwin,  goarch: arm64}
          - {goos: windows, goarch: amd64}
          - {goos: windows, goarch: arm64}
          - {goos: freebsd, goarch: amd64}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: '1.24', cache: true}
      - env:
          GOOS: ${{ matrix.target.goos }}
          GOARCH: ${{ matrix.target.goarch }}
          CGO_ENABLED: '0'
        run: go build -trimpath ./...

  benchmark:
    needs: quick
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: '1.24', cache: true}
      # Not a gate: CI runners are too noisy for reliable benchmark
      # comparison. This exists so the numbers are in the log when
      # someone investigates a regression report.
      - run: go test ./... -bench=. -benchmem -run=^$ -count=3 | tee bench.txt
      - uses: actions/upload-artifact@v4
        with: {name: benchmarks, path: bench.txt}
```

The "go.mod is tidy" check catches the classic PR that adds an import without updating
`go.sum`, which then fails for everyone else.

Noting that the benchmark job is *not* a gate is important. GitHub runners are shared and
noisy; a 20% swing between runs is normal. Failing CI on benchmark deltas trains people to
retry until green, which is worse than not measuring.

### `.goreleaser.yaml`

```yaml
version: 2

before:
  hooks:
    - go mod tidy
    - go test ./... -short

builds:
  - id: gecko
    main: ./cmd/gecko
    binary: gecko

    env:
      # A static binary: no dynamic loader at startup, no libc version
      # coupling, and it runs in scratch containers and on Alpine.
      - CGO_ENABLED=0

    flags:
      # Removes build-machine paths from the binary. Required for
      # reproducibility and avoids leaking the builder's home directory.
      - -trimpath

    ldflags:
      # -s strips the symbol table, -w the DWARF data: together about
      # 30% smaller. The cost is unusable stack traces from field
      # panics, which is an acceptable trade for a CLI.
      - -s -w
      - -X github.com/yourname/gecko/internal/cli.version={{.Version}}
      - -X github.com/yourname/gecko/internal/cli.commit={{.Commit}}
      # CommitDate, not the build time: using the wall clock would make
      # every build differ and destroy reproducibility.
      - -X github.com/yourname/gecko/internal/cli.date={{.CommitDate}}

    goos: [linux, darwin, windows, freebsd]
    goarch: [amd64, arm64, arm]
    goarm: ['7']

    ignore:
      - {goos: darwin,  goarch: arm}
      - {goos: windows, goarch: arm}

    mod_timestamp: '{{ .CommitTimestamp }}'   # deterministic archive mtimes

archives:
  - formats: [tar.gz]
    format_overrides:
      - goos: windows
        formats: [zip]
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}
    files:
      - README.md
      - LICENSE
      - CHANGELOG.md
      - docs/**/*

checksum:
  name_template: 'checksums.txt'
  algorithm: sha256

signs:
  # Keyless signing via Sigstore: identity comes from the workflow's
  # OIDC token and the signature is recorded in a public transparency
  # log. No private key to store, rotate or lose.
  - cmd: cosign
    signature: '${artifact}.sig'
    certificate: '${artifact}.pem'
    args:
      - sign-blob
      - '--output-signature=${signature}'
      - '--output-certificate=${certificate}'
      - '${artifact}'
      - --yes
    artifacts: checksum
    output: true

changelog:
  use: github
  sort: asc
  groups:
    - {title: Features,      regexp: '^feat',  order: 0}
    - {title: Bug fixes,     regexp: '^fix',   order: 1}
    - {title: Performance,   regexp: '^perf',  order: 2}
    - {title: Documentation, regexp: '^docs',  order: 3}
  filters:
    exclude: ['^test:', '^chore:', '^ci:', '^refactor:']

brews:
  - repository: {owner: yourname, name: homebrew-tap}
    homepage: https://github.com/yourname/gecko
    description: A cross-platform developer toolbox
    license: MIT
    test: |
      system "#{bin}/gecko", "version"

scoops:
  - repository: {owner: yourname, name: scoop-bucket}
    homepage: https://github.com/yourname/gecko
    description: A cross-platform developer toolbox
    license: MIT

nfpms:
  - formats: [deb, rpm, apk]
    maintainer: You <you@example.com>
    description: A cross-platform developer toolbox
    license: MIT
```

### `.github/workflows/release.yml`

```yaml
name: Release

on:
  push:
    tags: ['v*']

permissions:
  contents: write     # create the release
  id-token: write     # OIDC for keyless signing and attestation
  attestations: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0}   # GoReleaser needs full history for the changelog

      - uses: actions/setup-go@v5
        with: {go-version: '1.24', cache: true}

      - uses: sigstore/cosign-installer@v3

      - uses: goreleaser/goreleaser-action@v6
        with: {distribution: goreleaser, version: '~> v2', args: release --clean}
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          # A separate token: the default GITHUB_TOKEN cannot push to
          # the tap and bucket repositories.
          HOMEBREW_TAP_TOKEN: ${{ secrets.HOMEBREW_TAP_TOKEN }}

      # SLSA provenance: a signed statement of what was built, from
      # which commit, by which workflow. Verifiable with
      # "gh attestation verify".
      - uses: actions/attest-build-provenance@v1
        with:
          subject-path: 'dist/*.tar.gz,dist/*.zip,dist/checksums.txt'
```

### The security review pass

Walk the whole codebase against the categories from the brief. Not a checklist exercise —
open each file and look.

**Path traversal.** `serve` (chapter 7): `os.DirFS` + `ServeMux` cleaning, tested.
`clean` (chapter 5): `EvalSymlinks` + `filepath.Rel`, tested adversarially. `hash --check`:
did you resolve names relative to the checksum file and reject absolute paths? Check.

**Command injection.** Grep for it:

```bash
grep -rn 'exec.Command' --include='*.go' . | grep -E '"(sh|bash|cmd|powershell)"'
```

Every hit must be either a `shell: true` task (chapter 10, explicit opt-in) or
`OpenBrowser` (chapter 7, fixed argv). Anything else is a finding.

**File deletion.** Only `clean` and `plugin remove`. Both use `os.RemoveAll` with a
verified path. Neither accepts a user-supplied pattern.

**Downloaded content.** Chapter 14: size-bounded, checksum-verified, signature-verified,
verified before becoming executable.

**Temporary files.** `os.CreateTemp` everywhere (0600 by default, unpredictable name).
Grep for `/tmp/` string literals — a predictable temp path is a symlink-attack vector.

**Permissions.** Config 0600, config dir 0700, plugins 0755. Verify with a test.

**TLS.** `InsecureSkipVerify` appears exactly once, behind an explicit flag, with a stderr
warning, and unreachable from a config file.

**Secrets in logs.** Grep the codebase for logging of `Authorization`, `Cookie`, or
whole `http.Request` values. `slog` will happily serialise a struct containing a token.

**Error messages.** Does any error leak an absolute path from the build machine, an
internal IP, or a token? `-trimpath` handles the first.

Then write `SECURITY.md` — and include the *unmitigated* items from chapter 14's threat
model. A security policy that only lists strengths is not a security policy.

---

## F. Exercise

1. Profile `gecko find` over a real large repository. Before running it, write down where
   you think the time goes. Then run it. My prediction was wrong the first time and yours
   probably will be too — the interesting part is the delta.

2. Measure and reduce startup time. Run `GODEBUG=inittrace=1 ./gecko version`, find the
   most expensive `init`, and make it lazy. Measure again. Target: under 15 ms.

3. Get a reproducible build. Build twice, `cmp`, and if they differ, find out why. Common
   causes: a timestamp in ldflags, a missing `-trimpath`, `CGO_ENABLED=1` picking up
   different system headers.

4. Do the full release: tag `v0.1.0`, push, and verify every artifact. Download the Linux
   binary on a machine you didn't build on, check the checksum, verify the cosign
   signature, run `gh attestation verify`. If any step fails, the release process is
   broken and now is the time to find out.

5. Write the docs. All of them. `README.md`, `docs/commands.md`, `docs/plugins.md`,
   `docs/architecture.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CHANGELOG.md`. Then hand the
   README to someone who has never seen the project and watch them try to install and use
   it without help. Whatever they get stuck on is the actual bug.

---

## G. Testing the release itself

### Installed-binary smoke tests

Unit tests do not prove the shipped artifact works. Test the binary:

```go
//go:build e2e

package e2e

// TestReleaseBinary exercises the actual compiled binary, which is the
// only way to catch problems that unit tests structurally cannot: a
// broken ldflags stamp, a missing embedded template, an init that
// panics, or a build-tag error that left a platform without an
// implementation.
func TestReleaseBinary(t *testing.T) {
	bin := os.Getenv("GECKO_BINARY")
	if bin == "" {
		t.Skip("set GECKO_BINARY to the binary under test")
	}

	tests := []struct {
		name     string
		args     []string
		wantCode int
		contains string
	}{
		{"version", []string{"version"}, 0, "gecko"},
		{"version is stamped", []string{"version", "--short"}, 0, "v"},
		{"help", []string{"help"}, 0, "Core Commands"},
		{"unknown command", []string{"nope"}, 2, "unknown command"},
		{"tree", []string{"tree", "--depth", "1"}, 0, ""},
		{"doctor", []string{"doctor"}, 0, "System"},
		{"config path", []string{"config", "path"}, 0, "config.yaml"},
		{"hash stdin", []string{"hash", "-"}, 0, "sha256"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(bin, tt.args...)
			cmd.Stdin = strings.NewReader("")
			out, _ := cmd.CombinedOutput()
			code := cmd.ProcessState.ExitCode()

			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d\n%s", code, tt.wantCode, out)
			}
			if tt.contains != "" && !strings.Contains(string(out), tt.contains) {
				t.Errorf("output missing %q:\n%s", tt.contains, out)
			}
		})
	}
}

func TestVersionIsStampedNotDev(t *testing.T) {
	bin := os.Getenv("GECKO_BINARY")
	if bin == "" { t.Skip() }
	out, _ := exec.Command(bin, "version", "--short").Output()
	v := strings.TrimSpace(string(out))
	if v == "dev" || v == "" {
		t.Errorf("version is %q; the release ldflags did not apply", v)
	}
}
```

Run in the release workflow against every built artifact you can execute on the runner:

```yaml
- name: Smoke-test the release binary
  run: |
    tar xzf dist/gecko_*_linux_amd64.tar.gz
    GECKO_BINARY=$PWD/gecko go test -tags e2e ./e2e/...
```

`TestVersionIsStampedNotDev` catches the release-day classic: a refactor moved
`internal/cli` and the `-X` path in `.goreleaser.yaml` no longer matches any variable, so
`-X` silently does nothing and you ship a binary that reports `dev`. `-X` **fails
silently** on an unmatched path — there is no warning.

### Installation tests

```yaml
verify-install:
  needs: release
  strategy:
    matrix:
      os: [ubuntu-latest, macos-latest]
  runs-on: ${{ matrix.os }}
  steps:
    - name: Install from the published release
      run: |
        curl -fsSL https://github.com/yourname/gecko/releases/latest/download/install.sh | sh
        gecko version
    - name: Verify checksum and signature
      run: |
        cosign verify-blob \
          --certificate checksums.txt.pem \
          --signature checksums.txt.sig \
          --certificate-identity-regexp 'https://github.com/yourname/gecko/.*' \
          --certificate-oidc-issuer https://token.actions.githubusercontent.com \
          checksums.txt
```

Testing your own install path from a clean machine is the only way to know it works.

### Coverage: use it as a signal, not a target

```bash
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out | sort -k3 -nr | tail -20
go tool cover -html=coverage.out
```

Sorting ascending shows the *least*-covered functions, which is where to look. Chasing a
percentage produces tests that execute code without asserting anything.

Some things should be near 100%: error paths in security-relevant code (`clean`'s
containment, `plugin`'s verification), parsers, and the exit-code mapper. Some things
reasonably sit low: platform code you can't run, and terminal rendering.

---

## H. Review

- Which profile answers which question, and that heap profiles have four sample types
  with `inuse_space` as a misleading default for allocation work.
- CPU profiles are near-useless for a 50 ms process; traces are the right tool.
- `GODEBUG=inittrace=1` for startup cost, and that `init` work is usually the culprit.
- `-s -w -trimpath`, what each does, and what you give up.
- `govulncheck`'s call-graph analysis vs generic dependency scanning.
- Reproducible builds: the five requirements and why anyone cares.
- Exactly what GoReleaser does, step by step.
- Keyless signing with Sigstore removes key management, which is the part everyone gets
  wrong.
- CI structure: fast job first, `fail-fast: false`, pinned actions, least-privilege
  tokens, benchmarks as a record rather than a gate.
- `-X` fails silently on a path that matches nothing — test the stamped version.
- Coverage as a signal about *which* code is untested, not as a target.

---

## I. Refactoring — the final pass

Read the whole thing top to bottom. Things to look for specifically:

**Package cohesion.** Can you state each package's responsibility in one sentence without
"and"? `internal/cli` — "dispatch, help and exit codes". `internal/filesystem` — "file and
directory operations". If any package needs an "and", consider splitting.

**Dependency direction.** Verify mechanically:

```bash
go list -deps ./internal/filesystem | grep gecko    # should show nothing of ours
go mod graph | grep -c ' '                          # total dependency edges
```

Domain packages should depend on the standard library and each other, never on `cli`.

**Dead code.**

```bash
go install golang.org/x/tools/cmd/deadcode@latest
deadcode ./...
```

Chapter 6's `compareVersions` should already be gone. Anything else?

**The `TODO(ch11)` from chapter 7.** `isAddrInUse` still matches error strings. Now that
`internal/platform` has the build-tag machinery, fix it properly:

```go
//go:build !windows
func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

//go:build windows
func isAddrInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }
```

**Closing the loop on a tracked imperfection is the last refactoring, and it's the one
that distinguishes a finished project from an abandoned one.**

---

## Final documentation

`README.md` structure that works:

1. One-sentence description and a screenshot or asciinema cast.
2. Installation, all methods, copy-pasteable.
3. A 30-second quickstart with three commands that show value immediately.
4. A command table linking to `docs/commands.md`.
5. Plugins: one paragraph plus a link.
6. Contributing and licence.

Not: architecture diagrams, a philosophy essay, or a feature matrix. Those go in `docs/`.
The README's job is to get someone from "what is this" to "it's installed and I ran
something useful" in under two minutes.

`docs/architecture.md` should explain the *why*, since the code shows the what: why
executable plugins, why a hand-rolled dispatcher, why `os.DirFS` everywhere, why polling
exists alongside fsnotify. Future you will want this. So will your first contributor.

---

## Commit

```
perf: reduce startup time by deferring plugin discovery
feat: add hidden profiling flags
ci: add cross-platform test, lint and cross-compile matrix
ci: add release workflow with signing and provenance
build: add goreleaser configuration with reproducible builds
test: add end-to-end tests against the release binary
fix: use syscall error constants instead of string matching
docs: add README, architecture, contributing and security policy
chore: release v1.0.0
```

---

## What you've built

Roughly 12,000 lines of Go across 30 packages, running on three operating systems and six
architectures, with a plugin protocol other people can target.

More usefully, you can now: read `net/http`'s source and follow it; look at a profile and
know which view answers your question; decide whether a piece of platform code needs an
interface, a build tag or neither, and defend the answer; identify when an abstraction is
premature and when it's overdue; and write a threat model that includes the parts you
didn't solve.

The last one is the least common and the most valuable.

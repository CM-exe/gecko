# Gecko — An Advanced Go Course

Building a cross-platform developer toolbox from scratch.

---

## How to read this course

Sixteen files. Read them in order. Every chapter follows the same shape:

**Goal → Why → Concepts → Design → Implementation → Testing → Review**

Code blocks are complete and compile as written unless marked `// sketch`. Where a
chapter says "try this before reading on", there's a genuine design decision ahead
and the answer is worth arriving at yourself.

Replace `github.com/yourname/gecko` with your own module path throughout.

---

## The three principles

**1. We build things twice when it teaches something.**
A polling file watcher before `fsnotify`. Hand-rolled subcommand dispatch before we
evaluate Cobra. A naive `WalkDir` before a bounded worker pool. You learn what a
library buys you by having paid the cost yourself first.

**2. We let the architecture hurt before we fix it.**
Around chapter 5 the flat structure strains. That discomfort is the lesson.
A premature `internal/platform/` is exactly as wrong as a 900-line `main.go`.
Extraction happens when you need to test something independently or when a second
caller appears — not on a schedule.

**3. Cross-platform is not a polishing step.**
It starts in chapter 3 (config paths) and becomes the whole subject of chapter 11
(`ports`/`processes`). If you only own one machine, CI becomes your other two, and
we set that up earlier than most projects would.

---

## Chapter map

| # | Chapter | Feature | Difficulty | Est. |
|---|---------|---------|------------|------|
| 01 | Command dispatch | `version`, `help` | Intermediate | 3–4h |
| 02 | Filesystem traversal | `tree` | Intermediate | 4–5h |
| 03 | Config & platform paths | `config` | Intermediate | 4–5h |
| 04 | Streaming I/O | `hash` | Intermediate | 3–4h |
| 05 | Concurrency & safety | `find`, `clean` | Advanced | 8–10h |
| 06 | Subprocesses & Cobra | `doctor`, `project` | Advanced | 6–8h |
| 07 | HTTP servers | `serve` | Advanced | 8–10h |
| 08 | HTTP clients & network | `http`, `dns`, `ping`, data cmds | Advanced | 8–10h |
| 09 | File watching | `watch` | Advanced+ | 10–12h |
| 10 | Task DAGs | `run` | Advanced+ | 8–10h |
| 11 | Platform engineering | `ports`, `processes` | Expert | 10–14h |
| 12 | Terminal UX | `fun`, colors, TUI | Advanced | 6–8h |
| 13 | Plugin architecture | plugin protocol | Expert | 10–12h |
| 14 | Plugin ecosystem | SDK, registry, signing | Expert | 12–16h |
| 15 | Production | CI, profiling, release | Advanced | 10–12h |

~110–140 hours done properly.

---

## Feature roadmap

| Area | Commands |
|---|---|
| Core | `version`, `help`, `completion`, `config` |
| Files | `tree`, `hash`, `find`, `clean` |
| System | `doctor`, `info`, `env`, `ports`, `processes` |
| Project | `project info\|stats\|deps\|size` |
| Dev | `serve`, `watch`, `run` |
| Network | `http`, `dns`, `ping` |
| Data | `json`, `yaml`, `csv`, `decode` |
| Fun | `fun matrix\|ascii\|qr\|color\|fortune\|timer` |
| Plugins | `plugin list\|create\|install\|remove\|update\|search` |
| SDK | `github.com/yourname/gecko/sdk` |

---

## Architecture evolution

Four phases, each triggered by a specific pain rather than a date.

**Phase 0 (ch 1–2) — almost flat.** `cmd/gecko/main.go` plus `internal/cli`. Command
logic lives in the command files. This is *correct* at this size and I want you to
see that it is.

**Phase 1 (ch 3–5) — domain packages.** Trigger: logic substantial enough to test
without the CLI. `tree` traversal becomes `internal/filesystem`. Config becomes
`internal/config`.

**Phase 2 (ch 6–11) — the platform seam.** Once "open browser", "config dir" and
"list ports" all need OS-specific behaviour, the seam is obvious. Each piece then
gets classified four ways: fully portable / runtime branch / build-tag split /
interface. Most people reach for the interface first and it is usually wrong.

**Phase 3 (ch 13–14) — the public surface.** `sdk/` becomes the only non-`internal`
package, which makes it the only thing with compatibility obligations. Everything
else stays under `internal/` precisely so we can keep refactoring it.

**Invariants throughout:**
- Dependencies point inward. `cli` imports domain packages; domain never imports `cli`.
- No `utils` package. Ever. If you can't name it, you haven't found the concept.
- Interfaces are declared where consumed, not where implemented.
- Anything that would need to import `cli` means the abstraction is inverted.

---

## Final layout (arrived at, not assumed)

```
gecko/
├── cmd/gecko/main.go
├── internal/
│   ├── cli/          # dispatch, help, exit codes, command registry
│   ├── config/       # precedence chain, platform paths
│   ├── filesystem/   # walk, tree, find, hash, clean
│   ├── network/      # http client, dns, ping, tls
│   ├── process/      # exec, groups, signals, task runner
│   ├── platform/     # build-tag-split OS specifics
│   ├── plugin/       # discovery, protocol, manager
│   ├── project/      # detection, stats, deps
│   └── terminal/     # color, tty, ansi, prompts
├── sdk/              # public plugin SDK
├── testdata/
├── .github/workflows/
├── go.mod / go.sum
└── README.md / LICENSE / CHANGELOG.md
```

---

## Concept tracker

Tick these off as you go. Each chapter opens with the subset it introduces.

**Language & runtime**
- [ ] Generics and type sets (where they earn it) — ch 5, 10
- [ ] `errors.Is` / `As` / `Join`, sentinel vs typed errors — ch 1, 5
- [ ] `sync.WaitGroup`, `Mutex`, `Once`, `atomic` — ch 5, 6
- [ ] Escape analysis and allocation reduction — ch 4
- [ ] GMP scheduler, work stealing, `GOMAXPROCS` — ch 5
- [ ] Build constraints and file-suffix compilation — ch 11
- [ ] `go:embed` — ch 14
- [ ] `unsafe` boundaries in `x/sys` — ch 11

**Standard library depth**
- [ ] `flag.FlagSet` vs package-level `flag` — ch 1
- [ ] `io/fs`, `fs.FS`, `fs.WalkDir` — ch 2, 5
- [ ] `filepath` vs `path`, Windows path semantics — ch 2
- [ ] `io.Copy`, `io.MultiWriter`, buffer sizing — ch 4
- [ ] `context` propagation and cancellation trees — ch 5, 7, 10
- [ ] `os/exec`, `SysProcAttr`, process groups — ch 6, 9, 10
- [ ] `os/signal`, `signal.NotifyContext` — ch 7
- [ ] `net/http` server internals, timeouts, shutdown — ch 7
- [ ] `http.Client`, `Transport`, `httptrace` — ch 8
- [ ] `crypto/tls` introspection — ch 8
- [ ] `net.Resolver`, raw sockets, ICMP — ch 8
- [ ] `encoding/json` streaming `Decoder`/`Encoder` — ch 8
- [ ] `text/tabwriter` — ch 1
- [ ] `log/slog` structured logging — ch 6
- [ ] `testing/fstest`, `httptest`, `testing.B`, `testing.F` — ch 2, 4, 7

**Engineering practice**
- [ ] Package boundaries and dependency direction — ch 3, 5
- [ ] Consumer-defined interfaces, DI without a framework — ch 1, 6
- [ ] Table-driven and golden-file tests — ch 2
- [ ] Race detector — ch 5
- [ ] `pprof`: CPU, heap, block profiles — ch 4, 15
- [ ] Semantic versioning and API compatibility — ch 14
- [ ] Cross-compilation, `-ldflags` stamping, reproducible builds — ch 1, 15
- [ ] Supply-chain security: checksums, ed25519 signatures — ch 14

---

## Before you start

```bash
go version          # 1.22+ required; 1.24+ assumed for tooling notes
git --version
```

```bash
mkdir gecko && cd gecko
git init
go mod init github.com/yourname/gecko
```

Add a `.gitignore`:

```
/gecko
/gecko.exe
/dist/
*.test
*.out
*.prof
.DS_Store
```

Commit: `chore: initialise Go module`

Then open `01-cli-foundation.md`.

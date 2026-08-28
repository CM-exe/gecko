# Gecko — Advanced Go Course

Build a cross-platform developer toolbox from scratch. Sixteen chapters, ~110–140 hours,
complete working code throughout.

**Start here:** [`00-overview.md`](00-overview.md) — roadmap, architecture strategy,
concept tracker, initial repository setup.

---

## Chapters

| # | File | Feature | Difficulty |
|---|---|---|---|
| 00 | [Overview](00-overview.md) | Roadmap and setup | — |
| 01 | [CLI Foundation](01-cli-foundation.md) | `version`, `help`, dispatch | Intermediate |
| 02 | [Tree](02-tree.md) | `tree`, `io/fs`, traversal | Intermediate |
| 03 | [Config & Platform Paths](03-config-platform.md) | `config`, XDG, precedence | Intermediate |
| 04 | [Hash](04-hash.md) | `hash`, streaming I/O, benchmarking | Intermediate |
| 05 | [Find & Clean](05-find-clean.md) | Concurrency, destructive ops safety | Advanced |
| 06 | [Doctor & Project](06-doctor-project.md) | `os/exec`, slog, the Cobra decision | Advanced |
| 07 | [Serve](07-serve.md) | HTTP server, middleware, shutdown | Advanced |
| 08 | [Network Clients](08-network-clients.md) | `http`, `dns`, `ping`, data commands | Advanced |
| 09 | [Watch](09-watch.md) | File events, process trees | Advanced+ |
| 10 | [Task Runner](10-task-runner.md) | `run`, DAGs, parallel scheduling | Advanced+ |
| 11 | [Platform](11-platform.md) | `ports`, `processes`, build tags | Expert |
| 12 | [Terminal](12-terminal.md) | ANSI, raw mode, TUI, fun commands | Advanced |
| 13 | [Plugins](13-plugins.md) | Plugin protocol and discovery | Expert |
| 14 | [Plugin Ecosystem](14-plugin-ecosystem.md) | SDK, registry, signing | Expert |
| 15 | [Production](15-production.md) | Profiling, CI, release, docs | Advanced |

---

## Reading order and dependencies

Chapters build strictly on each other. Chapter 1's `Command` design is validated in
chapter 13; chapter 4's constant-time comparison is reused in chapter 14; chapter 9's
process groups are reused in chapter 10.

Three chapters can be skipped without breaking later ones: **08** (network clients),
**12** (terminal UX), and the second half of **14** (registry). Everything else is
load-bearing.

---

## Recurring themes worth watching for

**The extraction heuristic.** Chapter 2 rejects an interface (one implementation).
Chapter 5 accepts an extraction (three callers). Chapter 6 accepts an interface (a real
test double). Chapter 10 *rejects* an extraction that looks like duplication but isn't.
The rule emerges across all four.

**The four-way platform classification.** Introduced in chapter 3, refined in 7 and 9,
stated formally in chapter 11. Most people reach for an interface when a build tag would
do.

**Dependency decisions.** Five third-party dependencies are adopted across the course
(`yaml.v3`, `errgroup`, `pflag`, `fsnotify`, `x/sys`/`x/term`/`x/mod`) and several are
declined (Cobra, `miekg/dns`, `gopsutil`, Bubble Tea for most uses). Each decision is
argued against the same five-point checklist.

**Honest limitations.** Several implementations ship known-imperfect with a documented
reason: the string-matched `isAddrInUse` in chapter 7 (fixed in 15), `lsof` on macOS
instead of syscalls, unrotatable signing keys, no plugin sandbox. Naming what you didn't
solve is treated as part of the work.

---

## Prerequisites

- Go 1.22+ installed (1.24+ assumed for some tooling notes)
- Git
- Comfortable with Go fundamentals: interfaces, goroutines, channels, errors, testing

Replace `github.com/yourname/gecko` with your own module path throughout.

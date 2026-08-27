# Chapter 10 — `gecko run`: Task Graphs and Parallel Execution

```
Difficulty:   Advanced+
Est. time:    8–10 hours
Main concepts: DAGs, topological sort, cycle detection, Kahn's algorithm, parallel
               scheduling with dependency constraints, context trees, exit-code
               aggregation, environment composition, shell vs argv execution,
               errors.Join, generics for graph code
Prerequisites: Chapters 1–9
```

---

## A. Goal

```yaml
# gecko.yaml
tasks:
  lint:
    command: golangci-lint run
  test:
    command: go test -race ./...
    env:
      CGO_ENABLED: "1"
  build:
    command: go build -o dist/gecko ./cmd/gecko
    dir: .
  release:
    depends_on: [lint, test, build]
    command: goreleaser release --clean
```

```
$ gecko run release

  lint    ▶ running
  test    ▶ running
  build   ▶ running
  lint    ✓ 2.1s
  build   ✓ 3.4s
  test    ✓ 8.9s
  release ▶ running
  release ✓ 12.2s

  4 tasks, 4 succeeded, 9.1s wall (16.6s cpu, 1.8x speedup)
```

---

## B. Why this matters

A task runner is where three previously separate skills combine: graph algorithms,
concurrency with constraints, and process management. Unlike chapter 5's worker pool —
where tasks were independent — here the concurrency is *constrained by a partial order*,
and that's a materially harder scheduling problem.

It's also the chapter where Gecko becomes self-hosting: by the end, `gecko run test`
builds and tests Gecko.

---

## C. Concepts

### The graph

Tasks and dependencies form a **directed acyclic graph**. `release` depends on `test`
means there's an edge, and a cycle (`a → b → a`) makes execution impossible.

Two classic algorithms:

**Kahn's algorithm** (BFS): compute in-degrees, repeatedly emit nodes with in-degree 0,
decrementing successors. If nodes remain when no zero-in-degree node exists, there's a
cycle. O(V+E), and it naturally produces "levels" of independently-runnable tasks.

**DFS with three-colour marking**: white (unvisited), grey (on the current path), black
(done). Encountering a grey node means a cycle, and the grey stack *is* the cycle, which
gives you a good error message. Postorder emission gives reverse topological order.

**Decision: DFS for validation, Kahn-style scheduling for execution.**

Reasoning: DFS gives the best cycle error (`cycle: release → test → build → release`),
and cycle errors are the ones users actually hit. But Kahn's level structure doesn't
translate to optimal parallel execution anyway — a task should start the moment its own
dependencies finish, not when its entire level does. So execution uses neither directly:
it's an event-driven scheduler where completing a task decrements its dependents'
counters.

### Parallel scheduling with dependencies

The naive approach — compute levels, run each level with an errgroup, wait for the level
— is wrong. Consider:

```
a (1s) ──┐
         ├──> c
b (10s) ─┘
d (1s) ──────> e (1s)
```

Level-based: level 0 is {a, b, d}, taking 10s (bound by b). Level 1 is {c, e}. Total 11s.
But `e` only needs `d`, so it could have run at t=1. Optimal is 11s here by coincidence,
but with `d(1s) → e(1s) → f(1s) → g(1s)` alongside `b(10s)`, level-based takes 40s and
event-driven takes 10s.

**Event-driven scheduling:**

```go
remaining := map[string]int{}   // unmet dependency count per task
dependents := map[string][]string{}
ready := make(chan string, len(tasks))

for name, t := range tasks {
    remaining[name] = len(t.DependsOn)
    if remaining[name] == 0 { ready <- name }
    for _, dep := range t.DependsOn {
        dependents[dep] = append(dependents[dep], name)
    }
}

// On completion of `name`:
for _, d := range dependents[name] {
    remaining[d]--
    if remaining[d] == 0 { ready <- d }
}
```

The bookkeeping map must be mutex-protected, or owned by a single scheduler goroutine
that workers report to. **The single-owner version is better**: no lock, and the
scheduler is a plain sequential loop that's easy to reason about. This is a genuine case
where "share by communicating" produces the simpler code.

### Failure semantics

When `test` fails and `release` depends on it, what happens to `lint`, which is running?

Three defensible policies:
- **Fail fast**: cancel everything immediately. Fastest feedback.
- **Fail at barrier**: let running tasks finish, don't start new ones. No wasted work
  discarded, but no partial results lost either.
- **Keep going** (`make -k`): run everything runnable, report all failures.

**Decision: fail-at-barrier by default, `--keep-going` for the third, `--fail-fast` for
the first.** Rationale: cancelling a running `lint` to save two seconds discards useful
output the user wanted. But starting `release` when `test` failed is definitely wrong.

Skipped tasks (dependency failed) must be reported distinctly from failed ones. A summary
saying "1 failed" when 6 were skipped hides the shape of the failure.

### Aggregating errors: `errors.Join`

Go 1.20 added multi-error wrapping:

```go
err := errors.Join(err1, err2, err3)   // nil inputs are dropped; all-nil returns nil
errors.Is(err, target)                  // true if any joined error matches
```

`Error()` concatenates with newlines. For a task runner reporting several failures this
is exactly right, and it beats a hand-rolled `MultiError`.

### Shell or argv?

```yaml
command: go test ./...
```

Is that argv `["go", "test", "./..."]` or a shell string?

**Argv** (split on whitespace, honouring quotes) is safe — no injection, no shell needed,
identical across platforms. But `go build ./... && echo done`, `foo | bar`, `VAR=1 cmd`
and `~/bin/tool` don't work.

**Shell** (`sh -c` / `cmd /c`) supports all of that, but the command string is now
interpreted, and any interpolated value becomes injectable. It also differs across
platforms: `sh` isn't on Windows, `cmd` has different quoting, `&&` works in both but
`export` doesn't.

**Decision: argv by default with a shell-words parser; explicit `shell: true` per task.**

```yaml
build:
  command: go build ./...        # argv, safe
deploy:
  shell: true
  command: docker build . && docker push myimage
```

This makes the dangerous mode explicit and visible in the config file, which is where a
reviewer will see it. **Making the unsafe path require an opt-in keyword is a much better
security design than trying to sanitise the safe-looking path.**

The shell-words parser handles `"quoted args"`, `'single'`, and `\` escapes. Roughly 60
lines; write it, don't import it. Note that it must *not* do glob expansion, variable
substitution, or anything else — those are shell features and providing them halfway is
worse than not at all.

### Environment composition

```
os.Environ()
  ← global env from config
    ← task env
      ← --env flags
```

`exec.Cmd.Env` **replaces** the environment entirely when non-nil. A common bug is
setting `cmd.Env = []string{"FOO=bar"}` and wondering why the child can't find anything —
no `PATH`. Always start from `os.Environ()`.

Duplicate keys: `exec` passes the slice through; on Unix `execve` semantics mean the
**last** wins in most libc implementations, but this is not guaranteed. Deduplicate
yourself, keeping the last occurrence, so behaviour is defined.

### Generics, finally earning their place

Graph algorithms are the textbook case for generics:

```go
// TopoSort returns nodes in dependency order, or a CycleError.
func TopoSort[T comparable](nodes []T, deps func(T) []T) ([]T, error)
```

This is reusable for tasks now and for plugin dependency resolution in chapter 14. The
constraint is `comparable` because we need map keys — not `any`, and not a custom
interface.

**When generics are *not* worth it:** if you have one concrete type and no second use in
sight, `func TopoSort(tasks []*Task)` is clearer. The rule mirrors the interface rule from
chapter 6: two real uses, not one plus a hypothetical.

---

## D. Design

### Package structure

```
internal/
  task/
    config.go      # YAML schema, loading, validation
    graph.go       # generic topological sort and cycle detection
    runner.go      # the scheduler
    task_test.go
```

`task` imports `process` (chapter 6/9) for execution. `process` does not import `task`.

### Config discovery

Look for `gecko.yaml`, then `gecko.yml`, then `.gecko.yaml`, walking up from the working
directory to the filesystem root — like `git` finds `.git` and `go` finds `go.mod`.
Stop at the first hit. Working directory for tasks defaults to the **config file's**
directory, not the invocation directory, so `gecko run build` behaves the same from any
subdirectory. That's the behaviour `make` and `npm` have trained everyone to expect.

### The Task type

```go
type Task struct {
    Name        string            `yaml:"-"`
    Description string            `yaml:"description"`
    Command     string            `yaml:"command"`
    Commands    []string          `yaml:"commands"`     // sequence
    Shell       bool              `yaml:"shell"`
    Dir         string            `yaml:"dir"`
    Env         map[string]string `yaml:"env"`
    DependsOn   []string          `yaml:"depends_on"`
    Parallel    *bool             `yaml:"parallel"`     // nil = inherit global
    IgnoreError bool              `yaml:"ignore_error"`
}
```

`Parallel *bool` rather than `bool`: with a plain bool you cannot distinguish "explicitly
false" from "not specified", so you can't implement "default true, overridable to false".
This is the same problem chapter 3 solved with `fs.Visit` and it's the standard reason
config structs use pointers for optional booleans.

---

## E. Implementation

### `internal/task/graph.go`

```go
package task

import (
	"fmt"
	"sort"
	"strings"
)

// CycleError reports a dependency cycle, including the path that forms
// it. Reporting only "cycle detected" forces the user to find it by
// inspection, which in a 30-task file is genuinely difficult.
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return "dependency cycle: " + strings.Join(e.Path, " → ")
}

// MissingDepError reports a dependency on an undefined task.
type MissingDepError struct {
	Task, Dep string
	Suggest   string
}

func (e *MissingDepError) Error() string {
	msg := fmt.Sprintf("task %q depends on undefined task %q", e.Task, e.Dep)
	if e.Suggest != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", e.Suggest)
	}
	return msg
}

// TopoSort returns nodes in dependency order: every node appears after
// all of its dependencies.
//
// It is generic because chapter 14 resolves plugin dependencies with the
// same algorithm. With a single caller a concrete version would be
// clearer; with two, the type parameter earns its keep.
//
// The implementation is DFS with three-colour marking rather than Kahn's
// algorithm, because the grey stack at the moment a cycle is found *is*
// the cycle, which produces a far better error message.
func TopoSort[T comparable](roots []T, deps func(T) []T) ([]T, error) {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS path
		black = 2 // fully explored
	)

	colour := make(map[T]int)
	var order []T
	var path []T

	var visit func(n T) error
	visit = func(n T) error {
		switch colour[n] {
		case black:
			return nil
		case grey:
			// Trim the path to start at the repeated node so the error
			// shows only the cycle, not the route that led to it.
			start := 0
			for i, p := range path {
				if p == n {
					start = i
					break
				}
			}
			cyc := make([]string, 0, len(path)-start+1)
			for _, p := range path[start:] {
				cyc = append(cyc, fmt.Sprint(p))
			}
			cyc = append(cyc, fmt.Sprint(n))
			return &CycleError{Path: cyc}
		}

		colour[n] = grey
		path = append(path, n)

		// Sort dependencies for deterministic output. Without this,
		// map iteration order makes the execution order vary between
		// runs, which makes test failures irreproducible.
		d := deps(n)
		sorted := make([]T, len(d))
		copy(sorted, d)
		sort.Slice(sorted, func(i, j int) bool {
			return fmt.Sprint(sorted[i]) < fmt.Sprint(sorted[j])
		})

		for _, dep := range sorted {
			if err := visit(dep); err != nil {
				return err
			}
		}

		path = path[:len(path)-1]
		colour[n] = black
		order = append(order, n) // postorder: dependencies first
		return nil
	}

	for _, r := range roots {
		if err := visit(r); err != nil {
			return nil, err
		}
	}
	return order, nil
}
```

The `sort.Slice` with `fmt.Sprint` is a wart — it works for string-keyed tasks but is
slow and semantically odd for other types. The clean fix is a second type parameter
constrained by `cmp.Ordered`, or requiring `deps` to return sorted output. Either is
fine; note the wart rather than hiding it.

### `internal/task/runner.go` — the scheduler

```go
package task

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Status int

const (
	StatusPending Status = iota
	StatusRunning
	StatusSucceeded
	StatusFailed
	StatusSkipped
	StatusCancelled
)

type TaskResult struct {
	Name     string
	Status   Status
	ExitCode int
	Err      error
	Started  time.Time
	Duration time.Duration
}

type RunOptions struct {
	MaxParallel int
	KeepGoing   bool  // run everything runnable even after a failure
	FailFast    bool  // cancel running tasks on the first failure
	DryRun      bool
}

type Runner struct {
	tasks  map[string]*Task
	opts   RunOptions
	events chan<- Event   // progress reporting; may be nil
	exec   Executor       // injected so tests need no subprocesses
}

// Executor runs a single task. Declared here, in the consumer, so that
// tests can substitute a fake without touching os/exec.
type Executor interface {
	Execute(ctx context.Context, t *Task, env []string) (exitCode int, err error)
}

// Run executes target and everything it depends on.
//
// Scheduling is event-driven rather than level-based: a task starts the
// moment its own dependencies complete, not when its entire "level"
// does. With a long task in one branch and a chain in another, the
// level-based approach can be several times slower.
func (r *Runner) Run(ctx context.Context, targets []string) ([]TaskResult, error) {
	// 1. Validate and compute the reachable subgraph.
	order, err := TopoSort(targets, func(name string) []string {
		if t, ok := r.tasks[name]; ok {
			return t.DependsOn
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 2. Build the scheduling state. It is owned exclusively by the
	// loop below; workers communicate results over a channel rather
	// than mutating shared maps, which removes the need for a mutex
	// and makes the scheduler a plain sequential program.
	remaining := make(map[string]int, len(order))
	dependents := make(map[string][]string, len(order))
	inSubgraph := make(map[string]bool, len(order))

	for _, name := range order {
		inSubgraph[name] = true
	}
	for _, name := range order {
		t := r.tasks[name]
		for _, dep := range t.DependsOn {
			if !inSubgraph[dep] {
				continue
			}
			remaining[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}

	results := make(map[string]*TaskResult, len(order))
	for _, name := range order {
		results[name] = &TaskResult{Name: name, Status: StatusPending}
	}

	// 3. Seed the ready queue.
	var ready []string
	for _, name := range order {
		if remaining[name] == 0 {
			ready = append(ready, name)
		}
	}

	maxPar := r.opts.MaxParallel
	if maxPar <= 0 {
		maxPar = runtime.NumCPU()
	}

	// runCtx is what tasks receive. With FailFast we cancel it on the
	// first failure; otherwise it simply mirrors ctx.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	done := make(chan TaskResult, len(order))
	running := 0
	failed := false
	var firstErr error

	for {
		// Start whatever we can.
		for len(ready) > 0 && running < maxPar {
			if failed && !r.opts.KeepGoing {
				break // barrier: no new tasks after a failure
			}
			name := ready[0]
			ready = ready[1:]

			t := r.tasks[name]
			results[name].Status = StatusRunning
			results[name].Started = time.Now()
			r.emit(Event{Type: EventStart, Task: name})

			running++
			go func() {
				done <- r.runOne(runCtx, t)
			}()
		}

		if running == 0 {
			break // nothing running and nothing startable
		}

		res := <-done
		running--

		rp := results[res.Name]
		*rp = res
		r.emit(Event{Type: EventFinish, Task: res.Name, Result: res})

		if res.Status == StatusFailed {
			failed = true
			if firstErr == nil {
				firstErr = res.Err
			}
			if r.opts.FailFast {
				cancelRun()
			}
			if !r.opts.KeepGoing {
				// Mark everything downstream as skipped.
				r.markSkipped(res.Name, dependents, results)
				continue
			}
		}

		// Release dependents.
		for _, d := range dependents[res.Name] {
			remaining[d]--
			if remaining[d] == 0 && results[d].Status == StatusPending {
				ready = append(ready, d)
			}
		}
	}

	// 4. Collect in topological order for stable output.
	out := make([]TaskResult, 0, len(order))
	var errs []error
	for _, name := range order {
		out = append(out, *results[name])
		if results[name].Status == StatusFailed {
			errs = append(errs, fmt.Errorf("task %q: %w", name, results[name].Err))
		}
	}

	// errors.Join drops nils and returns nil when everything is nil, so
	// the happy path needs no special case.
	return out, errors.Join(errs...)
}

// markSkipped transitively marks dependents of a failed task.
func (r *Runner) markSkipped(failedName string, dependents map[string][]string, results map[string]*TaskResult) {
	queue := append([]string(nil), dependents[failedName]...)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if results[n].Status != StatusPending {
			continue
		}
		results[n].Status = StatusSkipped
		r.emit(Event{Type: EventSkip, Task: n})
		queue = append(queue, dependents[n]...)
	}
}

func (r *Runner) runOne(ctx context.Context, t *Task) TaskResult {
	start := time.Now()
	res := TaskResult{Name: t.Name, Started: start}

	if r.opts.DryRun {
		res.Status = StatusSucceeded
		return res
	}

	env, err := composeEnv(t)
	if err != nil {
		res.Status, res.Err, res.Duration = StatusFailed, err, time.Since(start)
		return res
	}

	code, err := r.exec.Execute(ctx, t, env)
	res.Duration = time.Since(start)
	res.ExitCode = code

	switch {
	case ctx.Err() != nil && code != 0:
		res.Status = StatusCancelled
		res.Err = ctx.Err()
	case err != nil && !t.IgnoreError:
		res.Status = StatusFailed
		res.Err = err
	case err != nil:
		// ignore_error: the failure is recorded but does not block
		// dependents. Useful for optional linters.
		res.Status = StatusSucceeded
		res.Err = err
	default:
		res.Status = StatusSucceeded
	}
	return res
}
```

Two things to notice.

**The scheduler loop owns all mutable state.** `remaining`, `ready`, `results`, `running`
are touched only by this one goroutine. Workers communicate exclusively through the `done`
channel. No mutex, no race, and the loop reads as ordinary sequential code. Compare with
a version using `sync.Mutex` around shared maps — functionally equivalent, considerably
harder to convince yourself is correct.

**`errors.Join(errs...)` handles the empty case.** No `if len(errs) == 0 { return nil }`.
Small thing, but it's the kind of API knowledge that removes a whole class of off-by-one
bug.

### Environment composition

```go
// composeEnv builds the child environment.
//
// exec.Cmd.Env replaces the environment entirely when non-nil, so it
// must start from os.Environ(); setting only the task's variables would
// leave the child without PATH, HOME or TMPDIR.
//
// Duplicate keys are resolved last-wins explicitly rather than relying
// on libc behaviour, which is unspecified for duplicates in execve.
func composeEnv(t *Task) ([]string, error) {
	merged := make(map[string]string, 64)
	order := make([]string, 0, 64)

	add := func(kv string) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return
		}
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}

	for _, kv := range os.Environ() {
		add(kv)
	}
	for k, v := range sortedPairs(t.Env) {
		add(k + "=" + v)
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out, nil
}
```

### The shell-words parser

```go
// SplitCommand splits a command string into argv, honouring single and
// double quotes and backslash escapes.
//
// It deliberately does NOT implement globbing, variable expansion,
// pipelines, redirection or operators. A task needing those must set
// "shell: true", which makes the shell dependency visible in the config
// file where a reviewer will see it. Silently supporting half of shell
// syntax is worse than supporting none.
func SplitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	var inArg bool

	const (
		plain = iota
		single
		double
	)
	state := plain

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case plain:
			switch {
			case c == ' ' || c == '\t':
				if inArg {
					args = append(args, cur.String())
					cur.Reset()
					inArg = false
				}
			case c == '\'':
				state, inArg = single, true
			case c == '"':
				state, inArg = double, true
			case c == '\\' && i+1 < len(s):
				i++
				cur.WriteByte(s[i])
				inArg = true
			default:
				cur.WriteByte(c)
				inArg = true
			}

		case single:
			// Single quotes are literal: no escapes at all, matching sh.
			if c == '\'' {
				state = plain
			} else {
				cur.WriteByte(c)
			}

		case double:
			switch {
			case c == '"':
				state = plain
			case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		}
	}

	if state != plain {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return args, nil
}
```

### The executor

```go
type execExecutor struct {
	out, errOut io.Writer
	grace       time.Duration
}

func (e *execExecutor) Execute(ctx context.Context, t *Task, env []string) (int, error) {
	argv, err := e.buildArgv(t)
	if err != nil {
		return -1, err
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = t.Dir
	cmd.Env = env
	cmd.Stdout = prefixWriter(e.out, t.Name)
	cmd.Stderr = prefixWriter(e.errOut, t.Name)

	// Reuse chapter 9's group handling: a task that spawns children
	// must not leave them running when the task is cancelled.
	configureGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan struct{})
	go func() { defer close(done); err = cmd.Wait() }()

	select {
	case <-done:
	case <-ctx.Done():
		terminateGroup(cmd, done, e.grace)
		<-done
		return -1, ctx.Err()
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), fmt.Errorf("exited with status %d", ee.ExitCode())
	}
	return 0, err
}

func (e *execExecutor) buildArgv(t *Task) ([]string, error) {
	if !t.Shell {
		return SplitCommand(t.Command)
	}
	if runtime.GOOS == "windows" {
		// cmd /c has quoting rules that differ from sh; passing the
		// whole command as one argument is the only reliable form.
		return []string{"cmd", "/c", t.Command}, nil
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{shell, "-c", t.Command}, nil
}
```

Note the `err` capture in the goroutine — that's a data race as written (the goroutine
writes `err`, the outer function reads it after `<-done`). It happens to be safe because
`close(done)` establishes a happens-before edge, but it's fragile and `-race` won't
complain, which makes it worse. Prefer an explicit channel:

```go
errCh := make(chan error, 1)
go func() { errCh <- cmd.Wait() }()
```

I've left the fragile version above deliberately so you can spot it. **Fix it.**

---

## F. Exercise

1. Implement `gecko run --list` showing tasks, descriptions and dependencies, and
   `gecko run --graph` emitting Graphviz DOT. The DOT output is ten lines and makes cycle
   errors instantly obvious.

2. Add task arguments: `gecko run test -- -run TestFoo` appends to the command. Decide
   what happens when the task has dependencies — do arguments propagate? (They shouldn't.
   Argue why.)

3. Add caching: skip a task if its declared `sources` haven't changed since the last
   successful run. This needs a hash of inputs (chapter 4), a state file in the cache
   directory (chapter 3), and careful thought about what invalidates it. This is how
   `make`, Bazel and Turborepo work, in miniature.

4. **The scheduling exercise.** Construct a task graph where level-based scheduling is
   3× slower than event-driven. Verify by implementing both and timing them with
   `sleep`-based tasks.

---

## G. Testing

### Graph tests need no processes at all

```go
func TestTopoSort(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"release": {"test", "build"},
		"test":    {"generate"},
		"build":   {"generate"},
		"generate": {},
	}
	deps := func(n string) []string { return graph[n] }

	order, err := TopoSort([]string{"release"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	pos := make(map[string]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	// Assert the invariant, not a specific permutation: several valid
	// orders exist and pinning one makes the test brittle.
	for node, ds := range graph {
		if _, ok := pos[node]; !ok {
			continue
		}
		for _, d := range ds {
			if pos[d] > pos[node] {
				t.Errorf("%q appears before its dependency %q", node, d)
			}
		}
	}
}

func TestTopoSortCycleMessage(t *testing.T) {
	graph := map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}}
	_, err := TopoSort([]string{"a"}, func(n string) []string { return graph[n] })

	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CycleError", err)
	}
	// The message must name the actual cycle, not just report one.
	msg := ce.Error()
	for _, n := range []string{"a", "b", "c"} {
		if !strings.Contains(msg, n) {
			t.Errorf("cycle message %q omits %q", msg, n)
		}
	}
}

func TestTopoSortSelfCycle(t *testing.T) { /* a → a */ }
func TestTopoSortDiamond(t *testing.T)   { /* d appears once, not twice */ }
```

"Assert the invariant, not the permutation" is worth internalising. A test asserting
`order == []string{"generate","build","test","release"}` fails when you change the sort
tiebreak, even though nothing is broken.

### Runner tests with a fake executor

```go
// fakeExecutor records call order and simulates durations and failures
// without spawning anything.
type fakeExecutor struct {
	mu       sync.Mutex
	started  []string
	finished []string
	maxConcurrent int
	current       int

	durations map[string]time.Duration
	failures  map[string]bool
}

func (f *fakeExecutor) Execute(ctx context.Context, t *Task, _ []string) (int, error) {
	f.mu.Lock()
	f.started = append(f.started, t.Name)
	f.current++
	if f.current > f.maxConcurrent {
		f.maxConcurrent = f.current
	}
	f.mu.Unlock()

	select {
	case <-time.After(f.durations[t.Name]):
	case <-ctx.Done():
		f.mu.Lock(); f.current--; f.mu.Unlock()
		return -1, ctx.Err()
	}

	f.mu.Lock()
	f.current--
	f.finished = append(f.finished, t.Name)
	f.mu.Unlock()

	if f.failures[t.Name] {
		return 1, errors.New("simulated failure")
	}
	return 0, nil
}
```

Now the interesting properties are testable deterministically:

```go
func TestRunnerRespectsMaxParallel(t *testing.T) {
	fake := &fakeExecutor{durations: map[string]time.Duration{}}
	tasks := map[string]*Task{}
	for i := 0; i < 10; i++ {
		n := fmt.Sprintf("t%d", i)
		tasks[n] = &Task{Name: n}
		fake.durations[n] = 50 * time.Millisecond
	}
	r := &Runner{tasks: tasks, exec: fake, opts: RunOptions{MaxParallel: 3}}

	names := make([]string, 0, 10)
	for n := range tasks { names = append(names, n) }
	if _, err := r.Run(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	if fake.maxConcurrent > 3 {
		t.Errorf("ran %d tasks concurrently, limit was 3", fake.maxConcurrent)
	}
	if fake.maxConcurrent < 2 {
		t.Errorf("never ran more than %d concurrently; parallelism is not working", fake.maxConcurrent)
	}
}

func TestRunnerSkipsDependentsOfFailure(t *testing.T) {
	tasks := map[string]*Task{
		"a": {Name: "a"},
		"b": {Name: "b", DependsOn: []string{"a"}},
		"c": {Name: "c", DependsOn: []string{"b"}},
		"d": {Name: "d"}, // independent
	}
	fake := &fakeExecutor{
		durations: map[string]time.Duration{"a": time.Millisecond, "d": time.Millisecond},
		failures:  map[string]bool{"a": true},
	}
	r := &Runner{tasks: tasks, exec: fake, opts: RunOptions{MaxParallel: 4}}

	results, err := r.Run(context.Background(), []string{"c", "d"})
	if err == nil {
		t.Fatal("expected a failure")
	}
	byName := map[string]TaskResult{}
	for _, r := range results { byName[r.Name] = r }

	if byName["a"].Status != StatusFailed  { t.Error("a should have failed") }
	if byName["b"].Status != StatusSkipped { t.Error("b should have been skipped, not run") }
	if byName["c"].Status != StatusSkipped { t.Error("c should have been transitively skipped") }
	if byName["d"].Status != StatusSucceeded {
		t.Error("d is independent of the failure and should still have run")
	}
}
```

The `d` assertion encodes the fail-at-barrier policy decision. If someone later changes
the default to fail-fast, this test tells them they changed a documented behaviour.

### Command parsing

```go
func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in   string
		want []string
		err  bool
	}{
		{`go test ./...`, []string{"go", "test", "./..."}, false},
		{`echo "hello world"`, []string{"echo", "hello world"}, false},
		{`echo 'single quoted'`, []string{"echo", "single quoted"}, false},
		{`echo "a \"b\" c"`, []string{"echo", `a "b" c`}, false},
		{`echo 'no \escape here'`, []string{"echo", `no \escape here`}, false},
		{`cmd  multiple   spaces`, []string{"cmd", "multiple", "spaces"}, false},
		{`echo ""`, []string{"echo", ""}, false},
		{`unterminated "quote`, nil, true},
		{``, nil, true},
	}
	...
}

func FuzzSplitCommand(f *testing.F) {
	f.Add(`go test ./...`)
	f.Add(`a "b" 'c'`)
	f.Fuzz(func(t *testing.T, s string) {
		args, err := SplitCommand(s)
		if err != nil { return }
		// Invariant: no argument may contain an unescaped quote
		// character that survived parsing incorrectly, and joining
		// must never produce more args than input characters.
		if len(args) > len(s)+1 {
			t.Errorf("produced %d args from %d characters", len(args), len(s))
		}
	})
}
```

### End-to-end, using Gecko on itself

```go
func TestRunnerEndToEnd(t *testing.T) {
	if testing.Short() { t.Skip() }
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gecko.yaml"), []byte(`
tasks:
  first:
    command: go version
  second:
    depends_on: [first]
    command: go env GOOS
`), 0o644)

	env, out, _ := testEnv(nil)
	env.WorkDir = dir
	if code := Main(context.Background(), []string{"run", "second"}, env); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out.String(), runtime.GOOS) {
		t.Errorf("second task output missing:\n%s", out)
	}
}
```

---

## H. Review

- DFS three-colour marking vs Kahn's algorithm, and why DFS gives better cycle errors.
- Why level-based parallel scheduling is suboptimal and event-driven isn't.
- Single-owner mutable state plus a results channel removes the mutex entirely.
- Fail-fast vs fail-at-barrier vs keep-going, and why skipped ≠ failed in reporting.
- `errors.Join` semantics: nils dropped, all-nil returns nil.
- `*bool` in config structs for "unset vs explicitly false".
- `cmd.Env` replaces rather than augments; always start from `os.Environ()`.
- argv by default, `shell: true` as a visible opt-in, and why that beats sanitising.
- Generics justified by a second real caller (chapter 14), not by elegance.
- Testing concurrency properties (max parallelism, skip propagation) with a fake
  executor and no subprocesses.

---

## I. Refactoring

Two.

**1. `Runner.Run` is about 90 lines and does four things:** validation, state
construction, the scheduling loop, and result collection. Split:

```go
func (r *Runner) Run(ctx, targets) ([]TaskResult, error) {
    plan, err := r.plan(targets)      // validate + build state
    if err != nil { return nil, err }
    return r.execute(ctx, plan)       // the loop
}
```

The `plan` half is then testable without any scheduling at all, and `--graph` and
`--list` can use it directly.

**2. `internal/watch`'s Supervisor and `internal/task`'s Runner both manage
process lifecycle with cancellation and grace periods.** Do they share code?

Look carefully before extracting. The Supervisor manages *one long-lived* process with
restarts. The Runner manages *many short-lived* processes with dependencies. The shared
part is exactly `configureGroup` + `terminateGroup`, which already lives in
`internal/process`. Everything else differs.

**Decline the extraction.** Two things that superficially resemble each other are not
duplication. The test: could a change to one plausibly need to be mirrored in the other?
Here, no. **Recognising a false duplication is as important as recognising a real one,
and the cost of a wrong extraction — a shared abstraction serving two masters badly — is
higher than the cost of a little repetition.**

---

## Commit

```
feat: add task graph with topological sort and cycle detection
feat: add task runner with dependency-aware parallel scheduling
feat: add shell-words parsing with explicit shell opt-in
test: add scheduler property tests with fake executor
```

Then, satisfyingly:

```yaml
# gecko.yaml — Gecko can now build itself
tasks:
  test:
    command: go test -race ./...
  lint:
    command: go vet ./...
  build:
    depends_on: [test, lint]
    command: go build -o dist/gecko ./cmd/gecko
```

```
chore: add gecko.yaml so Gecko builds itself
```

Next: `11-platform.md`.

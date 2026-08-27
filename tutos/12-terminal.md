# Chapter 12 — Terminal UX and the Fun Commands

```
Difficulty:   Advanced
Est. time:    6–8 hours
Main concepts: ANSI escape sequences, TTY detection, NO_COLOR, Windows VT mode,
               raw mode and termios, alternate screen buffer, terminal restoration on
               signal, Unicode width, progress rendering, the Elm architecture
Prerequisites: Chapters 1–11
```

---

## A. Goal

```
$ gecko fun timer 25m
  ╭──────────────────────────╮
  │        24:37             │
  │  ████████████░░░░░░░░░░  │
  ╰──────────────────────────╯
  [space] pause  [r] reset  [q] quit

$ gecko fun color "#ff00aa"
  ███  #FF00AA
       rgb(255, 0, 170)
       hsl(320, 100%, 50%)
       Nearest ANSI-256: 199

$ gecko clean | cat        # no colour, no cursor codes, pipe-safe
$ NO_COLOR=1 gecko doctor  # respected
```

---

## B. Why this matters

Every command written so far prints text. This chapter makes that text good — and, more
importantly, makes it **correct when it isn't a terminal**. A tool that emits escape
sequences into a pipe or a CI log is broken, and the number of published CLIs that do
this is remarkable.

The fun commands are a genuine vehicle for the hard parts: raw mode, terminal
restoration, and the discipline that a program which changed the terminal's state **must
restore it on every exit path including SIGINT and panic**. Get that wrong and you leave
the user with an invisible cursor and no echo.

---

## C. Concepts

### ANSI escape sequences

The prefix is `ESC [` (`\x1b[`), called CSI.

```
\x1b[31m         red foreground
\x1b[1;31m       bold red
\x1b[38;5;199m   256-colour foreground (index 199)
\x1b[38;2;255;0;170m   24-bit truecolour
\x1b[0m          reset all attributes

\x1b[2J          clear screen
\x1b[H           cursor to 1,1
\x1b[K           clear from cursor to end of line
\x1b[?25l        hide cursor
\x1b[?25h        show cursor
\x1b[?1049h      switch to alternate screen buffer
\x1b[?1049l      switch back
\x1b[nA/B/C/D    move cursor up/down/right/left n cells
\x1b[s / \x1b[u  save / restore cursor position
```

Three colour depths and you need to pick based on capability: 16-colour (universal),
256-colour (`TERM` contains `256color`), truecolour (`COLORTERM=truecolor` or `24bit`).
Detection is heuristic and always has been.

`\x1b[?1049h` (alternate screen) is what `vim` and `less` use: full-screen apps run in a
separate buffer, and on exit your scrollback is untouched. Any full-screen TUI should use
it. Any non-full-screen output should not.

### When to emit colour, precisely

The decision chain, in order:

1. `--no-color` flag → no.
2. `NO_COLOR` environment variable set to **anything, including empty** → no.
   (The `no-color.org` standard is explicit: presence, not value.)
3. `--color=always` or `FORCE_COLOR` / `CLICOLOR_FORCE=1` → yes, even when piped.
4. `TERM=dumb` → no.
5. stdout is not a character device → no.
6. Otherwise → yes.

Point 3 exists because CI systems capture output through a pipe but still render ANSI.
Without a force override, your CI logs are permanently monochrome.

Point 5 is TTY detection: `os.Stdout.Stat()` and check `ModeCharDevice`:

```go
func isTerminal(f *os.File) bool {
    info, err := f.Stat()
    if err != nil {
        return false
    }
    return info.Mode()&os.ModeCharDevice != 0
}
```

This works on Unix and mostly on Windows. It's what `golang.org/x/term.IsTerminal` does
more carefully (`ioctl(TCGETS)` on Unix, `GetConsoleMode` on Windows), and for a project
already depending on `x/sys`, using `x/term` is the right call.

**Check stdout and stderr independently.** `gecko find "*.go" > files.txt` should write
plain filenames to the file and may still colour the progress indicator on stderr.

### Windows terminals

Historically Windows console did not interpret ANSI at all; you had to call
`SetConsoleTextAttribute`. Since Windows 10 1511 there's **Virtual Terminal processing**,
which must be enabled explicitly:

```go
//go:build windows

func enableVirtualTerminal(f *os.File) error {
    h := windows.Handle(f.Fd())
    var mode uint32
    if err := windows.GetConsoleMode(h, &mode); err != nil {
        return err // not a console: a pipe or file
    }
    return windows.SetConsoleMode(h,
        mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
```

Windows Terminal, PowerShell 7 and VS Code's terminal enable it by default. Legacy
`conhost` may not. Call it at startup, ignore the error, and fall back to no colour if it
fails.

Also on Windows: the default code page may not be UTF-8, so `├` renders as mojibake.
`windows.SetConsoleOutputCP(windows.CP_UTF8)` fixes it for the session. Combined with the
`--ascii` fallback from chapter 2, that's adequate coverage.

### Raw mode

Normally the terminal is in **canonical (cooked) mode**: the kernel line-buffers input,
handles backspace, and only delivers a line when Enter is pressed. It also echoes. And
Ctrl-C generates SIGINT.

Raw mode turns all of that off — every keypress arrives immediately, nothing is echoed,
and Ctrl-C is just byte 0x03.

```go
import "golang.org/x/term"

oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
if err != nil { return err }
defer term.Restore(int(os.Stdin.Fd()), oldState)
```

`x/term` handles the `termios` `ioctl`s on Unix and `SetConsoleMode` on Windows. Writing
it yourself means `TCGETS`/`TCSETS` with a hand-defined `termios` struct per platform —
this is a `x/sys` binding job, not a Go lesson, and `x/term` is maintained by the Go team.
**Use it.**

**The restoration problem.** `defer term.Restore(...)` covers a normal return and a
panic. It does **not** cover:

- `os.Exit` anywhere in the program.
- SIGINT, which in raw mode you no longer receive — but SIGTERM and SIGHUP you still do.
- SIGKILL, which nothing can cover.

So a raw-mode program needs:

```go
// 1. defer, for normal exit and panic
defer restore()

// 2. a signal handler for SIGTERM/SIGHUP
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
go func() { <-sigCh; restore(); os.Exit(130) }()

// 3. recover-and-restore, because a panic with the cursor hidden and
//    echo off leaves the user with an unusable shell
defer func() {
    if r := recover(); r != nil {
        restore()
        panic(r)
    }
}()
```

Make `restore` idempotent with a `sync.Once` — it will be called more than once.

The user-visible failure if you get this wrong is a terminal with no cursor, no echo, and
a shell that appears frozen. They have to type `reset` blind. Take it seriously.

### Unicode width

`len("héllo")` is 6 bytes. `utf8.RuneCountInString` gives 5 runes. But the **display
width** is what matters for alignment, and it's neither:

- CJK ideographs and most emoji are **2 cells wide**.
- Combining marks (U+0301) are **0 cells**.
- Emoji ZWJ sequences (👨‍👩‍👧‍👦) are several runes and typically 2 cells, but terminals
  disagree.
- Some sequences render differently in different terminals. There is no universal answer.

`github.com/mattn/go-runewidth` implements the Unicode East Asian Width property, which
covers the common cases. `text/tabwriter` counts **runes**, not cells, so a table with
CJK text misaligns.

**Decision: accept rune-width for now, document it.** Adding `go-runewidth` for
`gecko tree` alignment is defensible but marginal. If you later add a TUI that renders
user data, take the dependency.

### Progress rendering without a TUI framework

For a single-line progress bar:

```go
fmt.Fprintf(w, "\r\x1b[K%s %3d%%", bar, pct)   // \r to column 0, \x1b[K clear line
```

`\r` plus clear-to-end-of-line is the whole technique. **Rate-limit updates to ~30/s** —
writing 10,000 updates per second makes the program slower than the work it's measuring
and can flood a slow terminal.

For multi-line updates, move the cursor up N lines with `\x1b[NA`, redraw, and track how
many lines you printed. This breaks if the output wrapped, which is why you need the
terminal width (`term.GetSize`) and why full-screen apps use the alternate buffer instead.

### TUI frameworks: Bubble Tea

`github.com/charmbracelet/bubbletea` implements the Elm architecture:

```go
type Model interface {
    Init() Cmd
    Update(Msg) (Model, Cmd)
    View() string
}
```

State is immutable-ish, `Update` is a pure function from (state, message) to (state,
command), `View` renders state to a string, and the runtime handles input, resizing, the
alternate screen, and restoration.

**Its real value is not the abstraction — it's the terminal handling.** Raw mode, signal
restoration, resize events, bracketed paste, mouse support, and Windows compatibility are
all things you would otherwise get wrong.

**Decision: hand-roll the simple animations (`matrix`, `progress`), use Bubble Tea only
for `fun timer`,** which needs real interactive input handling. That gives you both
experiences and a fair comparison. Its dependency tree is substantial (~15 packages), so
if you'd rather keep Gecko dependency-light, skip it and hand-roll the timer — you now
know what that costs.

---

## D. Design

### `internal/terminal`

```
internal/terminal/
  detect.go        # TTY, colour depth, width — unconstrained
  detect_windows.go # VT mode + UTF-8 code page enabling
  color.go         # Style type and rendering
  progress.go      # progress bars and spinners
  prompt.go        # confirm, select, input
  raw.go           # raw mode with guaranteed restoration
```

### The colour API

Not this:

```go
fmt.Println(terminal.Red("error"))   // decides globally, untestable
```

This:

```go
type Styler struct {
    enabled bool
    depth   ColorDepth
}

func (s *Styler) Red(text string) string
func (s *Styler) Bold(text string) string
```

A value, constructed once from the environment and passed down. Same reasoning as
chapter 1's `Env`: global state defeats parallel tests, and a `Styler{enabled: false}` in
a test asserts on plain strings.

Put it on `Env`:

```go
type Env struct {
    ...
    Style *terminal.Styler
}
```

### Style composition

```go
s.Bold(s.Red("error"))
// "\x1b[1m" + "\x1b[31m" + "error" + "\x1b[0m" + "\x1b[0m"
```

Nested resets. The inner `\x1b[0m` cancels the bold before the outer reset arrives, so
"bold red" renders as red. This is the classic bug.

Two fixes: use specific resets (`\x1b[22m` cancels bold, `\x1b[39m` cancels foreground),
or build a style as a set of attributes applied in one sequence:

```go
s.New().Bold().Fg(Red).Render("error")   // "\x1b[1;31merror\x1b[0m"
```

The second is cleaner and is what `lipgloss` does. Build it.

---

## E. Implementation

### `internal/terminal/detect.go`

```go
// Package terminal handles colour, cursor control and interactive input.
//
// Everything here degrades to plain text when the output is not a
// terminal. A tool that writes escape sequences into a pipe or a CI log
// is broken, so the detection below is conservative: colour is opt-out
// on a real terminal and opt-in everywhere else.
package terminal

import (
	"os"
	"strings"

	"golang.org/x/term"
)

type ColorDepth int

const (
	NoColor ColorDepth = iota
	Color16
	Color256
	ColorTrue
)

// Detect decides how much colour to use for f.
//
// The order of these checks is deliberate and follows the de facto
// conventions:
//
//  1. An explicit flag always wins.
//  2. NO_COLOR disables colour if it is *set at all*, regardless of
//     value, per the no-color.org convention.
//  3. FORCE_COLOR / CLICOLOR_FORCE enable colour even when piped. CI
//     systems capture output through a pipe but still render ANSI, so
//     without this every CI log would be monochrome.
//  4. TERM=dumb means a terminal that cannot render escapes.
//  5. Otherwise, colour only on a real terminal.
func Detect(f *os.File, getenv func(string) string, forceOn, forceOff bool) ColorDepth {
	if forceOff {
		return NoColor
	}
	if _, set := os.LookupEnv("NO_COLOR"); set && !forceOn {
		return NoColor
	}
	if getenv("TERM") == "dumb" {
		return NoColor
	}

	forced := forceOn ||
		getenv("FORCE_COLOR") != "" ||
		getenv("CLICOLOR_FORCE") == "1"

	if !forced && !IsTerminal(f) {
		return NoColor
	}

	switch {
	case strings.Contains(strings.ToLower(getenv("COLORTERM")), "truecolor"),
		strings.Contains(strings.ToLower(getenv("COLORTERM")), "24bit"):
		return ColorTrue
	case strings.Contains(getenv("TERM"), "256color"):
		return Color256
	case forced:
		// Forced but with no capability hints: 16 colours is the safe
		// assumption, since every ANSI-capable terminal supports them.
		return Color16
	default:
		return Color16
	}
}

// IsTerminal reports whether f is attached to a character device.
func IsTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// Width returns the terminal width in columns, or 80 when unknown.
//
// COLUMNS is checked first because it is how a user overrides the
// detected width, and because it is set in some CI environments where
// the ioctl fails.
func Width(f *os.File, getenv func(string) string) int {
	if v := getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}
```

### `internal/terminal/detect_windows.go`

```go
//go:build windows

package terminal

import (
	"os"

	"golang.org/x/sys/windows"
)

// init enables ANSI processing and UTF-8 output on Windows consoles.
//
// Before Windows 10 1511 the console did not interpret escape sequences
// at all; since then it does, but only when
// ENABLE_VIRTUAL_TERMINAL_PROCESSING is set on the handle. Windows
// Terminal and PowerShell 7 set it themselves; legacy conhost does not.
//
// The console output code page is set to UTF-8 as well, without which
// box-drawing characters and other non-ASCII output render as mojibake.
//
// Every call here is best-effort: failure means the handle is a pipe or
// a legacy console, and Detect will fall back to no colour.
func init() {
	enableVT(os.Stdout)
	enableVT(os.Stderr)
	_ = windows.SetConsoleOutputCP(windows.CP_UTF8)
}

func enableVT(f *os.File) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return // not a console
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
```

A matching `detect_other.go` with `//go:build !windows` and an empty `init` keeps the
build symmetric. (Or omit it — an absent function is fine if nothing calls it. Symmetry is
easier to read.)

### `internal/terminal/color.go`

```go
package terminal

import (
	"fmt"
	"strconv"
	"strings"
)

type Color uint8

const (
	Black Color = iota
	Red
	Green
	Yellow
	Blue
	Magenta
	Cyan
	White
)

// Style accumulates attributes and renders them as a single escape
// sequence.
//
// Composing by nesting (bold(red(s))) produces "\x1b[1m\x1b[31ms\x1b[0m\x1b[0m",
// where the inner reset cancels the bold before the outer one arrives —
// so "bold red" renders as plain red. Accumulating attributes and
// emitting one sequence avoids the whole class of problem.
type Style struct {
	styler *Styler
	attrs  []string
}

type Styler struct {
	depth ColorDepth
}

func NewStyler(depth ColorDepth) *Styler { return &Styler{depth: depth} }

func (s *Styler) Enabled() bool { return s.depth != NoColor }

func (s *Styler) New() *Style { return &Style{styler: s} }

func (st *Style) Bold() *Style      { return st.add("1") }
func (st *Style) Dim() *Style       { return st.add("2") }
func (st *Style) Italic() *Style    { return st.add("3") }
func (st *Style) Underline() *Style { return st.add("4") }

func (st *Style) Fg(c Color) *Style { return st.add(strconv.Itoa(30 + int(c))) }
func (st *Style) Bg(c Color) *Style { return st.add(strconv.Itoa(40 + int(c))) }

// FgRGB sets a 24-bit colour, degrading to the nearest 256-colour index
// or the nearest basic colour when the terminal cannot do better.
func (st *Style) FgRGB(r, g, b uint8) *Style {
	switch st.styler.depth {
	case ColorTrue:
		return st.add(fmt.Sprintf("38;2;%d;%d;%d", r, g, b))
	case Color256:
		return st.add(fmt.Sprintf("38;5;%d", rgbTo256(r, g, b)))
	case Color16:
		return st.add(strconv.Itoa(30 + int(rgbTo16(r, g, b))))
	default:
		return st
	}
}

func (st *Style) add(a string) *Style {
	st.attrs = append(st.attrs, a)
	return st
}

// Render wraps s in this style, or returns it unchanged when colour is
// disabled.
func (st *Style) Render(s string) string {
	if st.styler.depth == NoColor || len(st.attrs) == 0 {
		return s
	}
	return "\x1b[" + strings.Join(st.attrs, ";") + "m" + s + "\x1b[0m"
}

// Convenience helpers for the common cases.
func (s *Styler) Error(t string) string   { return s.New().Bold().Fg(Red).Render(t) }
func (s *Styler) Success(t string) string { return s.New().Fg(Green).Render(t) }
func (s *Styler) Warn(t string) string    { return s.New().Fg(Yellow).Render(t) }
func (s *Styler) Dim(t string) string     { return s.New().Dim().Render(t) }

// rgbTo256 maps a colour into the xterm 256-colour palette: a 6×6×6
// colour cube at indices 16-231 plus a 24-step greyscale ramp at
// 232-255. Greys are handled separately because the cube's grey
// diagonal is coarse.
func rgbTo256(r, g, b uint8) uint8 {
	if r == g && g == b {
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		return uint8(232 + (int(r)-8)*24/247)
	}
	return uint8(16 +
		36*(int(r)*5/255) +
		6*(int(g)*5/255) +
		(int(b) * 5 / 255))
}
```

### `internal/terminal/raw.go`

```go
package terminal

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"
)

// Session owns temporary changes to terminal state and guarantees they
// are undone.
//
// A program that hides the cursor, disables echo or switches to the
// alternate screen and then exits without restoring leaves the user with
// an apparently frozen shell that they must fix by typing "reset" blind.
// Restoration therefore happens on four paths: normal return, panic,
// SIGTERM/SIGHUP, and explicit Close.
type Session struct {
	in       *os.File
	out      io.Writer
	oldState *term.State
	once     sync.Once
	sigCh    chan os.Signal
	altScreen bool
}

func NewSession(in *os.File, out io.Writer) *Session {
	return &Session{in: in, out: out}
}

// EnterRaw puts the terminal into raw mode: no line buffering, no echo,
// and Ctrl-C delivered as byte 0x03 rather than SIGINT.
func (s *Session) EnterRaw() error {
	if !IsTerminal(s.in) {
		return fmt.Errorf("stdin is not a terminal")
	}
	st, err := term.MakeRaw(int(s.in.Fd()))
	if err != nil {
		return err
	}
	s.oldState = st

	// SIGINT is no longer delivered in raw mode, but SIGTERM and SIGHUP
	// still are — a closed terminal window sends SIGHUP, and killing
	// the process sends SIGTERM. Both must restore before exiting.
	s.sigCh = make(chan os.Signal, 1)
	signal.Notify(s.sigCh, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if _, ok := <-s.sigCh; !ok {
			return
		}
		s.Close()
		os.Exit(130)
	}()
	return nil
}

// EnterAltScreen switches to the alternate buffer, so the user's
// scrollback is untouched when the program exits.
func (s *Session) EnterAltScreen() {
	fmt.Fprint(s.out, "\x1b[?1049h\x1b[H")
	s.altScreen = true
}

func (s *Session) HideCursor() { fmt.Fprint(s.out, "\x1b[?25l") }
func (s *Session) ShowCursor() { fmt.Fprint(s.out, "\x1b[?25h") }
func (s *Session) Clear()      { fmt.Fprint(s.out, "\x1b[2J\x1b[H") }

// Close restores everything. It is idempotent, because it is reached
// from several paths that can overlap.
func (s *Session) Close() {
	s.once.Do(func() {
		if s.sigCh != nil {
			signal.Stop(s.sigCh)
			close(s.sigCh)
		}
		s.ShowCursor()
		if s.altScreen {
			fmt.Fprint(s.out, "\x1b[?1049l")
		}
		if s.oldState != nil {
			_ = term.Restore(int(s.in.Fd()), s.oldState)
		}
	})
}

// Guard restores the terminal if fn panics, then re-panics so the
// failure is not swallowed. Without this, a nil dereference inside a
// TUI leaves the terminal unusable and the stack trace unreadable.
func (s *Session) Guard(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.Close()
			panic(r)
		}
	}()
	defer s.Close()
	return fn()
}
```

### `internal/terminal/progress.go`

```go
package terminal

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Bar renders a single-line progress bar in place.
//
// Updates are rate-limited: a loop calling Update a million times would
// otherwise spend more time writing escape sequences than doing work,
// and would flood a slow terminal. 30 Hz is above the threshold where a
// human perceives smoothness.
type Bar struct {
	w         io.Writer
	total     int64
	width     int
	enabled   bool
	label     string

	mu       sync.Mutex
	current  int64
	lastDraw time.Time
	start    time.Time
}

const minRedrawInterval = 33 * time.Millisecond

func NewBar(w io.Writer, total int64, width int, enabled bool) *Bar {
	return &Bar{w: w, total: total, width: width, enabled: enabled, start: time.Now()}
}

func (b *Bar) Update(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = n
	if !b.enabled {
		return
	}
	if time.Since(b.lastDraw) < minRedrawInterval && n < b.total {
		return
	}
	b.lastDraw = time.Now()
	b.draw()
}

func (b *Bar) draw() {
	pct := 0.0
	if b.total > 0 {
		pct = float64(b.current) / float64(b.total)
	}
	filled := int(pct * float64(b.width))

	bar := strings.Repeat("█", filled) + strings.Repeat("░", b.width-filled)

	// \r returns to column 0; \x1b[K clears to end of line, which is
	// required because a shorter line would otherwise leave characters
	// from the previous, longer render.
	fmt.Fprintf(b.w, "\r\x1b[K%s [%s] %3.0f%% %s",
		b.label, bar, pct*100, b.eta())
}

// Finish leaves the completed bar on screen and moves to a new line, so
// subsequent output does not overwrite it.
func (b *Bar) Finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled {
		return
	}
	b.current = b.total
	b.draw()
	fmt.Fprintln(b.w)
}
```

### A fun command: `gecko fun matrix`

```go
// Matrix renders the falling-characters effect.
//
// The implementation is deliberately hand-rolled rather than built on a
// TUI framework: it needs no input handling, so all it requires is the
// alternate screen, cursor positioning and a ticker — which is exactly
// enough to see what a framework would be doing for you.
func Matrix(ctx context.Context, s *Session, w io.Writer, width, height int) error {
	s.EnterAltScreen()
	s.HideCursor()
	defer s.Close()

	// One falling column per screen column, each at its own speed and
	// phase so the effect does not look synchronised.
	type column struct {
		y      int
		speed  int
		length int
		tick   int
	}
	cols := make([]column, width)
	for i := range cols {
		cols[i] = column{
			y:      rand.IntN(height) - height,
			speed:  1 + rand.IntN(3),
			length: 5 + rand.IntN(15),
		}
	}

	const charset = "ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄ0123456789"
	runes := []rune(charset)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var buf bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		// Render the whole frame into a buffer and write once. Writing
		// per-cell would issue thousands of syscalls per frame and the
		// animation would visibly tear.
		buf.Reset()
		buf.WriteString("\x1b[H")

		for x := range cols {
			c := &cols[x]
			c.tick++
			if c.tick%c.speed != 0 {
				continue
			}
			c.y++
			if c.y-c.length > height {
				c.y = -rand.IntN(height)
				c.speed = 1 + rand.IntN(3)
			}
			for i := 0; i < c.length; i++ {
				y := c.y - i
				if y < 0 || y >= height {
					continue
				}
				ch := runes[rand.IntN(len(runes))]
				switch i {
				case 0:
					fmt.Fprintf(&buf, "\x1b[%d;%dH\x1b[97m%c", y+1, x+1, ch)
				default:
					shade := 255 - (i * 200 / c.length)
					fmt.Fprintf(&buf, "\x1b[%d;%dH\x1b[38;2;0;%d;0m%c", y+1, x+1, shade, ch)
				}
			}
		}
		buf.WriteString("\x1b[0m")
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
	}
}
```

`math/rand/v2` (`rand.IntN`) is Go 1.22+; on older versions use `rand.Intn`. v2 is
faster, has a better generator, and doesn't need seeding.

---

## F. Exercise

1. Implement `gecko fun color`. Parse `#RGB`, `#RRGGBB`, `rgb(r,g,b)` and named CSS
   colours. Output the swatch, all format conversions, and the nearest ANSI-256 index.
   RGB→HSL is a short algorithm worth writing once.

2. Implement `gecko fun ascii "TEXT"` with a built-in 5×7 bitmap font embedded via
   `go:embed`. Rendering the bitmap is easy; laying out variable-width glyphs with correct
   kerning is the interesting part.

3. Implement `gecko fun timer` with Bubble Tea, then again by hand. Compare line counts
   and, more importantly, compare what happens when you resize the terminal mid-run and
   when you press Ctrl-C. Write up which you'd ship.

4. Add `--color=auto|always|never` as a global flag, and verify every command respects it.
   Then run `gecko doctor | cat` and check the output byte-for-byte for escape sequences.

5. **Interactive prompts.** Implement a `Select` prompt (arrow keys, Enter). Then handle:
   the terminal being resized mid-prompt, stdin being a pipe (fall back to a numbered
   list read line-wise), and Ctrl-C (restore and exit 130).

---

## G. Testing

### Colour detection is a pure function, so table-test it

```go
func TestDetect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      map[string]string
		isTTY    bool
		forceOn  bool
		forceOff bool
		want     ColorDepth
	}{
		{"tty with 256color", map[string]string{"TERM": "xterm-256color"}, true, false, false, Color256},
		{"tty with truecolor", map[string]string{"COLORTERM": "truecolor"}, true, false, false, ColorTrue},
		{"piped output", map[string]string{"TERM": "xterm-256color"}, false, false, false, NoColor},
		{"piped but FORCE_COLOR", map[string]string{"FORCE_COLOR": "1"}, false, false, false, Color16},
		{"NO_COLOR set empty still disables", map[string]string{"NO_COLOR": ""}, true, false, false, NoColor},
		{"NO_COLOR beats TERM", map[string]string{"NO_COLOR": "1", "TERM": "xterm-256color"}, true, false, false, NoColor},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, true, false, false, NoColor},
		{"--no-color beats everything", map[string]string{"FORCE_COLOR": "1"}, true, false, true, NoColor},
	}
	...
}
```

The "NO_COLOR set empty still disables" case is the one people get wrong. `os.Getenv`
returns `""` for both unset and set-empty, so you **must** use `os.LookupEnv`. That test
catches it.

### Assert on bytes, not on appearance

```go
func TestStyleComposition(t *testing.T) {
	t.Parallel()
	s := NewStyler(Color16)

	got := s.New().Bold().Fg(Red).Render("error")
	want := "\x1b[1;31merror\x1b[0m"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// The composition bug this design prevents:
	if strings.Count(got, "\x1b[0m") != 1 {
		t.Errorf("multiple resets in %q; nested styling has crept back in", got)
	}
}

func TestNoColorProducesPlainText(t *testing.T) {
	t.Parallel()
	s := NewStyler(NoColor)
	got := s.New().Bold().Fg(Red).Render("error")
	if got != "error" {
		t.Errorf("got %q, want plain text", got)
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Error("escape sequence leaked into no-colour output")
	}
}
```

### The pipe-safety test, applied to every command

```go
// TestNoEscapeSequencesWhenPiped runs every command with a non-terminal
// stdout and asserts that no ANSI escapes reach the output. This is a
// single test that protects every present and future command.
func TestNoEscapeSequencesWhenPiped(t *testing.T) {
	commands := [][]string{
		{"version"},
		{"doctor"},
		{"tree", "--depth", "1"},
		{"config", "show"},
		{"help"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			env, out, errOut := testEnv(nil) // buffers, never a TTY
			Main(context.Background(), args, env)

			for name, buf := range map[string]*bytes.Buffer{"stdout": out, "stderr": errOut} {
				if bytes.ContainsRune(buf.Bytes(), '\x1b') {
					t.Errorf("%s contains an escape sequence when not a terminal:\n%q",
						name, buf.String())
				}
			}
		})
	}
}
```

This is the highest-value test in the chapter. It's ten lines and it catches the entire
class of bug, forever, for every command anyone adds later.

### Terminal restoration

You cannot easily assert on termios state from a test, but you can assert the escape
sequences:

```go
func TestSessionRestoresOnClose(t *testing.T) {
	var buf bytes.Buffer
	s := NewSession(os.Stdin, &buf)
	s.EnterAltScreen()
	s.HideCursor()
	s.Close()

	out := buf.String()
	if !strings.Contains(out, "\x1b[?25h") {
		t.Error("cursor was not shown again")
	}
	if !strings.Contains(out, "\x1b[?1049l") {
		t.Error("alternate screen was not exited")
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	s := NewSession(os.Stdin, &buf)
	s.HideCursor()
	s.Close()
	first := buf.Len()
	s.Close()
	s.Close()
	if buf.Len() != first {
		t.Error("Close wrote again on a second call; sync.Once is not doing its job")
	}
}

func TestGuardRestoresOnPanic(t *testing.T) {
	var buf bytes.Buffer
	s := NewSession(os.Stdin, &buf)
	s.HideCursor()

	defer func() {
		if r := recover(); r == nil {
			t.Error("Guard swallowed the panic")
		}
		if !strings.Contains(buf.String(), "\x1b[?25h") {
			t.Error("terminal not restored before the panic propagated")
		}
	}()

	s.Guard(func() error { panic("boom") })
}
```

---

## H. Review

- The colour decision chain, and why `NO_COLOR` needs `LookupEnv` not `Getenv`.
- Why `FORCE_COLOR` exists (CI captures through a pipe but renders ANSI).
- Checking stdout and stderr independently.
- Windows VT mode and UTF-8 code page, both opt-in.
- Raw mode via `x/term`, and the four restoration paths — defer, panic, signal, explicit.
- Why `sync.Once` makes restoration idempotent and why that's required, not tidy.
- Style accumulation vs nesting, and the reset-cancellation bug.
- The alternate screen buffer and when to use it.
- Rate-limiting redraws; `\r` + `\x1b[K` for in-place updates.
- Display width ≠ rune count ≠ byte count.
- What a TUI framework actually buys you (terminal handling, not the architecture).

---

## I. Refactoring

Every command currently formats its own output. Now that `Styler` exists, they all need
it, which means either threading it through every call or putting it on `Env`.

Put it on `Env` — but note this is the third thing added to `Env` (config, log, style).
`Env` is becoming the thing chapter 1 warned about: a grab bag.

Is it? The test: **does everything on `Env` share the property of being ambient process
state that a test needs to fake?** Stdin/stdout/stderr: yes. Getenv and WorkDir: yes.
Config, logger, styler: yes — all three are derived from the environment and all three
need faking. So it's cohesive after all; it's an explicit dependency-injection container,
not a junk drawer.

The line would be crossed by putting, say, an HTTP client on it — that's a *capability* a
specific command needs, not ambient state. Write that rule into `Env`'s doc comment so the
next person applies the same test:

```go
// Env carries ambient process state that commands read and tests must
// be able to fake: the standard streams, the environment, the working
// directory, and the configuration, logger and styler derived from
// them.
//
// It is not a service locator. Capabilities used by particular commands
// (HTTP clients, database handles, plugin registries) are constructed by
// those commands, not carried here.
type Env struct { ... }
```

---

## Commit

```
feat: add terminal package with colour detection and styling
feat: enable virtual terminal processing and UTF-8 on windows
feat: add raw-mode session with guaranteed restoration
feat: add progress bars and interactive prompts
feat: add fun commands (matrix, color, ascii, timer)
test: assert no escape sequences reach non-terminal output
```

Next: `13-plugins.md`.

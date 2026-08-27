# Chapter 11 — `gecko ports` and `gecko processes`: Platform Engineering

```
Difficulty:   Expert
Est. time:    10–14 hours
Main concepts: build constraints in depth, /proc parsing, sysctl and syscall on Darwin,
               golang.org/x/sys/windows, unsafe.Pointer boundaries, cgo avoidance,
               the four-way platform classification, CI as your other machines
Prerequisites: Chapters 1–10
```

---

## A. Goal

```
$ gecko ports
PORT   PROTO  PROCESS      PID     USER    STATE
3000   tcp    node         18231   you     LISTEN
5432   tcp    postgres     912     postgres LISTEN
8080   tcp    gecko        19283   you     LISTEN
5353   udp    mDNSResponder 421    _mdns   -

$ gecko ports 8080 --json
$ gecko ports 8080 --kill
About to send SIGTERM to gecko (pid 19283, started 4m ago). Continue? [y/N]
```

---

## B. Why this matters

This is the chapter where "cross-platform" stops being a slogan. There is no standard
library API for "which process is listening on which port". The three operating systems
expose the information through three unrelated mechanisms:

- **Linux**: parse `/proc/net/tcp` for inode numbers, then scan `/proc/*/fd/*` symlinks to
  map inodes back to PIDs.
- **macOS**: no `/proc`. Either the `NET_RT_IFLIST`/`sysctl` route (which doesn't give
  socket-to-process mapping), the private `libproc` API via cgo, or shell out to `lsof`.
- **Windows**: `GetExtendedTcpTable` from `iphlpapi.dll`, which returns PIDs directly —
  the nicest of the three.

Working through this teaches you more about Go's platform story than any amount of reading
about build tags.

---

## C. Concepts

### Build constraints, properly

Two mechanisms, both active simultaneously.

**Filename suffixes** — automatic, no comment needed:

```
ports_linux.go        GOOS=linux
ports_darwin.go       GOOS=darwin
ports_windows.go      GOOS=windows
ports_unix.go         any Unix-like GOOS (Go 1.19+ 'unix' build tag)
ports_linux_amd64.go  GOOS=linux AND GOARCH=amd64
ports_test.go         test file (not a constraint)
ports_linux_test.go   test file, linux only
```

The pattern is `name_GOOS.go`, `name_GOARCH.go`, or `name_GOOS_GOARCH.go`. Note the trap:
a file called `windows.go` is **not** constrained — the suffix must follow an underscore
and there must be a prefix. `_windows.go` (no prefix) is ignored by the build entirely,
which is a fun way to lose an afternoon.

**Build comments** — explicit, more expressive:

```go
//go:build linux || darwin || freebsd
//go:build !windows && !js
//go:build linux && amd64
//go:build cgo
//go:build go1.23
//go:build ignore
```

Must appear before the package clause, followed by a blank line. The old `// +build`
syntax was removed in Go 1.18 tooling's default behaviour; `gofmt` will add the new form
if it sees the old one.

Use both together: filename for the primary discriminator, comment for anything more
complex. Writing `//go:build windows` at the top of `ports_windows.go` is redundant but
good practice — it survives a rename and it's visible to a reader.

**Custom tags:**

```go
//go:build gecko_slowpath
```
```bash
go build -tags gecko_slowpath ./...
```

Useful for opt-in features. Don't overuse — every tag doubles the configuration space CI
must cover.

### The four-way classification

The brief asks when code should be portable, abstracted, split, or reimplemented. Here's
the decision procedure, with examples from this project:

**1. Fully portable — no platform code at all.**
When the standard library already abstracts it. `filepath.Join`, `os.ReadFile`,
`net.Listen`. Chapter 2's `tree` is entirely in this category.
*Test: does anything in the code mention an OS?* If no, you're done.

**2. Runtime branch — one file, `switch runtime.GOOS`.**
When the shape is identical and only a string or a small parameter differs, **and no
platform-specific imports are needed**. Chapter 3's `userDataDir` and chapter 7's
`OpenBrowser`.
*Test: would splitting into files duplicate more than it clarifies?* Three-line
differences don't justify three files.

**3. Build-tag split — same API, different files, different imports.**
When implementations need different imports or genuinely different algorithms. Chapter
9's process groups (`syscall` vs `x/sys/windows`) and this chapter's port enumeration.
*Test: does one platform need an import another can't compile?* If yes, you have no
choice — a file importing `golang.org/x/sys/windows` will not compile on Linux.

**4. Interface — runtime-selected implementations.**
When the *selection* is dynamic rather than compile-time. Chapter 9's poll-vs-fsnotify
choice, because a Linux machine might need either.
*Test: can more than one implementation be valid on the same machine?* If yes, an
interface; if no, build tags.

The most common mistake is jumping to 4 when 3 suffices. An interface with exactly one
implementation per platform is a build-tag split with extra indirection and a vtable
lookup.

### `/proc` on Linux

`/proc/net/tcp` (IPv4) and `/proc/net/tcp6`:

```
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 42891
```

Fields that matter:
- `local_address`: `IP:PORT` in **hex**, and the IP is **little-endian byte-reversed** on
  little-endian machines. `0100007F` is `7F.00.00.01` = 127.0.0.1. The port `1F90` is
  8080, big-endian. Mixed endianness in one field — a genuine wart.
- `st`: state. `0A` = LISTEN, `01` = ESTABLISHED.
- `inode`: the socket's inode number. This is the join key.

To get the PID you scan `/proc/[pid]/fd/`, where socket fds are symlinks to
`socket:[42891]`. Match the inode.

This is O(processes × fds), typically a few thousand `readlink` calls. It needs no
privileges for your own processes; other users' `/proc/[pid]/fd` is unreadable, so you get
a partial view unless root. **Report what you can see and note the limitation** — silently
showing an incomplete list is worse than saying "3 sockets belong to other users".

IPv6 in `/proc/net/tcp6` uses a different byte ordering again: 32-bit words each
byte-reversed. Test it.

### macOS: the awkward one

No `/proc`. The options:

1. **`libproc` via cgo** — `proc_pidfdinfo` with `PROC_PIDFDSOCKETINFO` gives exactly what
   we want. But it requires cgo, which means: no easy cross-compilation, a C toolchain in
   CI, slower builds, and `CGO_ENABLED=0` static binaries become impossible.
2. **Shell out to `lsof -iTCP -sTCP:LISTEN -P -n`** — works, present on every macOS,
   parseable. Costs ~100 ms and depends on output format.
3. **Raw syscall to `proc_pidinfo`** — the syscall is `SYS_PROC_INFO` (336). Callable via
   `syscall.Syscall6` with `unsafe.Pointer` to manually-defined structs. No cgo, but you
   are hand-marshalling C structs and any layout mistake is memory corruption.
4. **`gopsutil`** — a mature library that does all of this.

**Decision: `lsof` on Darwin, with a clear error if absent.**

The reasoning is worth stating carefully, because option 3 is tempting:

- cgo (option 1) would compromise the entire project's build story for one command. That's
  a bad trade — `CGO_ENABLED=0` static binaries are a major reason to write CLI tools in
  Go.
- Option 3 means defining `struct socket_fdinfo` in Go with exact field offsets, which
  Apple can change between releases and which I cannot test on every macOS version. A
  layout error is a crash or silent garbage, not a compile error.
- `lsof` is preinstalled on macOS, stable, and its failure mode (missing binary) is
  detectable and reportable.

**This is a case where the "worse" implementation is the right engineering call**, and
being able to articulate why is more valuable than the extra 90 ms.

Note the asymmetry: we're using `x/sys/windows` for direct syscalls on Windows but
shelling out on Darwin. That's not inconsistency — it's because `x/sys/windows` is a
maintained, tested binding of a documented stable API, while Darwin's equivalent is an
undocumented private interface with no maintained binding.

### Windows: `x/sys/windows`

```bash
go get golang.org/x/sys
```

`GetExtendedTcpTable` from `iphlpapi.dll` returns a `MIB_TCPTABLE_OWNER_PID`: a count
followed by a variable-length array of rows, each with local address, port, state and
**owning PID**. Directly what we need.

The Go binding pattern:

```go
var (
    modiphlpapi              = windows.NewLazySystemDLL("iphlpapi.dll")
    procGetExtendedTcpTable  = modiphlpapi.NewProc("GetExtendedTcpTable")
)
```

`NewLazySystemDLL` (not `NewLazyDLL`) is important for security: it loads only from the
system directory, preventing DLL-planting attacks where a malicious `iphlpapi.dll` in the
current directory gets loaded instead.

The call requires a two-pass buffer-sizing dance:

```go
var size uint32
// First call with a nil buffer returns ERROR_INSUFFICIENT_BUFFER and sets size.
ret, _, _ := procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, ...)
buf := make([]byte, size)
ret, _, _ = procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), ...)
```

**The race**: between the two calls the table can grow, and the second call then also
returns `ERROR_INSUFFICIENT_BUFFER`. Loop with a retry limit; don't assume one resize is
enough.

### `unsafe.Pointer` rules

Reading the returned table means casting a `[]byte` to a struct. The rules
(`unsafe.Pointer` documentation, patterns 1–6) that matter here:

- Converting `*T1` → `unsafe.Pointer` → `*T2` is valid only when T2's layout is a prefix
  of, or identical to, T1's *and* alignment is satisfied.
- Pointer arithmetic must go through `uintptr` **in a single expression**:
  `unsafe.Pointer(uintptr(p) + offset)`. Splitting it across statements is a bug — the GC
  may move or collect the object while only a `uintptr` (not a pointer) refers to it.
  `go vet` catches the obvious cases; it does not catch all of them.
- `unsafe.Slice(ptr, len)` (Go 1.17+) is the correct way to view a C array as a Go slice,
  replacing the old `reflect.SliceHeader` hack which is now explicitly discouraged.

Run `go vet` with the `unsafeptr` check on and, if you can, `GOEXPERIMENT` checkptr via
`-race` — the race detector also enables pointer-validity checks.

### Testing platform code you can't run

You have one machine. Three strategies:

**1. Extract the pure part.** Parsing `/proc/net/tcp` is string processing. Put it in a
function taking an `io.Reader`, test it with fixtures on any OS:

```go
//go:build linux   ← DON'T do this on the parser

func parseProcNetTCP(r io.Reader) ([]Socket, error)   // testable everywhere
```

Only the file-opening wrapper needs the build tag. **Push the platform boundary as close
to the syscall as possible** and everything above it becomes portable and testable. This
is the single most valuable technique in the chapter.

**2. Golden fixtures.** Commit real `/proc/net/tcp` and `lsof` output captured from each
platform to `testdata/`. Your Linux CI then tests the Darwin parser.

**3. CI as your other machines.** GitHub Actions gives you `ubuntu-latest`,
`macos-latest` and `windows-latest` free for public repos. Set it up **now**, not in
chapter 15.

---

## E. Implementation

### The shared API — `internal/platform/ports.go` (no build tag)

```go
// Package platform contains Gecko's operating-system-specific code.
//
// Each capability is declared here with a portable API and implemented
// in files constrained by build tag. Parsing and formatting live in
// unconstrained files so they can be tested on any host: the build
// boundary is pushed as close to the syscall as possible.
package platform

import (
	"context"
	"errors"
	"net"
)

// ErrUnsupported reports that a capability is unavailable on this
// platform or in this environment.
var ErrUnsupported = errors.New("not supported on this platform")

type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

type SocketState string

const (
	StateListen      SocketState = "LISTEN"
	StateEstablished SocketState = "ESTABLISHED"
	StateOther       SocketState = "OTHER"
)

// Socket is one listening or connected socket, with its owning process
// where that could be determined.
type Socket struct {
	Protocol   Protocol
	LocalAddr  net.IP
	LocalPort  int
	RemoteAddr net.IP
	RemotePort int
	State      SocketState
	PID        int    // 0 when unknown
	Process    string // empty when unknown
	User       string
}

// ListenersOptions filters the result.
type ListenersOptions struct {
	Port          int  // 0 = all
	IncludeUDP    bool
	IncludeRemote bool // include non-listening sockets
}

// Listeners returns sockets on the local machine.
//
// The result may be incomplete without elevated privileges: on Linux and
// macOS, sockets owned by other users cannot be attributed to a process.
// Implementations report what they can see and set Incomplete rather
// than failing, because a partial answer is useful and a silent partial
// answer is not.
func Listeners(ctx context.Context, opts ListenersOptions) ([]Socket, bool, error) {
	return listeners(ctx, opts) // implemented per platform
}
```

### Linux — the parser, unconstrained

`internal/platform/procnet.go` — **no build tag**, so it compiles and tests everywhere:

```go
package platform

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// procSocket is one row of /proc/net/tcp before process attribution.
type procSocket struct {
	LocalIP    net.IP
	LocalPort  int
	RemoteIP   net.IP
	RemotePort int
	State      SocketState
	UID        int
	Inode      uint64
}

// tcpStates maps the hex state codes used by /proc/net/tcp. They are the
// kernel's TCP_* enum, not the values in <netinet/tcp.h>.
var tcpStates = map[string]SocketState{
	"01": StateEstablished,
	"0A": StateListen,
}

// parseProcNet parses /proc/net/tcp, tcp6, udp or udp6.
//
// This function is deliberately not build-constrained: it is pure string
// processing and can therefore be tested on macOS and Windows against
// captured fixtures. Only the code that opens the file is Linux-only.
func parseProcNet(r io.Reader, ipv6 bool) ([]procSocket, error) {
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return nil, fmt.Errorf("empty /proc/net table")
	} // discard the header

	var out []procSocket
	line := 1
	for sc.Scan() {
		line++
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}

		lip, lport, err := parseHexAddr(fields[1], ipv6)
		if err != nil {
			return nil, fmt.Errorf("line %d: local address: %w", line, err)
		}
		rip, rport, err := parseHexAddr(fields[2], ipv6)
		if err != nil {
			return nil, fmt.Errorf("line %d: remote address: %w", line, err)
		}

		state, ok := tcpStates[strings.ToUpper(fields[3])]
		if !ok {
			state = StateOther
		}
		uid, _ := strconv.Atoi(fields[7])
		inode, _ := strconv.ParseUint(fields[9], 10, 64)

		out = append(out, procSocket{
			LocalIP: lip, LocalPort: lport,
			RemoteIP: rip, RemotePort: rport,
			State: state, UID: uid, Inode: inode,
		})
	}
	return out, sc.Err()
}

// parseHexAddr decodes an address of the form "0100007F:1F90".
//
// The encoding mixes endianness, which is the single most surprising
// thing about /proc/net/tcp:
//
//   - The address is written as the raw in-memory bytes of the kernel's
//     32-bit (or 4x32-bit) representation, so on a little-endian machine
//     "0100007F" is the bytes 01 00 00 7F read as a little-endian u32,
//     giving 127.0.0.1.
//   - The port is plain big-endian hex: "1F90" is 8080.
//
// IPv6 is four 32-bit words, each individually byte-reversed.
func parseHexAddr(s string, ipv6 bool) (net.IP, int, error) {
	host, portStr, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, fmt.Errorf("malformed address %q", s)
	}
	port64, err := strconv.ParseUint(portStr, 16, 32)
	if err != nil {
		return nil, 0, fmt.Errorf("port %q: %w", portStr, err)
	}

	raw, err := hexBytes(host)
	if err != nil {
		return nil, 0, err
	}

	if !ipv6 {
		if len(raw) != 4 {
			return nil, 0, fmt.Errorf("expected 4 bytes, got %d", len(raw))
		}
		v := binary.LittleEndian.Uint32(raw)
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, v)
		return ip, int(port64), nil
	}

	if len(raw) != 16 {
		return nil, 0, fmt.Errorf("expected 16 bytes, got %d", len(raw))
	}
	ip := make(net.IP, 16)
	for i := 0; i < 4; i++ {
		w := binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
		binary.BigEndian.PutUint32(ip[i*4:i*4+4], w)
	}
	return ip, int(port64), nil
}
```

Note that `parseProcNet` has no build tag. On a Mac, `go test ./internal/platform` runs
this parser against fixtures. **That's the technique: the platform boundary is the file
open, not the logic.**

### Linux — the constrained part

`internal/platform/ports_linux.go`:

```go
//go:build linux

package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func listeners(ctx context.Context, opts ListenersOptions) ([]Socket, bool, error) {
	var socks []procSocket

	tables := []struct {
		path string
		ipv6 bool
		prot Protocol
	}{
		{"/proc/net/tcp", false, TCP},
		{"/proc/net/tcp6", true, TCP},
	}
	if opts.IncludeUDP {
		tables = append(tables,
			struct {
				path string; ipv6 bool; prot Protocol
			}{"/proc/net/udp", false, UDP},
			struct {
				path string; ipv6 bool; prot Protocol
			}{"/proc/net/udp6", true, UDP})
	}

	protoOf := map[int]Protocol{}
	for _, tb := range tables {
		f, err := os.Open(tb.path)
		if err != nil {
			// tcp6 is absent when IPv6 is disabled; that is normal.
			if os.IsNotExist(err) {
				continue
			}
			return nil, false, fmt.Errorf("open %s: %w", tb.path, err)
		}
		parsed, err := parseProcNet(f, tb.ipv6)
		f.Close()
		if err != nil {
			return nil, false, err
		}
		for _, p := range parsed {
			protoOf[len(socks)] = tb.prot
			socks = append(socks, p)
		}
	}

	inodeToPID, incomplete := scanProcFDs(ctx)

	out := make([]Socket, 0, len(socks))
	for i, p := range socks {
		if !opts.IncludeRemote && p.State != StateListen {
			continue
		}
		if opts.Port != 0 && p.LocalPort != opts.Port {
			continue
		}
		s := Socket{
			Protocol: protoOf[i],
			LocalAddr: p.LocalIP, LocalPort: p.LocalPort,
			RemoteAddr: p.RemoteIP, RemotePort: p.RemotePort,
			State: p.State,
			User:  userName(p.UID),
		}
		if pid, ok := inodeToPID[p.Inode]; ok {
			s.PID = pid
			s.Process = processName(pid)
		}
		out = append(out, s)
	}
	return out, incomplete, nil
}

// scanProcFDs builds an inode → pid map by reading every process's file
// descriptor symlinks. Socket descriptors are symlinks with the target
// "socket:[12345]".
//
// Directories belonging to other users are unreadable without
// CAP_SYS_PTRACE or root, so the map is incomplete for an unprivileged
// caller. That is reported to the caller rather than hidden: a listing
// that silently omits other users' processes is misleading.
func scanProcFDs(ctx context.Context) (map[uint64]int, bool) {
	m := make(map[uint64]int, 256)
	incomplete := false

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return m, true
	}

	for _, e := range entries {
		if ctx.Err() != nil {
			return m, true
		}
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process directory
		}

		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// Almost always EACCES on another user's process.
			incomplete = true
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue // the fd closed between readdir and readlink
			}
			if !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inodeStr := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if inode, err := strconv.ParseUint(inodeStr, 10, 64); err == nil {
				m[inode] = pid
			}
		}
	}
	return m, incomplete
}

// processName reads the executable name from /proc/[pid]/comm, which is
// truncated to 15 characters. /proc/[pid]/cmdline has the full command
// but includes arguments; comm is the right choice for a table column.
func processName(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
```

### Darwin

`internal/platform/ports_darwin.go`:

```go
//go:build darwin

package platform

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/yourname/gecko/internal/process"
)

// listeners uses lsof.
//
// The alternatives were considered and rejected:
//
//   - libproc via cgo gives exactly the right data, but requiring cgo
//     would end CGO_ENABLED=0 static builds and simple cross-compilation
//     for the entire project. That is too high a price for one command.
//   - Calling proc_pidinfo directly through syscall.Syscall6 avoids cgo
//     but requires hand-defining Apple's C struct layouts in Go. Those
//     are private API, undocumented, and can change between macOS
//     releases; a layout error is silent memory corruption rather than a
//     compile failure.
//
// lsof is preinstalled on every macOS, its output format is stable, and
// its absence is detectable and reportable.
func listeners(ctx context.Context, opts ListenersOptions) ([]Socket, bool, error) {
	path, ok := process.Exists("lsof")
	if !ok {
		return nil, false, fmt.Errorf(
			"listing ports on macOS requires lsof, which was not found on PATH: %w",
			ErrUnsupported)
	}

	args := []string{"-nP", "-i"}
	if !opts.IncludeUDP {
		args = append(args, "-iTCP")
	}
	if !opts.IncludeRemote {
		args = append(args, "-sTCP:LISTEN")
	}
	args = append(args, "-F", "pcnPtu") // machine-readable field output

	res, err := process.Output(ctx, path, args...)
	if err != nil && res.ExitCode > 1 {
		// lsof exits 1 when it finds nothing, which is not an error.
		return nil, false, fmt.Errorf("lsof: %w", err)
	}

	socks, err := parseLsofFields(res.Stdout)
	if err != nil {
		return nil, false, err
	}
	// Without root, lsof cannot see other users' processes and prints a
	// warning to stderr rather than failing.
	incomplete := strings.Contains(res.Stderr, "Permission denied") ||
		strings.Contains(res.Stderr, "WARNING")
	return filterSockets(socks, opts), incomplete, nil
}
```

And `parseLsofFields` goes in an **unconstrained** file so it's testable on Linux CI.
`lsof -F` emits one field per line prefixed by a type character (`p` pid, `c` command,
`n` name, `P` protocol, `t` type, `u` uid), grouped per process. That format exists
precisely for programmatic consumption and is far more stable than the human table.

### Windows

`internal/platform/ports_windows.go`:

```go
//go:build windows

package platform

import (
	"context"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// NewLazySystemDLL, not NewLazyDLL: it resolves only from the
	// system directory, which prevents a DLL-planting attack where a
	// malicious iphlpapi.dll in the working directory would be loaded.
	modiphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modiphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable = modiphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	tcpTableOwnerPIDAll = 5
	afInet              = 2
	afInet6             = 23

	mibTCPStateListen = 2
)

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID from iphlpapi.h.
// Field order and sizes must match exactly; Go's struct layout for
// these uint32 fields is identical to the C layout on all supported
// architectures because every field is 4-byte aligned.
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32 // network byte order
	LocalPort  uint32 // low 16 bits, network byte order
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

func listeners(ctx context.Context, opts ListenersOptions) ([]Socket, bool, error) {
	rows, err := getTCPTable(afInet)
	if err != nil {
		return nil, false, err
	}
	// ... same again for afInet6 and, if requested, UDP.

	out := make([]Socket, 0, len(rows))
	for _, r := range rows {
		if !opts.IncludeRemote && r.State != mibTCPStateListen {
			continue
		}
		port := int(ntohs(uint16(r.LocalPort)))
		if opts.Port != 0 && port != opts.Port {
			continue
		}
		out = append(out, Socket{
			Protocol:  TCP,
			LocalAddr: uint32ToIP(r.LocalAddr),
			LocalPort: port,
			State:     tcpStateFromWindows(r.State),
			PID:       int(r.OwningPID),
			Process:   processName(int(r.OwningPID)),
		})
	}
	// Windows returns owning PIDs for all processes without elevation,
	// so unlike Linux and macOS the result is never partial.
	return out, false, nil
}

// getTCPTable calls GetExtendedTcpTable, sizing the buffer in a loop.
//
// The API requires a two-pass call: once with a nil buffer to learn the
// size, then again with a buffer of that size. Between the two calls the
// table can grow, so the second call can also fail with
// ERROR_INSUFFICIENT_BUFFER; retrying a bounded number of times is
// required for correctness, not merely defensive.
func getTCPTable(family uint32) ([]mibTCPRowOwnerPID, error) {
	var buf []byte
	var size uint32

	for attempt := 0; attempt < 5; attempt++ {
		var p unsafe.Pointer
		if len(buf) > 0 {
			p = unsafe.Pointer(&buf[0])
		}
		ret, _, _ := procGetExtendedTcpTable.Call(
			uintptr(p),
			uintptr(unsafe.Pointer(&size)),
			0, // bOrder = FALSE
			uintptr(family),
			uintptr(tcpTableOwnerPIDAll),
			0,
		)
		switch windows.Errno(ret) {
		case windows.ERROR_SUCCESS:
			return decodeTCPTable(buf[:size])
		case windows.ERROR_INSUFFICIENT_BUFFER:
			buf = make([]byte, size)
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", windows.Errno(ret))
		}
	}
	return nil, fmt.Errorf("GetExtendedTcpTable: table kept growing after 5 attempts")
}

// decodeTCPTable reads a MIB_TCPTABLE_OWNER_PID: a DWORD row count
// followed by that many rows.
func decodeTCPTable(buf []byte) ([]mibTCPRowOwnerPID, error) {
	const headerSize = 4
	if len(buf) < headerSize {
		return nil, fmt.Errorf("table too short: %d bytes", len(buf))
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))

	rowSize := int(unsafe.Sizeof(mibTCPRowOwnerPID{}))
	need := headerSize + int(n)*rowSize
	if need > len(buf) {
		return nil, fmt.Errorf("table claims %d rows (%d bytes) but buffer is %d",
			n, need, len(buf))
	}

	// unsafe.Slice is the supported way to view a C array as a Go
	// slice; the old reflect.SliceHeader construction is explicitly
	// discouraged and is not guaranteed to keep working.
	first := (*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[headerSize]))
	rows := unsafe.Slice(first, int(n))

	// Copy out of the unsafe view so callers hold ordinary Go memory.
	out := make([]mibTCPRowOwnerPID, n)
	copy(out, rows)
	return out, nil
}
```

The bounds check before `unsafe.Slice` is not optional. A malformed or truncated buffer
with a large `n` would otherwise produce a slice pointing past the allocation — a
memory-safety hole reachable from kernel-provided data.

---

## F. Exercise

1. Implement `gecko processes` with `--sort cpu|mem|name`, `--filter`, and `--tree`
   (parent/child hierarchy). Linux reads `/proc/[pid]/stat`; Darwin uses `ps`; Windows
   uses `CreateToolhelp32Snapshot` from `x/sys/windows`. The tree view needs PPIDs and a
   little graph code — reuse chapter 10's.

2. Implement `--kill`. Design questions before you code: what signal? Confirm by default?
   Refuse to kill PID 1, your own PID, or your parent? What if the PID was recycled
   between listing and killing? (That last one is a real TOCTOU: verify the process start
   time matches what you recorded, the same way `systemd` does.)

3. **CPU percentage is a trap.** `/proc/[pid]/stat` gives cumulative jiffies since
   process start. A single read gives you "average CPU since launch", which is not what
   anyone wants. Compute a real instantaneous percentage: two samples, delta over delta,
   divided by `runtime.NumCPU()`. Work out what interval to use and why too short is
   noise.

4. Get all three platforms green in CI before moving on. That's the real deliverable of
   this chapter.

---

## G. Testing

### Fixtures make platform parsers portable to test

```
internal/platform/testdata/
  proc_net_tcp
  proc_net_tcp6
  proc_net_tcp_malformed
  lsof_F_output.txt
  lsof_permission_denied.txt
```

```go
// No build tag: this test runs on every platform in CI, so the Linux
// parser is exercised by the macOS and Windows jobs too.
func TestParseProcNetTCP(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/proc_net_tcp")
	if err != nil { t.Fatal(err) }
	defer f.Close()

	socks, err := parseProcNet(f, false)
	if err != nil { t.Fatal(err) }

	var found bool
	for _, s := range socks {
		if s.LocalPort == 8080 && s.State == StateListen {
			found = true
			if !s.LocalIP.Equal(net.IPv4(127, 0, 0, 1)) {
				t.Errorf("LocalIP = %v, want 127.0.0.1 (endianness bug?)", s.LocalIP)
			}
		}
	}
	if !found {
		t.Error("did not find the listening socket on port 8080")
	}
}

func TestParseHexAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in     string
		ipv6   bool
		wantIP string
		wantPort int
	}{
		{"0100007F:1F90", false, "127.0.0.1", 8080},
		{"00000000:0050", false, "0.0.0.0", 80},
		{"0101A8C0:1538", false, "192.168.1.1", 5432},
		{"00000000000000000000000000000000:1F90", true, "::", 8080},
		{"0000000000000000FFFF00000100007F:0016", true, "::ffff:127.0.0.1", 22},
	}
	...
}
```

The `192.168.1.1` case is the one that catches endianness errors: `0101A8C0` reversed is
`C0.A8.01.01` = 192.168.1.1. A naive big-endian parse gives 1.1.168.192, which looks
plausible enough to ship.

### Fuzz the parsers

These parse kernel-provided data, but a container runtime or a `/proc` emulation could
produce something unexpected, and a panic in a CLI is a bad look.

```go
func FuzzParseProcNet(f *testing.F) {
	data, _ := os.ReadFile("testdata/proc_net_tcp")
	f.Add(string(data))
	f.Fuzz(func(t *testing.T, s string) {
		// The only requirement is: never panic, never hang.
		_, _ = parseProcNet(strings.NewReader(s), false)
	})
}
```

"Never panic" is a legitimate and valuable fuzz property when the function has no simple
oracle.

### Integration tests using a real listener

```go
func TestListenersFindsOurOwnSocket(t *testing.T) {
	if testing.Short() { t.Skip() }

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	socks, incomplete, err := Listeners(context.Background(), ListenersOptions{Port: port})
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("port listing unsupported here: %v", err)
	}
	if err != nil { t.Fatal(err) }

	if len(socks) == 0 {
		t.Fatalf("did not find our own listener on port %d (incomplete=%v)", port, incomplete)
	}
	if socks[0].PID != os.Getpid() {
		t.Errorf("PID = %d, want our own %d", socks[0].PID, os.Getpid())
	}
}
```

This is the strongest possible test: it binds a real socket and asserts the platform code
finds it and attributes it correctly. It runs on all three OSes in CI and it is the thing
that actually proves the chapter works.

### CI matrix — set it up now

`.github/workflows/ci.yml`:

```yaml
name: CI
on: [push, pull_request]

jobs:
  test:
    strategy:
      fail-fast: false   # see all platforms' failures, not just the first
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
        go: ['1.24']
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ matrix.go }}
          cache: true
      - run: go build ./...
      - run: go test ./... -race -timeout 5m
      - run: go vet ./...

  crosscompile:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - {goos: linux,   goarch: amd64}
          - {goos: linux,   goarch: arm64}
          - {goos: darwin,  goarch: amd64}
          - {goos: darwin,  goarch: arm64}
          - {goos: windows, goarch: amd64}
          - {goos: windows, goarch: arm64}
          - {goos: freebsd, goarch: amd64}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: '1.24'}
      # Compilation alone catches the majority of build-tag mistakes:
      # a missing implementation for a GOOS is a link error here.
      - run: GOOS=${{ matrix.goos }} GOARCH=${{ matrix.goarch }} go build ./...
```

`fail-fast: false` matters: with the default, a Windows failure cancels the macOS job and
you fix one platform per CI round trip instead of three.

The cross-compile job is cheap (no tests, just `go build`) and catches the single most
common build-tag error: forgetting an implementation for one GOOS, which produces
`undefined: listeners` at link time.

---

## H. Review

- Filename suffix constraints, `//go:build` expressions, and the `_windows.go` no-prefix
  trap.
- The four-way classification, and specifically why an interface with one implementation
  per platform is worse than a build-tag split.
- `/proc/net/tcp`'s mixed endianness, and the inode→fd→pid join.
- Why unprivileged results are partial, and why saying so beats hiding it.
- The cgo trade: one command's fidelity vs the whole project's static-build story.
- `NewLazySystemDLL` vs `NewLazyDLL` and DLL planting.
- Two-pass buffer sizing and why it needs a retry loop.
- `unsafe.Slice` over `reflect.SliceHeader`; single-expression pointer arithmetic;
  bounds-checking before viewing kernel data as a struct.
- **Push the build boundary to the syscall** so parsers stay portable and testable.
- CI matrix + cross-compile job as a substitute for owning three machines.

---

## I. Refactoring

`internal/platform` now holds `OpenBrowser` (chapter 7, `runtime.GOOS` switch),
`userDataDir` (chapter 3, also a switch), process groups (chapter 9, build tags) and port
enumeration (build tags). Is the mix of styles inconsistent?

No — and being able to defend that is the point of this chapter. The classification is
applied *per capability* based on whether the implementations need different imports.
Forcing `OpenBrowser` into three files would triple the surface area to change when you
add a fallback, for zero benefit.

What *should* change: add a `doc.go` to the package stating the classification rule
explicitly, so the next contributor doesn't "fix" the inconsistency.

```go
// Package platform contains Gecko's operating-system-specific code.
//
// Capabilities here use one of two styles, chosen deliberately:
//
//   - A runtime switch on runtime.GOOS, in a single unconstrained file,
//     when the implementations differ only in small details and need no
//     platform-specific imports (OpenBrowser, userDataDir).
//   - Build-tag-constrained files, when implementations need imports
//     that do not compile everywhere, or use fundamentally different
//     mechanisms (process groups, port enumeration).
//
// Parsing and formatting logic is kept in unconstrained files wherever
// possible so it can be tested on any host.
package platform
```

**Documenting a convention is a refactoring.** It prevents future churn just as
effectively as changing code.

---

## Commit

```
feat: add platform package with port enumeration
feat: implement port listing for linux via /proc
feat: implement port listing for darwin via lsof
feat: implement port listing for windows via GetExtendedTcpTable
feat: add processes command
ci: add cross-platform test and cross-compilation matrix
docs: document the platform package's build-tag conventions
```

Next: `12-terminal.md`.

# Chapter 7 — `gecko serve`: HTTP Servers, Middleware and Graceful Shutdown

```
Difficulty:   Advanced
Est. time:    8–10 hours
Main concepts: net/http server internals, ServeMux patterns (1.22), http.FileServer vs
               custom handlers, middleware composition, ResponseWriter wrapping,
               http.Server timeout fields, signal.NotifyContext, graceful shutdown,
               net.Listen with port 0, MIME types, gzip, CORS, SPA fallback,
               path traversal, httptest
Prerequisites: Chapters 1–6
```

---

## A. Goal

```
$ gecko serve ./dist --port 8080 --spa --open

  Gecko Server

  Directory  ./dist
  Local      http://localhost:8080
  Network    http://192.168.1.42:8080
  Mode       SPA fallback → index.html

  200  GET  /                     2.1 KB   1.2ms
  200  GET  /assets/app.js      184.0 KB   3.4ms
  304  GET  /assets/style.css        —     0.3ms
  404  GET  /favicon.ico             —     0.2ms

  Press Ctrl+C to stop
^C
  Shutting down... 2 connections drained
```

---

## B. Why this matters

`net/http` is the standard library's masterpiece and also the place where the most
production incidents originate. A server with default settings is vulnerable to Slowloris,
leaks goroutines on client disconnect, and takes unbounded time to shut down.

The specific things you'll take from this chapter into any Go service you ever write:
the four timeout fields and what each protects against, why `http.Server.Shutdown` needs
a context, and how middleware composition actually works when you need to observe the
status code.

Serving files is also the most direct path-traversal surface in the project. `http.Dir`
and `os.DirFS` both defend against it; understanding *how* is the point.

---

## C. Concepts

### What `http.Server` does per connection

`Serve(l net.Listener)` loops on `l.Accept()`. Each accepted connection gets **one
goroutine** (`conn.serve`), which loops reading requests (keep-alive) and calling the
handler. So concurrency is one goroutine per connection, plus whatever the handler
spawns.

Implications:
- 10,000 idle keep-alive connections = 10,000 goroutines ≈ 80 MB of stacks minimum. This
  is fine — Go handles it — but it's why `IdleTimeout` exists.
- A handler that blocks forever pins its goroutine forever. There is **no** built-in
  handler timeout that interrupts a running handler. `WriteTimeout` doesn't stop your
  code; it just makes writes fail.
- `http.TimeoutHandler` wraps a handler and returns 503 after a deadline, but the
  underlying goroutine keeps running — it just can't write. Read that sentence twice.

### The four timeouts

```go
srv := &http.Server{
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       120 * time.Second,
}
```

| Field | Covers | Protects against |
|---|---|---|
| `ReadHeaderTimeout` | accept → end of headers | **Slowloris**: a client dribbling headers one byte per second, holding a connection and goroutine forever |
| `ReadTimeout` | accept → end of body | slow body upload |
| `WriteTimeout` | end of headers → end of response write | slow client not draining the response |
| `IdleTimeout` | between keep-alive requests | connection hoarding |

`ReadHeaderTimeout` is the one people forget and the one that matters most. Go's
defaults for all four are **zero, meaning no timeout**. A default `http.Server` is
DoS-able by a single slow client. `gosec` flags this (G112) and it's right to.

For a static file server, `WriteTimeout` is awkward: a legitimately slow client
downloading a 500 MB file will be cut off. Options: set it generously, set it to zero and
rely on `IdleTimeout`, or use `http.ResponseController` (Go 1.20+) to extend the deadline
per-request:

```go
rc := http.NewResponseController(w)
rc.SetWriteDeadline(time.Now().Add(10 * time.Minute))
```

`ResponseController` is the modern replacement for type-asserting to `http.Flusher` and
`http.Hijacker`, and it works through middleware wrappers as long as they implement
`Unwrap() http.ResponseWriter`. Remember that — it comes up in section E.

### `ServeMux` in Go 1.22+

Go 1.22 substantially upgraded the standard mux. It now supports:

```go
mux.HandleFunc("GET /files/{name}", handler)      // method matching
mux.HandleFunc("POST /api/{id}/edit", handler)    // wildcards
mux.HandleFunc("GET /static/{path...}", handler)  // trailing wildcard
r.PathValue("name")                               // extraction
```

Precedence is by specificity, not registration order: the most specific pattern wins, and
conflicting patterns panic at registration time rather than silently shadowing. This
removes the main reason most projects reached for `gorilla/mux` or `chi`.

What it still doesn't do: regex constraints, route groups with shared middleware,
built-in middleware chaining. For Gecko, none of those are needed. **We use the standard
mux, no router dependency.**

One legacy behaviour to know: `ServeMux` redirects `/foo` to `/foo/` when only `/foo/` is
registered, and cleans paths (`//a/../b` → `/b`) before matching. That path cleaning is a
security feature, but relying on it alone is not enough — see the traversal section.

### `http.FileServer`, `http.Dir` and `http.FS`

```go
http.FileServer(http.Dir("./public"))     // classic
http.FileServer(http.FS(os.DirFS("./public")))  // io/fs-based
http.FileServerFS(os.DirFS("./public"))   // Go 1.22 shorthand
```

`FileServer` gives you a lot for free: `Content-Type` sniffing, `Last-Modified`,
`ETag`-less conditional requests via `If-Modified-Since`, **HTTP range requests** (essential
for video seeking), `HEAD` handling, and directory listings.

Range request support alone is a strong argument against hand-rolling. `http.ServeContent`
implements RFC 7233 range parsing including multipart ranges, and getting that right is
several hundred lines.

**Path traversal:** `http.Dir.Open` rejects paths containing `..` after cleaning, and
rejects absolute paths. `os.DirFS` additionally validates against `fs.ValidPath`. Both are
safe. What is *not* safe:

```go
// NEVER
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    http.ServeFile(w, r, "./public"+r.URL.Path)   // traversal
})
```

`r.URL.Path` is already cleaned by `ServeMux`, but if you register with `http.Handle` on
a raw server, or if the client sends `%2e%2e%2f` (URL-encoded `../`), you can be
surprised. `r.URL.Path` is decoded; `r.URL.RawPath` is not. **Use `http.ServeFileFS` with
an `fs.FS`, or `http.Dir`, and never concatenate.**

There's a second, subtler issue on Windows: `os.DirFS` uses forward slashes, but Windows
treats `\` as a separator too, and reserved names (`CON`, `PRN`, `AUX`, `NUL`, `COM1`)
resolve to devices. Go's `os.DirFS` on Windows rejects paths containing `\` and colons.
Verify this in a test if you're serving untrusted paths.

### Middleware

A middleware is `func(http.Handler) http.Handler`:

```go
func logging(logger *slog.Logger) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            start := time.Now()
            next.ServeHTTP(w, r)
            logger.Info("request", "method", r.Method, "path", r.URL.Path,
                "duration", time.Since(start))
        })
    }
}
```

Composition, applied so the first listed runs outermost:

```go
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
    for i := len(mw) - 1; i >= 0; i-- {
        h = mw[i](h)
    }
    return h
}
```

**The `ResponseWriter` wrapping problem.** To log the status code you must capture it,
which means wrapping:

```go
type statusRecorder struct {
    http.ResponseWriter
    status int
    bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
    r.status = code
    r.ResponseWriter.WriteHeader(code)
}
```

Embedding `http.ResponseWriter` promotes its methods, so the wrapper satisfies the
interface. But it **breaks optional interfaces**: the wrapper no longer satisfies
`http.Flusher`, `io.ReaderFrom` or `http.Hijacker`, even if the underlying writer does.
`http.FileServer` uses `io.ReaderFrom` for the `sendfile` fast path, so a careless wrapper
silently costs you zero-copy file serving.

Two fixes:
1. Implement `Unwrap() http.ResponseWriter` on the wrapper. `http.ResponseController`
   follows it. This is the Go 1.20+ answer and it's clean.
2. Explicitly forward the interfaces you care about.

Do both — `Unwrap` for `ResponseController`, explicit `ReadFrom`/`Flush` forwarding for
the fast paths, because `ResponseController` doesn't cover `io.ReaderFrom`.

### Graceful shutdown

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

go func() {
    if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
        errCh <- err
    }
}()

<-ctx.Done()

shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
err := srv.Shutdown(shutdownCtx)
```

What `Shutdown` does, precisely:
1. Closes all listeners (no new connections accepted).
2. Closes all **idle** connections.
3. Waits for **active** connections to become idle, polling.
4. Returns when all are done, or when the context expires — in which case it returns the
   context's error and **does not kill the active connections**.

Point 4 is important: `Shutdown` returning an error means connections are still running.
If you need them dead, follow with `srv.Close()`.

Also: `Shutdown` does **not** wait for hijacked connections (WebSockets) or for goroutines
your handlers spawned. `srv.RegisterOnShutdown(fn)` gives you a hook for the former.

The shutdown context must **not** derive from the signal context. If it does, and the user
hits Ctrl-C twice, your shutdown context is already cancelled and you skip the drain
entirely. Use `context.Background()` as the parent. This is a bug I have seen in
production code more than once.

### Binding: port 0 and address resolution

```go
ln, err := net.Listen("tcp", "localhost:0")   // OS picks a free port
addr := ln.Addr().(*net.TCPAddr)
port := addr.Port
```

Port 0 is the correct way to get a free port. The alternative — probe for a free port,
then bind — has a TOCTOU race where another process takes it in between.

**Create the listener before printing the URL.** If you print "listening on 8080" and
*then* bind, you've lied to the user when the bind fails. Bind first, read the actual
port from the listener, then print. This also makes `--port 0` work correctly.

`localhost` vs `0.0.0.0`: binding `localhost` (127.0.0.1 and ::1) is the safe default for
a dev server — it's unreachable from the network. `--host 0.0.0.0` exposes it. **Default
to localhost and require an explicit flag to expose**, and print a warning when exposed,
because "I ran a dev server on a café wifi" is a real incident class.

### MIME types

`mime.TypeByExtension(".js")` consults, in order: Go's built-in table, then the system
database (`/etc/mime.types` on Unix, the registry on Windows). The registry lookup is why
`.js` has historically returned `text/plain` on some Windows machines with broken registry
entries — a real, reported bug.

Defend explicitly:

```go
func init() {
    // Registering these overrides any broken system database entry.
    mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
    mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
    mime.AddExtensionType(".css", "text/css; charset=utf-8")
    mime.AddExtensionType(".json", "application/json")
    mime.AddExtensionType(".wasm", "application/wasm")
    mime.AddExtensionType(".webmanifest", "application/manifest+json")
}
```

`.wasm` matters: browsers refuse to instantiate WebAssembly streamed with the wrong type.

### gzip: what not to compress

Compressing already-compressed data (jpeg, png, webp, mp4, woff2, zip) wastes CPU and
often *increases* size. Compressing below ~1 KB is also net-negative once you account for
the gzip header and the loss of `sendfile`.

Critically: **gzip and `Content-Length` conflict.** You don't know the compressed length
until you've compressed, so a streaming gzip middleware must delete `Content-Length` and
let Go use chunked encoding. Forgetting this produces responses the browser truncates.

Also `Accept-Ranges`: a gzipped response can't serve byte ranges meaningfully, so drop
that header too.

---

## D. Design

### Package layout

```
internal/
  server/
    server.go      # Server type, lifecycle, listener setup
    handler.go     # file serving, SPA fallback, directory listing
    middleware.go  # logging, gzip, CORS, recovery, security headers
    server_test.go
  cli/
    serve.go
```

### The `Server` type

```go
type Options struct {
    Dir      string
    Host     string
    Port     int
    CORS     bool
    Gzip     bool
    SPA      bool
    NoListing bool
    Quiet    bool
}

type Server struct {
    opts Options
    log  *slog.Logger
    srv  *http.Server
    ln   net.Listener
}

func New(opts Options, log *slog.Logger) (*Server, error)   // validates, does not bind
func (s *Server) Listen() error                              // binds; Addr() valid after
func (s *Server) Addr() string
func (s *Server) Serve(ctx context.Context) error            // blocks until ctx done
```

Separating `Listen` from `Serve` is what makes testing sane: a test binds to port 0, reads
the real address, then serves. It's also what makes the "bind before you print" rule
implementable.

### SPA fallback, and why it's subtle

A single-page app needs `/users/42` to serve `index.html` so the client router can handle
it. Naive implementation: 404 → serve index.html. That's wrong in two ways:

1. A missing `/assets/app.js` returns `index.html` with a 200 and `Content-Type:
   text/html`. The browser tries to execute HTML as JavaScript and you get a
   baffling syntax error. **This is the single most common SPA server bug.**
2. `/api/missing` returns HTML to a fetch expecting JSON.

Correct rules:
- Fall back only when the request `Accept`s `text/html`.
- Never fall back for paths with a file extension that isn't `.html`.
- Never fall back for non-GET/HEAD methods.
- Return the fallback with status **200**, since the SPA route is legitimate.

### Directory listing

`http.FileServer` generates listings automatically, which is convenient and also an
information disclosure if someone serves a directory they didn't inspect. Keep it (it's
the expected `python -m http.server` behaviour) but:
- Add `--no-listing` to disable.
- Never list dotfiles.
- Refuse to serve `.git/` and `.env` entirely, regardless of listing. Someone running
  `gecko serve` in a project root and exposing `.git/config` with credentials in it is a
  realistic accident, and refusing it costs nothing.

That last rule is worth stating as a principle: **when a tool's convenient default has a
plausible catastrophic misuse, add the guard rail even if "the user should have known".**

---

## E. Implementation

### `internal/server/middleware.go`

```go
package server

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Middleware is the standard decorator shape.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first argument is outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// responseRecorder captures the status and byte count for logging.
//
// Embedding http.ResponseWriter satisfies the interface but hides the
// optional interfaces the underlying writer implements. Unwrap() lets
// http.ResponseController see through us; ReadFrom and Flush are
// forwarded explicitly because ResponseController does not cover
// io.ReaderFrom, and losing it would disable the sendfile fast path
// that makes http.FileServer fast.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wrote {
		return // a double WriteHeader is a bug; swallow it rather than panic
	}
	r.wrote = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.written += n
		return n, err
	}
	n, err := io.Copy(r.ResponseWriter, src)
	r.written += n
	return n, err
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Logging records each request. Output goes to the supplied writer,
// which for the serve command is the terminal; slog is used for the
// debug-level detail that only appears with --verbose.
func Logging(out io.Writer, log *slog.Logger, quiet bool) Middleware {
	var mu sync.Mutex // serialises writes so concurrent requests don't interleave
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			elapsed := time.Since(start)
			log.Debug("request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"bytes", rec.written, "duration", elapsed, "remote", r.RemoteAddr)

			if quiet {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			writeAccessLine(out, rec.status, r.Method, r.URL.Path, rec.written, elapsed)
		})
	}
}

// Recovery converts a handler panic into a 500 instead of killing the
// whole server. net/http already recovers per-connection, but it closes
// the connection abruptly and logs to the server's ErrorLog; catching it
// ourselves gives the client a proper response and us a useful log line.
func Recovery(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					// http.ErrAbortHandler is a sentinel meaning
					// "abort silently"; re-panic so net/http handles it.
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.Error("handler panic", "panic", v, "path", r.URL.Path,
						"stack", string(debug.Stack()))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORS permits cross-origin requests. It is deliberately permissive
// because this is a local development server; a production CORS
// middleware must validate the Origin against an allowlist rather than
// reflecting it.
func CORS() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// compressible lists types worth gzipping. Anything already compressed
// (images, video, woff2, zip) gains nothing and costs CPU.
var compressiblePrefixes = []string{
	"text/", "application/json", "application/javascript", "text/javascript",
	"application/xml", "image/svg+xml", "application/wasm",
}

const minCompressSize = 1024

type gzipWriter struct {
	http.ResponseWriter
	gz         *gzip.Writer
	wroteHdr   bool
	compressing bool
	buf        []byte // holds the first write so we can sniff type and size
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHdr {
		return
	}
	g.wroteHdr = true

	ct := g.Header().Get("Content-Type")
	if shouldCompress(ct) && g.contentLengthOK() {
		g.compressing = true
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		// Length is unknown until compression finishes; deleting it
		// makes Go fall back to chunked encoding. Leaving a stale
		// Content-Length here truncates the response in the browser.
		g.Header().Del("Content-Length")
		// Byte ranges over a compressed stream are not meaningful.
		g.Header().Del("Accept-Ranges")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.wroteHdr {
		g.WriteHeader(http.StatusOK)
	}
	if !g.compressing {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func shouldCompress(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	for _, p := range compressiblePrefixes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

// Gzip compresses eligible responses.
func Gzip() Middleware {
	pool := sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(w, r)
				return
			}
			gz := pool.Get().(*gzip.Writer)
			gz.Reset(w)
			gw := &gzipWriter{ResponseWriter: w, gz: gz}

			defer func() {
				if gw.compressing {
					gz.Close() // flushes the gzip trailer; skipping it corrupts output
				}
				pool.Put(gz)
			}()

			next.ServeHTTP(gw, r)
		})
	}
}

// SecurityHeaders sets conservative defaults. On a dev server these are
// mild, but nosniff in particular prevents a mis-typed file being
// executed as script.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			next.ServeHTTP(w, r)
		})
	}
}
```

Add `runtime/debug` to imports for `debug.Stack()`. The `contentLengthOK` helper is left
to you (it should decline compression when a known `Content-Length` is below
`minCompressSize`).

### `internal/server/handler.go`

```go
package server

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// blocked lists path prefixes never served, regardless of options.
//
// Someone running "gecko serve" in a project root and exposing
// .git/config (which may contain credentials in a remote URL) or .env
// is a realistic accident. The guard costs nothing and the convenience
// lost is zero.
var blockedPrefixes = []string{".git/", ".env", ".ssh/", ".aws/", ".netrc"}

func isBlocked(p string) bool {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, b := range blockedPrefixes {
		if p == strings.TrimSuffix(b, "/") || strings.HasPrefix(p, b) {
			return true
		}
	}
	return false
}

// fileHandler serves fsys, optionally with SPA fallback.
type fileHandler struct {
	fsys      fs.FS
	spa       bool
	noListing bool
	inner     http.Handler
}

func newFileHandler(fsys fs.FS, spa, noListing bool) http.Handler {
	h := &fileHandler{fsys: fsys, spa: spa, noListing: noListing}
	h.inner = http.FileServerFS(fsys)
	return h
}

func (h *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ServeMux has already cleaned r.URL.Path, and fs.FS rejects any
	// path with ".." or a leading slash, so traversal is prevented at
	// two independent layers. We never concatenate a user string onto
	// a filesystem path.
	upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if upath == "" {
		upath = "."
	}

	if isBlocked(upath) {
		http.NotFound(w, r) // 404, not 403: do not confirm existence
		return
	}

	if h.noListing {
		if info, err := fs.Stat(h.fsys, upath); err == nil && info.IsDir() {
			if _, err := fs.Stat(h.fsys, path.Join(upath, "index.html")); err != nil {
				http.NotFound(w, r)
				return
			}
		}
	}

	if h.spa && h.shouldFallback(r, upath) {
		// Status 200: the SPA route is legitimate, not an error.
		http.ServeFileFS(w, r, h.fsys, "index.html")
		return
	}

	h.inner.ServeHTTP(w, r)
}

// shouldFallback decides whether a missing path should be answered with
// index.html.
//
// The naive rule ("404 means serve index.html") breaks SPAs badly: a
// missing /assets/app.js is returned as HTML with a 200, and the browser
// reports a syntax error that gives no hint of the real cause. These
// three guards prevent that.
func (h *fileHandler) shouldFallback(r *http.Request, upath string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	if ext := path.Ext(upath); ext != "" && ext != ".html" && ext != ".htm" {
		return false
	}
	if _, err := fs.Stat(h.fsys, upath); err == nil {
		return false // it exists; serve it normally
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false // a real error, not a missing file
	}
	return true
}
```

### `internal/server/server.go`

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func init() {
	// Go consults the system MIME database, which on some Windows
	// installations returns text/plain for .js. Registering explicitly
	// overrides any broken system entry.
	for ext, typ := range map[string]string{
		".js":           "text/javascript; charset=utf-8",
		".mjs":          "text/javascript; charset=utf-8",
		".css":          "text/css; charset=utf-8",
		".json":         "application/json",
		".wasm":         "application/wasm",
		".webmanifest":  "application/manifest+json",
		".svg":          "image/svg+xml",
	} {
		_ = mime.AddExtensionType(ext, typ)
	}
}

type Options struct {
	Dir       string
	Host      string
	Port      int
	CORS      bool
	Gzip      bool
	SPA       bool
	NoListing bool
	Quiet     bool
}

type Server struct {
	opts   Options
	log    *slog.Logger
	out    io.Writer
	srv    *http.Server
	ln     net.Listener
	absDir string
}

func New(opts Options, out io.Writer, log *slog.Logger) (*Server, error) {
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("serve %s: %w", opts.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("serve %s: not a directory", opts.Dir)
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("port %d out of range", opts.Port)
	}
	if opts.Host == "" {
		opts.Host = "localhost"
	}
	return &Server{opts: opts, out: out, log: log, absDir: abs}, nil
}

// Listen binds the socket. It is separate from Serve so that the caller
// can learn the real address (important when Port is 0) before printing
// anything, and so tests can bind to an ephemeral port.
func (s *Server) Listen() error {
	addr := net.JoinHostPort(s.opts.Host, strconv.Itoa(s.opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Give a useful message for the overwhelmingly common case.
		if isAddrInUse(err) {
			return fmt.Errorf("port %d is already in use (try --port 0 for any free port)", s.opts.Port)
		}
		return err
	}
	s.ln = ln

	handler := newFileHandler(os.DirFS(s.absDir), s.opts.SPA, s.opts.NoListing)

	mw := []Middleware{
		Recovery(s.log),
		Logging(s.out, s.log, s.opts.Quiet),
		SecurityHeaders(),
	}
	if s.opts.CORS {
		mw = append(mw, CORS())
	}
	if s.opts.Gzip {
		mw = append(mw, Gzip())
	}

	s.srv = &http.Server{
		Handler: Chain(handler, mw...),

		// Zero is the Go default for every one of these, meaning no
		// limit. A default http.Server can be held open indefinitely by
		// a single slow client (Slowloris), so all four are set.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately generous: this server hands out
		// large files to potentially slow clients, and cutting off a
		// legitimate 500 MB download at 30 seconds would be wrong.
		WriteTimeout: 15 * time.Minute,

		MaxHeaderBytes: 1 << 20,

		// Route net/http's own errors into slog rather than the
		// default logger, which would write to stderr unstructured.
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	return nil
}

func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Port() int {
	if a, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// Serve runs until ctx is cancelled, then drains connections.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("Listen must be called before Serve")
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.srv.Serve(s.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil // the expected outcome of Shutdown
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// The shutdown context deliberately derives from Background, not
	// from ctx. If it derived from ctx it would already be cancelled at
	// this point and Shutdown would return immediately without draining.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown timed out: connections are still live. Kill them.
		s.log.Warn("graceful shutdown timed out; forcing close", "err", err)
		return s.srv.Close()
	}
	return <-errCh
}

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	var sysErr *os.SyscallError
	if !errors.As(opErr.Err, &sysErr) {
		return false
	}
	// syscall.EADDRINUSE on Unix; WSAEADDRINUSE on Windows. Comparing
	// the string keeps this file free of platform-specific imports;
	// chapter 11 introduces the build-tag machinery to do it properly.
	return strings.Contains(strings.ToLower(sysErr.Err.Error()), "address already in use") ||
		strings.Contains(strings.ToLower(sysErr.Err.Error()), "only one usage of each socket address")
}
```

That last function's string matching is genuinely ugly and I've left it deliberately, with
a comment pointing at chapter 11. The correct version compares against `syscall.EADDRINUSE`
and `windows.WSAEADDRINUSE` in build-tagged files. **Leaving a known-imperfect
implementation with an honest comment and a plan is better than either pretending it's
fine or blocking the feature on infrastructure you don't have yet.**

### Opening the browser

```go
// internal/platform/browser.go — our first genuinely OS-divergent code.
package platform

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenBrowser opens url in the user's default browser.
//
// The URL is passed as a separate argv element, never interpolated into
// a shell string, so a URL containing shell metacharacters cannot
// execute anything. On Windows the "start" builtin requires cmd, and
// its first quoted argument is the window title, hence the empty "".
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	// Reap in the background: the browser process outlives us, and
	// leaving it unreaped creates a zombie on Unix.
	go cmd.Wait()
	return nil
}
```

Note the Windows `start ""` quirk — without the empty string, a URL containing `&` is
parsed as the window title. Also note we still use `runtime.GOOS` rather than build tags:
same imports, same shape, three lines different. Chapter 11 will show you where the line
actually is.

---

## F. Exercise

1. Implement `--watch`: inject a small script into HTML responses that opens an SSE
   connection to `/__gecko/reload`, and push an event when a file changes. You'll need
   chapter 9's watcher, so stub the file-change source for now with a timer. The
   interesting part is HTML injection: you must buffer the response to find `</body>`,
   which means you can't stream, which means you should only do it for `text/html`.

2. Print the LAN address alongside localhost. `net.Interfaces()` → filter for up,
   non-loopback, IPv4. Careful: a machine can have several, and Docker adds virtual ones.

3. Load-test it. `hey -n 10000 -c 100 http://localhost:8080/large.bin`, then profile
   with `pprof` while it runs. Enable `net/http/pprof` behind a `--pprof` flag:
   ```go
   import _ "net/http/pprof"   // registers on http.DefaultServeMux
   ```
   Note that blank import registers on the **default** mux, which we don't use — so you
   must mount it explicitly. Figure out how, and consider why exposing pprof by default
   would be a security bug.

4. Verify the `sendfile` fast path survives your middleware. On Linux:
   `strace -f -e trace=sendfile,write ./gecko serve` then fetch a large file. If you see
   `sendfile`, `ReadFrom` forwarding works. If you see thousands of `write` calls, a
   wrapper broke it.

---

## G. Testing

`httptest` gives you two tools: `httptest.NewRecorder()` for handler-level unit tests
(no network), and `httptest.NewServer()` for a real server on a real ephemeral port.

```go
func TestFileHandlerServesFiles(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<h1>home</h1>")},
		"app.js":        {Data: []byte("console.log(1)")},
		"sub/page.html": {Data: []byte("<h1>sub</h1>")},
		".git/config":   {Data: []byte("[remote]\n url = https://tok@github.com/x")},
	}
	h := newFileHandler(fsys, false, false)

	tests := []struct {
		path       string
		wantStatus int
		wantType   string
		wantBody   string
	}{
		{"/", 200, "text/html", "home"},
		{"/app.js", 200, "text/javascript", "console.log"},
		{"/sub/page.html", 200, "text/html", "sub"},
		{"/missing", 404, "", ""},
		{"/.git/config", 404, "", ""}, // blocked, and 404 not 403
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantType != "" && !strings.Contains(rec.Header().Get("Content-Type"), tt.wantType) {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q", rec.Body.String())
			}
		})
	}
}
```

### Path traversal tests

```go
func TestNoPathTraversal(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"public/index.html": {Data: []byte("ok")}}
	h := newFileHandler(fsys, false, false)

	// These must all fail to escape. Note that some are normalised by
	// url.Parse before the handler ever sees them; the test documents
	// that the whole stack is safe, not just our code.
	for _, p := range []string{
		"/../etc/passwd",
		"/..%2fetc%2fpasswd",
		"/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/....//etc/passwd",
		"/public/../../etc/passwd",
		"/./././../etc/passwd",
	} {
		t.Run(p, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == 200 && strings.Contains(rec.Body.String(), "root:") {
				t.Fatalf("SERVED /etc/passwd for %q", p)
			}
		})
	}
}
```

### SPA fallback tests — the important ones

```go
func TestSPAFallback(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<div id=app></div>")},
		"app.js":     {Data: []byte("console.log(1)")},
	}
	h := newFileHandler(fsys, true, false)

	tests := []struct {
		name       string
		path       string
		accept     string
		wantStatus int
		wantHTML   bool
	}{
		{"deep route gets index", "/users/42", "text/html", 200, true},
		{"existing file wins", "/app.js", "text/html", 200, false},
		{"missing asset must 404, not HTML", "/missing.js", "text/html", 404, false},
		{"missing css must 404", "/style.css", "text/html", 404, false},
		{"json request must not get HTML", "/api/users", "application/json", 404, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Accept", tt.accept)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			isHTML := strings.Contains(rec.Body.String(), "id=app")
			if isHTML != tt.wantHTML {
				t.Errorf("served index.html = %v, want %v", isHTML, tt.wantHTML)
			}
		})
	}
}
```

The third and fourth cases are the ones that catch the classic SPA bug. If your
implementation returns `index.html` for `/missing.js`, this test fails and you've saved
someone a very confusing afternoon.

### Graceful shutdown, tested for real

```go
func TestGracefulShutdownDrainsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // hold the request open
		w.Write([]byte("finished"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)

	bodyCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			bodyCh <- "ERROR: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		bodyCh <- string(b)
	}()

	<-started

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	// Shutdown must NOT have completed yet: a request is in flight.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned while a request was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	if body := <-bodyCh; body != "finished" {
		t.Errorf("in-flight request was cut off: %q", body)
	}
	if err := <-shutdownDone; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
```

This is the test that proves graceful shutdown is real rather than aspirational. It's
worth writing once, carefully, because the pattern transfers to every Go service.

### Gzip correctness

```go
func TestGzipDoesNotSetStaleContentLength(t *testing.T) {
	body := strings.Repeat("hello world ", 500)
	h := Gzip()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write([]byte(body))
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length %q left set on a gzipped response; the browser will truncate", cl)
	}
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(gr)
	if string(got) != body {
		t.Errorf("round-trip mismatch: %d bytes vs %d", len(got), len(body))
	}
}
```

### Concurrency

```bash
go test ./internal/server -race -count=3
```

Run the server under concurrent load in a test with `-race`. The `sync.Mutex` in the
logging middleware and the `sync.Pool` in gzip are both places where a mistake produces a
race that only appears under concurrency.

---

## H. Review

- One goroutine per connection, and what that means for keep-alive at scale.
- The four timeout fields, what each protects against, and that all default to zero.
- Why `ReadHeaderTimeout` specifically is the Slowloris defence.
- `ResponseWriter` wrapping breaks optional interfaces; `Unwrap()` and explicit
  `ReadFrom` forwarding, and why `sendfile` depends on it.
- `Shutdown`'s four steps, and why its context must not derive from the signal context.
- `net.Listen` with port 0, and binding before printing.
- The three SPA fallback guards and the bug they prevent.
- gzip requires deleting `Content-Length` and `Accept-Ranges`.
- Path traversal defended at two layers (`ServeMux` cleaning + `fs.FS` validation), and
  why concatenation is never acceptable.
- `httptest.NewRecorder` vs `httptest.NewServer`, and when each is right.

---

## I. Refactoring

Three things.

**1. `Options` has eight bools and is growing.** Every new feature adds a field and a
conditional in `Listen`. This is the point where functional options genuinely start to
pay — but only if the package becomes public. It doesn't. Instead, group related fields:

```go
type Options struct {
    Dir  string
    Bind BindOptions   // Host, Port
    HTTP HTTPOptions   // CORS, Gzip, headers
    Files FileOptions  // SPA, NoListing, Index
}
```

Nested structs cost nothing at runtime, read better, and make the config-file mapping
(chapter 3) one-to-one.

**2. Middleware ordering is implicit.** `Recovery` must be outermost or a panic in
`Logging` isn't caught; `Gzip` must be innermost or it compresses before headers are set.
Encode that in the code, not in a comment: build the chain in a single function with the
ordering constraints documented at each step, and add a test that asserts a panic in the
inner handler still produces a logged 500.

**3. `isAddrInUse` string matching.** Flagged already. Leave it, but add a `TODO(ch11)`
and an issue. A tracked known-imperfection is a different thing from a hidden one.

---

## Commit

```
feat: add serve command with static file server
feat: add logging, gzip, CORS and recovery middleware
feat: add SPA fallback with correct asset handling
feat: add graceful shutdown with connection draining
test: add path traversal and shutdown drain tests
```

Five commits because each is independently reviewable and independently revertable. The
path-traversal test commit in particular is one you want findable in `git log`.

Next: `08-network-clients.md`.

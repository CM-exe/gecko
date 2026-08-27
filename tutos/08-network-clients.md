# Chapter 8 — `gecko http`, `dns`, `ping` and the Data Commands

```
Difficulty:   Advanced
Est. time:    8–10 hours
Main concepts: http.Client and Transport tuning, connection pooling, httptrace,
               crypto/tls introspection, redirect and cookie policy, net.Resolver,
               ICMP and raw sockets, privilege handling, TCP fallback, streaming
               JSON with Decoder/Encoder, encoding/csv
Prerequisites: Chapters 1–7
```

---

## A. Goal

```
$ gecko http GET https://api.github.com/users/torvalds --json

200 OK  ·  312ms  ·  1.4 KB

  DNS      12ms
  Connect  38ms
  TLS      94ms
  TTFB    287ms
  Total   312ms

  TLS 1.3  ·  X25519MLKEM768  ·  *.github.com  ·  expires in 214d

{
  "login": "torvalds",
  "id": 1024025
}

$ gecko dns example.com --type MX
$ gecko ping github.com
$ gecko json --query '.items[].name' data.json
```

---

## B. Why this matters

The HTTP *client* is where most Go network bugs live, and they're different bugs from the
server side. A misconfigured `Transport` leaks file descriptors. A body you didn't drain
kills connection reuse. A missing timeout hangs forever, because `http.Client`'s zero
value has **no timeout at all**.

`ping` is the chapter's most interesting problem: doing it properly requires raw sockets,
which require privileges, which differ across all three platforms. It's a genuine
"there is no clean answer" engineering situation and working through it honestly is worth
more than the feature.

---

## C. Concepts

### `http.Client` and `http.Transport`

```go
client := &http.Client{}   // NO TIMEOUT. Will hang forever.
```

`http.DefaultClient` also has no timeout. This is the single most common Go networking
mistake. Always:

```go
client := &http.Client{Timeout: 30 * time.Second}
```

`Client.Timeout` covers the **whole** exchange: dial, TLS, request, response headers, and
reading the body. It's implemented by setting a deadline on the underlying connection, so
a slow body read counts against it. For streaming downloads that's wrong — use a context
with a deadline for the header phase and leave `Client.Timeout` at zero, or use
`http.NewRequestWithContext` and manage it yourself.

`Transport` is where connection pooling lives:

```go
tr := &http.Transport{
    Proxy: http.ProxyFromEnvironment,        // honours HTTP_PROXY/HTTPS_PROXY/NO_PROXY
    DialContext: (&net.Dialer{
        Timeout:   10 * time.Second,
        KeepAlive: 30 * time.Second,
    }).DialContext,
    ForceAttemptHTTP2:     true,
    MaxIdleConns:          100,
    MaxIdleConnsPerHost:   10,   // DefaultTransport uses 2 — a classic bottleneck
    IdleConnTimeout:       90 * time.Second,
    TLSHandshakeTimeout:   10 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
    ResponseHeaderTimeout: 15 * time.Second,
}
```

Three things to internalise:

**`MaxIdleConnsPerHost` defaults to 2.** For a tool making many requests to one host,
that's a severe throttle: connections beyond 2 are closed after each request and must be
re-established, re-handshaking TLS every time. Raise it.

**Reuse one Transport.** A `Transport` holds the pool. Creating one per request defeats
pooling entirely and leaks connections until GC. Create it once; `http.Client` is
goroutine-safe.

**Body draining controls connection reuse.** This is the rule people get wrong:

```go
resp, err := client.Do(req)
if err != nil { return err }
defer resp.Body.Close()
```

`Close()` alone is **not** enough for reuse. If you close a body you haven't fully read,
the transport cannot know where the next response begins, so it closes the TCP connection.
To reuse:

```go
defer func() {
    io.Copy(io.Discard, resp.Body)   // drain
    resp.Body.Close()
}()
```

But don't drain unboundedly — a 10 GB body you're abandoning shouldn't be downloaded.
`io.CopyN(io.Discard, resp.Body, 64<<10)` then close is the pragmatic compromise.

Also: **the body must be closed even on non-2xx responses.** `client.Do` returns a
non-nil response with a non-nil body for a 500. Forgetting the close leaks a connection
and eventually a file descriptor.

### `httptrace` for phase timing

```go
var dnsStart, connectStart, tlsStart, firstByte time.Time

trace := &httptrace.ClientTrace{
    DNSStart:  func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
    DNSDone:   func(httptrace.DNSDoneInfo) { dnsDur = time.Since(dnsStart) },
    ConnectStart: func(net, addr string) { connectStart = time.Now() },
    ConnectDone:  func(net, addr string, err error) { connectDur = time.Since(connectStart) },
    TLSHandshakeStart: func() { tlsStart = time.Now() },
    TLSHandshakeDone:  func(cs tls.ConnectionState, err error) { tlsDur = time.Since(tlsStart) },
    GotFirstResponseByte: func() { ttfb = time.Since(start) },
    GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused },
}
req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
```

Two caveats worth knowing: the callbacks may run on different goroutines than the caller
(so if you're doing anything beyond assigning timestamps, synchronise), and with a pooled
connection `DNSStart`/`ConnectStart` never fire at all — `GotConn` reports `Reused: true`.
Report "reused" rather than "0ms" in that case; showing 0ms for DNS is a lie.

### TLS introspection

`resp.TLS` is a `*tls.ConnectionState`, non-nil for HTTPS:

```go
cs := resp.TLS
tls.VersionName(cs.Version)          // Go 1.21+: "TLS 1.3"
tls.CipherSuiteName(cs.CipherSuite)
cert := cs.PeerCertificates[0]       // leaf first, then the chain
cert.NotAfter, cert.Subject.CommonName, cert.DNSNames, cert.Issuer.Organization
```

Never add an `--insecure` flag that sets `InsecureSkipVerify` without a loud warning to
stderr. It's occasionally necessary (internal CAs, expired staging certs) but it disables
all certificate verification including hostname checking, which turns HTTPS into
obfuscated HTTP. If you offer it: print a warning, and never let it come from a config
file — only from an explicit flag on the command line, so it can't be silently on.

### Redirects and cookies

`http.Client` follows up to 10 redirects by default. Override:

```go
client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
    if len(via) >= maxRedirects {
        return fmt.Errorf("stopped after %d redirects", maxRedirects)
    }
    return nil
}
```

Return `http.ErrUseLastResponse` to stop following and return the redirect response
itself — that's what `--no-follow` should do.

A security detail Go handles for you: on a cross-domain redirect, sensitive headers
(`Authorization`, `Cookie`, `WWW-Authenticate`) are **stripped**. If you're setting an
auth header manually and wondering why it vanished, that's why, and it's correct
behaviour — you don't want your GitHub token following a redirect to `evil.com`.

### DNS: `net.Resolver` and its limits

```go
r := &net.Resolver{PreferGo: true}       // pure-Go resolver, ignores /etc/nsswitch.conf
ips, err := r.LookupIPAddr(ctx, host)
mx, err := r.LookupMX(ctx, host)
txt, err := r.LookupTXT(ctx, host)
cname, err := r.LookupCNAME(ctx, host)
ns, err := r.LookupNS(ctx, host)
names, err := r.LookupAddr(ctx, ip)      // reverse
```

Go has two resolvers: the **cgo resolver** (calls `getaddrinfo`, honours nsswitch, mDNS,
and other system config) and the **pure-Go resolver** (reads `/etc/resolv.conf`, speaks
DNS directly, doesn't block an OS thread). Selection is automatic and can be forced with
`GODEBUG=netdns=go` or `netdns=cgo`. On Windows and macOS the situation differs again —
macOS always uses cgo for some lookups because of its system resolver.

What `net.Resolver` **cannot** do: query a specific server (except by overriding `Dial`),
return TTLs, return raw records for arbitrary types (SRV is supported, but not CAA, DS,
or DNSKEY), or do DNSSEC validation.

For `gecko dns --server 8.8.8.8`, override the dialer:

```go
r := &net.Resolver{
    PreferGo: true,
    Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
        d := net.Dialer{Timeout: 5 * time.Second}
        return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
    },
}
```

For anything richer you need `github.com/miekg/dns`. **Decision: standard library only.**
Justification: our use case is "what does this hostname resolve to", which the stdlib
covers. `miekg/dns` is excellent but it's a full DNS implementation and adding it for
TTL display is not proportionate. Document the limitation in `--help`.

### `ping`: the hard problem

ICMP echo requires either a raw socket (`SOCK_RAW`, needs root/CAP_NET_RAW) or an
unprivileged ICMP socket (`SOCK_DGRAM` with `IPPROTO_ICMP`), and support varies:

| Platform | Unprivileged ICMP | Notes |
|---|---|---|
| Linux | Yes, if `net.ipv4.ping_group_range` includes your GID | Default range varies by distro; often `1 0` (nobody) |
| macOS | Yes | `SOCK_DGRAM`/`IPPROTO_ICMP` works for normal users |
| Windows | No | Raw sockets need admin; the OS provides `IcmpSendEcho` via `iphlpapi.dll` instead |

Four options:

1. **`golang.org/x/net/icmp`** — real ICMP, handles both socket types. Needs the
   privilege above, still won't work unprivileged on Windows.
2. **Shell out to the system `ping`** — works everywhere, but output parsing differs
   across platforms and locales (a French `ping` prints different text), and you inherit
   whatever the system binary does.
3. **TCP "ping"** — connect to a port, measure the handshake, close. Not ICMP, but it
   answers "can I reach this service" which is usually the real question. Zero privileges,
   identical on all platforms.
4. **Windows `IcmpSendEcho` via syscall** — correct on Windows, requires `x/sys/windows`
   and a build-tagged file.

**Our decision: TCP connectivity check by default, ICMP behind `--icmp` using
`x/net/icmp`, with a clear error when privileges are missing.** Reasoning: the default
must work for every user on every platform without sudo. Silently falling back from ICMP
to TCP would be worse than being explicit, because the two measure different things and a
user comparing to `ping(8)` would be confused by the numbers.

This is a good example of a general principle: **when the "correct" implementation
requires privileges most users don't have, the honest design is to default to the
approximation and name it clearly, not to fail or to silently substitute.**

### Streaming JSON

```go
// Whole-document: allocates the entire structure.
var v any
json.Unmarshal(data, &v)

// Streaming: constant memory for a top-level array.
dec := json.NewDecoder(r)
dec.UseNumber()               // preserves int64 precision and number formatting
tok, _ := dec.Token()         // consumes '['
for dec.More() {
    var item Item
    dec.Decode(&item)
}
dec.Token()                   // consumes ']'
```

`UseNumber()` matters more than people expect: without it, JSON numbers become `float64`,
and a 64-bit ID like `9007199254740993` silently loses precision. For a formatting tool
that round-trips data, that's data corruption.

`dec.DisallowUnknownFields()` is the client-side counterpart to chapter 3's
`KnownFields(true)`.

For pretty-printing, `json.Indent` works on bytes; `json.Encoder` with `SetIndent` works
on a stream. Neither preserves key order — Go maps are unordered and `encoding/json`
sorts map keys alphabetically when marshalling. If you need original order you must parse
into an ordered structure yourself, which for a formatter is worth doing. Note it as a
limitation if you don't.

---

## D. Design

### Package structure

```
internal/
  network/
    client.go     # shared http.Client construction
    request.go    # gecko http: building and executing
    timing.go     # httptrace collection
    tlsinfo.go    # certificate summary
    dns.go
    ping.go
    ping_icmp.go  // +build with x/net/icmp
  format/
    json.go       # pretty printing, shared with every --json flag
    table.go
```

`internal/format` is created because chapter 5's `find --json`, chapter 6's
`doctor --json`, and this chapter's `gecko json` all need the same pretty-printer. Three
callers — the extraction rule from chapter 5 is satisfied.

### One shared client

```go
// Client returns Gecko's shared HTTP client.
//
// A single Transport is used process-wide so that connection pooling
// works: creating a Transport per request defeats pooling entirely and
// leaks connections until GC.
var sharedTransport = sync.OnceValue(func() *http.Transport { ... })
```

`sync.OnceValue` (Go 1.21) is the clean form of the lazy-singleton pattern and replaces
the `sync.Once` + package var dance.

### Request building and safety

`gecko http POST url --data @file.json` reads from a file. `--data @-` reads stdin.
That's `curl`'s convention and worth matching.

Header parsing (`-H "Name: value"`) must reject header injection: a value containing
`\r\n` could inject additional headers. `http.Header.Set` doesn't validate, but
`http.Request.Write` does reject invalid header values — still, validate at the parse
boundary so the error message is good:

```go
if strings.ContainsAny(value, "\r\n") {
    return fmt.Errorf("header %q: value contains a newline", name)
}
```

---

## E. Implementation

### `internal/network/client.go`

```go
package network

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// ClientOptions configures the shared HTTP client.
type ClientOptions struct {
	Timeout         time.Duration
	Insecure        bool
	MaxRedirects    int
	NoFollow        bool
	ProxyFromEnv    bool
}

// sharedTransport is created once per process. A Transport holds the
// connection pool; creating one per request defeats pooling and leaks
// connections until the garbage collector runs their finalizers.
var sharedTransport = sync.OnceValue(func() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      100,
		// DefaultTransport uses 2 here, which throttles a tool making
		// repeated requests to one host: connections beyond the second
		// are torn down and the TLS handshake repeated each time.
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
})

func NewClient(opts ClientOptions) *http.Client {
	tr := sharedTransport()

	if opts.Insecure {
		// Clone rather than mutate: the shared transport must not have
		// verification disabled for every other caller in the process.
		tr = tr.Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit user opt-in
	}

	c := &http.Client{
		Transport: tr,
		// Note: Client.Timeout covers the entire exchange including
		// body reads, which is wrong for large downloads. Callers that
		// stream should pass zero here and use a request context for
		// the header phase.
		Timeout: opts.Timeout,
	}

	max := opts.MaxRedirects
	if max == 0 {
		max = 10
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if opts.NoFollow {
			return http.ErrUseLastResponse
		}
		if len(via) >= max {
			return fmt.Errorf("stopped after %d redirects", max)
		}
		return nil
	}
	return c
}
```

### `internal/network/timing.go`

```go
package network

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

// Timing holds per-phase durations for one request.
type Timing struct {
	DNS        time.Duration
	Connect    time.Duration
	TLS        time.Duration
	TTFB       time.Duration
	Total      time.Duration
	ConnReused bool
	RemoteAddr string
}

// traceCollector accumulates timings. Its fields are guarded by a mutex
// because httptrace callbacks may run on goroutines other than the one
// issuing the request.
type traceCollector struct {
	mu    sync.Mutex
	start time.Time
	t     Timing

	dnsStart, connStart, tlsStart time.Time
}

func WithTiming(ctx context.Context) (context.Context, *traceCollector) {
	c := &traceCollector{start: time.Now()}
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			c.mu.Lock(); c.dnsStart = time.Now(); c.mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			c.mu.Lock(); c.t.DNS = time.Since(c.dnsStart); c.mu.Unlock()
		},
		ConnectStart: func(_, _ string) {
			c.mu.Lock(); c.connStart = time.Now(); c.mu.Unlock()
		},
		ConnectDone: func(_, addr string, _ error) {
			c.mu.Lock()
			c.t.Connect = time.Since(c.connStart)
			c.t.RemoteAddr = addr
			c.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			c.mu.Lock(); c.tlsStart = time.Now(); c.mu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			c.mu.Lock(); c.t.TLS = time.Since(c.tlsStart); c.mu.Unlock()
		},
		GotConn: func(i httptrace.GotConnInfo) {
			c.mu.Lock(); c.t.ConnReused = i.Reused; c.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			c.mu.Lock(); c.t.TTFB = time.Since(c.start); c.mu.Unlock()
		},
	}
	return httptrace.WithClientTrace(ctx, trace), c
}

// Result finalises and returns the timings. On a reused connection the
// DNS and Connect phases never fire, so those durations are zero — the
// caller must render that as "reused" rather than "0ms", which would be
// misleading.
func (c *traceCollector) Result() Timing {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t.Total = time.Since(c.start)
	return c.t
}
```

### `internal/network/ping.go`

```go
package network

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"
)

// PingResult summarises a connectivity check.
type PingResult struct {
	Target   string
	Address  string
	Method   string // "tcp" or "icmp"
	RTTs     []time.Duration
	Sent     int
	Received int
}

func (r PingResult) Loss() float64 {
	if r.Sent == 0 {
		return 0
	}
	return float64(r.Sent-r.Received) / float64(r.Sent) * 100
}

func (r PingResult) Stats() (min, avg, max, stddev time.Duration) { /* ... */ }

// TCPPing measures TCP handshake round-trip time.
//
// This is not ICMP. It measures whether a TCP service is reachable and
// how long the three-way handshake takes, which is usually the question
// a developer actually has ("is the server up?") and which requires no
// privileges on any platform. ICMP echo is available behind --icmp for
// users who specifically want it and have the necessary rights.
func TCPPing(ctx context.Context, host string, port int, count int, interval time.Duration) (PingResult, error) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	res := PingResult{Target: host, Method: "tcp"}

	// Resolve once so that per-attempt timings measure the connection,
	// not repeated DNS lookups.
	raddr, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return res, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(raddr) == 0 {
		return res, fmt.Errorf("resolve %s: no addresses", host)
	}
	res.Address = raddr[0].String()
	addr = net.JoinHostPort(res.Address, fmt.Sprint(port))

	d := net.Dialer{Timeout: 5 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 0; i < count; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-ticker.C:
			}
		}

		start := time.Now()
		conn, err := d.DialContext(ctx, "tcp", addr)
		rtt := time.Since(start)
		res.Sent++

		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			continue // record as loss
		}
		conn.Close()
		res.Received++
		res.RTTs = append(res.RTTs, rtt)
	}

	sort.Slice(res.RTTs, func(i, j int) bool { return res.RTTs[i] < res.RTTs[j] })
	return res, nil
}
```

For the ICMP variant, `golang.org/x/net/icmp` plus `golang.org/x/net/ipv4`:

```go
// ICMPPing requires elevated privileges on most systems:
//   Linux:   CAP_NET_RAW, or a GID within net.ipv4.ping_group_range
//   macOS:   usually works unprivileged via SOCK_DGRAM
//   Windows: requires administrator; the OS alternative is IcmpSendEcho
//            in iphlpapi.dll, which we do not currently wrap.
func ICMPPing(ctx context.Context, host string, count int) (PingResult, error) {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0") // unprivileged where supported
	if err != nil {
		conn, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0") // raw; needs privilege
		if err != nil {
			return PingResult{}, fmt.Errorf(
				"ICMP requires elevated privileges on this system (%w); "+
					"omit --icmp to use a TCP connectivity check instead", err)
		}
	}
	defer conn.Close()
	...
}
```

That error message is the important part. It says what failed, why, and what to do
instead. Compare with `permission denied` — technically accurate, practically useless.

### `internal/format/json.go`

```go
package format

import (
	"encoding/json"
	"fmt"
	"io"
)

// PrettyJSON reformats a JSON stream with indentation.
//
// It streams rather than buffering the whole document, so it handles
// files larger than memory. UseNumber preserves integer precision:
// without it, a 64-bit identifier such as 9007199254740993 becomes a
// float64 and silently changes value.
//
// Limitation: object key order is not preserved. encoding/json decodes
// objects into maps, which are unordered, and re-encodes with sorted
// keys. Preserving order requires a custom ordered representation.
func PrettyJSON(w io.Writer, r io.Reader, indent string, colour bool) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()

	enc := json.NewEncoder(w)
	enc.SetIndent("", indent)
	enc.SetEscapeHTML(false) // do not mangle < > & in string values

	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if err == io.EOF {
				return nil
			}
			// Report the byte offset: for a 40 MB file, "invalid
			// character" without a position is unusable.
			if se, ok := err.(*json.SyntaxError); ok {
				return fmt.Errorf("invalid JSON at byte %d: %w", se.Offset, err)
			}
			return err
		}
		if err := enc.Encode(v); err != nil {
			return err
		}
	}
}
```

Handling multiple top-level values in the loop gives you JSON Lines support for free —
`dec.Decode` in a loop reads a stream of concatenated documents, which is exactly the
NDJSON format that logging tools emit.

---

## F. Exercise

1. Implement `gecko http` fully: `-H`, `--data`, `--data @file`, `--form`, `-o output`,
   `--follow`/`--no-follow`, basic auth, bearer token. Then check: does your
   implementation leak a connection when the response is a 404? Add a test with
   `httptest.NewServer` that makes 100 requests and asserts the server saw few
   connections (use `httptest.Server.Config.ConnState` to count).

2. **Streaming download.** `gecko http GET url -o big.iso` should write to disk as it
   receives, showing progress, without buffering. Then work out what `Client.Timeout`
   does to a 20-minute download and fix it.

3. Implement `gecko dns` with `--type A|AAAA|MX|TXT|NS|CNAME|PTR|SRV`, `--server`, and
   `--all` (query every type concurrently). The concurrent version is a good errgroup
   exercise.

4. Read the `x/net/icmp` source for `ListenPacket` and work out exactly which socket type
   it opens on each platform. Then decide whether the `udp4`-then-`ip4:icmp` fallback in
   the sketch above is correct on Linux.

---

## G. Testing

### `httptest.NewServer` for client tests

```go
func TestRequestTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx, collector := WithTiming(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := NewClient(ClientOptions{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	tm := collector.Result()
	if tm.TTFB < 50*time.Millisecond {
		t.Errorf("TTFB = %v, expected at least the handler's 50ms sleep", tm.TTFB)
	}
	if tm.Total < tm.TTFB {
		t.Errorf("Total (%v) < TTFB (%v), which is impossible", tm.Total, tm.TTFB)
	}
}
```

The last assertion is a good habit: check invariants, not just values. A timing bug that
makes `Total < TTFB` is caught even when you don't know the right number.

### TLS testing without a real CA

`httptest.NewTLSServer` generates a self-signed certificate and exposes a client
configured to trust it:

```go
srv := httptest.NewTLSServer(handler)
defer srv.Close()
client := srv.Client()   // trusts the test cert
resp, _ := client.Get(srv.URL)
if resp.TLS == nil {
	t.Fatal("expected TLS state")
}
```

And to prove verification actually works:

```go
func TestInsecureRequiredForSelfSignedCert(t *testing.T) {
	srv := httptest.NewTLSServer(okHandler)
	defer srv.Close()

	// Default client must refuse the self-signed cert.
	_, err := NewClient(ClientOptions{Timeout: 5 * time.Second}).Get(srv.URL)
	if err == nil {
		t.Fatal("accepted a self-signed certificate without --insecure")
	}
	var ce *tls.CertificateVerificationError
	if !errors.As(err, &ce) && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("unexpected error type: %v", err)
	}

	// With Insecure it must succeed.
	resp, err := NewClient(ClientOptions{Timeout: 5 * time.Second, Insecure: true}).Get(srv.URL)
	if err != nil {
		t.Fatalf("--insecure did not work: %v", err)
	}
	resp.Body.Close()
}
```

A test that proves the security control is *on* by default is more valuable than one that
proves the escape hatch works.

### Connection reuse

```go
func TestConnectionReuse(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewServer(okHandler)
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	defer srv.Close()

	client := NewClient(ClientOptions{Timeout: 5 * time.Second})
	for i := 0; i < 20; i++ {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) // drain: without this, no reuse
		resp.Body.Close()
	}
	if n := conns.Load(); n > 2 {
		t.Errorf("opened %d connections for 20 requests; pooling is not working", n)
	}
}
```

Delete the `io.Copy` line and watch this fail with 20 connections. That's the drain rule
demonstrated rather than asserted.

### Network tests in CI

Tests that hit the real internet are flaky and slow. Gate them:

```go
func TestRealDNS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}
	...
}
```

Run `go test -short ./...` in the fast CI job and the full suite in a nightly. Never let
a DNS outage at your CI provider turn every PR red.

---

## H. Review

- `http.Client`'s zero value has no timeout, and what `Client.Timeout` actually covers.
- Why one `Transport` per process, and what `MaxIdleConnsPerHost: 2` costs you.
- Draining the body is required for connection reuse; closing alone is not.
- `httptrace` callbacks may run on other goroutines, and are absent on reused connections.
- `resp.TLS` introspection, and why `InsecureSkipVerify` must be loud and flag-only.
- Go strips `Authorization` on cross-domain redirects, and why that's right.
- The two Go resolvers and what `net.Resolver` cannot do.
- The ICMP privilege matrix across three platforms, and why "default to the
  approximation and name it" beats both failing and silently substituting.
- `dec.UseNumber()` prevents silent integer precision loss.
- `httptest.NewTLSServer` and testing that a security control is on by default.

---

## I. Refactoring

`gecko http` output formatting, `gecko dns` output, `doctor`'s report and `find --json`
now all format structured data for a terminal. Four callers, and the code is diverging.

Introduce a small output abstraction — but carefully, because "an output abstraction" is
exactly the kind of thing that becomes an unusable god-object:

```go
// Renderer writes a command result in one of Gecko's output formats.
// It deliberately does not attempt to be general: each command supplies
// a Human method and a value that marshals to JSON, and Renderer picks.
type Renderer struct {
    Format Format // FormatHuman, FormatJSON
    Out    io.Writer
}

func (r *Renderer) Render(human func(io.Writer) error, jsonValue any) error
```

Two paths, one decision point. The temptation is to build a table/tree/column DSL; resist
it. The commands' human output is genuinely different from each other and a DSL would
make each one worse. **An abstraction that only captures the part that's actually common
is better than one that forces the uncommon parts into a shared mould.**

---

## Commit

```
feat: add http client with phase timing and TLS inspection
feat: add dns command with concurrent multi-type lookup
feat: add ping with TCP default and optional ICMP
feat: add json/yaml/csv formatting commands
refactor: unify human and JSON output behind a renderer
```

Next: `09-watch.md`.

# Chapter 14 — The Plugin Ecosystem: SDK, Installation, Registry

```
Difficulty:   Expert
Est. time:    12–16 hours
Main concepts: public API design and compatibility, go:embed and text/template,
               semantic versioning, atomic installation, fsync durability, TOCTOU on
               install, checksum and ed25519 signature verification, TUF concepts,
               supply-chain threat modelling
Prerequisites: Chapter 13
```

---

## A. Goal

```
$ gecko plugin create mytool
Created ./gecko-mytool
  main.go, plugin.go, go.mod, README.md, .github/workflows/release.yml
Next:  cd gecko-mytool && go build && gecko plugin install ./gecko-mytool

$ gecko plugin install ./gecko-mytool
Installed mytool v0.1.0 → ~/.local/share/gecko/plugins/gecko-mytool

$ gecko plugin install docker
Resolving docker from registry…
  gecko-docker v1.2.0 (linux/amd64, 4.2 MB)
  sha256 verified ✓
  signature verified ✓ (key 3f9a…c21b, Gecko Plugin Registry)
Installed docker v1.2.0

$ gecko plugin update --all
docker    1.2.0 → 1.3.1  ✓
postgres  0.4.1 (current)
```

---

## B. Why this matters

Two distinct hard problems.

**The SDK is an API-design problem with permanent consequences.** `sdk/` is the first
non-`internal` package in the project, which means every exported identifier is a promise
under Go's module compatibility rules. Adding a method to an exported interface is a
breaking change. Getting this wrong costs a major version bump.

**Installation is a supply-chain security problem.** Downloading and executing a binary
from the internet is the most dangerous thing this tool does. Checksums, signatures,
atomic writes and threat modelling are not optional garnish here — they're the feature.

---

## C. Concepts

### `internal/` and the compatibility boundary

Go enforces `internal/`: `github.com/yourname/gecko/internal/cli` is importable only by
code under `github.com/yourname/gecko/`. That's why everything so far has been freely
refactorable.

`sdk/` is different. Once published and imported by someone, Go's compatibility rules
bind you:

**Breaking (requires v2+):** removing or renaming anything exported; changing a function
signature; adding a method to an exported interface; changing a struct field's type;
making an exported type unexported.

**Non-breaking:** adding a function, type or constant; adding a field to a struct
(unless callers use unkeyed literals — which is why you add `_ struct{}` to prevent
them, or simply document that unkeyed literals aren't supported); adding a method to a
concrete type.

Go's module system makes v2+ painful by design: `module github.com/you/gecko/sdk/v2` and
every import path changes. **Assume you get one shot.**

Three defensive techniques:

**1. Prefer concrete types over interfaces in the public API.** Adding a method to
`type Logger interface` breaks every implementor. Adding a method to `type Logger struct`
breaks nobody.

**2. Functional options for anything configurable.**

```go
func New(name string, opts ...Option) *Plugin
```

Now `WithVersion`, `WithHomepage`, `WithTimeout` can be added forever. This is where
chapter 2's rejected pattern finally earns its place — and the distinguishing factor is
precisely that this API is public and permanent.

**3. An unexported field in exported structs**, to prevent unkeyed literals:

```go
type Command struct {
    Name string
    Run  func(*Context) error

    _ struct{} // prevents unkeyed literals, so fields can be added later
}
```

Zero-size, zero-cost, and it turns a future breaking change into a non-breaking one.

### `go:embed` and scaffolding

```go
//go:embed templates/*
var templates embed.FS
```

Files are compiled into the binary. Rules that catch people: the directive must
immediately precede a package-level `var`; patterns are relative to the source file's
directory; `..` is forbidden; and `templates/*` **excludes** files beginning with `.` or
`_` — use `all:templates` to include them, which matters when your template includes a
`.github/` directory.

Templating with `text/template` (not `html/template` — that escapes for HTML and would
mangle Go source):

```go
tmpl, _ := template.ParseFS(templates, "templates/*.tmpl")
tmpl.ExecuteTemplate(w, "main.go.tmpl", data)
```

Name templates `.tmpl` so `go build` doesn't try to compile the embedded `main.go`.

### Semantic versioning

```bash
go get golang.org/x/mod
```

`golang.org/x/mod/semver` is maintained by the Go team, has no dependencies, and matches
the Go module system's own semantics. It requires the `v` prefix (`v1.2.3`), which
`semver.IsValid` enforces.

```go
semver.Compare("v1.2.0", "v1.10.0")  // -1 (numeric, not lexical)
semver.Major("v1.2.3")               // "v1"
semver.Prerelease("v1.0.0-beta.1")   // "-beta.1"
```

The precedence rules that trip people: a prerelease sorts **before** its release
(`v1.0.0-beta` < `v1.0.0`), and build metadata (`+build.1`) is ignored entirely for
comparison. Chapter 6's hand-rolled comparator got the first wrong and didn't handle the
second — this is the moment to replace it, and the justification is that we now have
version constraints where correctness actually matters.

Constraint syntax (`^1.2.0`, `~1.2.0`, `>=1.2 <2.0`) is npm's, not semver's, and `x/mod`
doesn't implement it. Either write a small parser or restrict to exact versions and
`latest`. **Decision: exact and `latest` only**, plus `--version` for pinning. Constraint
resolution is a package-manager problem and Gecko is not a package manager.

### Atomic installation

An installed plugin must never be half-written. The sequence:

```
1. Download to <plugindir>/.tmp-<random>
2. Verify checksum       ← before it is executable
3. Verify signature      ← before it is executable
4. chmod 0755
5. fsync the file
6. rename to <plugindir>/gecko-<name>
7. fsync the directory   ← Unix only; makes the rename durable
```

Order matters enormously. Verifying **after** the rename means there's a window where an
unverified binary sits at the final path and a concurrent `gecko docker ps` would run it.
Chmod before rename for the same reason — never expose a partially-configured file at the
final name.

Step 7 is the one people skip. `rename(2)` is atomic with respect to *readers*, but on
a crash the directory entry may not have reached disk. `fsync` on the parent directory
fixes it. On Windows there is no directory fsync and `os.Rename` fails if the target is
open — handle that error with a clear message ("the plugin is currently running").

Writing to the same directory matters too: `rename` is only atomic within a filesystem.
`/tmp` is often a different mount, in which case `os.Rename` falls back to copy-and-delete
and loses atomicity.

### Checksums

```
sha256  4e0740…  gecko-docker_1.2.0_linux_amd64
```

A checksum proves **integrity**: the bytes are what the manifest said. It proves nothing
about **authenticity** — if an attacker controls the manifest, they control the checksum.

So checksums defend against: corruption in transit, a compromised CDN or mirror, a
truncated download. They do not defend against: a compromised registry, a malicious
publisher, or a compromised release pipeline.

Compare in constant time (chapter 4's `subtle.ConstantTimeCompare`). Here the expected
value can be attacker-influenced, so the timing concern is less theoretical than it was
for file hashing.

### Signatures with ed25519

Signatures give authenticity: the artifact was signed by the holder of a private key.

```go
import "crypto/ed25519"

pub, priv, _ := ed25519.GenerateKey(rand.Reader)
sig := ed25519.Sign(priv, message)
ok := ed25519.Verify(pub, message, sig)   // constant-time internally
```

Why ed25519 over RSA or ECDSA: 32-byte public keys, 64-byte signatures, fast
verification, no parameter choices to get wrong, and — critically — **deterministic
signing**, so it has no nonce-reuse catastrophe (the flaw that leaked Sony's PS3 key and
several Bitcoin wallets via ECDSA).

**Sign the manifest, not each binary.** The manifest contains every artifact's checksum,
so one signature covers the whole release and rollback attacks on individual files become
impossible.

Key distribution is the real problem, and it has no cryptographic solution. Options:
embed the registry's public key in the Gecko binary (TOFU on install, no rotation without
a Gecko update), fetch it over TLS (trusts the CA system), or implement TUF (correct, and
a large project).

**Decision: embed the key, plus `--trusted-key` to add others, plus a documented rotation
procedure.** Name the limitation in the security policy: a compromised registry key is
not recoverable without a Gecko release. That's the honest position, and it's the same
position `apt` and `homebrew` are in.

### Threat model, written down

| Threat | Defence | Residual risk |
|---|---|---|
| MITM on download | HTTPS with cert verification | CA compromise |
| Corrupted download | SHA-256 against signed manifest | none material |
| Compromised CDN | Signature on manifest | none material |
| Compromised registry key | — | **unmitigated**: requires Gecko release to rotate |
| Malicious publisher | — | **unmitigated**: no review process; same as npm |
| Downgrade attack | Refuse to install older than installed without `--allow-downgrade` | user override |
| Typosquatting | Suggest close matches; warn on low download count | user judgement |
| Plugin does something bad at runtime | — | **unmitigated by design**: no sandbox |

Writing the unmitigated rows down is the point. **A threat model that lists only the
threats you defeat is marketing.**

---

## D. Design

### SDK API

```go
package sdk

// Plugin is a Gecko plugin. Construct with New and register commands
// before calling Run.
type Plugin struct{ /* unexported */ }

func New(name string, opts ...Option) *Plugin

type Option func(*Plugin)
func WithVersion(v string) Option
func WithDescription(d string) Option
func WithHomepage(url string) Option

func (p *Plugin) Command(c Command)
func (p *Plugin) Run()            // handles argv, metadata, dispatch, exit
func (p *Plugin) Main() int       // same, but returns instead of exiting

type Command struct {
    Name        string
    Description string
    Usage       string
    Flags       func(*flag.FlagSet)
    Run         func(*Context) error

    _ struct{}
}

// Context carries per-invocation state.
type Context struct {
    Args   []string
    Stdin  io.Reader
    Stdout io.Writer
    Stderr io.Writer

    // Host reports the Gecko version and protocol that invoked us.
    Host HostInfo
}

func (c *Context) Printf(format string, a ...any)
func (c *Context) Errorf(format string, a ...any)
func (c *Context) Confirm(prompt string) (bool, error)
func (c *Context) IsTerminal() bool
```

Both `Run` (exits) and `Main` (returns an int) exist: `Run` for the 99% case, `Main` for
testability. Chapter 1's single-exit-point rule applies to plugin authors too.

`Context` is a **struct, not an interface**. Adding `Context.Style()` later is
non-breaking; adding a method to an interface would break every implementor. This is the
concrete-over-interface rule applied where it counts.

### Registry format

Static JSON files on any HTTP server. No API, no database, no server to run:

```
https://registry.gecko.dev/
  index.json                    # all plugin names and latest versions
  plugins/docker.json           # every version of docker
  plugins/docker.json.sig       # ed25519 signature of the above
```

`plugins/docker.json`:

```json
{
  "name": "docker",
  "description": "Docker utilities",
  "repository": "https://github.com/someone/gecko-docker",
  "versions": [
    {
      "version": "v1.2.0",
      "published": "2026-01-15T10:00:00Z",
      "min_protocol": 1,
      "artifacts": [
        {
          "os": "linux", "arch": "amd64",
          "url": "https://github.com/someone/gecko-docker/releases/download/v1.2.0/gecko-docker_linux_amd64.tar.gz",
          "sha256": "4e07408562bedb8b60ce05c1decfe3ad16b72230967de01f640b7e4729b49fce",
          "size": 4404019
        }
      ]
    }
  ]
}
```

Static files mean the registry can be a GitHub Pages site or an S3 bucket, it's trivially
cacheable and mirrorable, and there is no server to compromise beyond the signing key.
For a project at this scale that's strictly better than a service.

---

## E. Implementation

### `sdk/plugin.go`

```go
// Package sdk provides the framework for writing Gecko plugins.
//
// A minimal plugin:
//
//	func main() {
//	    p := sdk.New("hello", sdk.WithVersion("1.0.0"))
//	    p.Command(sdk.Command{
//	        Name:        "greet",
//	        Description: "Print a greeting",
//	        Run: func(c *sdk.Context) error {
//	            c.Printf("Hello, %s!\n", c.Arg(0, "world"))
//	            return nil
//	        },
//	    })
//	    p.Run()
//	}
//
// # Compatibility
//
// This package follows semantic import versioning. Within a major
// version, exported identifiers will not be removed or change meaning.
// Exported structs contain an unexported field, so they must be
// constructed with keyed literals; this allows fields to be added
// without breaking callers.
package sdk

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

// Protocol is the plugin protocol version this SDK implements.
const Protocol = 1

const metaArg = "__gecko_meta"

type Plugin struct {
	name        string
	version     string
	description string
	homepage    string

	commands map[string]Command
	order    []string

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	_ struct{}
}

type Option func(*Plugin)

func WithVersion(v string) Option     { return func(p *Plugin) { p.version = v } }
func WithDescription(d string) Option { return func(p *Plugin) { p.description = d } }
func WithHomepage(u string) Option    { return func(p *Plugin) { p.homepage = u } }

// WithStreams overrides the standard streams. Intended for tests.
func WithStreams(in io.Reader, out, errOut io.Writer) Option {
	return func(p *Plugin) { p.stdin, p.stdout, p.stderr = in, out, errOut }
}

func New(name string, opts ...Option) *Plugin {
	p := &Plugin{
		name:     name,
		version:  "0.0.0",
		commands: make(map[string]Command),
		stdin:    os.Stdin,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

type Command struct {
	Name        string
	Description string
	Usage       string
	Flags       func(*flag.FlagSet)
	Run         func(*Context) error

	// Prevents unkeyed composite literals so that fields can be added
	// in a future minor version without breaking callers.
	_ struct{}
}

func (p *Plugin) Command(c Command) {
	if _, dup := p.commands[c.Name]; !dup {
		p.order = append(p.order, c.Name)
	}
	p.commands[c.Name] = c
}

// Run executes the plugin and exits the process.
func (p *Plugin) Run() { os.Exit(p.Main()) }

// Main executes the plugin and returns the exit code.
//
// It exists alongside Run so that a plugin's behaviour can be tested
// without spawning a process or intercepting os.Exit.
func (p *Plugin) Main() int {
	args := os.Args[1:]

	// The metadata probe. Output must be JSON on stdout and nothing
	// else: Gecko parses stdout strictly, and a stray Println here is
	// the single most common plugin bug.
	if len(args) == 1 && args[0] == metaArg {
		if err := p.writeMetadata(p.stdout); err != nil {
			fmt.Fprintln(p.stderr, err)
			return 1
		}
		return 0
	}

	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		p.usage(p.stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}

	cmd, ok := p.commands[args[0]]
	if !ok {
		fmt.Fprintf(p.stderr, "%s: unknown command %q\n", p.name, args[0])
		p.usage(p.stderr)
		return 2
	}

	fs := flag.NewFlagSet(p.name+" "+cmd.Name, flag.ContinueOnError)
	fs.SetOutput(p.stderr)
	if cmd.Flags != nil {
		cmd.Flags(fs)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	ctx := &Context{
		Args:   fs.Args(),
		Stdin:  p.stdin,
		Stdout: p.stdout,
		Stderr: p.stderr,
		Host:   hostInfo(),
	}

	if err := cmd.Run(ctx); err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			if ee.Err != nil {
				fmt.Fprintf(p.stderr, "%s: %v\n", p.name, ee.Err)
			}
			return ee.Code
		}
		fmt.Fprintf(p.stderr, "%s: %v\n", p.name, err)
		return 1
	}
	return 0
}

func (p *Plugin) writeMetadata(w io.Writer) error {
	type commandMeta struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Usage       string `json:"usage,omitempty"`
	}
	meta := struct {
		Protocol    int           `json:"protocol"`
		Name        string        `json:"name"`
		Version     string        `json:"version"`
		Description string        `json:"description"`
		Homepage    string        `json:"homepage,omitempty"`
		Commands    []commandMeta `json:"commands"`
	}{
		Protocol: Protocol, Name: p.name, Version: p.version,
		Description: p.description, Homepage: p.homepage,
	}

	names := append([]string(nil), p.order...)
	sort.Strings(names)
	for _, n := range names {
		c := p.commands[n]
		meta.Commands = append(meta.Commands, commandMeta{c.Name, c.Description, c.Usage})
	}

	enc := json.NewEncoder(w)
	return enc.Encode(meta)
}

// HostInfo describes the Gecko that invoked this plugin, letting a
// plugin adapt to an older host.
type HostInfo struct {
	Version  string
	Protocol int

	_ struct{}
}

func hostInfo() HostInfo {
	proto, _ := strconv.Atoi(os.Getenv("GECKO_PROTOCOL_VERSION"))
	return HostInfo{Version: os.Getenv("GECKO_VERSION"), Protocol: proto}
}
```

### `internal/plugin/install.go`

```go
package plugin

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// maxPluginSize bounds a download. Without it, a hostile or broken
// server can fill the user's disk.
const maxPluginSize = 200 << 20 // 200 MiB

// Install places a verified plugin binary at its final path.
//
// The ordering below is security-critical and is not merely tidy:
//
//	1. write to a temporary file in the destination directory
//	2. verify the checksum
//	3. verify the signature
//	4. set the executable bit
//	5. fsync the file
//	6. rename into place
//	7. fsync the directory
//
// Verification happens before the file is executable and before it
// occupies its final name, so there is no window in which a concurrent
// "gecko docker ps" could execute unverified bytes. The temporary file
// is created in the destination directory because rename(2) is only
// atomic within a filesystem; /tmp is frequently a different mount, in
// which case os.Rename degrades to copy-and-delete.
func Install(ctx context.Context, src io.Reader, spec InstallSpec, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(destDir, ".gecko-install-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	// Cleanup on every failure path below.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	// 1. Copy, hashing as we go and bounding the size.
	h := sha256.New()
	limited := io.LimitReader(src, maxPluginSize+1)
	n, err := io.Copy(io.MultiWriter(tmp, h), limited)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if n > maxPluginSize {
		return "", fmt.Errorf("plugin exceeds the %d byte limit", maxPluginSize)
	}
	if spec.Size > 0 && n != spec.Size {
		return "", fmt.Errorf("size mismatch: got %d bytes, manifest says %d", n, spec.Size)
	}

	// 2. Checksum. Constant-time because the expected value can be
	// influenced by whoever controls the manifest.
	got := h.Sum(nil)
	want, err := hex.DecodeString(spec.SHA256)
	if err != nil {
		return "", fmt.Errorf("manifest checksum is not valid hex: %w", err)
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return "", &ChecksumError{Want: spec.SHA256, Got: hex.EncodeToString(got)}
	}

	// 3. Signature over the manifest, verified by the caller before we
	// get here; this asserts that contract loudly rather than trusting
	// it silently.
	if !spec.ManifestVerified && !spec.AllowUnsigned {
		return "", errors.New("refusing to install from an unverified manifest")
	}

	// 4. Executable bit before the file is reachable by name.
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o755); err != nil {
			return "", err
		}
	}

	// 5. Flush to disk before the rename, so a crash cannot leave the
	// final name pointing at an incompletely written file.
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	// 6. Rename into place.
	final := filepath.Join(destDir, executableName(spec.Name))
	if err := os.Rename(tmpName, final); err != nil {
		if runtime.GOOS == "windows" {
			return "", fmt.Errorf("could not replace %s; is the plugin currently running? (%w)", final, err)
		}
		return "", err
	}

	// 7. fsync the directory so the rename itself is durable. There is
	// no equivalent on Windows, where the filesystem provides different
	// guarantees.
	if runtime.GOOS != "windows" {
		if d, err := os.Open(destDir); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return final, nil
}

type ChecksumError struct{ Want, Got string }

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("checksum mismatch: manifest says %s, downloaded file is %s "+
		"(the download may be corrupt, or the artifact may have been tampered with)",
		e.Want, e.Got)
}

func executableName(name string) string {
	n := "gecko-" + name
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}
```

### Manifest signature verification

```go
// registryPublicKey is the Gecko plugin registry's signing key, embedded
// at build time.
//
// Limitation, stated plainly: rotating this key requires a new Gecko
// release. A compromise of the corresponding private key cannot be
// recovered from by any mechanism available to an already-installed
// Gecko. The same is true of apt's and Homebrew's trust roots; solving
// it properly requires TUF, which is out of scope for this project.
//
// Users can add their own trusted keys with --trusted-key, which is the
// supported path for private registries.
var registryPublicKey = ed25519.PublicKey{ /* 32 bytes */ }

// VerifyManifest checks an ed25519 signature over the raw manifest bytes.
//
// The signature covers the manifest rather than each artifact. That is
// deliberate: one signature then attests to every artifact's checksum
// simultaneously, which prevents mix-and-match attacks where an
// adversary serves version 1.0.0's signed binary in response to a
// request for 1.3.0.
func VerifyManifest(raw, sig []byte, keys []ed25519.PublicKey) error {
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, expected %d", len(sig), ed25519.SignatureSize)
	}
	for _, k := range keys {
		// ed25519.Verify is constant-time internally.
		if ed25519.Verify(k, raw, sig) {
			return nil
		}
	}
	return errors.New("manifest signature does not verify against any trusted key")
}
```

### Version resolution

```go
package plugin

import (
	"fmt"
	"runtime"
	"sort"

	"golang.org/x/mod/semver"
)

// Resolve selects the artifact to install.
//
// semver.Compare is used rather than the simplified comparator in
// internal/doctor because the ordering rules matter here: a prerelease
// sorts before its release (v1.0.0-beta < v1.0.0) and build metadata is
// ignored for comparison. Getting either wrong means installing the
// wrong version.
func Resolve(m *Manifest, constraint string) (*Artifact, *Version, error) {
	versions := make([]Version, 0, len(m.Versions))
	for _, v := range m.Versions {
		if !semver.IsValid(v.Version) {
			continue // ignore malformed entries rather than failing the whole resolve
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		return nil, nil, fmt.Errorf("plugin %q has no valid versions", m.Name)
	}

	sort.Slice(versions, func(i, j int) bool {
		return semver.Compare(versions[i].Version, versions[j].Version) > 0
	})

	var chosen *Version
	switch constraint {
	case "", "latest":
		// Skip prereleases unless nothing else exists.
		for i := range versions {
			if semver.Prerelease(versions[i].Version) == "" {
				chosen = &versions[i]
				break
			}
		}
		if chosen == nil {
			chosen = &versions[0]
		}
	default:
		want := constraint
		if !semver.IsValid(want) {
			want = "v" + want // accept "1.2.0" as well as "v1.2.0"
		}
		if !semver.IsValid(want) {
			return nil, nil, fmt.Errorf("%q is not a valid version", constraint)
		}
		for i := range versions {
			if semver.Compare(versions[i].Version, want) == 0 {
				chosen = &versions[i]
				break
			}
		}
		if chosen == nil {
			return nil, nil, fmt.Errorf("version %s not found; available: %s",
				want, listVersions(versions))
		}
	}

	if chosen.MinProtocol > ProtocolVersion {
		return nil, nil, fmt.Errorf(
			"%s %s requires plugin protocol %d, but this Gecko supports %d; upgrade Gecko",
			m.Name, chosen.Version, chosen.MinProtocol, ProtocolVersion)
	}

	for i := range chosen.Artifacts {
		a := &chosen.Artifacts[i]
		if a.OS == runtime.GOOS && a.Arch == runtime.GOARCH {
			return a, chosen, nil
		}
	}
	return nil, nil, fmt.Errorf("%s %s has no build for %s/%s (available: %s)",
		m.Name, chosen.Version, runtime.GOOS, runtime.GOARCH, listPlatforms(chosen.Artifacts))
}
```

### Scaffolding with `go:embed`

```go
package plugin

import (
	"embed"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Templates for "gecko plugin create".
//
// "all:" is required because the tree contains a .github directory, and
// the default embed patterns exclude entries beginning with "." or "_".
//
//go:embed all:templates
var scaffoldFS embed.FS

type scaffoldData struct {
	Name       string // "mytool"
	BinaryName string // "gecko-mytool"
	Module     string
	SDKVersion string
	Year       int
}

// Scaffold writes a new plugin project.
func Scaffold(dir string, data scaffoldData) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	return fs.WalkDir(scaffoldFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "templates/")
		// Templates are named *.tmpl so that "go build" and "go vet" do
		// not try to compile the embedded Go sources.
		target := filepath.Join(dir, strings.TrimSuffix(rel, ".tmpl"))

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		src, err := scaffoldFS.ReadFile(p)
		if err != nil {
			return err
		}
		// text/template, not html/template: the latter escapes for HTML
		// and would corrupt Go source.
		t, err := template.New(rel).Parse(string(src))
		if err != nil {
			return err
		}
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		defer f.Close()
		return t.Execute(f, data)
	})
}
```

---

## F. Exercise

1. Write the scaffold templates: `main.go.tmpl`, `go.mod.tmpl`, `README.md.tmpl`,
   `.goreleaser.yaml.tmpl`, `.github/workflows/release.yml.tmpl`. The GitHub Actions one
   should cross-compile for all six targets and publish a signed manifest.

2. Build the registry generator: a tool that takes a directory of release artifacts,
   computes checksums, emits `plugins/<name>.json`, and signs it. Then host it on GitHub
   Pages and install from it for real.

3. **Downgrade protection.** `gecko plugin install docker --version 1.0.0` when 1.3.0 is
   installed: should it work? Implement your decision, including the escape hatch. Then
   consider: does refusing actually protect anyone, given the user typed the version
   explicitly?

4. **Key rotation.** Design a procedure that doesn't require a Gecko release. Read the
   TUF specification's root-rotation section first. Write up why you did or didn't
   implement it — a well-argued "we chose not to, here's the residual risk" is a valid
   and professional answer.

5. **Adversarial installation tests.** Write a malicious registry that: serves a
   different binary than the manifest describes; serves a 10 GB response; serves a
   manifest signed with the wrong key; serves a valid old version's manifest for a new
   version request; redirects the artifact URL to `file:///etc/passwd`. Verify each is
   caught. (That last one — check whether your HTTP client follows non-http schemes.)

---

## G. Testing

### Verification tests are the ones that matter

```go
func TestInstallRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("#!/bin/sh\necho pwned\n")

	spec := InstallSpec{
		Name:             "evil",
		SHA256:           strings.Repeat("00", 32), // wrong
		ManifestVerified: true,
	}
	_, err := Install(context.Background(), bytes.NewReader(content), spec, dir)

	var ce *ChecksumError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ChecksumError", err)
	}
	// The critical assertion: nothing was left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		var names []string
		for _, e := range entries { names = append(names, e.Name()) }
		t.Errorf("files left after a failed install: %v", names)
	}
}

func TestInstallRejectsOversizedDownload(t *testing.T) {
	dir := t.TempDir()
	// An endless reader: without the size limit this fills the disk.
	endless := io.LimitReader(rand.Reader, maxPluginSize+1<<20)

	_, err := Install(context.Background(), endless,
		InstallSpec{Name: "big", ManifestVerified: true}, dir)
	if err == nil {
		t.Fatal("accepted an oversized download")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want a size-limit error", err)
	}
}

func TestInstallRefusesUnverifiedManifest(t *testing.T) {
	dir := t.TempDir()
	content := []byte("binary")
	sum := sha256.Sum256(content)

	_, err := Install(context.Background(), bytes.NewReader(content), InstallSpec{
		Name:             "x",
		SHA256:           hex.EncodeToString(sum[:]),
		ManifestVerified: false, // correct checksum, unverified manifest
	}, dir)
	if err == nil {
		t.Fatal("installed from an unverified manifest")
	}
}
```

That second-to-last test is the one that distinguishes integrity from authenticity: the
checksum is *correct* and the install is still refused, because a correct checksum from
an unsigned manifest proves nothing.

### Signature tests

```go
func TestVerifyManifest(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)

	manifest := []byte(`{"name":"docker","versions":[]}`)
	sig := ed25519.Sign(priv, manifest)

	t.Run("valid", func(t *testing.T) {
		if err := VerifyManifest(manifest, sig, []ed25519.PublicKey{pub}); err != nil {
			t.Error(err)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		if err := VerifyManifest(manifest, sig, []ed25519.PublicKey{other}); err == nil {
			t.Error("verified against the wrong key")
		}
	})
	t.Run("tampered manifest", func(t *testing.T) {
		bad := append([]byte(nil), manifest...)
		bad[10] ^= 0xff
		if err := VerifyManifest(bad, sig, []ed25519.PublicKey{pub}); err == nil {
			t.Error("verified a tampered manifest")
		}
	})
	t.Run("truncated signature", func(t *testing.T) {
		if err := VerifyManifest(manifest, sig[:32], []ed25519.PublicKey{pub}); err == nil {
			t.Error("accepted a truncated signature")
		}
	})
	t.Run("multiple keys, one matches", func(t *testing.T) {
		if err := VerifyManifest(manifest, sig, []ed25519.PublicKey{other, pub}); err != nil {
			t.Error(err)
		}
	})
}
```

### End-to-end against a fake registry

```go
func TestInstallFromRegistry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	binary := []byte("#!/bin/sh\necho hi\n")
	sum := sha256.Sum256(binary)

	mux := http.NewServeMux()
	var manifestJSON []byte

	mux.HandleFunc("/artifacts/gecko-test", func(w http.ResponseWriter, r *http.Request) {
		w.Write(binary)
	})
	mux.HandleFunc("/plugins/test.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write(manifestJSON)
	})
	mux.HandleFunc("/plugins/test.json.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write(ed25519.Sign(priv, manifestJSON))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	manifestJSON, _ = json.Marshal(Manifest{
		Name: "test",
		Versions: []Version{{
			Version:     "v1.0.0",
			MinProtocol: 1,
			Artifacts: []Artifact{{
				OS: runtime.GOOS, Arch: runtime.GOARCH,
				URL:    srv.URL + "/artifacts/gecko-test",
				SHA256: hex.EncodeToString(sum[:]),
				Size:   int64(len(binary)),
			}},
		}},
	})

	dir := t.TempDir()
	client := NewRegistryClient(srv.URL, []ed25519.PublicKey{pub})
	path, err := client.Install(context.Background(), "test", "latest", dir)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, binary) {
		t.Error("installed content differs from the served artifact")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode()&0o111 == 0 {
			t.Error("installed plugin is not executable")
		}
	}
}
```

Then mutate the fake registry to be hostile and assert each attack fails. The test
infrastructure you just built makes each additional attack about five lines.

### SDK conformance

```go
// TestSDKProducesValidMetadata verifies that a plugin built with the SDK
// satisfies the parser in internal/plugin. Without this, the SDK and the
// host can drift apart and nobody notices until a user reports it.
func TestSDKProducesValidMetadata(t *testing.T) {
	var buf bytes.Buffer
	p := sdk.New("test",
		sdk.WithVersion("1.0.0"),
		sdk.WithDescription("Test plugin"),
		sdk.WithStreams(nil, &buf, io.Discard))
	p.Command(sdk.Command{Name: "foo", Description: "Do foo", Run: func(*sdk.Context) error { return nil }})

	os.Args = []string{"gecko-test", "__gecko_meta"}
	if code := p.Main(); code != 0 {
		t.Fatalf("metadata probe exited %d", code)
	}

	// Parse with the host's own parser and validator.
	var m plugin.Metadata
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("host cannot parse SDK metadata: %v\n%s", err, buf.String())
	}
	if err := plugin.ValidateMeta(&m, "test"); err != nil {
		t.Fatalf("host rejects SDK metadata: %v", err)
	}
}
```

This test is the contract between the two halves of the system, and it belongs in CI.

### API compatibility checking

```bash
go install golang.org/x/exp/cmd/gorelease@latest
gorelease -base=v0.1.0
```

`gorelease` compares your working tree against a released version and reports whether the
changes are compatible, and what version number they imply. Run it on every release of
`sdk/`. It will catch the "I added a method to an exported interface" mistake before your
users do.

---

## H. Review

- `internal/` vs a public package, and exactly which changes Go considers breaking.
- Concrete types over interfaces in public APIs; functional options; the `_ struct{}`
  trick for future-proofing structs.
- `go:embed` patterns, the dot-file exclusion, and why `all:` and `.tmpl` matter.
- `x/mod/semver`: prereleases sort before releases, build metadata is ignored.
- The seven-step atomic install, and why verification must precede both chmod and rename.
- Same-filesystem temp files; `fsync` on file and directory; the Windows differences.
- Checksums give integrity, signatures give authenticity, and neither gives the other.
- ed25519 over ECDSA: deterministic signing, no nonce catastrophe, small keys.
- Signing the manifest, not the artifact, to prevent mix-and-match.
- Key distribution has no cryptographic solution; TUF exists; documenting the residual
  risk is the professional minimum.
- **A threat model that lists only defeated threats is marketing.**

---

## I. Refactoring

Chapter 6's `compareVersions` is now dead weight and, worse, wrong in ways that matter
(prerelease ordering). Delete it; use `semver.Compare` everywhere. Two consequences:
`doctor`'s `MinVersion` strings need a `v` prefix, and the tool-version strings need
normalising (`go version` gives `1.24.0`, semver wants `v1.24.0`).

**Deleting code you wrote earlier because a dependency now does it better is a good
outcome, not an admission of waste.** You understand what `semver.Compare` does because
you wrote a worse one, and you can now read its source and recognise every case it
handles that yours didn't.

Second: `internal/plugin` is now discovery + protocol + cache + install + registry +
scaffold. That's six responsibilities and about 1,500 lines. Split:

```
internal/plugin/          # discovery, protocol, execution — the runtime half
internal/plugin/registry/ # manifests, resolution, download, verification
internal/plugin/scaffold/ # go:embed templates and generation
```

The trigger: `registry` and `scaffold` are used only by `gecko plugin install|create`,
while the core is used by every invocation. Splitting means the common path doesn't
compile in the templates or the HTTP client. That's a real benefit (binary size, build
time, and a smaller surface for the code that runs on every command), not an aesthetic
one.

---

## Commit

```
feat(sdk): add public plugin SDK with functional options
feat: add plugin scaffolding with embedded templates
feat: add semver-based version resolution
feat: add atomic plugin installation with checksum verification
feat: add ed25519 manifest signature verification
feat: add registry client and plugin install/update/remove
test: add adversarial registry and installation tests
docs: add plugin development guide and security policy
refactor: split plugin package into runtime, registry and scaffold
```

The `sdk` scope marker matters: `sdk/` is versioned as part of the module but has
different compatibility obligations, and scoped commits make its history filterable.

Next: `15-production.md`.

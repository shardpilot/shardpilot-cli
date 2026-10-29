// Package cli implements the shardpilot command dispatch.
//
// The command tree is hand-rolled on stdlib flag — matching the org's
// stdlib-only bias for SDK-adjacent repos — with one FlagSet per subcommand.
package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
)

// version is the CLI version, also sent as the User-Agent suffix.
// version is overridden at release time by the linker:
//
//	go build -ldflags "-X github.com/shardpilot/shardpilot-cli/internal/cli.version=v1.2.3"
//
// It is a var rather than a const for exactly that reason — `-X` cannot write
// to a const, and a build that silently could not stamp it would ship binaries
// reporting the development version under a release tag, in `shardpilot
// version` and in every User-Agent.
var version = developmentVersion

const developmentVersion = "0.2.0-dev"

// resolveVersion returns the linker-stamped version when there is one, and
// otherwise falls back to the module version Go records in the binary.
//
// WHY THE FALLBACK EXISTS. `go install module/cmd/x@vX.Y.Z` compiles the
// package directly — it never runs this repository's Makefile and therefore
// never applies `-ldflags -X`. Without this, every binary a user installs the
// documented way reports the development version from `shardpilot version`
// and in its User-Agent, contradicting the exact version they asked for.
//
// debug.ReadBuildInfo gives `(devel)` for a build from a working tree and the
// real module version for a `go install module@version`, which is precisely
// the case the linker cannot reach.
func resolveVersion() string {
	if version != developmentVersion {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

// Environment variable names.
const (
	// envSymbolsKey is the ONLY way to pass the API key. It is deliberately
	// not a flag: secrets on the command line leak into argv, process
	// listings, and shell history.
	envSymbolsKey = "SHARDPILOT_SYMBOLS_KEY"
	// envSymbolsURL overrides the symbols endpoint base URL; --url overrides
	// it in turn.
	envSymbolsURL = "SHARDPILOT_SYMBOLS_URL"
)

// defaultSymbolsURL is the public symbols host, used when neither --url nor
// SHARDPILOT_SYMBOLS_URL is set. It is a var only so tests can point it at a
// local stub.
var defaultSymbolsURL = "https://symbols.shardpilot.com"

// endpointBaseURL resolves the symbols endpoint base URL: the --url flag, then
// SHARDPILOT_SYMBOLS_URL, then the public symbols host. It returns the base
// URL without a trailing slash and its host for messages.
//
// Plain http is refused unless the host is localhost or a loopback address:
// every request carries the API key, and over http anyone on the path reads
// it. The check runs before any request is made.
func endpointBaseURL(flagValue string, getenv func(string) string) (base, host string, err error) {
	raw := flagValue
	if raw == "" {
		raw = getenv(envSymbolsURL)
	}
	if raw == "" {
		raw = defaultSymbolsURL
	}
	parsed, perr := url.Parse(raw)
	if perr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", fmt.Errorf("invalid endpoint URL %q: need https://host[:port]", raw)
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", "", fmt.Errorf("refusing plain http to %s: the API key would be sent unencrypted; use https (http is allowed only to localhost or a loopback address)", parsed.Host)
	}
	return strings.TrimRight(raw, "/"), parsed.Host, nil
}

// newHTTPClient returns the client both commands use. It never follows a
// redirect: Go keeps the Authorization header on a redirect to the same
// hostname, whatever the port or scheme, so following one could carry the API
// key past the https check in endpointBaseURL. The symbols routes never
// redirect, so a 3xx comes back as the response itself (see notServed).
func newHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// notServed reports whether status means the URL that was set does not serve
// the endpoint at method path — a 404, which the symbols routes never answer
// themselves, or a redirect, which is never followed — and says so.
func notServed(host, method, path string, status int) (string, bool) {
	switch {
	case status == http.StatusNotFound:
		return fmt.Sprintf("%s does not serve %s %s (http 404); nothing is wrong with your API key or your symbols", host, method, path), true
	case status >= 300 && status < 400:
		return fmt.Sprintf("%s answered %s %s with a redirect (http %d); the CLI does not follow redirects, so the API key goes only to the URL you set: point --url or %s at the endpoint itself", host, method, path, status, envSymbolsURL), true
	}
	return "", false
}

// isLoopbackHost reports whether host is localhost or a loopback IP address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Exit codes. Typed and stable so CI scripts can branch on them.
const (
	// ExitOK: everything uploaded.
	ExitOK = 0
	// ExitUsage: bad flags/arguments, missing environment, unreadable
	// file, undetectable or not-yet-live format — nothing reached the
	// server, or the request was never worth sending.
	ExitUsage = 1
	// ExitRejected: the server said no (a 4xx other than 404: auth,
	// validation, size cap). Never retried.
	ExitRejected = 2
	// ExitUnavailable: 5xx after retries, network failure, or timeout.
	ExitUnavailable = 3
	// ExitNotServed: the URL does not serve the endpoint — the host answered
	// 404, or a redirect, which is never followed.
	ExitNotServed = 4
)

const rootUsage = `usage: shardpilot <command> [arguments]

commands:
  symbols upload   upload debug symbol files to the ShardPilot symbol-upload endpoint
  symbols missing  list the debug symbol files your crashes needed and did not find
  version          print the CLI version
  help             show this help

Run "shardpilot symbols upload -h" or "shardpilot symbols missing -h" for a
command's flags, environment variables, and exit codes.
`

// Run executes the CLI and returns its process exit code. getenv, stdout and
// stderr are injected so tests drive the real command path end-to-end.
func Run(argv []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprint(stderr, rootUsage)
		return ExitUsage
	}
	switch argv[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, rootUsage)
		return ExitOK
	case "version", "--version":
		fmt.Fprintf(stdout, "shardpilot %s\n", resolveVersion())
		return ExitOK
	case "symbols":
		if len(argv) >= 2 && argv[1] == "upload" {
			return runSymbolsUpload(argv[2:], getenv, stdout, stderr)
		}
		if len(argv) >= 2 && argv[1] == "missing" {
			return runSymbolsMissing(argv[2:], getenv, stdout, stderr)
		}
		if len(argv) >= 2 {
			fmt.Fprintf(stderr, "shardpilot: unknown symbols command %q\n\n", argv[1])
		}
		fmt.Fprint(stderr, rootUsage)
		return ExitUsage
	default:
		fmt.Fprintf(stderr, "shardpilot: unknown command %q\n\n", argv[0])
		fmt.Fprint(stderr, rootUsage)
		return ExitUsage
	}
}

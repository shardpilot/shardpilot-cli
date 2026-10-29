package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shardpilot/shardpilot-cli/internal/symbols"
)

const missingUsage = `usage: shardpilot symbols missing [flags]

Lists the debug symbol files your crashes needed and did not find
(GET /api/v1/symbols/wanted): one line per identity, with the platform,
format and debug id an upload would carry. By default only identities that
are still missing are listed; --all adds the ones a later upload satisfied.
Every page is read. Transient failures (5xx, network) are retried 4 times
with backoff.

flags:
`

const missingUsageTrailer = `
environment:
  SHARDPILOT_SYMBOLS_KEY API key with the symbols:write scope (required; env
                         only).
  SHARDPILOT_SYMBOLS_URL Endpoint base URL when --url is not given
                         (default: https://symbols.shardpilot.com). Plain
                         http is refused unless the host is localhost or a
                         loopback address.

exit codes:
  0  listed (the list may be empty)
  1  usage or local error (bad flags, plain http to a remote host)
  2  the server rejected the request (4xx other than 404: auth, validation)
  3  server or network failure (5xx after retries, timeout)
  4  this URL does not serve the endpoint (it answered 404, or a redirect,
     which is never followed)
`

func runSymbolsMissing(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shardpilot symbols missing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		baseURL = fs.String("url", "", "endpoint base URL (default: $SHARDPILOT_SYMBOLS_URL, then https://symbols.shardpilot.com)")
		all     = fs.Bool("all", false, "also list identities a later upload satisfied")
		jsonOut = fs.Bool("json", false, "machine-readable output: one JSON object per identity on stdout")
		timeout = fs.Duration("timeout", 2*time.Minute, "timeout for the whole listing")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, missingUsage)
		fs.PrintDefaults()
		fmt.Fprint(stderr, missingUsageTrailer)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	usagef := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "shardpilot symbols missing: "+format+"\n", args...)
		return ExitUsage
	}
	if fs.NArg() > 0 {
		return usagef("takes no arguments (saw %q); run with -h for usage", fs.Arg(0))
	}
	base, host, err := endpointBaseURL(*baseURL, getenv)
	if err != nil {
		return usagef("%v", err)
	}
	apiKey := getenv(envSymbolsKey)
	if apiKey == "" {
		return usagef("%s is not set (the API key is read from the environment only, never from a flag)", envSymbolsKey)
	}

	resolvedVersion := resolveVersion()
	identity, err := sdkIdentityForVersion(resolvedVersion)
	if err != nil {
		return usagef("%v", err)
	}
	lister := &symbols.Lister{
		BaseURL:     base,
		APIKey:      apiKey,
		Client:      newHTTPClient(),
		Retries:     symbols.DefaultRetries,
		Backoff:     retryBackoff,
		UserAgent:   "shardpilot-cli/" + resolvedVersion,
		SDKIdentity: identity,
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	listed, missing := 0, 0
	for cursor := ""; ; {
		page, err := lister.Page(ctx, cursor)
		if err != nil {
			return missingFailure(stderr, host, err)
		}
		for _, row := range page.Wanted {
			if row.AvailableSince != nil && !*all {
				continue
			}
			listed++
			if row.AvailableSince == nil {
				missing++
			}
			printWanted(stdout, row, *jsonOut)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	fmt.Fprintf(stderr, "%d missing, %d listed\n", missing, listed)
	return ExitOK
}

// missingFailure maps a failed read to its exit code. The route never answers
// 404 itself and redirects are never followed, so either means the URL does
// not serve it.
func missingFailure(stderr io.Writer, host string, err error) int {
	var apiErr *symbols.APIError
	if errors.As(err, &apiErr) {
		if message, ok := notServed(host, http.MethodGet, symbols.WantedPath, apiErr.StatusCode); ok {
			fmt.Fprintf(stderr, "shardpilot symbols missing: %s\n", message)
			return ExitNotServed
		}
	}
	switch {
	case errors.As(err, &apiErr) && !apiErr.Retryable():
		fmt.Fprintf(stderr, "shardpilot symbols missing: %v\n", err)
		return ExitRejected
	default:
		fmt.Fprintf(stderr, "shardpilot symbols missing: %v\n", err)
		return ExitUnavailable
	}
}

func printWanted(w io.Writer, row symbols.WantedSymbol, asJSON bool) {
	if asJSON {
		line, _ := json.Marshal(row)
		fmt.Fprintf(w, "%s\n", line)
		return
	}
	status := "missing"
	if row.AvailableSince != nil {
		status = "available since " + *row.AvailableSince
	}
	app := ""
	if row.AppKey != "" {
		app = "  app " + row.AppKey
	}
	fmt.Fprintf(w, "%s  %s  %s%s  seen %dx, last %s  %s\n", row.Platform, row.Format, row.SymbolID, app, row.OccurrenceCount, row.LastSeenAt, status)
}

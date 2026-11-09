package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shardpilot/shardpilot-cli/internal/symbols"
)

// retryBackoff overrides the uploader's retry backoff when non-nil. Tests
// set it to zero-delay so the 5xx retry path runs instantly.
var retryBackoff func(retry int) time.Duration

// singleRequestMaxFileBytes is the largest file the single-request door
// carries: its 64 MiB cap bounds the whole multipart body, so 1 MiB is left
// for the frame. A variable only so tests can exercise the lane with small
// files.
var singleRequestMaxFileBytes int64 = 63 << 20

// lanePoll overrides the lane's status poll interval when non-nil (tests).
var lanePoll func(n int) time.Duration

const uploadUsage = `usage: shardpilot symbols upload [flags] <path>...

Uploads debug symbol files to the ShardPilot symbol-upload endpoint
(POST /api/v1/symbols/upload). Formats: breakpad (.sym), dsym (zipped dSYM
bundle), pdb, elf (.so), proguard (an R8/ProGuard mapping) and il2cpp_map.
proguard and il2cpp_map are never detected: name them with --format. Files
are streamed — multi-GiB symbol files never need to fit in memory — and
uploaded serially, one result line per file. Flags must come before file
paths.

A pdb or elf file too large for one request (over 63 MiB) goes through the
direct multipart lane (POST /api/v1/symbols/upload-sessions): its parts go
straight to object storage, --timeout bounds each 64 MiB part, and the CLI
waits for the server to verify the file. A larger file in any other format,
or any file over 2 GiB, is refused before a request (upload_too_large).

Transient failures (5xx, network) are retried 4 times with backoff; 4xx
rejections are never retried.

flags:
`

const uploadUsageTrailer = `
environment:
  SHARDPILOT_SYMBOLS_KEY API key with the symbols:write scope (required).
                         Deliberately environment-only, never a flag: secrets
                         on the command line leak into argv, process
                         listings, and shell history.
  SHARDPILOT_SYMBOLS_URL Endpoint base URL when --url is not given
                         (default: https://symbols.shardpilot.com). Plain
                         http is refused unless the host is localhost or a
                         loopback address.
  SHARDPILOT_SYMBOLS_CHANNEL
                         Build channel when --channel is not given: develop
                         or release. One of the two is required.

exit codes:
  0  every file uploaded
  1  usage or local error (bad flags, plain http to a remote host,
     unreadable file, undetectable format)
  2  the server rejected an upload (4xx other than 404: auth, validation,
     size cap)
  3  server or network failure (5xx after retries, timeout)
  4  this URL does not serve the endpoint (it answered 404, or a redirect,
     which is never followed), or does not serve the multipart lane a
     large file needs

With multiple files the exit code reflects the first failed file.
`

// fileResult is one file's outcome — printed as a text line, or as one JSON
// object per line under --json.
type fileResult struct {
	Path     string                  `json:"path"`
	OK       bool                    `json:"ok"`
	Format   string                  `json:"format,omitempty"`
	Platform string                  `json:"platform,omitempty"`
	DebugID  string                  `json:"debug_id,omitempty"`
	Status   int                     `json:"status,omitempty"`
	Attempts int                     `json:"attempts,omitempty"`
	Symbol   *symbols.UploadedSymbol `json:"symbol,omitempty"`
	// SessionID and Bytes describe a file sent through the multipart lane.
	SessionID string     `json:"session_id,omitempty"`
	Bytes     int64      `json:"bytes,omitempty"`
	Error     *fileError `json:"error,omitempty"`

	exit int
}

type fileError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func runSymbolsUpload(args []string, getenv func(string) string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shardpilot symbols upload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		baseURL  = fs.String("url", "", "endpoint base URL (default: $SHARDPILOT_SYMBOLS_URL, then https://symbols.shardpilot.com)")
		channel  = fs.String("channel", "", "build channel (required): develop or release (default: $SHARDPILOT_SYMBOLS_CHANNEL)")
		platform = fs.String("platform", "", "symbol platform (required): "+strings.Join(symbols.Platforms, ", ")+"; case is ignored, and mac, osx, darwin, win, win64, Windows* target names, linuxarm64, iphone, ipad and ipados are folded")
		format   = fs.String("format", "auto", "symbol format: auto, breakpad, dsym, pdb, elf, proguard or il2cpp_map")
		debugID  = fs.String("debug-id", "", "debug identifier, single file only: required for pdb and il2cpp_map, the app build id for proguard, optional for elf")
		timeout  = fs.Duration("timeout", 5*time.Minute, "upload timeout: per file on the single-request door; per request and per part on the multipart lane, whose session bounds the file")
		jsonOut  = fs.Bool("json", false, "machine-readable output: one JSON object per file on stdout")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, uploadUsage)
		fs.PrintDefaults()
		fmt.Fprint(stderr, uploadUsageTrailer)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	usagef := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "shardpilot symbols upload: "+format+"\n", args...)
		return ExitUsage
	}

	if *timeout <= 0 { // one meaning on both doors: the lane would read it as unset
		return usagef("--timeout must be positive (got %s)", *timeout)
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return usagef("missing symbol file path(s); run with -h for usage")
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "-") {
			return usagef("flags must come before file paths (saw %q after the first path)", p)
		}
	}
	base, host, err := endpointBaseURL(*baseURL, getenv)
	if err != nil {
		return usagef("%v", err)
	}
	apiKey := getenv(envSymbolsKey)
	if apiKey == "" {
		return usagef("%s is not set (the API key is read from the environment only — never from a flag — so it stays out of argv and shell history)", envSymbolsKey)
	}
	if strings.TrimSpace(*platform) == "" {
		return usagef("--platform is required; accepted: %s", strings.Join(symbols.Platforms, ", "))
	}
	sendPlatform, ok := symbols.NormalizePlatform(*platform)
	if !ok {
		return usagef("--platform %q is not a platform the endpoint accepts; accepted: %s", *platform, strings.Join(symbols.Platforms, ", "))
	}
	if sendPlatform != *platform && !*jsonOut {
		fmt.Fprintf(stderr, "shardpilot symbols upload: --platform %q is sent as %q\n", *platform, sendPlatform)
	}
	// A given flag wins over the environment, even when empty; the value is
	// taken exactly as given, and a set but empty variable is not unset.
	channelGiven := false
	fs.Visit(func(f *flag.Flag) { channelGiven = channelGiven || f.Name == "channel" })
	sendChannel, channelSource := *channel, "--channel"
	if !channelGiven {
		sendChannel, channelGiven = lookupEnv(envSymbolsChannel)
		channelSource = envSymbolsChannel
	}
	switch {
	case sendChannel == "develop" || sendChannel == "release":
	case !channelGiven:
		return usagef("--channel is required: develop or release (or set %s)", envSymbolsChannel)
	default:
		return usagef("%s %q is not a build channel; accepted: develop, release", channelSource, sendChannel)
	}
	explicitFormat := strings.ToLower(strings.TrimSpace(*format))
	switch {
	case explicitFormat == "auto":
		explicitFormat = ""
	case symbols.KnownFormat(explicitFormat):
		// ok
	default:
		return usagef("unknown format %q; accepted: auto, breakpad, dsym, pdb, elf, proguard, il2cpp_map", explicitFormat)
	}
	if *debugID != "" && len(paths) > 1 {
		return usagef("--debug-id applies to exactly one file (every symbol file has its own debug id)")
	}

	resolvedVersion := resolveVersion()
	identity, err := sdkIdentityForVersion(resolvedVersion)
	if err != nil {
		return usagef("%v", err)
	}
	uploader := &symbols.Uploader{
		BaseURL:     base,
		APIKey:      apiKey,
		Client:      newHTTPClient(),
		Retries:     symbols.DefaultRetries,
		Backoff:     retryBackoff,
		UserAgent:   "shardpilot-cli/" + resolvedVersion,
		SDKIdentity: identity,
		LanePoll:    lanePoll,
	}

	exitCode := ExitOK
	uploaded := 0
	for _, path := range paths {
		res := uploadOne(uploader, host, path, sendPlatform, sendChannel, explicitFormat, *debugID, *timeout)
		if *jsonOut {
			if err := json.NewEncoder(stdout).Encode(res); err != nil {
				fmt.Fprintf(stderr, "shardpilot symbols upload: write result: %v\n", err)
			}
		} else {
			printResult(stdout, res)
		}
		if res.OK {
			uploaded++
		} else if exitCode == ExitOK {
			exitCode = res.exit
		}
	}
	if !*jsonOut && len(paths) > 1 {
		fmt.Fprintf(stderr, "uploaded %d/%d files\n", uploaded, len(paths))
	}
	return exitCode
}

func uploadOne(uploader *symbols.Uploader, host, path, platform, channel, explicitFormat, debugID string, timeout time.Duration) fileResult {
	res := fileResult{Path: path, Platform: platform, DebugID: debugID}
	fail := func(exit int, code, message string) fileResult {
		res.OK = false
		res.exit = exit
		res.Error = &fileError{Code: code, Message: message}
		return res
	}

	info, err := os.Stat(path)
	if err != nil {
		return fail(ExitUsage, "", err.Error())
	}
	if info.IsDir() {
		return fail(ExitUsage, "", fmt.Sprintf("%s is a directory (zip dSYM bundles before uploading)", path))
	}

	format := explicitFormat
	if format == "" {
		head, err := readHead(path)
		if err != nil {
			return fail(ExitUsage, "", fmt.Sprintf("read %s: %v", path, err))
		}
		format = symbols.DetectFormat(path, head)
		if format == "" {
			return fail(ExitUsage, "", "unable to detect symbol format from content or filename; pass --format (proguard and il2cpp_map are never detected)")
		}
	}
	res.Format = format
	if symbols.RequiresDebugID(format) && debugID == "" {
		return fail(ExitUsage, "", debugIDRequired[format])
	}

	if info.Size() > singleRequestMaxFileBytes {
		if format != symbols.FormatPDB && format != symbols.FormatELF {
			return fail(ExitUsage, "upload_too_large", fmt.Sprintf("%s is %d bytes: too large for one request, and only pdb and elf go through the multipart lane", path, info.Size()))
		}
		if info.Size() > symbols.LaneMaxBytes {
			return fail(ExitUsage, "upload_too_large", fmt.Sprintf("%s is %d bytes: over the multipart lane's 2 GiB", path, info.Size()))
		}
		return uploadThroughLane(uploader, host, res, symbols.LaneRequest{Path: path, Platform: platform,
			Channel: channel, Format: format, DebugID: debugID, PartTimeout: timeout})
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := uploader.Upload(ctx, symbols.Request{
		Path:     path,
		Platform: platform,
		Channel:  channel,
		Format:   format,
		DebugID:  debugID,
	})
	res.Attempts = result.Attempts
	res.Status = result.StatusCode
	if err != nil {
		var apiErr *symbols.APIError
		if errors.As(err, &apiErr) {
			res.Status = apiErr.StatusCode
			if message, ok := notServed(host, http.MethodPost, symbols.UploadPath, apiErr.StatusCode); ok {
				return fail(ExitNotServed, apiErr.Code, message)
			}
			exit := ExitRejected
			if apiErr.StatusCode >= 500 {
				exit = ExitUnavailable
			}
			return fail(exit, apiErr.Code, err.Error())
		}
		return fail(ExitUnavailable, "", err.Error())
	}
	res.OK = true
	if result.Symbol.Filename != "" || result.Symbol.SHA256Hex != "" {
		symbol := result.Symbol
		res.Symbol = &symbol
	}
	return res
}

// uploadThroughLane sends one file through the multipart lane, which bounds
// itself: each part by --timeout, the whole file by its session's expiry.
func uploadThroughLane(uploader *symbols.Uploader, host string, res fileResult, req symbols.LaneRequest) fileResult {
	lane, err := uploader.UploadLane(context.Background(), req)
	res.SessionID = lane.SessionID
	if err == nil {
		res.OK, res.Bytes = true, lane.Size
		return res
	}
	fail := func(exit int, code string) fileResult {
		res.exit, res.Error = exit, &fileError{Code: code, Message: err.Error()}
		return res
	}
	var apiErr *symbols.APIError
	var partErr *symbols.PartError
	var aborted *symbols.LaneAbortedError
	switch {
	case errors.As(err, &aborted):
		return fail(ExitRejected, aborted.Reason)
	case errors.As(err, &apiErr) && apiErr.Code == "symbol_upload_lane_disabled":
		res.Status = apiErr.StatusCode
		err = fmt.Errorf("%s does not serve the multipart upload lane, and the file is too large for one request", host)
		return fail(ExitNotServed, apiErr.Code)
	case errors.As(err, &apiErr):
		res.Status = apiErr.StatusCode
		if message, ok := notServed(host, http.MethodPost, symbols.LanePath, apiErr.StatusCode); ok {
			err = errors.New(message)
			return fail(ExitNotServed, apiErr.Code)
		}
		if apiErr.StatusCode >= 500 {
			return fail(ExitUnavailable, apiErr.Code)
		}
		return fail(ExitRejected, apiErr.Code)
	case errors.As(err, &partErr) && partErr.StatusCode >= 400 && partErr.StatusCode < 500:
		return fail(ExitRejected, partErr.Code)
	}
	return fail(ExitUnavailable, "")
}

// debugIDRequired says why each format that needs --debug-id needs it.
var debugIDRequired = map[string]string{
	symbols.FormatPDB:       "pdb uploads require --debug-id (the endpoint cannot parse PDB metadata yet)",
	symbols.FormatProguard:  "proguard uploads require --debug-id: the app build id your crash reports carry",
	symbols.FormatIL2CPPMap: "il2cpp_map uploads require --debug-id: the debug id of the native module the map belongs to",
}

func printResult(w io.Writer, res fileResult) {
	if !res.OK {
		fmt.Fprintf(w, "FAIL  %s  %s\n", res.Path, res.Error.Message)
		return
	}
	line := fmt.Sprintf("ok    %s  format=%s platform=%s", res.Path, res.Format, res.Platform)
	if res.Symbol != nil {
		if len(res.Symbol.DebugIDs) > 0 {
			line += " debug_id=" + strings.Join(res.Symbol.DebugIDs, ",")
		} else if res.Symbol.DebugID != "" {
			line += " debug_id=" + res.Symbol.DebugID
		}
		if res.Symbol.ByteSize > 0 {
			line += fmt.Sprintf(" bytes=%d", res.Symbol.ByteSize)
		}
	} else if res.DebugID != "" {
		line += " debug_id=" + res.DebugID
	}
	if res.SessionID != "" {
		line += fmt.Sprintf(" bytes=%d session=%s", res.Bytes, res.SessionID)
	}
	fmt.Fprintln(w, line)
}

// readHead returns the file's first symbols.SniffLen bytes (fewer when the
// file is shorter) for format detection — detection never reads the whole
// file.
func readHead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, symbols.SniffLen)
	n, err := io.ReadFull(f, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	return buf[:n], err
}

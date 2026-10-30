package symbols

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UploadPath is the route the client posts to.
const UploadPath = "/api/v1/symbols/upload"

// FormatHintHeader mirrors the endpoint's optional X-Shardpilot-Symbol-Format
// admission hint: naming the format in a HEADER lets the server admit the
// request up to that format's cap before reading a body byte, once its
// per-format large-upload handling is enabled server-side.
// Today's server ignores it — sending it is forward-compatible, never
// behavior-changing. The multipart `format` field stays authoritative.
const FormatHintHeader = "X-Shardpilot-Symbol-Format"

// DefaultRetries is how many times an upload is re-attempted after the first
// try on 5xx or transport failures. 4xx responses are NEVER retried: the server
// already said no, and replaying an auth or validation failure only burns
// quota and log signal.
const DefaultRetries = 4

// maxRespBody bounds how much of a endpoint response body is read into memory.
// Success envelopes and error envelopes are both tiny; 1 MiB is paranoia.
const maxRespBody = 1 << 20

// openUpload opens the file an attempt streams from. It is a package
// variable only so the tests can prove the body is streamed, not buffered
// (see TestUploadStreamsFileInsteadOfBuffering).
var openUpload = func(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// Uploader posts symbol files to the endpoint with bearer auth, streaming bodies
// and bounded retries. The zero value is not usable; fill BaseURL and APIKey.
type Uploader struct {
	// BaseURL is the endpoint base URL, e.g. https://ingest.example.com;
	// UploadPath is appended.
	BaseURL string
	// APIKey is the ShardPilot data-plane API key (symbols:write scope),
	// sent as `Authorization: Bearer <key>`.
	APIKey string
	// Client is the HTTP client to use; http.DefaultClient when nil.
	// Deadlines come from the Upload context, not from Client.Timeout.
	Client *http.Client
	// Retries is the number of re-attempts after the first try on 5xx or
	// transport failures. Negative means zero.
	Retries int
	// Backoff returns the delay before re-attempt retry (0-based).
	// defaultBackoff when nil.
	Backoff func(retry int) time.Duration
	// UserAgent is sent verbatim when non-empty.
	UserAgent string
	// SDKIdentity is the CLI's validated request identity.
	SDKIdentity string
}

// Request describes one symbol file upload.
type Request struct {
	// Path is the local file to stream.
	Path string
	// Filename is the multipart part filename; filepath.Base(Path) when
	// empty.
	Filename string
	// Platform is the endpoint's required `platform` field (linux, android,
	// ios, macos, windows, ...).
	Platform string
	// Channel is the endpoint's required `channel` field: the build
	// channel, develop or release.
	Channel string
	// Format is the resolved symbol format, sent as the explicit `format`
	// field — the CLI always names the format it detected so client and
	// server never disagree about what was uploaded.
	Format string
	// DebugID is the optional `debug_id` field. The endpoint requires it for
	// pdb, proguard (the app build id) and il2cpp_map; see RequiresDebugID.
	DebugID string
}

// UploadedSymbol mirrors the endpoint's UploadedSymbol schema (api/openapi.yaml).
type UploadedSymbol struct {
	Platform   string   `json:"platform"`
	Format     string   `json:"format"`
	DebugID    string   `json:"debug_id,omitempty"`
	DebugIDs   []string `json:"debug_ids,omitempty"`
	Filename   string   `json:"filename"`
	ByteSize   int64    `json:"byte_size"`
	SHA256Hex  string   `json:"sha256_hex"`
	UploadedAt string   `json:"uploaded_at"`
}

// Result is a completed (2xx) upload.
type Result struct {
	StatusCode int
	// Attempts is how many HTTP attempts were made, including the
	// successful one. It is also set alongside a non-nil error.
	Attempts int
	Symbol   UploadedSymbol
}

// APIError is a non-2xx endpoint response, carrying the ErrorEnvelope fields
// when the body parsed.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (http %d)", e.Code, msg, e.StatusCode)
	}
	return fmt.Sprintf("%s (http %d)", msg, e.StatusCode)
}

// Retryable reports whether the response class may be retried: only 5xx.
func (e *APIError) Retryable() bool {
	return e.StatusCode >= 500
}

// defaultBackoff is 500ms, 1s, 2s, 4s ... capped at 8s.
func defaultBackoff(retry int) time.Duration {
	d := 500 * time.Millisecond << uint(retry)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// Upload posts one symbol file, retrying per the Uploader policy. The file is
// re-opened and re-streamed on every attempt, so nothing needs to fit in
// memory. Result.Attempts is populated on failure too.
func (u *Uploader) Upload(ctx context.Context, req Request) (Result, error) {
	if req.Filename == "" {
		req.Filename = filepath.Base(req.Path)
	}
	retries := u.Retries
	if retries < 0 {
		retries = 0
	}
	backoff := u.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	for attempt := 0; ; attempt++ {
		res, err := u.attempt(ctx, req)
		res.Attempts = attempt + 1
		if err == nil {
			return res, nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			return res, err
		}
		if attempt >= retries {
			return res, err
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			return res, fmt.Errorf("upload canceled while waiting to retry: %w (last error: %v)", ctx.Err(), err)
		}
	}
}

// attempt performs a single streaming POST. The multipart body flows
// disk → multipart writer → pipe → socket through a fixed-size copy buffer;
// the file is never read fully into memory (game-scale PDBs are the point of
// this lane).
func (u *Uploader) attempt(ctx context.Context, req Request) (Result, error) {
	file, err := openUpload(req.Path)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", req.Path, err)
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(u.BaseURL, "/")+UploadPath, pr)
	if err != nil {
		file.Close()
		pw.Close()
		pr.Close()
		return Result{}, fmt.Errorf("build upload request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+u.APIKey)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Accept", "application/json")
	if u.UserAgent != "" {
		httpReq.Header.Set("User-Agent", u.UserAgent)
	}
	if u.SDKIdentity != "" {
		httpReq.Header.Set("X-ShardPilot-SDK", u.SDKIdentity)
	}
	if req.Format != "" {
		httpReq.Header.Set(FormatHintHeader, req.Format)
	}
	// Declare the EXACT body length whenever the file size is knowable: the
	// endpoint's per-format large-file admission requires a positive
	// Content-Length (a chunked body is capped at the legacy bound —
	// fasthttp's multipart disk-spool needs a declared length), and the
	// multipart framing is a pure function of the boundary + fields, so the
	// total is exact while the body still STREAMS through the pipe. A file
	// that cannot be stat'd falls back to chunked — today's behavior. A file
	// that changes size mid-stream fails the attempt with a transport
	// length-mismatch error and retries like any transport failure.
	if size, sizeErr := uploadFileSize(req.Path); sizeErr == nil {
		if frame, frameErr := multipartFrameBytes(mw.Boundary(), req); frameErr == nil {
			httpReq.ContentLength = frame + size
		}
	}

	// The writer side runs concurrently with the transport reading pr. If
	// the transport stops early (error, or the endpoint responded without
	// draining the body), writes fail with ErrClosedPipe and the goroutine
	// exits — no leak.
	go func() {
		werr := writeMultipart(mw, req, file)
		file.Close()
		if werr != nil {
			pw.CloseWithError(werr)
			return
		}
		pw.CloseWithError(mw.Close())
	}()

	client := u.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("post %s: %w", UploadPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return Result{}, fmt.Errorf("read endpoint response (http %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var envelope struct {
			Symbol UploadedSymbol `json:"symbol"`
		}
		// The 201 contract carries a SymbolUploadResponse envelope; if a
		// proxy mangled it the upload still landed, so an unparseable
		// success body is reported as success with empty metadata.
		_ = json.Unmarshal(body, &envelope)
		return Result{StatusCode: resp.StatusCode, Symbol: envelope.Symbol}, nil
	}

	return Result{StatusCode: resp.StatusCode}, newAPIError(resp.StatusCode, body)
}

// newAPIError reads the ErrorEnvelope from a non-2xx body, shared by every
// endpoint this package calls.
func newAPIError(status int, body []byte) *APIError {
	apiErr := &APIError{StatusCode: status}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && (envelope.Error.Code != "" || envelope.Error.Message != "") {
		apiErr.Code = envelope.Error.Code
		apiErr.Message = envelope.Error.Message
	} else if snippet := strings.Join(strings.Fields(string(body)), " "); snippet != "" {
		// Non-envelope body (edge/proxy error page): keep result lines
		// one-line by collapsing whitespace, and bound the length.
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		apiErr.Message = snippet
	}
	return apiErr
}

// beginMultipart writes the endpoint's metadata fields and opens the symbol_file
// part — the SINGLE definition of the multipart frame, shared by the real
// body writer and the Content-Length precomputation so they can never drift.
func beginMultipart(mw *multipart.Writer, req Request) (io.Writer, error) {
	fields := [][2]string{
		{"platform", req.Platform},
		{"channel", req.Channel},
		{"format", req.Format},
		{"debug_id", req.DebugID},
	}
	for _, field := range fields {
		if field[1] == "" {
			continue
		}
		if err := mw.WriteField(field[0], field[1]); err != nil {
			return nil, fmt.Errorf("write %s field: %w", field[0], err)
		}
	}
	part, err := mw.CreateFormFile("symbol_file", req.Filename)
	if err != nil {
		return nil, fmt.Errorf("create symbol_file part: %w", err)
	}
	return part, nil
}

// writeMultipart writes the endpoint's multipart fields — metadata first, then
// the symbol_file part — streaming the file through a fixed 64 KiB buffer.
func writeMultipart(mw *multipart.Writer, req Request, file io.Reader) error {
	part, err := beginMultipart(mw, req)
	if err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	if _, err := io.CopyBuffer(part, file, buf); err != nil {
		return fmt.Errorf("stream %s: %w", req.Filename, err)
	}
	return nil
}

// multipartFrameBytes counts the multipart FRAMING bytes (boundaries, part
// headers, metadata fields, terminator) for req under the given boundary —
// everything except the file content itself — by running the identical
// beginMultipart writes against a counter. total body = frame + file size.
func multipartFrameBytes(boundary string, req Request) (int64, error) {
	var counter countingWriter
	mw := multipart.NewWriter(&counter)
	if err := mw.SetBoundary(boundary); err != nil {
		return 0, fmt.Errorf("mirror multipart boundary: %w", err)
	}
	if _, err := beginMultipart(mw, req); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, fmt.Errorf("close frame counter: %w", err)
	}
	return counter.n, nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// uploadFileSize stats the upload for its byte size. A package variable only
// so tests can exercise the chunked fallback deterministically.
var uploadFileSize = func(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	return info.Size(), nil
}

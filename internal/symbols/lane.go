package symbols

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// LanePath is the direct multipart upload lane's route: one JSON
// action, with the parts PUT straight to presigned object-store URLs.
const LanePath = "/api/v1/symbols/upload-sessions"

const (
	// LaneMaxBytes is the lane's object ceiling.
	LaneMaxBytes       int64 = 2 << 30
	defaultPartTimeout       = 5 * time.Minute
	abortTimeout             = 30 * time.Second // its own context: the session's may have passed
)

// newLaneHash is the hash pass's hash, a variable only so a test can count
// what the pass read.
var newLaneHash = sha256.New

// ctxReader ends a read loop once its context is done, so a cancelled
// upload stops hashing within one copy buffer instead of reading 2 GiB.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ErrLaneTooLarge refuses a file over LaneMaxBytes before any request.
var ErrLaneTooLarge = errors.New("symbol file is over the lane's 2 GiB ceiling")

// LaneRequest describes one file sent through the lane.
type LaneRequest struct {
	Path string
	// Filename is sent as the file's name; filepath.Base(Path) when empty.
	Filename string
	Platform string
	Channel  string
	Format   string
	DebugID  string
	AppKey   string
	// PartTimeout bounds each part PUT; 5 minutes when zero or negative.
	PartTimeout time.Duration
}

// LaneResult is a file the lane settled as done.
type LaneResult struct {
	SessionID string
	Size      int64
	SHA256Hex string
	Parts     int
}

// PartError is a failed part PUT. It never carries the presigned URL, which
// is a capability, because errors are printed.
type PartError struct {
	Number     int
	Of         int
	StatusCode int    // the object store's answer; 0 when none arrived
	Code       string // the object store's error code, when it sent one
	Err        error
}

func (e *PartError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("part %d of %d: %v", e.Number, e.Of, e.Err)
	}
	return strings.TrimSpace(fmt.Sprintf("part %d of %d: object store answered http %d %s", e.Number, e.Of, e.StatusCode, e.Code))
}

func (e *PartError) Unwrap() error { return e.Err }

// LaneAbortedError is a session the server aborted, with its reason.
type LaneAbortedError struct {
	SessionID string
	Reason    string
}

func (e *LaneAbortedError) Error() string {
	return fmt.Sprintf("upload session %s was aborted by the server: %s", e.SessionID, e.Reason)
}

// defaultLanePoll waits 2 s and doubles to 15 s, far inside the edge's shared
// per-source rate limit.
func defaultLanePoll(n int) time.Duration {
	if n >= 3 {
		return 15 * time.Second
	}
	return 2 * time.Second << uint(n)
}

type lanePart struct {
	Number int    `json:"number"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
}

type laneSession struct {
	SessionID string     `json:"session_id"`
	State     string     `json:"state"`
	Reason    string     `json:"reason"`
	ExpiresAt time.Time  `json:"expires_at"`
	Parts     []lanePart `json:"parts"`
}

// UploadLane hashes the file, initiates, PUTs the parts in order, completes
// and polls until the server settles the session. A failure before complete is
// accepted aborts the session, so a failed run holds no live session.
func (u *Uploader) UploadLane(ctx context.Context, req LaneRequest) (LaneResult, error) {
	file, err := os.Open(req.Path)
	if err != nil {
		return LaneResult{}, fmt.Errorf("open %s: %w", req.Path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return LaneResult{}, fmt.Errorf("stat %s: %w", req.Path, err)
	}
	if info.Size() > LaneMaxBytes {
		return LaneResult{}, fmt.Errorf("%w: %s is %d bytes", ErrLaneTooLarge, req.Path, info.Size())
	}
	hash := newLaneHash()
	if _, err := io.Copy(hash, ctxReader{ctx, file}); err != nil {
		return LaneResult{}, fmt.Errorf("hash %s: %w", req.Path, err)
	}
	res := LaneResult{Size: info.Size(), SHA256Hex: hex.EncodeToString(hash.Sum(nil))}
	if req.Filename == "" {
		req.Filename = filepath.Base(req.Path)
	}

	var session laneSession
	body := map[string]any{"action": "initiate", "format": req.Format, "platform": req.Platform,
		"channel": req.Channel, "filename": req.Filename, "debug_id": req.DebugID, "app_key": req.AppKey,
		"size": res.Size, "sha256": res.SHA256Hex}
	// Only a server 5xx is retried: a lost reply may have created a session.
	// Each attempt is bounded like a part: no session deadline exists yet.
	partTimeout := req.PartTimeout
	if partTimeout <= 0 {
		partTimeout = defaultPartTimeout
	}
	if err := u.retry(ctx, true, func() error {
		return u.boundedCall(ctx, partTimeout, body, &session, http.StatusCreated)
	}); err != nil {
		return res, err
	}
	res.SessionID, res.Parts = session.SessionID, len(session.Parts)
	if err := checkPlan(session.Parts, res.Size); err != nil {
		u.abort(session.SessionID)
		return res, err
	}
	if !session.ExpiresAt.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, session.ExpiresAt)
		defer cancel()
	}

	var offset int64
	for _, part := range session.Parts {
		section := func() io.Reader { return io.NewSectionReader(file, offset, part.Size) }
		if err := u.retry(ctx, false, func() error { return u.putPart(ctx, part, len(session.Parts), section, partTimeout) }); err != nil {
			u.abort(session.SessionID)
			return res, err
		}
		offset += part.Size
	}

	complete := map[string]any{"action": "complete", "session_id": session.SessionID}
	if err := u.retry(ctx, false, func() error {
		return u.boundedCall(ctx, partTimeout, complete, nil, http.StatusAccepted, http.StatusOK)
	}); err != nil {
		// The client acts only on outcomes it knows: a complete whose reply was
		// lost may have taken effect, so the session is read, not assumed.
		// A readable session decides, not the last reply.
		var now laneSession
		switch readErr := u.boundedCall(ctx, partTimeout, map[string]any{"action": "status", "session_id": session.SessionID}, &now, http.StatusOK); {
		case readErr != nil:
			return res, err // the outcome is still unknown
		case now.State == "open": // it did not take effect
			u.abort(session.SessionID)
			return res, err
		}
	}
	return res, u.awaitSettled(ctx, session.SessionID, partTimeout)
}

// checkPlan refuses parts out of order, parts that do not cover the file, or a
// part host the file would reach in the clear.
func checkPlan(parts []lanePart, size int64) error {
	var total int64
	for i, part := range parts {
		if part.Number != i+1 || part.Size <= 0 {
			return fmt.Errorf("the server's part plan is malformed at part %d", i+1)
		}
		parsed, err := url.Parse(part.URL)
		if err != nil || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname()))) {
			return fmt.Errorf("the server's part %d is not an https URL; the CLI sends symbol bytes in the clear only to localhost", part.Number)
		}
		total += part.Size
	}
	if len(parts) == 0 || total != size {
		return fmt.Errorf("the server's %d planned parts cover %d bytes of a %d-byte file", len(parts), total, size)
	}
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var storeErrorCode = regexp.MustCompile(`<Code>([A-Za-z0-9]{1,64})</Code>`)

// putPart PUTs one part with no credential of ours: the URL is the whole
// authorization. A 5xx or a transport failure is retryable.
func (u *Uploader) putPart(ctx context.Context, part lanePart, of int, section func() io.Reader, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, part.URL, section())
	if err != nil {
		return &PartError{Number: part.Number, Of: of, Err: errors.New("the part URL could not be used")}
	}
	httpReq.ContentLength = part.Size
	if u.UserAgent != "" {
		httpReq.Header.Set("User-Agent", u.UserAgent)
	}
	resp, err := u.partClient().Do(httpReq)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) { // its message is "<op> <url>: <cause>"
			err = urlErr.Err
		}
		return &retryable{&PartError{Number: part.Number, Of: of, Err: err}}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	partErr := &PartError{Number: part.Number, Of: of, StatusCode: resp.StatusCode}
	if m := storeErrorCode.FindSubmatch(answer); m != nil {
		partErr.Code = string(m[1])
	}
	if resp.StatusCode >= 500 {
		return &retryable{partErr}
	}
	return partErr
}

// awaitSettled polls until the session settles. A failed poll is not a
// verdict: polling goes on until the session's deadline.
func (u *Uploader) awaitSettled(ctx context.Context, sessionID string, bound time.Duration) error {
	poll := u.LanePoll
	if poll == nil {
		poll = defaultLanePoll
	}
	status := map[string]any{"action": "status", "session_id": sessionID}
	var last error
	for n := 0; ; n++ {
		select {
		case <-time.After(poll(n)):
		case <-ctx.Done():
			return fmt.Errorf("upload session %s was not settled before its deadline: %w (last poll: %v)", sessionID, ctx.Err(), last)
		}
		var session laneSession
		last = u.boundedCall(ctx, bound, status, &session, http.StatusOK)
		var apiErr *APIError
		if errors.As(last, &apiErr) && !apiErr.Retryable() {
			return last
		}
		switch {
		case last != nil:
		case session.State == "done":
			return nil
		case session.State == "aborted":
			return &LaneAbortedError{SessionID: sessionID, Reason: session.Reason}
		}
	}
}

// abort releases a session the run will not complete, best effort.
func (u *Uploader) abort(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	_ = u.laneCall(ctx, map[string]any{"action": "abort", "session_id": sessionID}, nil, http.StatusOK)
}

// boundedCall runs one lane action under its own per-request bound, so no
// call waits on the session's deadline alone; that deadline bounds the file.
func (u *Uploader) boundedCall(ctx context.Context, d time.Duration, body any, out any, want ...int) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return u.laneCall(ctx, body, out, want...)
}

// laneCall posts one JSON action to the lane and decodes a wanted answer into
// out. Any other answer is an *APIError; a transport failure is retryable.
func (u *Uploader) laneCall(ctx context.Context, body any, out any, want ...int) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode lane request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(u.BaseURL, "/")+LanePath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build lane request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+u.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if u.UserAgent != "" {
		httpReq.Header.Set("User-Agent", u.UserAgent)
	}
	if u.SDKIdentity != "" {
		httpReq.Header.Set("X-ShardPilot-SDK", u.SDKIdentity)
	}
	resp, err := u.client().Do(httpReq)
	if err != nil {
		return &retryable{fmt.Errorf("post %s: %w", LanePath, err)}
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return &retryable{fmt.Errorf("read lane response (http %d): %w", resp.StatusCode, err)}
	}
	for _, status := range want {
		if resp.StatusCode == status {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(answer, out); err != nil {
				return fmt.Errorf("decode lane response (http %d): %w", resp.StatusCode, err)
			}
			return nil
		}
	}
	return newAPIError(resp.StatusCode, answer)
}

// retryable marks a transport failure or an object-store 5xx.
type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

// retry runs call under the Uploader's retry policy. A 5xx APIError is
// retryable; so are a transport failure and an object-store 5xx, unless
// apiOnly is set for a call whose lost reply may already have taken effect.
func (u *Uploader) retry(ctx context.Context, apiOnly bool, call func() error) error {
	retries := max(u.Retries, 0)
	backoff := u.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	for attempt := 0; ; attempt++ {
		err := call()
		if err == nil {
			return nil
		}
		var apiErr *APIError
		var again *retryable
		canRetry := (errors.As(err, &apiErr) && apiErr.Retryable()) || (!apiOnly && errors.As(err, &again))
		if errors.As(err, &again) {
			err = again.err
		}
		if !canRetry || attempt >= retries {
			return err
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			return fmt.Errorf("canceled while waiting to retry: %w (last error: %v)", ctx.Err(), err)
		}
	}
}

// partClient never follows a redirect: its target was not checked by
// checkPlan, and following one sends a Referer carrying the presigned query.
// A 3xx is then a final PartError.
func (u *Uploader) partClient() *http.Client {
	c := *u.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (u *Uploader) client() *http.Client {
	if u.Client == nil {
		return http.DefaultClient
	}
	return u.Client
}

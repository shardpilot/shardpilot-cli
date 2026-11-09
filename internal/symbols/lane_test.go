package symbols

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// laneFake is the upload-session endpoint and the object store in one
// server: the lane's JSON actions on LanePath, presigned part PUTs on /store/.
type laneFake struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	file     []byte
	partSize int
	partURL  func(n int) string // overrides the presigned URL of part n
	planLie  int64              // added to the last planned part's size

	initStatus     int
	initCode       string
	completeStatus int
	completeCode   string
	completeDrop   int            // the first n completes are taken and dropped unanswered
	hang           map[string]int // lane actions to leave unanswered, and how many times
	release        chan struct{}  // ends every hang; closed in cleanup before the server
	statuses       []string       // states status answers, the last one repeated
	reason         string
	partFail       map[int][]int // statuses part n answers before it succeeds
	partRedirect   map[int]int   // a redirect status part n answers instead
	redirected     []string      // Referer of every request a redirect reached

	actions  []string
	initiate map[string]any
	parts    map[int][]byte
	partHits map[int]int
	partAuth []string
	aborted  int
}

func newLaneFake(t *testing.T, file []byte, partSize int) *laneFake {
	f := &laneFake{t: t, file: file, partSize: partSize, statuses: []string{"done"},
		partFail: map[int][]int{}, partRedirect: map[int]int{}, parts: map[int][]byte{}, partHits: map[int]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *laneFake) serve(w http.ResponseWriter, r *http.Request) {
	if f.release != nil && r.URL.Path == LanePath {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var peek struct{ Action string }
		_ = json.Unmarshal(body, &peek)
		f.mu.Lock()
		hang := f.hang[peek.Action] > 0
		f.hang[peek.Action]--
		f.mu.Unlock()
		if hang {
			select {
			case <-r.Context().Done():
			case <-f.release:
			}
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/redirected" {
		f.redirected = append(f.redirected, r.Header.Get("Referer"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/store/part/") {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/store/part/"), "%d", &n)
		f.partHits[n]++
		f.partAuth = append(f.partAuth, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		if status := f.partRedirect[n]; status != 0 {
			http.Redirect(w, r, f.srv.URL+"/redirected", status)
			return
		}
		if queue := f.partFail[n]; len(queue) > 0 {
			f.partFail[n] = queue[1:]
			w.WriteHeader(queue[0])
			fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		if r.ContentLength != int64(len(body)) {
			f.t.Errorf("part %d declared %d bytes and sent %d", n, r.ContentLength, len(body))
		}
		f.parts[n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
		return
	}
	if r.URL.Path != LanePath || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer key" {
		http.Error(w, "unexpected request", http.StatusTeapot)
		return
	}
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	action, _ := req["action"].(string)
	f.actions = append(f.actions, action)
	reply := func(status int, code string, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if code != "" {
			body = map[string]any{"error": map[string]string{"code": code, "message": code}}
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	switch action {
	case "initiate":
		f.initiate = req
		if f.initStatus != 0 {
			reply(f.initStatus, f.initCode, nil)
			return
		}
		var parts []map[string]any
		for n, off := 1, 0; off < len(f.file); n, off = n+1, off+f.partSize {
			size := int64(min(f.partSize, len(f.file)-off))
			url := fmt.Sprintf("%s/store/part/%d?X-Amz-Signature=secret%d", f.srv.URL, n, n)
			if f.partURL != nil {
				url = f.partURL(n)
			}
			parts = append(parts, map[string]any{"number": n, "size": size, "url": url})
		}
		parts[len(parts)-1]["size"] = parts[len(parts)-1]["size"].(int64) + f.planLie
		reply(http.StatusCreated, "", map[string]any{"session_id": "s-1", "state": "open",
			"expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339), "part_size": f.partSize, "parts": parts})
	case "complete":
		if f.completeDrop > 0 {
			f.completeDrop--
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if f.completeStatus != 0 {
			reply(f.completeStatus, f.completeCode, nil)
			return
		}
		reply(http.StatusAccepted, "", map[string]any{"session_id": "s-1", "state": "completing"})
	case "status":
		state := f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
		reply(http.StatusOK, "", map[string]any{"session_id": "s-1", "state": state, "reason": f.reason})
	case "abort":
		f.aborted++
		reply(http.StatusOK, "", map[string]any{"session_id": "s-1", "state": "aborted"})
	}
}

func (f *laneFake) uploader() *Uploader {
	return &Uploader{BaseURL: f.srv.URL, APIKey: "key", Client: f.srv.Client(), Retries: 2,
		Backoff: noBackoff, LanePoll: func(int) time.Duration { return 0 }}
}

func (f *laneFake) upload() (LaneResult, error) {
	path := writeTempFile(f.t, "big.pdb", f.file)
	return f.uploader().UploadLane(context.Background(), LaneRequest{Path: path, Platform: "windows",
		Channel: "release", Format: FormatPDB, DebugID: "ABC", PartTimeout: 5 * time.Second})
}

func TestLaneUploadsEveryPartAndWaitsForDone(t *testing.T) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.statuses = []string{"completing", "completing", "done"}
	res, err := f.upload()
	digest := sha256.Sum256(f.file)
	if err != nil || res.SessionID != "s-1" || res.Parts != 3 || res.Size != 10 || res.SHA256Hex != hex.EncodeToString(digest[:]) {
		t.Fatalf("UploadLane = %+v, %v", res, err)
	}
	if f.initiate["sha256"] != hex.EncodeToString(digest[:]) || f.initiate["size"] != float64(10) ||
		f.initiate["format"] != "pdb" || f.initiate["filename"] != "big.pdb" || f.initiate["debug_id"] != "ABC" {
		t.Fatalf("initiate carried %v", f.initiate)
	}
	for n, want := range map[int]string{1: "0123", 2: "4567", 3: "89"} {
		if string(f.parts[n]) != want {
			t.Fatalf("part %d = %q, want %q", n, f.parts[n], want)
		}
	}
	for _, auth := range f.partAuth {
		if auth != "" {
			t.Fatalf("a part PUT carried Authorization %q: the API key reached the object store", auth)
		}
	}
	if got := strings.Join(f.actions, ","); got != "initiate,complete,status,status,status" {
		t.Fatalf("actions = %s", got)
	}
}

func TestLaneRetriesAPartAfterAServerError(t *testing.T) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.partFail[2] = []int{http.StatusServiceUnavailable}
	if _, err := f.upload(); err != nil || f.partHits[2] != 2 || string(f.parts[2]) != "4567" {
		t.Fatalf("UploadLane = %v after %d tries of part 2", err, f.partHits[2])
	}
}

func TestLaneAbortsWhenTheStoreRefusesAPart(t *testing.T) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.partFail[2] = []int{http.StatusForbidden, http.StatusForbidden, http.StatusForbidden}
	_, err := f.upload()
	var partErr *PartError
	if !errors.As(err, &partErr) || partErr.Number != 2 || partErr.StatusCode != http.StatusForbidden {
		t.Fatalf("UploadLane = %v, want part 2's 403", err)
	}
	if f.partHits[2] != 1 || f.aborted != 1 || strings.Contains(strings.Join(f.actions, ","), "complete") {
		t.Fatalf("part 2 tried %d times, %d aborts, actions %v: a 4xx is final and aborts the session", f.partHits[2], f.aborted, f.actions)
	}
}

// A presigned part URL is a capability: no error may carry it.
func TestLaneErrorsNeverCarryAPartURL(t *testing.T) {
	const closed = "127.0.0.1:1" // nothing listens: a synthetic transport failure
	for name, setup := range map[string]func(*laneFake){
		"refused": func(f *laneFake) { f.partFail[1] = []int{http.StatusForbidden} },
		"unreachable": func(f *laneFake) {
			f.partURL = func(n int) string {
				return fmt.Sprintf("http://%s/store/part/%d?X-Amz-Signature=secret%d", closed, n, n)
			}
		},
	} {
		f := newLaneFake(t, []byte("0123456789"), 4)
		setup(f)
		_, err := f.upload()
		var partErr *PartError
		if !errors.As(err, &partErr) || partErr.Number != 1 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "X-Amz") {
			t.Fatalf("%s: UploadLane = %v; the error must name the part, never its URL", name, err)
		}
	}
}

func TestLaneReportsTheReasonOfAnAbortedSession(t *testing.T) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.statuses, f.reason = []string{"completing", "aborted"}, "digest_mismatch"
	_, err := f.upload()
	var aborted *LaneAbortedError
	if !errors.As(err, &aborted) || aborted.Reason != "digest_mismatch" || aborted.SessionID != "s-1" || f.aborted != 0 {
		t.Fatalf("UploadLane = %v with %d client aborts; want the server's digest_mismatch", err, f.aborted)
	}
}

func TestLaneAbortsWhenCompleteIsRefused(t *testing.T) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.completeStatus, f.completeCode = http.StatusUnprocessableEntity, "symbol_upload_parts_invalid"
	f.statuses = []string{"open"} // the refusal left the session open
	_, err := f.upload()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "symbol_upload_parts_invalid" || f.aborted != 1 {
		t.Fatalf("UploadLane = %v with %d aborts", err, f.aborted)
	}
}

func TestLaneDisabledServerIsAnAPIError(t *testing.T) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.initStatus, f.initCode = http.StatusNotFound, "symbol_upload_lane_disabled"
	_, err := f.upload()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "symbol_upload_lane_disabled" ||
		len(f.partHits) != 0 || f.aborted != 0 {
		t.Fatalf("UploadLane = %v, %d parts tried, %d aborts", err, len(f.partHits), f.aborted)
	}
}

// The plan is the server's, but the client checks it before sending a byte:
// a part host it would send the file to in the clear, or parts that do not
// cover the file exactly, abort the session.
func TestLaneRefusesAPlanItCannotTrust(t *testing.T) {
	for name, setup := range map[string]func(*laneFake){
		"plain http to a remote host": func(f *laneFake) {
			f.partURL = func(n int) string { return fmt.Sprintf("http://objects.example.com/part/%d", n) }
		},
		"parts that do not cover the file": func(f *laneFake) { f.planLie = 3 },
	} {
		f := newLaneFake(t, []byte("0123456789"), 4)
		setup(f)
		_, err := f.upload()
		var partErr *PartError
		if err == nil || errors.As(err, &partErr) || len(f.partHits) != 0 || f.aborted != 1 {
			t.Fatalf("%s: UploadLane = %v, %d parts tried, %d aborts", name, err, len(f.partHits), f.aborted)
		}
	}
}

func TestLaneRefusesAnObjectOverItsCeiling(t *testing.T) {
	f := newLaneFake(t, nil, 4)
	path := filepath.Join(t.TempDir(), "huge.pdb")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, LaneMaxBytes+1); err != nil { // sparse: no bytes written
		t.Fatal(err)
	}
	_, err := f.uploader().UploadLane(context.Background(), LaneRequest{Path: path, Platform: "windows",
		Channel: "release", Format: FormatPDB, DebugID: "ABC"})
	if !errors.Is(err, ErrLaneTooLarge) || len(f.actions) != 0 {
		t.Fatalf("UploadLane = %v with actions %v; over 2 GiB is refused before any request", err, f.actions)
	}
}

func TestLanePollBacksOffToItsCap(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 2 * time.Second, 1: 4 * time.Second, 2: 8 * time.Second, 3: 15 * time.Second, 40: 15 * time.Second} {
		if got := defaultLanePoll(n); got != want {
			t.Fatalf("defaultLanePoll(%d) = %s, want %s", n, got, want)
		}
	}
}

// Round 1: a part URL that redirects is refused, never followed. Following it
// would reach a target checkPlan never saw, with a Referer carrying the
// presigned query, and could read the target's answer as the part's.
func TestLanePartRedirectIsRefused(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		f := newLaneFake(t, []byte("0123456789"), 4)
		f.partRedirect[1] = status
		_, err := f.upload()
		var partErr *PartError
		if !errors.As(err, &partErr) || partErr.StatusCode != status || len(f.redirected) != 0 || f.aborted != 1 {
			t.Fatalf("%d: UploadLane = %v; redirect reached with Referers %q, %d aborts", status, err, f.redirected, f.aborted)
		}
	}
}

// countingHash counts the bytes a hash pass wrote.
type countingHash struct {
	hash.Hash
	n *int64
}

func (c countingHash) Write(p []byte) (int, error) { *c.n += int64(len(p)); return c.Hash.Write(p) }

// Round 2: a cancelled upload stops hashing at once rather than reading the
// whole file before its first context-aware call.
func TestLaneStopsHashingWhenCanceled(t *testing.T) {
	var hashed int64
	orig := newLaneHash
	newLaneHash = func() hash.Hash { return countingHash{sha256.New(), &hashed} }
	t.Cleanup(func() { newLaneHash = orig })
	f := newLaneFake(t, nil, 4)
	path := filepath.Join(t.TempDir(), "big.pdb")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 64<<20); err != nil { // sparse
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.uploader().UploadLane(ctx, LaneRequest{Path: path, Platform: "windows", Channel: "release", Format: FormatPDB})
	if !errors.Is(err, context.Canceled) || hashed != 0 || len(f.actions) != 0 {
		t.Fatalf("UploadLane = %v after hashing %d bytes, actions %v", err, hashed, f.actions)
	}
}

// Round 3: the client acts only on outcomes it knows. A complete whose every
// reply is lost is resolved by reading the session: completing means it took
// effect and the verdict is awaited; open means it did not, and is aborted.
func TestLaneResolvesACompleteWhoseReplyWasLost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drops    int
		statuses []string
		ok       bool
		aborts   int
	}{
		{"took effect", 99, []string{"completing", "done"}, true, 0},
		{"did not", 99, []string{"open"}, false, 1},
		{"settled before the retry", 1, []string{"done"}, true, 0}, // round 4: the retry conflicts
	} {
		f := newLaneFake(t, []byte("0123456789"), 4)
		f.completeDrop, f.statuses = tc.drops, tc.statuses
		f.completeStatus, f.completeCode = http.StatusConflict, "symbol_upload_session_state"
		_, err := f.upload()
		if (err == nil) != tc.ok || f.aborted != tc.aborts {
			t.Fatalf("%s: UploadLane = %v with %d aborts, want ok=%v and %d", tc.name, err, f.aborted, tc.ok, tc.aborts)
		}
	}
}

// hung runs one upload with a 200 ms request bound against a fake leaving
// the given actions unanswered, failing if it outlives a 5 s guard.
func hung(t *testing.T, actions map[string]int) (*laneFake, error) {
	f := newLaneFake(t, []byte("0123456789"), 4)
	f.hang, f.release = actions, make(chan struct{})
	t.Cleanup(func() { close(f.release) }) // runs before the server closes
	path, done := writeTempFile(t, "big.pdb", f.file), make(chan error, 1)
	go func() {
		_, err := f.uploader().UploadLane(context.Background(), LaneRequest{Path: path, Platform: "windows",
			Channel: "release", Format: FormatPDB, PartTimeout: 200 * time.Millisecond})
		done <- err
	}()
	select {
	case err := <-done:
		return f, err
	case <-time.After(5 * time.Second):
		t.Fatalf("with %v unanswered, a lane request waited past its bound", actions)
		return f, nil
	}
}

// 7b rounds 1 and 3: every lane request runs under its own bound, never the
// session deadline alone. A hung initiate or complete-and-status ends the
// upload unaborted (no session, or an unknown outcome); one hung poll does not.
func TestLaneBoundsEveryRequest(t *testing.T) {
	if f, err := hung(t, map[string]int{"initiate": 99}); !errors.Is(err, context.DeadlineExceeded) || len(f.partHits) != 0 {
		t.Fatalf("initiate: UploadLane = %v with %d parts tried", err, len(f.partHits))
	}
	if f, err := hung(t, map[string]int{"complete": 99, "status": 99}); err == nil || f.aborted != 0 {
		t.Fatalf("complete and status: UploadLane = %v with %d aborts", err, f.aborted)
	}
	if f, err := hung(t, map[string]int{"status": 1}); err != nil || f.aborted != 0 {
		t.Fatalf("one status poll: UploadLane = %v with %d aborts", err, f.aborted)
	}
}

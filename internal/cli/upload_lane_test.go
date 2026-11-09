package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shardpilot/shardpilot-cli/internal/symbols"
)

// doors is the symbols endpoint with both doors: the single-request upload,
// the multipart lane, and the object store the lane's parts go to.
type doors struct {
	mu         sync.Mutex
	srv        *httptest.Server
	single     []string // filenames the single-request door received
	initiated  []string // filenames the lane initiated
	parts      int
	initStatus int
	initCode   string
	state      string // what status answers
	reason     string
}

func newDoors(t *testing.T) *doors {
	d := &doors{state: "done"}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == symbols.UploadPath:
			if r.ContentLength > 1<<20 { // refused unread: no scene needs a large body here
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err == nil {
				d.single = append(d.single, r.MultipartForm.File["symbol_file"][0].Filename)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, createdEnvelope)
		case strings.HasPrefix(r.URL.Path, "/store/"):
			io.Copy(io.Discard, r.Body)
			d.parts++
		case r.URL.Path == symbols.LanePath:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req["action"] {
			case "initiate":
				d.initiated = append(d.initiated, fmt.Sprint(req["filename"]))
				if d.initStatus != 0 {
					w.WriteHeader(d.initStatus)
					fmt.Fprintf(w, `{"error":{"code":%q,"message":"refused"}}`, d.initCode)
					return
				}
				size := int64(req["size"].(float64))
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"session_id":"s-1","state":"open","expires_at":%q,"part_size":%d,"parts":[{"number":1,"size":%d,"url":%q}]}`,
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339), size, size, d.srv.URL+"/store/part/1")
			case "complete":
				w.WriteHeader(http.StatusAccepted)
				io.WriteString(w, `{"session_id":"s-1","state":"completing"}`)
			case "status":
				fmt.Fprintf(w, `{"session_id":"s-1","state":%q,"reason":%q}`, d.state, d.reason)
			case "abort":
				io.WriteString(w, `{"session_id":"s-1","state":"aborted"}`)
			}
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// smallDoor lowers the single-request door's file bound for one test, so the
// lane can be exercised with files of a few bytes.
func smallDoor(t *testing.T, limit int64) {
	t.Helper()
	origLimit, origPoll := singleRequestMaxFileBytes, lanePoll
	singleRequestMaxFileBytes, lanePoll = limit, func(int) time.Duration { return 0 }
	t.Cleanup(func() { singleRequestMaxFileBytes, lanePoll = origLimit, origPoll })
}

type laneLine struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	Error     struct {
		Code string `json:"code"`
	} `json:"error"`
}

func lines(t *testing.T, stdout string) []laneLine {
	t.Helper()
	var out []laneLine
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var res laneLine
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Fatalf("parse --json line %q: %v", line, err)
		}
		out = append(out, res)
	}
	return out
}

// What the single-request door can carry stays on it; a pdb or elf it cannot
// carry goes through the lane.
func TestUploadRoutesWhatTheSingleRequestDoorCannotCarryToTheLane(t *testing.T) {
	smallDoor(t, 4)
	d := newDoors(t)
	small, big := writeSymFile(t, "small.so", "abc"), writeSymFile(t, "big.so", "0123456789")
	stdout, stderr, code := runCLI(t, []string{"symbols", "upload", "--json", "--format", "elf", "--platform", "linux", small, big}, envWith(d.srv.URL))
	res := lines(t, stdout)
	if code != ExitOK || len(res) != 2 || !res[0].OK || !res[1].OK || res[1].SessionID != "s-1" {
		t.Fatalf("exit %d, results %+v, stderr %s", code, res, stderr)
	}
	if strings.Join(d.single, ",") != "small.so" || strings.Join(d.initiated, ",") != "big.so" || d.parts != 1 {
		t.Fatalf("single-request door got %v, the lane %v with %d parts", d.single, d.initiated, d.parts)
	}
}

// A file over the single-request door in a format the lane does not serve is
// refused locally, before any request.
func TestUploadRefusesALargeFileNoDoorServes(t *testing.T) {
	smallDoor(t, 4)
	d := newDoors(t)
	path := writeSymFile(t, "big.sym", "MODULE linux x86_64 A1B2C3 app\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--json", "--platform", "linux", path}, envWith(d.srv.URL))
	if res := lines(t, stdout); code != ExitUsage || res[0].Error.Code != "upload_too_large" || len(d.single)+len(d.initiated) != 0 {
		t.Fatalf("exit %d, result %+v, single %v, lane %v", code, res, d.single, d.initiated)
	}
}

func TestUploadRefusesAFileOverTheLaneCeilingLocally(t *testing.T) {
	d := newDoors(t)
	path := filepath.Join(t.TempDir(), "huge.so")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, symbols.LaneMaxBytes+1); err != nil { // sparse
		t.Fatal(err)
	}
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--json", "--format", "elf", "--platform", "linux", path}, envWith(d.srv.URL))
	if res := lines(t, stdout); code != ExitUsage || res[0].Error.Code != "upload_too_large" || len(d.single)+len(d.initiated) != 0 {
		t.Fatalf("exit %d, result %+v, single %v, lane %v", code, res, d.single, d.initiated)
	}
}

// A server whose lane is not armed does not serve the file at all: the
// single-request door cannot carry it either.
func TestUploadLaneDisabledExitsNotServed(t *testing.T) {
	smallDoor(t, 4)
	d := newDoors(t)
	d.initStatus, d.initCode = http.StatusNotFound, "symbol_upload_lane_disabled"
	path := writeSymFile(t, "big.so", "0123456789")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--format", "elf", "--platform", "linux", path}, envWith(d.srv.URL))
	if code != ExitNotServed || !strings.Contains(stdout, "lane") || len(d.single) != 0 {
		t.Fatalf("exit %d, stdout %q, single-request door got %v", code, stdout, d.single)
	}
}

// The server's verdict on a lane upload is a rejection with its reason.
func TestUploadLaneAbortExitsRejectedWithItsReason(t *testing.T) {
	smallDoor(t, 4)
	d := newDoors(t)
	d.state, d.reason = "aborted", "digest_mismatch"
	path := writeSymFile(t, "big.so", "0123456789")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--json", "--format", "elf", "--platform", "linux", path}, envWith(d.srv.URL))
	if res := lines(t, stdout); code != ExitRejected || res[0].OK || res[0].Error.Code != "digest_mismatch" || res[0].SessionID != "s-1" {
		t.Fatalf("exit %d, result %+v", code, res)
	}
}

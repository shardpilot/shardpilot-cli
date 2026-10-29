package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const createdEnvelope = `{"symbol":{"platform":"windows","format":"breakpad","debug_id":"A1B2C3","filename":"app.sym","byte_size":42,"sha256_hex":"deadbeef","uploaded_at":"2026-07-19T00:00:00Z"}}`

func instantRetries(t *testing.T) {
	t.Helper()
	orig := retryBackoff
	retryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { retryBackoff = orig })
}

func runCLI(t *testing.T, argv []string, env map[string]string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(argv, func(key string) string { return env[key] }, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func writeSymFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func envWith(url string) map[string]string {
	return map[string]string{
		"SHARDPILOT_SYMBOLS_KEY": "sp_test_key",
		"SHARDPILOT_SYMBOLS_URL": url,
	}
}

func TestUploadSuccess(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE windows x86_64 A1B2C3 app.pdb\n")
	stdout, stderr, code := runCLI(t, []string{"symbols", "upload", "--platform", "windows", path}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "ok    "+path) || !strings.Contains(stdout, "format=breakpad") {
		t.Fatalf("stdout = %q", stdout)
	}
	if auth := gotAuth.Load(); auth != "Bearer sp_test_key" {
		t.Fatalf("Authorization = %v, want Bearer sp_test_key", auth)
	}
}

func TestUploadURLFlagOverridesEnv(t *testing.T) {
	instantRetries(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	env := envWith("http://127.0.0.1:1") // unreachable; the flag must win
	_, stderr, code := runCLI(t, []string{"symbols", "upload", "--url", srv.URL, "--platform", "linux", path}, env)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
}

func TestUploadUnauthorizedExit2JSON(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"unauthorized","message":"api key rejected"}}`)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--json", "--platform", "linux", path}, envWith(srv.URL))
	if code != ExitRejected {
		t.Fatalf("exit = %d, want %d", code, ExitRejected)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 — 4xx must never be retried", hits.Load())
	}
	var res struct {
		Path     string `json:"path"`
		OK       bool   `json:"ok"`
		Status   int    `json:"status"`
		Attempts int    `json:"attempts"`
		Error    struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res); err != nil {
		t.Fatalf("parse --json output %q: %v", stdout, err)
	}
	if res.OK || res.Status != 401 || res.Error.Code != "unauthorized" || res.Attempts != 1 || res.Path != path {
		t.Fatalf("json result = %+v", res)
	}
}

func TestUploadPayloadTooLargeExit2(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, `{"error":{"code":"symbol_too_large","message":"symbol object exceeds the per-file cap"}}`)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "linux", path}, envWith(srv.URL))
	if code != ExitRejected {
		t.Fatalf("exit = %d, want %d", code, ExitRejected)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 — 413 is a 4xx and must not be retried", hits.Load())
	}
	if !strings.Contains(stdout, "FAIL") || !strings.Contains(stdout, "symbol_too_large") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUpload5xxRetriesThenExit3(t *testing.T) {
	instantRetries(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "backend down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	_, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "linux", path}, envWith(srv.URL))
	if code != ExitUnavailable {
		t.Fatalf("exit = %d, want %d", code, ExitUnavailable)
	}
	if hits.Load() != 5 {
		t.Fatalf("server hits = %d, want 5 (first attempt + 4 retries)", hits.Load())
	}
}

func TestUpload5xxThenSuccess(t *testing.T) {
	instantRetries(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "flaky", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "linux", path}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if hits.Load() != 3 {
		t.Fatalf("server hits = %d, want 3", hits.Load())
	}
	if !strings.Contains(stdout, "ok    ") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUploadMultiFilePartialFailureExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		_, header, err := r.FormFile("symbol_file")
		if err != nil {
			t.Errorf("symbol_file: %v", err)
			http.Error(w, "missing part", http.StatusBadRequest)
			return
		}
		if header.Filename == "bad.sym" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"missing_scope","message":"key lacks symbols:write"}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.sym")
	bad := filepath.Join(dir, "bad.sym")
	for _, p := range []string{good, bad} {
		if err := os.WriteFile(p, []byte("MODULE a b c d\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr, code := runCLI(t, []string{"symbols", "upload", "--platform", "linux", good, bad}, envWith(srv.URL))
	if code != ExitRejected {
		t.Fatalf("exit = %d, want %d (first failure is a 403)", code, ExitRejected)
	}
	if !strings.Contains(stdout, "ok    "+good) {
		t.Fatalf("missing ok line for %s in %q", good, stdout)
	}
	if !strings.Contains(stdout, "FAIL  "+bad) || !strings.Contains(stdout, "missing_scope") {
		t.Fatalf("missing FAIL line for %s in %q", bad, stdout)
	}
	if !strings.Contains(stderr, "uploaded 1/2 files") {
		t.Fatalf("stderr = %q, want the 1/2 summary", stderr)
	}
}

func TestUploadPDBRequiresDebugID(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	path := writeSymFile(t, "app.pdb", "Microsoft C/C++ MSF 7.00\r\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "windows", path}, envWith(srv.URL))
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0 — the missing debug id must fail before any upload", hits.Load())
	}
	if !strings.Contains(stdout, "--debug-id") {
		t.Fatalf("stdout = %q, want a --debug-id hint", stdout)
	}
}

func TestUploadPDBWithDebugID(t *testing.T) {
	var gotDebugID atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		gotDebugID.Store(r.FormValue("debug_id"))
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"symbol":{"platform":"windows","format":"pdb","debug_id":"0123ABCD12345678123456781234567AB","filename":"app.pdb","byte_size":26,"sha256_hex":"aa","uploaded_at":"2026-07-19T00:00:00Z"}}`)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.pdb", "Microsoft C/C++ MSF 7.00\r\n")
	stdout, stderr, code := runCLI(t, []string{
		"symbols", "upload", "--platform", "windows", "--debug-id", "0123ABCD12345678123456781234567AB", path,
	}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := gotDebugID.Load(); got != "0123ABCD12345678123456781234567AB" {
		t.Fatalf("debug_id field = %v", got)
	}
	if !strings.Contains(stdout, "format=pdb") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUploadUsageErrors(t *testing.T) {
	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	otherPath := writeSymFile(t, "b.sym", "MODULE a b c d\n")
	env := envWith("http://127.0.0.1:1")

	cases := []struct {
		name       string
		argv       []string
		env        map[string]string
		wantStderr string
	}{
		{
			name:       "missing api key names the env var and the why",
			argv:       []string{"symbols", "upload", "--platform", "linux", path},
			env:        map[string]string{"SHARDPILOT_SYMBOLS_URL": "http://127.0.0.1:1"},
			wantStderr: "SHARDPILOT_SYMBOLS_KEY",
		},
		{
			name:       "plain http to a remote host",
			argv:       []string{"symbols", "upload", "--platform", "linux", path},
			env:        map[string]string{"SHARDPILOT_SYMBOLS_KEY": "k", "SHARDPILOT_SYMBOLS_URL": "http://symbols.example.invalid"},
			wantStderr: "refusing plain http to symbols.example.invalid",
		},
		{
			name:       "missing platform",
			argv:       []string{"symbols", "upload", path},
			env:        env,
			wantStderr: "--platform is required",
		},
		{
			name:       "missing paths",
			argv:       []string{"symbols", "upload", "--platform", "linux"},
			env:        env,
			wantStderr: "missing symbol file path",
		},
		{
			name:       "flags after paths",
			argv:       []string{"symbols", "upload", "--platform", "linux", path, "--json"},
			env:        env,
			wantStderr: "flags must come before file paths",
		},
		{
			name:       "unknown format",
			argv:       []string{"symbols", "upload", "--platform", "linux", "--format", "minidump", path},
			env:        env,
			wantStderr: "unknown format",
		},
		{
			name:       "debug-id with multiple files",
			argv:       []string{"symbols", "upload", "--platform", "windows", "--debug-id", "X", path, otherPath},
			env:        env,
			wantStderr: "--debug-id applies to exactly one file",
		},
		{
			name:       "nonexistent file",
			argv:       []string{"symbols", "upload", "--platform", "linux", filepath.Join(t.TempDir(), "missing.sym")},
			env:        env,
			wantStderr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.argv, tc.env)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)", code, ExitUsage, stdout, stderr)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantStderr)
			}
		})
	}
}

func TestUploadUndetectableFormat(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	path := writeSymFile(t, "mapping.txt", "com.example.Game -> a.a:\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "android", path}, envWith(srv.URL))
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0", hits.Load())
	}
	if !strings.Contains(stdout, "pass --format") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUploadJSONSuccessLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE windows x86_64 A1B2C3 app.pdb\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--json", "--platform", "windows", path}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var res struct {
		Path   string `json:"path"`
		OK     bool   `json:"ok"`
		Format string `json:"format"`
		Status int    `json:"status"`
		Symbol struct {
			SHA256Hex string `json:"sha256_hex"`
			ByteSize  int64  `json:"byte_size"`
		} `json:"symbol"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &res); err != nil {
		t.Fatalf("parse --json output %q: %v", stdout, err)
	}
	if !res.OK || res.Format != "breakpad" || res.Status != 201 || res.Symbol.SHA256Hex != "deadbeef" || res.Symbol.ByteSize != 42 {
		t.Fatalf("json result = %+v", res)
	}
}

func TestRootDispatch(t *testing.T) {
	env := map[string]string{}
	cases := []struct {
		name     string
		argv     []string
		wantCode int
		wantOut  string
	}{
		{"no args is usage", nil, ExitUsage, ""},
		{"help", []string{"help"}, ExitOK, "usage: shardpilot"},
		{"version", []string{"version"}, ExitOK, "shardpilot " + version},
		{"unknown command", []string{"frobnicate"}, ExitUsage, ""},
		// `symbols missing` is a later deliverable and deliberately does
		// not exist yet — not even as a stub.
		{"symbols missing does not exist", []string{"symbols", "missing"}, ExitUsage, ""},
		{"bare symbols", []string{"symbols"}, ExitUsage, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, code := runCLI(t, tc.argv, env)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d", code, tc.wantCode)
			}
			if tc.wantOut != "" && !strings.Contains(stdout, tc.wantOut) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout, tc.wantOut)
			}
		})
	}
}

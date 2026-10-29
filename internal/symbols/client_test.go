package symbols

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func noBackoff(int) time.Duration { return 0 }

func writeTempFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const createdEnvelope = `{"symbol":{"platform":"windows","format":"breakpad","debug_id":"A1B2C3","filename":"app.sym","byte_size":42,"sha256_hex":"deadbeef","uploaded_at":"2026-07-19T00:00:00Z"}}`

// countingReadCloser counts bytes handed out from disk so the stream test
// can observe how far ahead of the server the client has read.
type countingReadCloser struct {
	f *os.File
	n *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.f.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReadCloser) Close() error { return c.f.Close() }

// TestUploadStreamsFileInsteadOfBuffering is the stream-not-buffer proof: a
// 32 MiB file is uploaded while a guard asserts the client had read no more
// than half of it from disk by the time the server had consumed the first
// 64 KiB of the body. A buffering implementation (io.ReadAll before POST)
// reads the entire file before the server sees a single body byte, so it
// trips the guard; the io.Pipe implementation stays within socket-buffer
// distance of the server's read position.
func TestUploadStreamsFileInsteadOfBuffering(t *testing.T) {
	const fileSize = 32 << 20
	const serverRead = 64 << 10
	const bufferGuard = fileSize / 2

	content := bytes.Repeat([]byte{0xAB}, fileSize)
	path := writeTempFile(t, "big.pdb", content)

	var fromDisk atomic.Int64
	orig := openUpload
	openUpload = func(p string) (io.ReadCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return &countingReadCloser{f: f, n: &fromDisk}, nil
	}
	t.Cleanup(func() { openUpload = orig })

	var readAtCheckpoint atomic.Int64
	var bodyTotal atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, serverRead)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			t.Errorf("read first %d body bytes: %v", serverRead, err)
		}
		readAtCheckpoint.Store(fromDisk.Load())
		rest, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Errorf("drain body: %v", err)
		}
		bodyTotal.Store(rest + serverRead)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "k", Backoff: noBackoff}
	res, err := uploader.Upload(context.Background(), Request{
		Path:     path,
		Platform: "windows",
		Format:   FormatPDB,
		DebugID:  "A1B2C3",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", res.Attempts)
	}
	if got := readAtCheckpoint.Load(); got >= bufferGuard {
		t.Fatalf("client had read %d bytes from disk when the server had consumed only %d body bytes — the body looks buffered, not streamed (guard %d)", got, serverRead, bufferGuard)
	}
	if got := bodyTotal.Load(); got <= fileSize {
		t.Fatalf("server received %d body bytes, want more than the %d-byte file (multipart framing)", got, fileSize)
	}
}

// TestUploadMultipartContract pins the wire shape against the endpoint contract
// (POST /api/v1/symbols/upload): bearer
// auth, and the multipart fields platform, format, debug_id, symbol_file.
func TestUploadMultipartContract(t *testing.T) {
	content := []byte("MODULE windows x86_64 A1B2C3 app.pdb\n")
	path := writeTempFile(t, "app.sym", content)

	var gotAuth, gotPlatform, gotFormat, gotDebugID, gotFilename, gotPath string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		gotPlatform = r.FormValue("platform")
		gotFormat = r.FormValue("format")
		gotDebugID = r.FormValue("debug_id")
		file, header, err := r.FormFile("symbol_file")
		if err != nil {
			t.Errorf("symbol_file part: %v", err)
			http.Error(w, "missing symbol_file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		gotFilename = header.Filename
		gotFile, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL + "/", APIKey: "sp_test_abc", UserAgent: "shardpilot-cli/test", Backoff: noBackoff}
	res, err := uploader.Upload(context.Background(), Request{
		Path:     path,
		Platform: "windows",
		Format:   FormatBreakpad,
		DebugID:  "A1B2C3",
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gotPath != UploadPath {
		t.Fatalf("posted to %q, want %q", gotPath, UploadPath)
	}
	if gotAuth != "Bearer sp_test_abc" {
		t.Fatalf("Authorization = %q, want Bearer sp_test_abc", gotAuth)
	}
	if gotPlatform != "windows" || gotFormat != "breakpad" || gotDebugID != "A1B2C3" {
		t.Fatalf("fields platform=%q format=%q debug_id=%q", gotPlatform, gotFormat, gotDebugID)
	}
	if gotFilename != "app.sym" {
		t.Fatalf("symbol_file filename = %q, want app.sym", gotFilename)
	}
	if !bytes.Equal(gotFile, content) {
		t.Fatalf("symbol_file content mismatch: got %d bytes, want %d", len(gotFile), len(content))
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
	if res.Symbol.SHA256Hex != "deadbeef" || res.Symbol.Format != "breakpad" {
		t.Fatalf("parsed symbol envelope = %+v", res.Symbol)
	}
}

// TestUploadRetriesReopenFile proves a retried attempt re-streams the entire
// file from disk (the pipe body is not rewindable; every attempt re-opens).
func TestUploadRetriesReopenFile(t *testing.T) {
	content := bytes.Repeat([]byte("MODULE linux arm64 FF libgame.so\n"), 1000)
	path := writeTempFile(t, "libgame.sym", content)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 2 {
			http.Error(w, `{"error":{"code":"storage_unavailable","message":"try later"}}`, http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			t.Errorf("parse multipart on retry: %v", err)
		}
		file, _, err := r.FormFile("symbol_file")
		if err != nil {
			t.Errorf("symbol_file on retry: %v", err)
		} else {
			defer file.Close()
			got, _ := io.ReadAll(file)
			if !bytes.Equal(got, content) {
				t.Errorf("retried upload sent %d bytes, want the full %d", len(got), len(content))
			}
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "k", Retries: DefaultRetries, Backoff: noBackoff}
	res, err := uploader.Upload(context.Background(), Request{Path: path, Platform: "linux", Format: FormatBreakpad})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two 500s then success)", res.Attempts)
	}
	if hits.Load() != 3 {
		t.Fatalf("server hits = %d, want 3", hits.Load())
	}
}

func TestUpload4xxNeverRetried(t *testing.T) {
	path := writeTempFile(t, "app.sym", []byte("MODULE a b c d\n"))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"unauthorized","message":"missing bearer token"}}`)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "bad", Retries: DefaultRetries, Backoff: noBackoff}
	res, err := uploader.Upload(context.Background(), Request{Path: path, Platform: "linux", Format: FormatBreakpad})
	if err == nil {
		t.Fatal("want error on 401")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "unauthorized" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want exactly 1 — 4xx must never be retried", hits.Load())
	}
	if res.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", res.Attempts)
	}
}

func TestUpload5xxExhaustsRetriesThenFails(t *testing.T) {
	path := writeTempFile(t, "app.sym", []byte("MODULE a b c d\n"))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "backend down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "k", Retries: DefaultRetries, Backoff: noBackoff}
	res, err := uploader.Upload(context.Background(), Request{Path: path, Platform: "linux", Format: FormatBreakpad})
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want *APIError 503", err)
	}
	wantHits := int32(1 + DefaultRetries)
	if hits.Load() != wantHits {
		t.Fatalf("server hits = %d, want %d (first attempt + %d retries)", hits.Load(), wantHits, DefaultRetries)
	}
	if res.Attempts != int(wantHits) {
		t.Fatalf("attempts = %d, want %d", res.Attempts, wantHits)
	}
}

func TestUploadNetworkErrorRetried(t *testing.T) {
	path := writeTempFile(t, "app.sym", []byte("MODULE a b c d\n"))
	// A server that is immediately closed: every attempt is a connection
	// error, which must be retried like a 5xx.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "k", Retries: 2, Backoff: noBackoff}
	res, err := uploader.Upload(context.Background(), Request{Path: path, Platform: "linux", Format: FormatBreakpad})
	if err == nil {
		t.Fatal("want transport error")
	}
	if errors.As(err, new(*APIError)) {
		t.Fatalf("transport failure must not be an *APIError: %v", err)
	}
	if res.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (first + 2 retries)", res.Attempts)
	}
}

// TestUploadDeclaresExactContentLengthAndFormatHint pins the large-upload
// endpoint contract: the client sends the
// X-Shardpilot-Symbol-Format admission hint and an EXACT Content-Length
// (multipart frame + file size — never chunked transfer-encoding) whenever
// the file size is knowable, because the endpoint's raised per-format admission
// requires a declared length. The body still streams (the streaming test
// above), only the length is precomputed.
func TestUploadDeclaresExactContentLengthAndFormatHint(t *testing.T) {
	content := bytes.Repeat([]byte{0xCD}, 256<<10)
	path := writeTempFile(t, "big.pdb", content)

	var gotHint string
	var gotContentLength int64
	var gotTransferEncoding []string
	var gotBody int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHint = r.Header.Get(FormatHintHeader)
		gotContentLength = r.ContentLength
		gotTransferEncoding = r.TransferEncoding
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Errorf("drain body: %v", err)
		}
		gotBody = n
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "k", Backoff: noBackoff}
	if _, err := uploader.Upload(context.Background(), Request{
		Path:     path,
		Platform: "windows",
		Format:   FormatPDB,
		DebugID:  "A1B2C3",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gotHint != FormatPDB {
		t.Fatalf("%s = %q, want %q", FormatHintHeader, gotHint, FormatPDB)
	}
	if len(gotTransferEncoding) != 0 {
		t.Fatalf("TransferEncoding = %v, want none (declared length, not chunked)", gotTransferEncoding)
	}
	if gotContentLength <= int64(len(content)) {
		t.Fatalf("Content-Length = %d, want > the %d-byte file (frame included)", gotContentLength, len(content))
	}
	if gotContentLength != gotBody {
		t.Fatalf("Content-Length %d != %d bytes actually received — the declared length must be exact", gotContentLength, gotBody)
	}
}

// TestUploadFallsBackToChunkedWhenSizeUnknown pins the fallback: a file whose
// size cannot be stat'd still uploads exactly as before this change —
// chunked, no declared length (the endpoint then applies its legacy cap).
func TestUploadFallsBackToChunkedWhenSizeUnknown(t *testing.T) {
	content := []byte("MODULE windows x86_64 A1B2C3 app.pdb\n")
	path := writeTempFile(t, "app.sym", content)

	origSize := uploadFileSize
	uploadFileSize = func(string) (int64, error) { return 0, os.ErrNotExist }
	t.Cleanup(func() { uploadFileSize = origSize })

	var gotContentLength int64
	var gotBody int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		n, _ := io.Copy(io.Discard, r.Body)
		gotBody = n
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	uploader := &Uploader{BaseURL: srv.URL, APIKey: "k", Backoff: noBackoff}
	if _, err := uploader.Upload(context.Background(), Request{
		Path:     path,
		Platform: "windows",
		Format:   FormatBreakpad,
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gotContentLength != -1 {
		t.Fatalf("ContentLength = %d, want -1 (chunked fallback)", gotContentLength)
	}
	if gotBody <= int64(len(content)) {
		t.Fatalf("received %d body bytes, want the full multipart body", gotBody)
	}
}

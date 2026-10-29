package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The CLI sends every format the endpoint accepts: elf is detected from its
// magic, proguard and il2cpp_map are named with --format. Each upload carries
// the format in the multipart field and in the admission hint header.
func TestUploadSendsEveryEndpointFormat(t *testing.T) {
	type got struct{ format, hint, debugID string }
	var mu sync.Mutex
	var received []got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, got{r.FormValue("format"), r.Header.Get("X-Shardpilot-Symbol-Format"), r.FormValue("debug_id")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	elf := writeSymFile(t, "libgame.so", "\x7fELF\x02\x01\x01\x00")
	mapping := writeSymFile(t, "mapping.txt", "com.example.Game -> a.a:\n")
	il2cpp := writeSymFile(t, "LineNumberMappings.json", `{"a":[1,2]}`)
	cases := []struct {
		name string
		argv []string
		want got
	}{
		{"elf, detected, no debug id", []string{"--platform", "android", elf}, got{"elf", "elf", ""}},
		{"elf, detected, with a debug id", []string{"--platform", "android", "--debug-id", "B7E1C2D3A4F5", elf}, got{"elf", "elf", "B7E1C2D3A4F5"}},
		{"proguard, named", []string{"--platform", "android", "--format", "proguard", "--debug-id", "build-42", mapping}, got{"proguard", "proguard", "build-42"}},
		{"il2cpp_map, named", []string{"--platform", "android", "--format", "il2cpp_map", "--debug-id", "0123ABCD", il2cpp}, got{"il2cpp_map", "il2cpp_map", "0123ABCD"}},
	}
	for i, tc := range cases {
		argv := append([]string{"symbols", "upload"}, tc.argv...)
		stdout, stderr, code := runCLI(t, argv, envWith(srv.URL))
		if code != ExitOK {
			t.Errorf("%s: exit = %d, want 0; stdout %q stderr %q", tc.name, code, stdout, stderr)
			continue
		}
		mu.Lock()
		if len(received) != i+1 || received[i] != tc.want {
			t.Errorf("%s: server received %+v, want %+v", tc.name, received, tc.want)
		}
		mu.Unlock()
	}
}

// proguard and il2cpp_map need a debug id the endpoint cannot derive, so the
// CLI refuses them without --debug-id before any request, and says what to
// pass.
func TestUploadDebugIDRequiredFormats(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	mapping := writeSymFile(t, "mapping.txt", "com.example.Game -> a.a:\n")
	for format, why := range map[string]string{
		"proguard":   "the app build id",
		"il2cpp_map": "the debug id of the native module",
	} {
		stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "android", "--format", format, mapping}, envWith(srv.URL))
		if code != ExitUsage || !strings.Contains(stdout, format+" uploads require --debug-id") || !strings.Contains(stdout, why) {
			t.Errorf("%s: exit = %d stdout = %q, want %d naming --debug-id and %q", format, code, stdout, ExitUsage, why)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0", hits.Load())
	}
}

// A file the CLI cannot identify points at --format, and says which formats
// are never detected.
func TestUploadUndetectableNamesTheNeverDetectedFormats(t *testing.T) {
	path := writeSymFile(t, "mapping.txt", "com.example.Game -> a.a:\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "android", path}, envWith("http://127.0.0.1:1"))
	if code != ExitUsage || !strings.Contains(stdout, "pass --format (proguard and il2cpp_map are never detected)") {
		t.Fatalf("exit = %d stdout = %q", code, stdout)
	}
}

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// shippedDefaultSymbolsURL is the default as built, captured before TestMain
// points it at a closed loopback port so that no test can reach the real host.
var shippedDefaultSymbolsURL = defaultSymbolsURL

func TestMain(m *testing.M) {
	defaultSymbolsURL = "http://127.0.0.1:1"
	os.Exit(m.Run())
}

func TestDefaultSymbolsURLIsThePublicSymbolsHost(t *testing.T) {
	if shippedDefaultSymbolsURL != "https://symbols.shardpilot.com" {
		t.Fatalf("default symbols URL = %q, want https://symbols.shardpilot.com", shippedDefaultSymbolsURL)
	}
}

// With neither --url nor SHARDPILOT_SYMBOLS_URL, both commands use the default
// symbols host instead of refusing to run.
func TestNoURLUsesTheDefaultSymbolsHost(t *testing.T) {
	var paths []string
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/symbols/wanted" {
			io.WriteString(w, `{"wanted":[],"total":0}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()
	orig := defaultSymbolsURL
	defaultSymbolsURL = srv.URL
	t.Cleanup(func() { defaultSymbolsURL = orig })

	env := map[string]string{"SHARDPILOT_SYMBOLS_KEY": "sp_test_key"}
	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	for _, argv := range [][]string{
		{"symbols", "upload", "--platform", "linux", path},
		{"symbols", "missing"},
	} {
		if _, stderr, code := runCLI(t, argv, env); code != ExitOK {
			t.Errorf("%s: exit = %d, want 0; stderr: %s", argv[1], code, stderr)
		}
	}
	if hits.Load() != 2 || paths[0] != "POST /api/v1/symbols/upload" || paths[1] != "GET /api/v1/symbols/wanted" {
		t.Fatalf("requests to the default host = %v", paths)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":              true,
		"LOCALHOST":              true,
		"127.0.0.1":              true,
		"127.10.20.30":           true,
		"::1":                    true,
		"localhost.example.com":  false,
		"symbols.shardpilot.com": false,
		"10.0.0.5":               false,
		"192.168.1.10":           false,
		"0.0.0.0":                false,
		"::":                     false,
		"":                       false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

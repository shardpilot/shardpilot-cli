package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type symbolsKeyTransport func(*http.Request) (*http.Response, error)

func (f symbolsKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSymbolsKeyEnvironment(t *testing.T) {
	path := writeSymFile(t, "app.sym", "MODULE linux x86_64 A1B2C3 app\n")
	for _, command := range []string{"upload", "missing"} {
		for _, tc := range []struct {
			name    string
			current string
			legacy  string
		}{
			{"current only", "new-symbols-key", ""},
			{"both names", "new-symbols-key", "old-api-key"},
			{"legacy only", "", "old-api-key"},
			{"neither name", "", ""},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				calls := 0
				original := http.DefaultTransport
				http.DefaultTransport = symbolsKeyTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if got := r.Header.Get("Authorization"); got != "Bearer "+tc.current {
						t.Errorf("request did not use the symbols key")
					}
					body, status := `{"wanted":[],"total":0}`, http.StatusOK
					if command == "upload" {
						if r.Method != http.MethodPost || r.URL.Path != "/api/v1/symbols/upload" {
							t.Errorf("unexpected upload request: %s %s", r.Method, r.URL.Path)
						}
						if _, err := io.Copy(io.Discard, r.Body); err != nil {
							t.Errorf("read upload: %v", err)
						}
						r.Body.Close()
						body, status = createdEnvelope, http.StatusCreated
					} else if r.Method != http.MethodGet || r.URL.Path != "/api/v1/symbols/wanted" {
						t.Errorf("unexpected listing request: %s %s", r.Method, r.URL.Path)
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				t.Cleanup(func() { http.DefaultTransport = original })
				argv := []string{"symbols", command}
				if command == "upload" {
					argv = append(argv, "--platform", "linux", path)
				}
				stdout, stderr, code := runCLI(t, argv, map[string]string{
					"SHARDPILOT_SYMBOLS_URL":     "https://symbols.example.invalid",
					"SHARDPILOT_SYMBOLS_KEY":     tc.current,
					"SHARDPILOT_SYMBOLS_CHANNEL": "develop",
					"SHARDPILOT_API_KEY":         tc.legacy,
				})
				if tc.current == "" {
					if code != ExitUsage || calls != 0 || !strings.Contains(stderr, "SHARDPILOT_SYMBOLS_KEY is not set") {
						t.Errorf("missing symbols key: exit=%d requests=%d stderr=%q", code, calls, stderr)
					}
				} else if code != ExitOK || calls != 1 {
					t.Errorf("symbols key: exit=%d requests=%d stderr=%q", code, calls, stderr)
				}
				for _, secret := range []string{"new-symbols-key", "old-api-key"} {
					if strings.Contains(stdout+stderr, secret) {
						t.Error("command output exposed a credential")
					}
				}
			})
		}
	}
}

func TestSymbolsKeyHelp(t *testing.T) {
	for _, command := range []string{"upload", "missing"} {
		t.Run(command, func(t *testing.T) {
			_, stderr, code := runCLI(t, []string{"symbols", command, "-h"}, nil)
			if code != ExitOK || !strings.Contains(stderr, "SHARDPILOT_SYMBOLS_KEY") || strings.Contains(stderr, "SHARDPILOT_API_KEY") {
				t.Errorf("help must name only the symbols key variable: exit=%d stderr=%q", code, stderr)
			}
		})
	}
}

func TestSymbolsKeyVersion(t *testing.T) {
	t.Run("development", func(t *testing.T) {
		stdout, stderr, code := runCLI(t, []string{"version"}, nil)
		if code != ExitOK || stdout != "shardpilot 0.3.0-dev\n" || stderr != "" {
			t.Errorf("version: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
	t.Run("release stamp", func(t *testing.T) {
		original := version
		version = "v1.2.3"
		t.Cleanup(func() { version = original })
		stdout, _, code := runCLI(t, []string{"version"}, nil)
		if code != ExitOK || stdout != "shardpilot v1.2.3\n" {
			t.Errorf("release version: exit=%d stdout=%q", code, stdout)
		}
	})
}

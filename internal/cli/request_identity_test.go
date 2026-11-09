package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSDKIdentityEveryRequest(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })
	instantRetries(t)
	for _, tc := range []struct{ name, version, identity string }{
		{"development", developmentVersion, "cli/0.4.0-dev"},
		{"tagged", "v1.2.3-alpha.1+build.9", "cli/1.2.3-alpha.1"},
		{"module-version", "v0.0.0-20261009010203-abcdef012345", "cli/0.0.0-20261009010203-abcdef012345"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.version
			var mu sync.Mutex
			var requests []struct{ path, userAgent, identity string }
			uploadHits, wantedHits := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, struct{ path, userAgent, identity string }{r.URL.Path, r.UserAgent(), r.Header.Get("X-ShardPilot-SDK")})
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/symbols/upload":
					uploadHits++
					if uploadHits == 1 {
						w.WriteHeader(503)
						_, _ = io.WriteString(w, `{"error":"unavailable"}`)
						return
					}
					w.WriteHeader(201)
					_, _ = io.WriteString(w, createdEnvelope)
				case "/api/v1/symbols/wanted":
					wantedHits++
					if wantedHits == 1 {
						w.WriteHeader(503)
						_, _ = io.WriteString(w, `{"error":"unavailable"}`)
						return
					}
					switch r.URL.Query().Get("cursor") {
					case "":
						_, _ = io.WriteString(w, wantedPage1)
					case "cursor-2":
						_, _ = io.WriteString(w, wantedPage2)
					default:
						t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
						w.WriteHeader(400)
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			path := writeSymFile(t, "synthetic.sym", "MODULE windows x86_64 A1B2C3 synthetic.pdb\n")
			for _, args := range [][]string{{"symbols", "upload", "--platform", "windows", path}, {"symbols", "missing"}} {
				_, errOutput, code := runCLI(t, args, envWith(srv.URL))
				if code != ExitOK {
					t.Fatalf("command exit%d: %s", code, errOutput)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			t.Run("request-count", func(t *testing.T) {
				if uploadHits != 2 || wantedHits != 3 || len(requests) != 5 {
					t.Fatalf("upload%d wanted%d requests%d", uploadHits, wantedHits, len(requests))
				}
			})
			for i, request := range requests {
				t.Run(strconv.Itoa(i), func(t *testing.T) {
					t.Run("user-agent", func(t *testing.T) {
						if request.userAgent != "shardpilot-cli/"+tc.version {
							t.Fatalf("User-Agent=%q", request.userAgent)
						}
					})
					t.Run("sdk-identity", func(t *testing.T) {
						if request.identity != tc.identity {
							t.Fatalf("%s SDK identity=%q, want%q", request.path, request.identity, tc.identity)
						}
					})
				})
			}
		})
	}
}

func TestSDKIdentityVersionGrammar(t *testing.T) {
	for _, tc := range []struct{ name, version, identity string }{
		{"development", developmentVersion, "cli/0.4.0-dev"},
		{"tag", "v1.2.3", "cli/1.2.3"},
		{"untagged", "1.2.3", "cli/1.2.3"},
		{"metadata", "v1.2.3-alpha.1+build.009", "cli/1.2.3-alpha.1"},
		{"numeric-prerelease", "1.2.3-0.9.123", "cli/1.2.3-0.9.123"},
		{"alphanumeric-prerelease", "1.2.3-01a.0-1", "cli/1.2.3-01a.0-1"},
		{"pseudo-version", "v0.0.0-20261009010203-abcdef012345", "cli/0.0.0-20261009010203-abcdef012345"},
		{"64-bytes", "1.2.3-" + strings.Repeat("a", 54), "cli/1.2.3-" + strings.Repeat("a", 54)},
		{"long-metadata", "1.2.3+" + strings.Repeat("a", 100), "cli/1.2.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sdkIdentityForVersion(tc.version)
			if err != nil || got != tc.identity {
				t.Fatalf("identity=%q err=%v, want %q", got, err, tc.identity)
			}
		})
	}
	for _, tc := range []struct{ name, version string }{
		{"empty", ""}, {"devel", "(devel)"}, {"repeated-v", "vv1.2.3"},
		{"missing-patch", "1.2"}, {"extra-core", "1.2.3.4"},
		{"leading-major-zero", "01.2.3"}, {"leading-minor-zero", "1.02.3"}, {"leading-patch-zero", "1.2.03"},
		{"leading-prerelease-zero", "1.2.3-alpha.01"},
		{"empty-prerelease", "1.2.3-"}, {"empty-prerelease-part", "1.2.3-alpha..1"},
		{"empty-metadata", "1.2.3+"}, {"empty-metadata-part", "1.2.3+build..1"},
		{"repeated-metadata", "1.2.3+build+other"}, {"metadata-space", "1.2.3+build other"},
		{"non-ascii", "1.2.3-β"}, {"underscore", "1.2.3-alpha_one"},
		{"crlf", "1.2.3\r\nInjected: value"}, {"trailing-newline", "1.2.3\n"},
		{"tab", "1.2.3\t"}, {"del", "1.2.3-\x7f"},
		{"leading-space", " 1.2.3"}, {"slash", "1.2.3/path"},
		{"65-bytes", "1.2.3-" + strings.Repeat("a", 55)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := sdkIdentityForVersion(tc.version); err == nil || got != "" {
				t.Fatalf("invalid version accepted: identity=%q err=%v", got, err)
			}
		})
	}
}

func TestSDKIdentityInvalidVersionSendsNothing(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })
	for _, invalid := range []string{"invalid", "v1.2.3-alpha.01", "v1.2.3+bad metadata", "1.2.3\r\nInjected: value", "1.2.3-" + strings.Repeat("a", 55)} {
		version = invalid
		for _, command := range []string{"upload", "missing"} {
			t.Run(command+"/"+strconv.Itoa(len(invalid)), func(t *testing.T) {
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if command == "upload" {
						w.WriteHeader(http.StatusCreated)
						_, _ = io.WriteString(w, createdEnvelope)
					} else {
						_, _ = io.WriteString(w, `{"wanted":[]}`)
					}
				}))
				defer srv.Close()
				args := []string{"symbols", command}
				if command == "upload" {
					args = append(args, "--platform", "windows", writeSymFile(t, "synthetic.sym", "MODULE windows x86_64 A1B2C3 synthetic.pdb\n"))
				}
				_, errOutput, code := runCLI(t, args, envWith(srv.URL))
				if code != ExitUsage || !strings.Contains(errOutput, "request identity") || hits.Load() != 0 {
					t.Fatalf("exit=%d requests=%d stderr=%s", code, hits.Load(), errOutput)
				}
			})
		}
	}
}

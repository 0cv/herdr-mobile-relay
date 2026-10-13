package appdeploy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestVerifyPublicAllowsPagesEntryDirectoryRedirect(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		t.Run(map[bool]string{false: "relative", true: "absolute"}[absolute], func(t *testing.T) {
			root := t.TempDir()
			writeWebReleaseFixture(t, root)
			descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
			if err != nil {
				t.Fatal(err)
			}
			entry := descriptor.Entry
			directory := strings.TrimSuffix(entry, "index.html")
			var canonicalIdentity, canonicalBrotli, queryIdentity, queryBrotli atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == entry {
					location := directory
					if absolute {
						location = "http://" + request.Host + location
					}
					if request.URL.RawQuery != "" {
						location += "?" + request.URL.RawQuery
					}
					http.Redirect(writer, request, location, http.StatusPermanentRedirect)
					return
				}
				if request.URL.Path == directory {
					if request.URL.RawQuery == "" {
						if request.Header.Get("Cache-Control") != "" {
							t.Error("canonical redirect bypassed the edge cache")
						}
						if request.Header.Get("Accept-Encoding") == "br" {
							canonicalBrotli.Add(1)
						} else {
							canonicalIdentity.Add(1)
						}
					} else if request.Header.Get("Accept-Encoding") == "br" {
						queryBrotli.Add(1)
					} else {
						queryIdentity.Add(1)
					}
					request = request.Clone(request.Context())
					request.URL.Path = entry
				}
				webFixtureHandler(root, nil)(writer, request)
			}))
			defer server.Close()

			job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
			if err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
				t.Fatal("valid Pages bundle was retried")
				return 0
			}); err != nil {
				t.Fatal(err)
			}
			for name, count := range map[string]int32{
				"canonical identity": canonicalIdentity.Load(), "canonical Brotli": canonicalBrotli.Load(),
				"query identity": queryIdentity.Load(), "query Brotli": queryBrotli.Load(),
			} {
				if count != 1 {
					t.Errorf("%s requests = %d, want 1", name, count)
				}
			}
		})
	}
}

func TestVerifyPublicRejectsOtherEntryRedirects(t *testing.T) {
	for _, variant := range []string{
		"query", "empty query", "fragment", "other build", "encoded path", "cross origin",
		"credentials", "301", "302", "307", "chain", "loop",
	} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			writeWebReleaseFixture(t, root)
			descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
			if err != nil {
				t.Fatal(err)
			}
			entry := descriptor.Entry
			directory := strings.TrimSuffix(entry, "index.html")
			var targetRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				targetRequests.Add(1)
				http.NotFound(writer, request)
			}))
			defer upstream.Close()
			var directoryRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == entry && request.URL.RawQuery == "" {
					location, status := directory, http.StatusPermanentRedirect
					switch variant {
					case "query":
						location += "?token=private-redirect-marker"
					case "empty query":
						location += "?"
					case "fragment":
						location += "#private-redirect-marker"
					case "other build":
						location = "/builds/1.2.2-1-bbbbbbbbbbbbbbbb/"
					case "encoded path":
						location = strings.Replace(directory, "builds", "%62uilds", 1)
					case "cross origin":
						location = upstream.URL + directory
					case "credentials":
						location = "http://private-redirect-marker@" + request.Host + directory
					case "301":
						status = http.StatusMovedPermanently
					case "302":
						status = http.StatusFound
					case "307":
						status = http.StatusTemporaryRedirect
					}
					http.Redirect(writer, request, location, status)
					return
				}
				if request.URL.Path == directory && (variant == "chain" || variant == "loop") {
					directoryRequests.Add(1)
					location := "/private-redirect-marker"
					if variant == "loop" {
						location = directory
					}
					http.Redirect(writer, request, location, http.StatusPermanentRedirect)
					return
				}
				if request.URL.Path != "/release.json" {
					targetRequests.Add(1)
				}
				webFixtureHandler(root, nil)(writer, request)
			}))
			defer server.Close()

			job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
			err = verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
				t.Fatal("unsafe redirect was retried")
				return 0
			})
			want := errPublicCanonicalRedirect
			if variant == "cross origin" || variant == "credentials" {
				want = errPublicOriginRedirect
			}
			if !errors.Is(err, want) || !strings.Contains(err.Error(), entry) || strings.Contains(err.Error(), "private-redirect-marker") {
				t.Fatalf("verifyPublicWith() error = %v", err)
			}
			if targetRequests.Load() != 0 {
				t.Fatalf("unsafe redirect followed: %d requests", targetRequests.Load())
			}
			wantDirectoryRequests := int32(0)
			if variant == "chain" || variant == "loop" {
				wantDirectoryRequests = 1
			}
			if directoryRequests.Load() != wantDirectoryRequests {
				t.Fatalf("directory requests = %d, want %d", directoryRequests.Load(), wantDirectoryRequests)
			}
		})
	}
}

func TestVerifyPublicRejectsAssetDirectoryRedirects(t *testing.T) {
	for _, kind := range []string{"javascript", "stylesheet"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeWebReleaseFixture(t, root)
			descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
			if err != nil {
				t.Fatal(err)
			}
			var targetRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/"+descriptor.Files[kind].Path {
					http.Redirect(writer, request, "/assets/", http.StatusPermanentRedirect)
					return
				}
				if request.URL.Path == "/assets/" {
					targetRequests.Add(1)
				}
				webFixtureHandler(root, nil)(writer, request)
			}))
			defer server.Close()

			job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
			err = verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
				t.Fatal("asset redirect was retried")
				return 0
			})
			if !errors.Is(err, errPublicCanonicalRedirect) || targetRequests.Load() != 0 {
				t.Fatalf("error = %v, redirected requests = %d", err, targetRequests.Load())
			}
		})
	}
}

func TestVerifyPublicEntryRedirectPreservesClientPolicy(t *testing.T) {
	var targetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/index.html") {
			http.Redirect(writer, request, strings.TrimSuffix(request.URL.Path, "index.html"), http.StatusPermanentRedirect)
			return
		}
		targetRequests.Add(1)
	}))
	defer server.Close()
	refused := errors.New("caller refused redirect")
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return refused }
	_, _, err := fetchPublicResource(t.Context(), publicClient(client, server.URL), server.URL,
		"builds/1.2.3-1-aaaaaaaaaaaaaaaa/index.html", "", "identity")
	if !errors.Is(err, refused) || targetRequests.Load() != 0 {
		t.Fatalf("error = %v, redirected requests = %d", err, targetRequests.Load())
	}
}

func TestVerifyPublicChecksRedirectedEntryBytesAndMIME(t *testing.T) {
	for _, encoding := range []string{"identity", "br"} {
		for _, fault := range []string{"digest", "Content-Type"} {
			t.Run(encoding+"/"+fault, func(t *testing.T) {
				root := t.TempDir()
				writeWebReleaseFixture(t, root)
				descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
				if err != nil {
					t.Fatal(err)
				}
				entry := descriptor.Entry
				directory := strings.TrimSuffix(entry, "index.html")
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == entry {
						http.Redirect(writer, request, directory, http.StatusPermanentRedirect)
						return
					}
					if request.URL.Path == directory {
						if request.Header.Get("Accept-Encoding") == encoding {
							if fault == "digest" {
								writer.Header().Set("Content-Type", "text/html")
								_, _ = writer.Write([]byte("private-response-marker"))
								return
							}
							writer.Header().Set("Content-Type", "text/plain")
						}
						request = request.Clone(request.Context())
						request.URL.Path = entry
					}
					webFixtureHandler(root, nil)(writer, request)
				}))
				defer server.Close()

				job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
				err = verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
					t.Fatal("invalid entry was retried")
					return 0
				})
				if err == nil || !strings.Contains(err.Error(), entry) || !strings.Contains(err.Error(), fault) || strings.Contains(err.Error(), "private-response-marker") {
					t.Fatalf("verifyPublicWith() error = %v", err)
				}
			})
		}
	}
}

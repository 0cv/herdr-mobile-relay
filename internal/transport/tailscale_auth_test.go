package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/coder/websocket"
)

func TestTailscaleS2OriginlessAdmission(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(key, "")
	}
	for _, keyed := range []bool{true, false} {
		name := "keyed"
		if !keyed {
			name = "legacy-loopback"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Host: "127.0.0.1", Transport: config.TransportCloudflare}
			if keyed {
				cfg.Transport = config.TransportTailscale
				cfg.Token = strings.Repeat("k", 32)
			}
			hub := NewHub(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			// Synthetic unit identity only: not device enrollment, TLS, or Layer 1.
			hub.SetE2EEAuthResolver(fixedE2EEAuthResolver{secret: []byte(cfg.Token)})
			var connections, messages atomic.Int64
			delivered := make(chan string, 16)
			hub.SetOnConnect(func(c *ClientConn) { connections.Add(1) })
			hub.SetHandler(func(c *ClientConn, msg map[string]any, admitted func()) {
				defer admitted()
				messages.Add(1)
				_, authenticated := c.Identity()
				nonce, _ := msg["nonce"].(string)
				if authenticated != keyed {
					nonce = "unexpected-identity"
				}
				delivered <- nonce
			})
			completed := make(chan string, 16)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if shutdownErr := hub.Shutdown(ctx); shutdownErr != nil {
					t.Error(shutdownErr)
				}
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { completed <- r.URL.Path }()
				if r.Header.Get("Origin") != "" {
					t.Error("fixture unexpectedly supplied Origin")
				}
				hub.HandleWebSocket(w, r)
			}), ReadHeaderTimeout: 2 * time.Second}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(listener) }()
			shutdown := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := hub.Shutdown(ctx); err != nil {
					t.Error("bounded hub shutdown:", err)
				}
				if err := server.Close(); err != nil {
					t.Error(err)
				}
				select {
				case err := <-serveDone:
					if !errors.Is(err, http.ErrServerClosed) {
						t.Error(err)
					}
				case <-ctx.Done():
					t.Error("HTTP server did not finish")
				}
			}
			defer shutdown()
			url := "ws://" + listener.Addr().String()
			httpTransport := &http.Transport{Proxy: nil}
			defer httpTransport.CloseIdleConnections()
			client := &http.Client{Transport: httpTransport, Timeout: 3 * time.Second}
			waitComplete := func(t *testing.T, path string) {
				t.Helper()
				select {
				case got := <-completed:
					if got != path {
						t.Fatal("wrong completed connection")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("connection handler did not finish")
				}
			}
			dial := func(ctx context.Context, path string, protocol bool, authorization bool) (*websocket.Conn, *http.Response, error) {
				options := &websocket.DialOptions{HTTPClient: client, HTTPHeader: make(http.Header)}
				if protocol {
					options.Subprotocols = []string{e2eeSubprotocol}
				}
				if authorization {
					options.HTTPHeader.Set("Authorization", "Bearer forbidden-fixture")
				}
				return websocket.Dial(ctx, url+path, options)
			}
			positive := func(nonce string) []byte {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				conn, _, err := dial(ctx, "/"+nonce, keyed, false)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				var session *e2eeSession
				var hello []byte
				if keyed {
					session, hello, _ = testClientE2EEHandshake(t, ctx, conn, cfg.Token, func() {})
				}
				data, err := json.Marshal(map[string]string{"type": "s2_probe", "nonce": nonce})
				if err != nil {
					t.Fatal(err)
				}
				if keyed {
					data, err = session.seal(data)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-delivered:
					if got != nonce {
						t.Fatal("wrong encrypted nonce or identity")
					}
				case <-ctx.Done():
					t.Fatal("positive control did not dispatch")
				}
				conn.CloseNow()
				waitComplete(t, "/"+nonce)
				return hello
			}
			hello := positive("before")
			if !keyed {
				if connections.Load() != 1 || messages.Load() != 1 {
					t.Fatal("legacy control counts")
				}
				return
			}
			for _, tc := range []struct {
				name                    string
				protocol, authorization bool
				query                   string
				handshake               bool
			}{
				{name: "no-subprotocol"}, {name: "authorization", protocol: true, authorization: true}, {name: "query-key", protocol: true, query: "?token=forbidden-fixture"},
				{name: "plaintext", protocol: true, handshake: true}, {name: "unknown-credential", protocol: true, handshake: true}, {name: "bad-proof", protocol: true, handshake: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					path := "/" + tc.name
					conn, response, err := dial(ctx, path+tc.query, tc.protocol, tc.authorization)
					if !tc.handshake {
						if conn != nil {
							conn.CloseNow()
							t.Fatal("forbidden upgrade succeeded")
						}
						if err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
							t.Fatal("missing explicit upgrade rejection")
						}
						if response.Body != nil {
							response.Body.Close()
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						defer conn.CloseNow()
						payload := []byte(`{"type":"s2_probe","nonce":"plaintext-negative"}`)
						if tc.name != "plaintext" {
							var greeting e2eeClientHello
							if err := json.Unmarshal(hello, &greeting); err != nil {
								t.Fatal(err)
							}
							greeting.Nonce = base64.RawURLEncoding.EncodeToString([]byte((tc.name + strings.Repeat("-", 32))[:32]))
							if tc.name == "unknown-credential" {
								greeting.AuthID = "unknown-s2"
							} else {
								greeting.Proof = strings.Repeat("A", 43)
							}
							payload, err = json.Marshal(greeting)
							if err != nil {
								t.Fatal(err)
							}
						}
						if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
							t.Fatal(err)
						}
						if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
							t.Fatal("expected explicit close, not data or deadline")
						}
						conn.CloseNow()
					}
					waitComplete(t, path)
					if connections.Load() != 1 || messages.Load() != 1 {
						t.Fatal("failed attempt registered or dispatched")
					}
				})
			}
			positive("after")
			// Drain all ingress and handler work before the final negative assertion;
			// HTTP completion plus shutdown excludes an immediate-empty-channel race.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := hub.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if connections.Load() != 2 || messages.Load() != 2 {
				t.Fatal("unexpected registration or dispatch after drain")
			}
		})
	}
}

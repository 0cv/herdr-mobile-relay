package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
)

// The sweep runs through the injected function only; these tests never reach
// the machine's launchd. The listener-bound signal is replaced too, so no test
// binds a socket.
func replaceListenerBound(t *testing.T, bound <-chan struct{}) {
	t.Helper()
	previous := serverListenerBound
	serverListenerBound = func(*app.Server) <-chan struct{} { return bound }
	t.Cleanup(func() { serverListenerBound = previous })
}

func TestServeSweepsWorkerJobsOnlyAfterTheListenerIsBound(t *testing.T) {
	for _, mode := range []string{config.TransportCloudflare, config.TransportGateway} {
		for _, command := range []string{"serve", "serve-installed"} {
			for _, failing := range []bool{false, true} {
				t.Run(mode+"/"+command+"/sweep-failing-"+strconv.FormatBool(failing), func(t *testing.T) {
					installedCommandEnv(t, mode)
					bound := make(chan struct{})
					replaceListenerBound(t, bound)
					var listening atomic.Bool
					var sweeps atomic.Int32
					swept := make(chan bool, 4)
					code, err := runCommand([]string{command}, func(server *app.Server, ctx context.Context) error {
						if server == nil || ctx.Err() != nil {
							t.Fatal("the runner started without a constructed server")
						}
						select {
						case <-swept:
							t.Fatal("worker jobs were swept before the listener was bound")
						default:
						}
						listening.Store(true)
						close(bound)
						select {
						case atBind := <-swept:
							if !atBind {
								t.Fatal("worker jobs were swept before the listener was bound")
							}
						case <-time.After(10 * time.Second):
							t.Fatal("worker jobs were not swept after the listener was bound")
						}
						return nil
					}, func(context.Context) error {
						sweeps.Add(1)
						swept <- listening.Load()
						if failing {
							return errors.New("launchctl list: exit status 1")
						}
						return nil
					})
					if code != 0 || err != nil {
						t.Fatalf("%s = (%d, %v); a worker sweep failure must not block startup", command, code, err)
					}
					if got := sweeps.Load(); got != 1 {
						t.Fatalf("sweeps = %d, want exactly one", got)
					}
				})
			}
		}
	}
}

// A duplicate start fails to bind the listener another process holds: the
// runner returns its error and the signal never closes, so nothing is swept.
func TestFailedBindNeverSweepsWorkerJobs(t *testing.T) {
	for _, mode := range []string{config.TransportCloudflare, config.TransportGateway} {
		for _, command := range []string{"serve", "serve-installed"} {
			t.Run(mode+"/"+command, func(t *testing.T) {
				installedCommandEnv(t, mode)
				replaceListenerBound(t, make(chan struct{}))
				var sweeps atomic.Int32
				bindErr := errors.New("listen tcp: address already in use")
				code, err := runCommand([]string{command}, func(*app.Server, context.Context) error {
					return bindErr
				}, func(context.Context) error {
					sweeps.Add(1)
					return nil
				})
				if code != 1 || !errors.Is(err, bindErr) {
					t.Fatalf("%s = (%d, %v), want the bind failure", command, code, err)
				}
				if got := sweeps.Load(); got != 0 {
					t.Fatalf("a relay that never bound its listener swept worker jobs %d times", got)
				}
			})
		}
	}
}

func TestRefusedStartupNeverSweepsWorkerJobs(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{name: "tokenless", key: "HERDR_RELAY_TOKEN"},
		{name: "managed identity", key: "HERDR_RELAY_RUN_ID", value: "foreground"},
		{name: "managed transport", key: "HERDR_RELAY_TRANSPORT", value: config.TransportTailscale},
		{name: "BYO transport", key: "HERDR_RELAY_TRANSPORT", value: config.TransportTailscaleExternal},
		{name: "CLI transport", key: "HERDR_RELAY_TRANSPORT", value: config.TransportTailscaleCLI},
	} {
		t.Run(test.name, func(t *testing.T) {
			installedCommandEnv(t, config.TransportCloudflare)
			t.Setenv(test.key, test.value)
			code, err := runCommand([]string{"serve-installed"}, func(*app.Server, context.Context) error {
				t.Fatal("refused installed startup reached the server")
				return nil
			}, func(context.Context) error {
				t.Fatal("refused installed startup swept launchd worker jobs")
				return nil
			})
			if code != 1 || err == nil {
				t.Fatalf("refused startup = (%d, %v), want a refusal", code, err)
			}
		})
	}
}

func TestSweepWorkerJobsStaysOffTailscaleTransports(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	closed := make(chan struct{})
	close(closed)
	for transport, want := range map[string]int{
		config.TransportCloudflare:        1,
		config.TransportGateway:           1,
		config.TransportTailscale:         0,
		config.TransportTailscaleCLI:      0,
		config.TransportTailscaleExternal: 0,
		"":                                0,
		"unknown":                         0,
	} {
		t.Run(transport, func(t *testing.T) {
			calls := 0
			sweepWorkerJobs(t.Context(), transport, func(context.Context) error {
				calls++
				return errors.New("launchctl list: exit status 1")
			}, logger)
			if calls != want {
				t.Fatalf("sweeps = %d, want %d", calls, want)
			}
			sweepWorkerJobs(t.Context(), transport, nil, logger)

			// Waiting on an already-bound listener sweeps exactly where the
			// direct call does, and never for a transport that stays off launchd.
			var sweeps atomic.Int32
			done := make(chan struct{}, 4)
			sweepWorkerJobsOnceBound(t.Context(), closed, transport, func(context.Context) error {
				sweeps.Add(1)
				done <- struct{}{}
				return nil
			}, logger)
			if want == 0 {
				time.Sleep(20 * time.Millisecond)
				if got := sweeps.Load(); got != 0 {
					t.Fatalf("a %q relay swept %d times once bound", transport, got)
				}
				return
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("a bound %q relay did not sweep", transport)
			}
		})
	}
}

func TestSweepWorkerJobsOnceBoundWaitsForTheListener(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("never bound", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var sweeps atomic.Int32
		sweepWorkerJobsOnceBound(ctx, make(chan struct{}), config.TransportCloudflare, func(context.Context) error {
			sweeps.Add(1)
			return nil
		}, logger)
		cancel()
		time.Sleep(20 * time.Millisecond)
		if got := sweeps.Load(); got != 0 {
			t.Fatalf("a relay cancelled before binding swept %d times", got)
		}
	})
	t.Run("no signal", func(t *testing.T) {
		var sweeps atomic.Int32
		sweepWorkerJobsOnceBound(t.Context(), nil, config.TransportCloudflare, func(context.Context) error {
			sweeps.Add(1)
			return nil
		}, logger)
		time.Sleep(20 * time.Millisecond)
		if got := sweeps.Load(); got != 0 {
			t.Fatalf("a server without a bind signal swept %d times", got)
		}
	})
}

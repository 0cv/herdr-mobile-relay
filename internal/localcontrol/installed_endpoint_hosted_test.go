//go:build (linux || darwin) && herdr_installed_inert_test && herdr_installed_hosted_test

package localcontrol

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func requireInstalledEndpointHosted(t *testing.T) {
	t.Helper()
	if os.Getenv("HERDR_INSTALLED_HOSTED_TEST") != "1" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Skip("requires explicitly authorized GitHub-hosted execution")
	}
	want := map[string]string{"linux": "Linux", "darwin": "macOS"}[runtime.GOOS]
	if want == "" || os.Getenv("RUNNER_OS") != want || os.Getenv("GITHUB_SHA") == "" || os.Getenv("GITHUB_RUN_ID") == "" || os.Getenv("GITHUB_RUN_ATTEMPT") == "" {
		t.Fatal("incomplete hosted execution identity")
	}
}

// Synthetic backend only; real native transport is NOT actual app admission.
func endpointHostedCore(t *testing.T, selected config.InstalledConfig) (InstalledControl, InstalledCredentials) {
	t.Helper()
	cfg := selected.Snapshot()
	core, c, e := NewInstalledResumeControl(cfg.InstanceID, InstalledSelection{Selection: "fixture", Node: "fixture", Profile: "fixture", Origin: cfg.TailscaleCLIOrigin, HTTPSPort: 443, RelayPort: uint16(cfg.Port)}, InstalledBackend{
		Status:  func(context.Context) (InstalledObservation, error) { return InstalledObservation{}, nil },
		Admit:   func(context.Context) error { return nil },
		Arm:     func(context.Context) (string, error) { return "synthetic-invitation", nil },
		Reprint: func(context.Context) (string, error) { return "synthetic-invitation", nil },
	})
	if e != nil {
		t.Fatal("core")
	}
	return core, c
}
func endpointHostedStart(t *testing.T, selected config.InstalledConfig) (InstalledCredentials, func() error) {
	t.Helper()
	core, c := endpointHostedCore(t, selected)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result error
	go func() { defer close(done); result = ServeInstalledEndpoint(ctx, selected, core, c) }()
	finish := func() error { cancel(); <-done; return result }
	t.Cleanup(func() { _ = finish() })
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if s, e := ReadInstalledHandoff(ctx, selected); e == nil && s.credentials == c {
			return c, finish
		}
		select {
		case <-done:
			t.Fatal("endpoint startup refused")
		case <-deadline.C:
			t.Fatal("discovery not published")
		case <-time.After(time.Millisecond):
		}
	}
}
func TestInstalledEndpointHostedRestart(t *testing.T) {
	requireInstalledEndpointHosted(t)
	selected, _, _ := handoffFixture(t)
	c, finish := endpointHostedStart(t, selected)
	client, e := NewInstalledClient(context.Background(), selected, c.Run)
	if e != nil {
		t.Fatal("client discovery")
	}
	result, e := client.Do(context.Background(), InstalledIntent{Kind: InstalledArm, ExpectedRun: c.Run, OperationID: "stable"})
	if e != nil || result.Reply.Invitation != "synthetic-invitation" {
		t.Fatal("authenticated arm")
	}
	if e = finish(); e != nil {
		t.Fatal("joined cleanup")
	}
	next, finishNext := endpointHostedStart(t, selected)
	if next.Run == c.Run {
		t.Fatal("generation reused")
	}
	result, e = client.Do(context.Background(), InstalledIntent{Kind: InstalledArm, ExpectedRun: c.Run, OperationID: "stable"})
	if e == nil || result.Transmission != InstalledNotSent || result.Reply.Invitation != "" {
		t.Fatal("stale client transmitted")
	}
	fresh, e := NewInstalledClient(context.Background(), selected, next.Run)
	if e != nil {
		t.Fatal("new discovery")
	}
	result, e = fresh.Do(context.Background(), InstalledIntent{Kind: InstalledArm, ExpectedRun: c.Run, OperationID: "stable"})
	if e == nil || result.Transmission != InstalledNotSent {
		t.Fatal("old ID/generation accepted")
	}
	if finishNext() != nil {
		t.Fatal("restart cleanup")
	}
}
func TestInstalledEndpointHostedDisconnect(t *testing.T) {
	requireInstalledEndpointHosted(t)
	selected, _, _ := handoffFixture(t)
	c, finish := endpointHostedStart(t, selected)
	snapshot, e := ReadInstalledHandoff(context.Background(), selected)
	if e != nil {
		t.Fatal("discovery")
	}
	conn, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: snapshot.socket, Net: "unix"})
	if e != nil {
		t.Fatal("connect")
	}
	if installedPeer(conn) != nil {
		conn.Close()
		t.Fatal("peer")
	}
	conn.Close()
	client, e := NewInstalledClient(context.Background(), selected, c.Run)
	if e != nil {
		t.Fatal("client")
	}
	result, e := client.Do(context.Background(), InstalledIntent{Kind: InstalledGetStatus})
	if e != nil || result.Reply.Code != "ok" {
		t.Fatal("disconnect retired endpoint")
	}
	if finish() != nil {
		t.Fatal("join")
	}
}
func TestInstalledEndpointHostedReplacement(t *testing.T) {
	requireInstalledEndpointHosted(t)
	selected, _, _ := handoffFixture(t)
	_, finish := endpointHostedStart(t, selected)
	snapshot, e := ReadInstalledHandoff(context.Background(), selected)
	if e != nil {
		t.Fatal("discovery")
	}
	displaced := filepath.Join(filepath.Dir(snapshot.socket), "displaced")
	if os.Rename(snapshot.socket, displaced) != nil {
		t.Fatal("fixture replacement")
	}
	if os.WriteFile(snapshot.socket, []byte("foreign"), 0600) != nil {
		t.Fatal("fixture canary")
	}
	if finish() == nil {
		t.Fatal("replacement cleanup uncertainty lost")
	}
	data, e := os.ReadFile(snapshot.socket)
	if e != nil || string(data) != "foreign" {
		t.Fatal("foreign leaf removed")
	}
	if _, e = os.Lstat(displaced); e != nil {
		t.Fatal("displaced socket removed")
	}
}

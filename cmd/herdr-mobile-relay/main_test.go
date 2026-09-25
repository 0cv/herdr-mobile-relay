package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestVerifyReleaseIdentity(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = "1.2.3", "candidate-revision"
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	manifest := release.Manifest{
		Version:  "1.2.3",
		Revision: "candidate-revision",
		Target:   release.CurrentTarget(),
	}
	if err := verifyReleaseIdentity(manifest, "1.2.3", "candidate-revision", release.CurrentTarget(), false); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}

	tests := []struct {
		name             string
		manifest         release.Manifest
		expectedVersion  string
		expectedRevision string
		expectedTarget   string
		allowCrossTarget bool
		errorPart        string
	}{
		{
			name:             "workflow version",
			manifest:         manifest,
			expectedVersion:  "1.2.4",
			expectedRevision: "candidate-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected version",
		},
		{
			name:             "workflow revision",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "other-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected revision",
		},
		{
			name:             "workflow target",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "candidate-revision",
			expectedTarget:   "other/target",
			errorPart:        "expected target",
		},
		{
			name: "binary version",
			manifest: release.Manifest{
				Version:  "1.2.4",
				Revision: "candidate-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary version",
		},
		{
			name: "binary revision",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "other-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary revision",
		},
		{
			name: "binary target",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "candidate-revision",
				Target:   "other/target",
			},
			expectedTarget: "other/target",
			errorPart:      "binary target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyReleaseIdentity(
				test.manifest,
				test.expectedVersion,
				test.expectedRevision,
				test.expectedTarget,
				test.allowCrossTarget,
			)
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("identity error = %v, want %q", err, test.errorPart)
			}
		})
	}

	crossTarget := manifest
	crossTarget.Target = "other/target"
	if err := verifyReleaseIdentity(crossTarget, "", "", "other/target", true); err != nil {
		t.Fatalf("cross-target build-host verification rejected: %v", err)
	}
}

func TestCheckPortSupportsUDP(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.LocalAddr().(*net.UDPAddr).Port)
	code, err := run([]string{"check-port", "--host", "127.0.0.1", "--port", port, "--protocol", "udp"})
	if code == 0 || err == nil {
		t.Fatalf("occupied UDP port accepted: code=%d err=%v", code, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	code, err = run([]string{"check-port", "--host", "127.0.0.1", "--port", port, "--protocol", "udp"})
	if code != 0 || err != nil {
		t.Fatalf("free UDP port rejected: code=%d err=%v", code, err)
	}
}

func TestVerifyReleaseRejectsCrossTargetCandidateMode(t *testing.T) {
	for _, candidateFlag := range []string{"--version", "--revision"} {
		t.Run(candidateFlag, func(t *testing.T) {
			code, err := run([]string{"verify-release", "--allow-cross-target", candidateFlag, "candidate"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("run() = (%d, %v), want usage error", code, err)
			}
		})
	}
}

func TestPairingControlKeepsDecodedNegativeRepliesDistinctFromTransportFailure(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "relay-control-cli.")
	if err != nil {
		t.Fatal("create private pairing-control fixture")
	}
	defer os.RemoveAll(root)

	const runID = "run-negative-cli"
	const instance = "instance-negative-cli"
	const fixtureSecret = "fixture-private-authorization-proof"
	socket := filepath.Join(root, "control.sock")
	server, err := localcontrol.NewManaged(socket, runID, instance, localcontrol.Callbacks{
		Status: func(context.Context) localcontrol.Status {
			return localcontrol.Status{OwnerHeld: true, RunID: runID, Instance: instance}
		},
		Activate: func(context.Context) (localcontrol.Status, error) {
			return localcontrol.Status{RegistrationOutcome: "settled-no-write"}, errors.New(fixtureSecret)
		},
		Arm: func(context.Context) (localcontrol.Status, error) {
			return localcontrol.Status{ArmOutcome: "not-committed"}, errors.New(fixtureSecret)
		},
	})
	if err != nil {
		t.Fatal("create pairing-control fixture")
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := server.Close(); err != nil {
			t.Error("close pairing-control fixture")
		}
		<-serverDone
	})

	for _, test := range []struct {
		name      string
		operation string
		outcome   string
	}{
		{name: "activation", operation: "activate", outcome: "settled-no-write"},
		{name: "arm", operation: "arm_bootstrap", outcome: "not-committed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"pairing-control", "--socket", socket, "--operation", test.operation, "--run-id", runID, "--instance", instance}
			code, runErr, output := captureRunStdout(t, args)
			if code != 0 || runErr != nil {
				t.Fatal("decoded negative pairing-control result was treated as a transport failure")
			}
			var response localcontrol.Response
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatal("pairing-control did not emit a JSON reply")
			}
			if response.OK || response.Error == "" || response.RunID != runID || response.Instance != instance || strings.Contains(response.Error, fixtureSecret) {
				t.Fatal("decoded negative pairing-control response lost its identity or exposed private error detail")
			}
			if test.name == "activation" && response.RegistrationOutcome != test.outcome {
				t.Fatal("activation negative outcome was not preserved")
			}
			if test.name == "arm" && response.ArmOutcome != test.outcome {
				t.Fatal("arm negative outcome was not preserved")
			}
			if strings.Contains(string(output), fixtureSecret) {
				t.Fatal("private authorization proof leaked into decoded response")
			}
		})
	}

	missingSocket := filepath.Join(root, "missing.sock")
	code, runErr, output := captureRunStdout(t, []string{"pairing-control", "--socket", missingSocket, "--operation", "activate", "--run-id", runID, "--instance", instance})
	if code == 0 || runErr == nil || len(output) != 0 {
		t.Fatal("transport failure was not kept distinct from a decoded negative reply")
	}
}

func captureRunStdout(t *testing.T, args []string) (int, error, []byte) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal("create pairing-control output capture")
	}
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous }()
	code, runErr := run(args)
	if err := writer.Close(); err != nil {
		t.Fatal("close pairing-control output capture")
	}
	os.Stdout = previous
	output, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal("close pairing-control output reader")
	}
	if err != nil {
		t.Fatal("read pairing-control output")
	}
	return code, runErr, output
}

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestDevelopmentWorkflowRejectsPortEnvironmentOverridesBeforePreflight(t *testing.T) {
	for _, name := range []string{
		"HERDR_DEV_TAILSCALE_CLI_PORT",
		"HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT",
		"HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "18377")
			workflow, cfg, err := developmentCLIFromEnvironment(context.Background(), "setup")
			if err == nil || !strings.Contains(err.Error(), name) || workflow != nil || cfg != nil {
				t.Fatalf("port override reached workflow construction: workflow=%v config=%v err=%v", workflow, cfg, err)
			}
		})
	}
}

func TestActivationScopeKeepsProductionDisabledAndDevelopmentExplicit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runTailscaleCLIWithInput([]string{"activation-check"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "production activation remains disabled") || stdout.Len() != 0 {
		t.Fatalf("production activation-check = code %d err=%v stdout=%q", code, err, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code, err = runTailscaleCLIWithInput([]string{"activation-check", "--scope", "development"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || err != nil || !strings.Contains(stdout.String(), "profile-checked isolated dev-tailscale-cli workflow") ||
		!strings.Contains(stdout.String(), "runtime qualification remains pending") {
		t.Fatalf("development activation-check conflated qualification states: code %d err=%v stdout=%q", code, err, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code, err = runTailscaleCLIWithInput([]string{"status", "--scope", "production", "--binary", "/not-a-real-cli"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
		t.Fatalf("standalone status was not refused before CLI selection: code %d err=%v", code, err)
	}
}

func TestStandaloneManagerOperationsRequireWorkflowBeforeCLIUse(t *testing.T) {
	base := t.TempDir()
	sentinel := filepath.Join(base, "cli-invoked")
	binary := filepath.Join(base, "fake-tailscale")
	contents := "#!/bin/sh\nprintf invoked > \"" + sentinel + "\"\nexit 97\n"
	if err := os.WriteFile(binary, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"preflight", "status", "recover", "assert-ready", "publish", "unpublish", "reserve-backend-port", "release-backend-port", "reconcile"} {
		t.Run(operation, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := runTailscaleCLIWithInput([]string{operation, "--binary", binary},
				strings.NewReader(""), &stdout, &stderr)
			if code != 2 || !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
				t.Fatalf("standalone %s = code %d err=%v, want workflow refusal", operation, code, err)
			}
		})
	}
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("standalone manager operations invoked the CLI: %v", err)
	}
}

func TestRouteConfirmationReaderBindsExactInputAndRejectsTokens(t *testing.T) {
	expected := tailscalecli.PublishRouteConfirmation("node-example", "https://relay.example.test:8443", 8443, 18377)
	if got, err := readRouteConfirmation(strings.NewReader(expected+"\n"), expected); err != nil || got != expected {
		t.Fatalf("exact stdin route confirmation = %q, %v", got, err)
	}
	for _, input := range []string{"PUBLISH\n", expected, expected + " extra\n", strings.Repeat("x", 600) + "\n"} {
		if _, err := readRouteConfirmation(strings.NewReader(input), expected); err == nil {
			t.Fatalf("accepted non-exact route confirmation %q", input)
		}
	}
}

func TestTailscalePreflightExitClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "explicit transient CLI outage retries", err: tailscalecli.ErrTransientUnavailable, want: 75},
		{name: "wrapped explicit transient outage retries", err: errors.Join(errors.New("inspection failed"), tailscalecli.ErrTransientUnavailable), want: 75},
		{name: "logged out is permanent", err: tailscalecli.ErrLoggedOut, want: 78},
		{name: "permission denied is permanent", err: tailscalecli.ErrPermissionDenied, want: 78},
		{name: "profile selection is permanent", err: tailscalecli.ErrProfileUnavailable, want: 78},
		{name: "unsupported state is permanent", err: tailscalecli.ErrUnsupported, want: 78},
		{name: "invalid output is permanent", err: tailscalecli.ErrInvalidJSON, want: 78},
		{name: "oversized output is permanent", err: tailscalecli.ErrOutputTooLong, want: 78},
		{name: "route conflict is permanent", err: tailscalecli.ErrConflict, want: 78},
		{name: "nonzero command failure is unclassified and permanent for retry", err: tailscalecli.CommandFailureError{}, want: 78},
		{name: "uncertain mutation is not a retry status", err: tailscalecli.ErrUncertain, want: 78},
		{name: "caller cancellation is not a retry status", err: context.Canceled, want: 78},
		{name: "caller deadline is not a retry status", err: context.DeadlineExceeded, want: 78},
		{name: "unknown failure is non-retryable", err: errors.New("unclassified fixture error"), want: 78},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := tailscalePreflightExitCode(test.err); got != test.want {
				t.Fatalf("preflight exit code = %d, want %d", got, test.want)
			}
		})
	}
}

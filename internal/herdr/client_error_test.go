package herdr

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandResultParsesCLIErrorEnvelope(t *testing.T) {
	waitErr := exec.Command("sh", "-c", "exit 1").Run()
	if waitErr == nil {
		t.Fatal("expected command failure")
	}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	_, _ = stderr.Write([]byte(`{"id":"cli:pane:list","error":{"code":"server_not_running","message":"no herdr server is running"}}`))

	_, err := commandResult(&limitedBuffer{}, stderr, waitErr)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("commandResult() error = %v, want CLIError", err)
	}
	if cliErr.Code != "server_not_running" || cliErr.Message != "no herdr server is running" {
		t.Fatalf("CLIError = %#v", cliErr)
	}
	if !errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("commandResult() error = %v, want dispatched-unknown outcome", err)
	}
}

func TestCommandResultKeepsRawStderrWhenNotAnEnvelope(t *testing.T) {
	waitErr := exec.Command("sh", "-c", "exit 1").Run()
	if waitErr == nil {
		t.Fatal("expected command failure")
	}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	_, _ = stderr.Write([]byte("plain diagnostic"))

	_, err := commandResult(&limitedBuffer{}, stderr, waitErr)
	if err == nil || !strings.Contains(err.Error(), "command failed: plain diagnostic") {
		t.Fatalf("commandResult() error = %v, want raw diagnostic", err)
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		t.Fatalf("commandResult() error = %v, unexpectedly parsed CLIError", err)
	}
}

package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestStartKindAgentDoesNotRetryProtocolMismatch(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "attempts")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"printf '%s\\n' '{\"error\":{\"code\":\"protocol_mismatch\",\"message\":\"incompatible\"},\"id\":\"cli:agent:start\"}' >&2\n"+
		"exit 1\n")
	lifecycle := &Lifecycle{herdr: herdr.NewClient(bin, filepath.Join(dir, "missing.sock"))}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := lifecycle.startKindAgent(ctx, "codex", "project", "pane-1")
	if err == nil || !herdr.IsRefused(err) || herdr.IsTransientRefused(err) {
		t.Fatalf("error = %v, want permanent refusal without transient retry", err)
	}
	data, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if attempts := strings.Count(string(data), "agent start"); attempts != 1 {
		t.Fatalf("attempts = %d, want one protocol-refused dispatch", attempts)
	}
}

package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestQuestionDeadlineDuringInterKeyDelayIsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "keys.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n")
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "sock")),
		NewState(testLogger()),
		nil,
		testLogger(),
	)
	ctx, cancel := context.WithTimeout(context.Background(), questionKeyDelay/2)
	defer cancel()

	err := dispatcher.sendQuestionKeys(ctx, "pane-1", []string{"Down", "Enter"})
	if !errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("partial question input = %v, want dispatched_unknown", err)
	}
	if errors.Is(err, herdr.ErrNotStarted) {
		t.Fatalf("partial question input was also classified not_started: %v", err)
	}
	data, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if sends := strings.Count(string(data), "pane send-keys"); sends != 1 {
		t.Fatalf("send count = %d, want exactly the first key before expiry\n%s", sends, data)
	}
}

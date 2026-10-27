package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStartAgentInvalidSuccessIsDispatchedUnknown(t *testing.T) {
	for _, response := range []string{"not-json", `{}`, `{"result":null}`, `{"result":{"pane_id":42}}`, `{"result":{}}`} {
		t.Run(response, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "herdr")
			t.Setenv("HERDR_TEST_RESPONSE", response)
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$HERDR_TEST_RESPONSE\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			client := NewClient(bin, filepath.Join(dir, "sock"))
			t.Cleanup(func() { _ = client.Close() })
			_, err := client.StartAgent(context.Background(), "agent", "claude", "", 1000)
			if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
				t.Fatalf("error = %v, want uncertain dispatch", err)
			}
		})
	}
}

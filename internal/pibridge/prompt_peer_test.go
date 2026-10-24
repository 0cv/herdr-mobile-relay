package pibridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPiPromptRejectsWrongPeerBeforeWriting(t *testing.T) {
	dir, lease, count := promptEndpoint(t, nil, false)
	old := filepath.Join(dir, fmtPID(lease.PID)+".sock")
	lease.PID++
	if err := os.Rename(old, filepath.Join(dir, fmtPID(lease.PID)+".sock")); err != nil {
		t.Fatal(err)
	}
	_, attempted, err := promptExchange(context.Background(), dir, lease, lease.Incarnation+"_req-1", "hello")
	if err == nil || attempted || count.Load() != 0 {
		t.Fatalf("forged peer received prompt: %v %v %d", err, attempted, count.Load())
	}
}

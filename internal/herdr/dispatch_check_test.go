package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDispatchCheckRunsAfterCapacityWait(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClient(bin, filepath.Join(dir, "sock"))
	t.Cleanup(func() { _ = c.Close() })
	for range cap(c.sem) {
		c.sem <- struct{}{}
	}
	refused := errors.New("target changed")
	checked := make(chan struct{})
	ctx := WithDispatchCheck(context.Background(), func() error { close(checked); return refused })
	result := make(chan error, 1)
	go func() { result <- c.Prompt(ctx, "pane", "hello") }()
	select {
	case <-checked:
		t.Fatal("checked before capacity became available")
	default:
	}
	<-c.sem
	err := <-result
	if !errors.Is(err, ErrNotStarted) || !errors.Is(err, refused) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("subprocess ran: %v", err)
	}
}

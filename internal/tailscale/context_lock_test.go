package tailscale

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContextLockExpiredWaiterDoesNotAcquireLater(t *testing.T) {
	var lock contextLock
	release, err := lock.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lock.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired lifecycle lock acquisition = %v", err)
	}
	release()
	acquired, err := lock.Lock(context.Background())
	if err != nil {
		t.Fatalf("later lifecycle operation could not acquire lock: %v", err)
	}
	acquired()
}

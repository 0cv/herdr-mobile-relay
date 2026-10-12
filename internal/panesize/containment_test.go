package panesize

import (
	"context"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type forbiddenProvider struct{ calls int }

func (p *forbiddenProvider) PaneProcessInfo(context.Context, string) (*herdr.PaneProcessInfo, error) {
	p.calls++
	return nil, errors.New("must not resolve a production terminal")
}

func TestProductionBackendIsUnavailableWithoutAnyIO(t *testing.T) {
	provider := &forbiddenProvider{}
	m := NewManager(provider, nil)
	if m.Available() {
		t.Fatal("legacy production backend is eligible")
	}
	ctx := context.Background()
	for range 3 { // stale acquisition and renewals
		if _, _, err := m.Acquire(ctx, "client", "pane", 80, 24); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("acquire: %v", err)
		}
	}
	for _, err := range []error{m.Release(ctx, "client", "pane"), m.ReleaseClient(ctx, "client"), m.SweepExpired(ctx), m.Shutdown(ctx)} {
		if err != nil {
			t.Fatalf("empty cleanup: %v", err)
		}
	}
	if provider.calls != 0 || len(m.panes) != 0 {
		t.Fatalf("disabled backend resolved %d targets and retained %d panes", provider.calls, len(m.panes))
	}
}

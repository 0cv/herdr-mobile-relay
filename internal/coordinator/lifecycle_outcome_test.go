package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestCustomAgentTimeoutAfterPaneRunIsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2\" = \"agent get\" ]; then\n"+
		"  printf '{\"result\":{\"pane_id\":\"pane-1\",\"running\":false}}\\n'\n"+
		"else\n"+
		"  printf '{\"result\":{}}\\n'\n"+
		"fi\n")
	lifecycle := &Lifecycle{herdr: herdr.NewClient(bin, filepath.Join(dir, "sock"))}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := lifecycle.startInTarget(ctx, profiles.Profile{Argv: []string{"custom-agent"}}, "agent", "pane-1")
	if !errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("custom agent timeout = %v, want dispatched_unknown", err)
	}
	if errors.Is(err, herdr.ErrNotStarted) {
		t.Fatalf("custom agent timeout was also classified not_started: %v", err)
	}
}

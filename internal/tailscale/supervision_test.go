package tailscale

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSupervisionFixture installs an executable shell fixture in a per-test
// temporary directory. The fixtures below intentionally background a descendant
// that inherits stdout/stderr so the output pipes stay open after the direct
// child exits.
func writeSupervisionFixture(t *testing.T, script string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

// TestRunDoesNotHangOnDescendantHeldPipe proves that a descendant inheriting
// stdout/stderr cannot extend the inspection beyond the pipe bound. The direct
// child prints a valid JSON line, backgrounds a descendant and exits; without
// command.WaitDelay the descendant would keep the pipe open until it exits.
func TestRunDoesNotHangOnDescendantHeldPipe(t *testing.T) {
	binary := writeSupervisionFixture(t, `#!/bin/sh
printf '%s\n' '{"BackendState":"Running"}'
sleep 5 &
exit 0
`)
	start := time.Now()
	output, err := run(context.Background(), binary, "status", "--json")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if !strings.Contains(string(output), `{"BackendState":"Running"}`) {
		t.Fatalf("run output = %q; want the JSON line printed before the descendant was backgrounded", output)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("run blocked for %v on the descendant-held pipe; want < 3s", elapsed)
	}
}

// TestRunTimeoutStillBounded proves that the context deadline is honored even
// when the direct child's descendant keeps the output pipes open indefinitely.
// The fixture backgrounds a descendant (which outlives the deadline) and then
// replaces itself with a never-exiting sleep, so run must time out and must not
// wait for the descendant. The bound is the context deadline plus the pipe
// bound (CommandTimeout + waitDelay), with a one-second scheduling allowance.
func TestRunTimeoutStillBounded(t *testing.T) {
	binary := writeSupervisionFixture(t, `#!/bin/sh
sleep 10 &
exec sleep 100
`)
	start := time.Now()
	_, err := run(context.Background(), binary, "status", "--json")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("run returned no error for the fixture that never exits")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("run error = %v; want a timeout", err)
	}
	bound := CommandTimeout + 3*time.Second
	if elapsed >= bound {
		t.Fatalf("run blocked for %v after the deadline; want < %v", elapsed, bound)
	}
}

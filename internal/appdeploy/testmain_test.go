package appdeploy

import (
	"context"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// On macOS the real sweep would remove launchd jobs on the machine running the tests.
	sweepWorkers = func(context.Context) error { return nil }
	os.Exit(m.Run())
}

package main

import (
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestTailscalePreflightExitClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "transient CLI outage retries", err: tailscalecli.ErrTransientUnavailable, want: 75},
		{name: "wrapped transient CLI outage retries", err: errors.Join(errors.New("inspection failed"), tailscalecli.ErrTransientUnavailable), want: 75},
		{name: "logged out is permanent", err: tailscalecli.ErrLoggedOut, want: 78},
		{name: "profile selection is permanent", err: tailscalecli.ErrProfileUnavailable, want: 78},
		{name: "unsupported state is permanent", err: tailscalecli.ErrUnsupported, want: 78},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := tailscalePreflightExitCode(test.err); got != test.want {
				t.Fatalf("preflight exit code = %d, want %d", got, test.want)
			}
		})
	}
}

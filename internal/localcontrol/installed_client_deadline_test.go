//go:build linux || darwin

package localcontrol

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInstalledClientDeadlineClasses(t *testing.T) {
	_, r, _ := installedFixture(t, nil)
	credentials := InstalledCredentials{r.Run, r.Installation, r.Capability}
	for _, tc := range []struct {
		name   string
		kind   InstalledIntentKind
		budget time.Duration
		reply  string
	}{
		{"status", InstalledGetStatus, StatusTimeout, `{"code":"ok","publication":"disabled","status":{"registered":false,"invitation_available":false,"quarantined":false}}`},
		{"publish", InstalledPublish, ActivateTimeout, `{"code":"ok","publication":"acknowledged"}`},
		{"reprint", InstalledReprint, ArmTimeout, `{"code":"ok","publication":"disabled","invitation":"synthetic"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A scripted peer supplies a reply; no actual backend acknowledgement.
			c := &installedScriptConn{reads: strings.NewReader(tc.reply + "\n"), successWrite: true}
			before := time.Now()
			result, e := installedClientExchange(context.Background(), c, credentials, InstalledIntent{Kind: tc.kind}, []byte(`{}`))
			if e != nil || result.Transmission != InstalledValidatedReply || len(c.readDeadlines) != 2 || len(c.writeDeadlines) != 1 {
				t.Fatal("scripted exchange")
			}
			first := c.readDeadlines[0].Sub(before)
			frame := c.readDeadlines[1].Sub(before)
			if first < tc.budget+installedFrameTimeout || first > tc.budget+installedFrameTimeout+time.Second || frame < installedFrameTimeout || frame > installedFrameTimeout+time.Second {
				t.Fatal("operation and frame deadlines conflated")
			}
		})
	}
}
func TestInstalledEndpointInputDeadline(t *testing.T) {
	core, _, calls := installedFixture(t, nil)
	c := &installedScriptConn{reads: strings.NewReader("")}
	before := time.Now()
	installedServeConnection(context.Background(), c, core)
	if len(c.readDeadlines) != 1 || *calls != [5]int{} {
		t.Fatal("input failure dispatched")
	}
	delta := c.readDeadlines[0].Sub(before)
	if delta < installedFrameTimeout || delta > installedFrameTimeout+time.Second {
		t.Fatal("input frame deadline")
	}
}

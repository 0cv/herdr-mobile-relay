//go:build herdr_installed_inert_test

package tailscalecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const installedFixtureLong = "1.102.4-t3caf7d9e7d-g084ee3b64537"
const installedFixtureStatus = `{"Version":"` + installedFixtureLong + `","BackendState":"Running","Self":{"ID":"node-fixture","UserID":123,"DNSName":"herdr.tailnet.ts.net."},"CurrentTailnet":{"Name":"fixture-account","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":true},"CertDomains":["herdr.tailnet.ts.net"],"User":{"123":{"ID":123,"LoginName":"private@example.invalid","DisplayName":"Fixture","ProfilePicURL":""}}}`
const installedFixtureVersion = `{"majorMinorPatch":"1.102.4","short":"1.102.4","long":"` + installedFixtureLong + `","gitCommit":"3caf7d9e7dcaba589cfc58beda596929733e4fea","daemonLong":"` + installedFixtureLong + `","extraGitCommit":"084ee3b64537a1276e56fc38cdf0a711da9f4936","osVariant":"appstore","cap":142}`
const installedFixtureRoute = `{"TCP":{"8443":{"HTTPS":true}},"Web":{"herdr.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:18377"}}}}}`

// NewInstalledInertFixture exists only in the explicit inert test build. It
// seeds an acknowledged journal, never invokes a CLI or fabricates runtime
// qualification, and rejects every command other than the three observations.
func NewInstalledInertFixture(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, coordination := filepath.Join(base, "registration"), filepath.Join(base, "coordination")
	for _, p := range []string{state, coordination} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := "/inert-fixture/tailscale"
	client := newTestClientForPlatform(binary, func(ctx context.Context, _ string, args ...string) (commandResult, error) {
		if err := ctx.Err(); err != nil {
			return commandResult{}, err
		}
		var output string
		switch strings.Join(args, " ") {
		case "status --json":
			output = installedFixtureStatus
		case "version --json --daemon":
			output = installedFixtureVersion
		case "serve status --json":
			output = installedFixtureRoute
		default:
			t.Errorf("unexpected CLI mutation: %v", args)
			return commandResult{}, ErrUnsupported
		}
		return commandResult{stdout: []byte(output), dispatched: true}, nil
	}, "darwin", "arm64")
	m := &Manager{stateRoot: state, coordinationRoot: coordination, client: client, fixtureMutations: true}
	record := registration{
		Schema: 1, InstallationID: "install-fixture", Scope: "production", NodeID: "node-fixture",
		DNSName: "herdr.tailnet.ts.net", Profile: ProfileAppStoreSupplied, BinaryPath: binary,
		HTTPSPort: 8443, BackendPort: 18377, Path: "/", Backend: "http://127.0.0.1:18377",
		ConsentScope: publishConsentScope, OperationID: "00000000000000000000000000000001",
		State: StateRegistered, MutationAcknowledged: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := m.writeRegistration(record); err != nil {
		t.Fatal(err)
	}
	if err := m.writeBackendReservation(reservationForRegistration(record, StateRegistered)); err != nil {
		t.Fatal(err)
	}
	return m, state, coordination, binary
}

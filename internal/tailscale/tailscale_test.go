package tailscale

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStatusUsesAuthenticatedSelfDNSName(t *testing.T) {
	status, err := ParseStatus([]byte(sourceStatus))
	if err != nil {
		t.Fatal(err)
	}
	if !status.LoggedIn || status.NodeID != "node-1" || status.DNSName != "relay.tailnet.ts.net" {
		t.Fatalf("status = %#v", status)
	}
	origin, err := Origin(status.DNSName, 443)
	if err != nil || origin != "https://relay.tailnet.ts.net" {
		t.Fatalf("origin = %q, err = %v", origin, err)
	}
}

func TestParseStatusDistinguishesLoginRequired(t *testing.T) {
	status, err := ParseStatus([]byte(`{"BackendState":"NeedsLogin","Self":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.LoggedIn {
		t.Fatalf("status = %#v, want logged out", status)
	}
}

func TestParseServeStatusRejectsRoutesAndFunnel(t *testing.T) {
	serve, err := ParseServeStatus([]byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"relay.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}}},"AllowFunnel":{"relay.ts.net:443":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !serve.Configured || !serve.FunnelConfigured || len(serve.Routes) != 1 {
		t.Fatalf("serve = %#v", serve)
	}
	empty, err := ParseServeStatus([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Configured || empty.FunnelConfigured {
		t.Fatalf("empty serve = %#v", empty)
	}
	withoutFunnel, err := ParseServeStatus([]byte(`{"Web":{},"AllowFunnel":{"443":false}}`))
	if err == nil || withoutFunnel.Complete {
		t.Fatalf("disabled funnel = %#v, err = %v", withoutFunnel, err)
	}
	emptyObjectFunnel, err := ParseServeStatus([]byte(`{"Web":{},"AllowFunnel":{"443":{}}}`))
	if err == nil || emptyObjectFunnel.Complete {
		t.Fatalf("empty-object funnel = %#v, err = %v", emptyObjectFunnel, err)
	}
}

func TestInspectUsesBoundedReadOnlyCommands(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "tailscale")
	script := `#!/bin/sh
case "$1 $2" in
  "status --json") printf '%s' '` + sourceStatus + `' ;;
  "version --json") printf '%s' '` + sourceVersion + `' ;;
  "serve status") printf '%s' '` + sourceForeground + `' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	inspection, err := Inspect(context.Background(), binary, 443)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.LoggedIn || inspection.Origin != "https://relay.tailnet.ts.net" ||
		!inspection.ServeConfigured || inspection.FunnelConfigured || inspection.ServeRouteCount != 1 ||
		inspection.ServeRouteOwned || !inspection.ServeInspected || !inspection.ExposureComplete {
		t.Fatalf("inspection = %#v", inspection)
	}
}

func TestParsersRejectMalformedOrOversizedOutput(t *testing.T) {
	if _, err := ParseStatus([]byte(`{"Self":{}}`)); err == nil {
		t.Fatal("status without backend state accepted")
	}
	if _, err := ParseServeStatus([]byte("not-json")); err == nil {
		t.Fatal("malformed serve status accepted")
	}
	if _, err := ParseServeStatus([]byte(`{"unexpected":true}`)); err == nil {
		t.Fatal("unknown serve status accepted")
	}
	if _, err := Origin("relay.example.com/path", 443); err == nil {
		t.Fatal("invalid DNS name accepted")
	}
	if _, err := ParseStatus([]byte(strings.Repeat("x", MaxOutputBytes+1))); err == nil {
		t.Fatal("oversized status accepted")
	}
}

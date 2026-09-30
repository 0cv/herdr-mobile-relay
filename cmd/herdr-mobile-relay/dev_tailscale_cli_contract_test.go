package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDevelopmentRunbookAndCleanupPromptMatchOwnerScope(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source tree for hosted documentation regression checks")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	read := func(path string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(contents)
	}

	qualification := read("docs/tailscale-cli-qualification.md")
	start := strings.Index(qualification, "## Development risk acceptance and live runbook")
	if start < 0 {
		t.Fatal("development live runbook section is missing")
	}
	section := qualification[start:]
	if next := strings.Index(section[len("## Development risk acceptance and live runbook"):], "\n## "); next >= 0 {
		section = section[:len("## Development risk acceptance and live runbook")+next]
	}
	for _, want := range []string{
		"four limits only for this isolated development route",
		"CLI check-to-write race is not atomic with respect to external Serve writers.",
		"persistent route and local backend-port reuse can survive relay stop",
		"There is no automatic global rollback",
		"There is no guarantee that remote connections drain after route removal.",
		"worker may perform source, static, build and compile-only work and hosted fixtures",
		"must never perform live Tailscale access",
		"After exact-revision hosted checks and independent approval",
		"already owner-authorized development-only sequence on this Mac's current\nnode/account",
		"not a request for another owner grant",
		"exact App Store Tailscale 1.102.4 candidate",
		"HTTPS 8443 -> `127.0.0.1:18377`",
		"plugin listener 18378",
		"HERDR_SOCKET_PATH",
		"complete private Serve baseline",
		"installed-service status",
		"trusted system TLS/hostname verification",
		"exact bundle and readiness",
		"Leave the development route available",
		"controller and reader\n   role behavior as separate cells",
		"Keep reader access read-only and controller-only permissions distinct.",
		"Disconnect/reconnect once",
		"credential resumes without re-pairing.",
		"fresh exact-route runtime confirmation",
		"Never retry an\n   ambiguous write",
		"mark every untested qualification cell **pending**",
		"No live qualification, production enablement, deployment or release is\nestablished by these edits.",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("qualification runbook is missing required scope text %q", want)
		}
	}
	for _, stale := range []string{
		"requires\nits own explicit owner authorization",
		"That authorization does not include unpublishing",
		"Do not unpublish under this authorization",
		"Route removal needs separate\nexplicit authorization",
		"Do not install or\n   start a service, enroll a phone, or print/share a setup link",
		"Physical-phone qualification remains a separate future phase",
	} {
		if strings.Contains(section, stale) {
			t.Errorf("qualification runbook contains stale prohibition %q", stale)
		}
	}

	contract := read("docs/tailscale-cli-contract.md")
	if strings.Contains(contract, "separate cleanup authorization") ||
		!strings.Contains(contract, "already authorized") ||
		!strings.Contains(contract, "fresh exact-route\nruntime confirmation") ||
		!strings.Contains(contract, "not blanket consent") {
		t.Error("generic contract cleanup wording is inconsistent with the scoped owner authorization and runtime confirmation")
	}

	prompt := read("cmd/herdr-mobile-relay/dev_tailscale_cli.go")
	for _, want := range []string{
		"After the authorized owner phone test, this exact-route cleanup still requires the displayed runtime confirmation.",
		"The check-to-write interval is not atomic and remote connections may not drain.",
		"This removes only the exact journaled route; it does not reset unrelated Serve state.",
		"readRouteConfirmation(stdin, confirmation)",
		"RouteRemovalAccepted: true",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("cleanup prompt or exact-consent guard is missing %q", want)
		}
	}
	if strings.Contains(prompt, "Separate authorization is required") {
		t.Error("cleanup prompt incorrectly demands a further owner grant")
	}
}

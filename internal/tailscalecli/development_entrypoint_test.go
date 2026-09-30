package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDevelopmentWorkflowPublicEntrypointInventory(t *testing.T) {
	entries := []struct {
		name       string
		nativeOnly bool
		run        func(*testing.T)
	}{
		{
			name: "positive synthetic fixture workflow",
			run: func(t *testing.T) {
				fixture := newFakeCLI(t)
				workflow, _, _, _ := developmentWorkflowFixture(t, fixture, "darwin", "arm64")
				if workflow == nil || workflow.Preflight().Profile != ProfileAppStoreSupplied ||
					!workflow.Preflight().DevelopmentQualificationEnabled || workflow.Preflight().RuntimeQualified {
					t.Fatalf("synthetic workflow did not preserve the intended profile/qualification split: %+v", workflow)
				}
				if got := strings.Join(flattenCalls(fixture.calls), "|"); got != "status --json|version --json --daemon|serve status --json" {
					t.Fatalf("synthetic workflow performed unexpected CLI operations: %q", got)
				}
			},
		},
		{
			name:       "crafted root rejected before CLI contact",
			nativeOnly: true,
			run: func(t *testing.T) {
				layout, _, _, _ := developmentIsolationFixture(t)
				binary, sentinel := installEntrypointSentinelCLI(t, layout)
				craftedRoot := filepath.Join(filepath.Dir(layout.root), "crafted-development-root")
				if _, _, err := NewDevelopmentWorkflow(context.Background(), craftedRoot, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("workflow accepted a caller-crafted root that differs from its validated process environment")
				}
				if _, err := PreflightDevelopmentWorkflow(context.Background(), craftedRoot, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("read-only preflight accepted a caller-crafted root")
				}
				assertCLIWasNotContacted(t, sentinel)
			},
		},
		{
			name:       "wrong backend port rejected before CLI contact",
			nativeOnly: true,
			run: func(t *testing.T) {
				layout, _, _, _ := developmentIsolationFixture(t)
				binary, sentinel := installEntrypointSentinelCLI(t, layout)
				t.Setenv("HERDR_RELAY_PORT", "18379")
				if _, _, err := NewDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("workflow accepted a backend port outside the fixed tuple")
				}
				if _, err := PreflightDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("read-only preflight accepted a backend port outside the fixed tuple")
				}
				assertCLIWasNotContacted(t, sentinel)
			},
		},
		{
			name:       "wrong plugin port rejected before CLI contact",
			nativeOnly: true,
			run: func(t *testing.T) {
				layout, _, _, _ := developmentIsolationFixture(t)
				binary, sentinel := installEntrypointSentinelCLI(t, layout)
				t.Setenv("HERDR_RELAY_PLUGIN_PORT", "18379")
				if _, _, err := NewDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("workflow accepted a plugin port outside the fixed tuple")
				}
				if _, err := PreflightDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("read-only preflight accepted a plugin port outside the fixed tuple")
				}
				assertCLIWasNotContacted(t, sentinel)
			},
		},
		{
			name:       "wrong HTTPS port rejected before CLI contact",
			nativeOnly: true,
			run: func(t *testing.T) {
				layout, _, _, _ := developmentIsolationFixture(t)
				binary, sentinel := installEntrypointSentinelCLI(t, layout)
				t.Setenv("HERDR_TAILSCALE_CLI_HTTPS_PORT", "9443")
				if _, _, err := NewDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("workflow accepted an HTTPS port outside the fixed tuple")
				}
				if _, err := PreflightDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); err == nil {
					t.Fatal("read-only preflight accepted an HTTPS port outside the fixed tuple")
				}
				assertCLIWasNotContacted(t, sentinel)
			},
		},
		{
			name:       "production-overlapping root rejected before CLI contact",
			nativeOnly: true,
			run: func(t *testing.T) {
				layout, home, _, _ := developmentIsolationFixture(t)
				binary, sentinel := installEntrypointSentinelCLI(t, layout)
				productionEnv := filepath.Join(home, "installed", "relay.env")
				writeDevelopmentFixtureFile(t, productionEnv,
					"XDG_CONFIG_HOME='"+layout.configHome+"'\n", 0o600)
				t.Setenv("HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE", productionEnv)
				if _, _, err := NewDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); !errors.Is(err, ErrPermissionDenied) {
					t.Fatalf("workflow production overlap = %v, want permission refusal", err)
				}
				if _, err := PreflightDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); !errors.Is(err, ErrPermissionDenied) {
					t.Fatalf("read-only preflight production overlap = %v, want permission refusal", err)
				}
				assertCLIWasNotContacted(t, sentinel)
			},
		},
		{
			name:       "missing development opt-in rejected before CLI contact",
			nativeOnly: true,
			run: func(t *testing.T) {
				layout, _, _, _ := developmentIsolationFixture(t)
				binary, sentinel := installEntrypointSentinelCLI(t, layout)
				t.Setenv("HERDR_DEV_TAILSCALE_CLI_ENABLE", "")
				if _, _, err := NewDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); !errors.Is(err, ErrWorkflowRequired) {
					t.Fatalf("workflow without explicit opt-in = %v, want workflow-required refusal", err)
				}
				if _, err := PreflightDevelopmentWorkflow(context.Background(), layout.root, layout.state, layout.coordination, binary); !errors.Is(err, ErrWorkflowRequired) {
					t.Fatalf("read-only preflight without explicit opt-in = %v, want workflow-required refusal", err)
				}
				assertCLIWasNotContacted(t, sentinel)
			},
		},
	}

	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			if entry.nativeOnly && (runtime.GOOS != "darwin" || runtime.GOARCH != "arm64") {
				t.Skip("real CLI entrypoint isolation requires native Darwin/arm64")
			}
			entry.run(t)
		})
	}
}

func installEntrypointSentinelCLI(t *testing.T, layout *developmentIsolation) (string, string) {
	t.Helper()
	binary := os.Getenv("HERDR_TAILSCALE_CLI_BIN")
	sentinel := filepath.Join(filepath.Dir(binary), "unexpected-cli-contact")
	contents := "#!/bin/sh\n# " + syntheticFixtureCLIMarker + "\nprintf contacted > \"$(dirname \"$0\")/unexpected-cli-contact\"\n"
	if err := os.WriteFile(binary, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(layout.relayEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "HERDR_TAILSCALE_CLI_BIN='"+binary+"'") {
		t.Fatalf("fixture relay environment does not bind selected CLI path %q", binary)
	}
	t.Setenv("HERDR_TAILSCALE_CLI_BIN", binary)
	return binary, sentinel
}

func assertCLIWasNotContacted(t *testing.T, sentinel string) {
	t.Helper()
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe workflow contacted the Tailscale CLI before refusal: %v", err)
	}
}

func flattenCalls(calls [][]string) []string {
	result := make([]string, 0, len(calls))
	for _, call := range calls {
		result = append(result, strings.Join(call, " "))
	}
	return result
}

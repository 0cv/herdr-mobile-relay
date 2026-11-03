package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	herdrmobilerelay "github.com/0cv/herdr-mobile-relay"
	"github.com/0cv/herdr-mobile-relay/internal/hc"
	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/piintegration"
)

func TestRealCorePiDiscovery(t *testing.T) {
	if os.Getenv("HERDR_RELAY_HC_INTEGRATION") != "1" {
		t.Skip("set HERDR_RELAY_HC_INTEGRATION=1 to run an isolated core")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not on PATH")
	}
	var root string
	socket, env := hctest.StartCore(t, func(scratch string, env []string) []string {
		root = scratch
		dir := filepath.Join(root, "pi")
		manager := piintegration.New(herdrmobilerelay.PiCommandBridge(), func() []string { return []string{dir} })
		if state := manager.Set(true); !state.Enabled {
			t.Fatalf("install scratch bridge: %+v", state)
		}
		isolated := []string{}
		for _, value := range env {
			if !strings.HasPrefix(value, "PI_") && !strings.HasPrefix(value, "HOME=") {
				isolated = append(isolated, value)
			}
		}
		return append(isolated, "HOME="+root, "PI_CODING_AGENT_DIR="+dir, "PI_CODING_AGENT_SESSION_DIR="+filepath.Join(dir, "sessions"), "PI_OFFLINE=1", "PI_TELEMETRY=0")
	})
	instance, err := pibridge.Instance(socket)
	if err != nil {
		t.Fatal(err)
	}
	directory := pibridge.Directory(instance)
	t.Logf("owned bridge directory: %s", directory)
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Errorf("bridge directory remains: %v", err)
		}
	})
	client := hc.New(socket)
	for index, shape := range []string{"shell", "direct"} {
		t.Run(shape, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			session := fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1)
			args := []string{"pi", "--session-id", session, "--offline", "--no-mcp", "--no-approve", "--no-context-files"}
			argv := args
			if shape == "shell" {
				argv = []string{"/bin/sh"}
			}
			output, err := hctest.Run(env, append([]string{"run", "--bg", "--json", "--no-parent", "--cwd", root, "--"}, argv...)...)
			if err != nil {
				t.Fatalf("start terminal: %v: %s", err, output)
			}
			var terminal struct {
				Address string `json:"address"`
			}
			if err := json.Unmarshal(output, &terminal); err != nil || terminal.Address == "" {
				t.Fatalf("terminal.run: %s (%v)", output, err)
			}
			address := terminal.Address
			t.Cleanup(func() {
				for _, command := range []string{"kill", "rm"} {
					if output, err := hctest.Run(env, command, address); err != nil {
						t.Errorf("cleanup %s %s: %v: %s", command, address, err, output)
					}
				}
			})
			defer func() {
				output, err := hctest.Run(env, "get", address, "--json")
				t.Logf("terminal.get: %s (error: %v)", output, err)
				entries, err := os.ReadDir(directory)
				names := []string{}
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Logf("bridge directory %s: %v (error: %v)", directory, names, err)
			}()
			if shape == "shell" {
				if err := client.Prompt(ctx, address, strings.Join(args, " ")); err != nil {
					t.Fatal(err)
				}
			}
			var pid, size int
			var discoveryErr error
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				info, err := client.PaneProcessInfoFresh(ctx, address)
				discoveryErr = err
				if err == nil {
					pid, discoveryErr = foregroundRoot(info, address)
					if discoveryErr == nil {
						catalog, err := runtimePiCatalog(ctx, client, socket, address, session)
						size, discoveryErr = len(catalog.Commands), err
						if err == nil && size > 0 {
							break
						}
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("Pi pid=%d; session=%s (CLI --session-id); catalog size=%d; error=%v", pid, session, size, discoveryErr)
			if discoveryErr != nil || size == 0 {
				t.Errorf("Pi discovery failed: pid=%d catalog=%d error=%v", pid, size, discoveryErr)
			}
			inventory, err := client.GetInventory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			listed, agent := false, ""
			for _, pane := range inventory.Panes {
				if pane.ID == address {
					listed, agent = true, pane.Agent
				}
			}
			panes, err := client.PaneList(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, pane := range panes {
				if pane.ID == address {
					agent = pane.Agent
				}
			}
			t.Logf("inventory listed=%t; pane Agent=%q", listed, agent)
			if !listed || agent == "" {
				t.Error("Pi is not listed as an agent in inventory; the phone would not show Pi")
			}
		})
	}
}

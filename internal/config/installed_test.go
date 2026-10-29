package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installedConfigFixture(t *testing.T) *Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return &Config{Transport: TransportTailscaleCLI, Host: "127.0.0.1", Token: "01234567890123456789012345678901",
		Port: 8375, PluginPort: 8376, InstanceID: "installed", TailscaleCLIScope: "production", TailscaleCLIBin: "/inert/tailscale",
		TailscaleCLIOrigin: "https://relay.example.invalid", PhoneAppOrigin: "https://app.example.invalid",
		TailscaleCLIStateRoot: filepath.Join(root, "registration"), TailscaleCLICoordinationRoot: filepath.Join(root, "coordination"),
		ConfigHome: filepath.Join(root, "config"), RuntimeDir: filepath.Join(root, "config", "relay"), CacheDir: filepath.Join(root, "cache"), ReleaseRoot: filepath.Join(root, "release"),
		AllowedOrigins: []string{"https://app.example.invalid"}}
}

// All tests use temporary files only; no environment loaders or external effects.
func TestInstalledBindingSnapshot(t *testing.T) {
	cfg := installedConfigFixture(t)
	if err := os.Mkdir(cfg.ConfigHome, 0755); err != nil {
		t.Fatal(err)
	}
	selected, err := BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", selected, selected, selected), cfg.Token) {
		t.Fatal("report exposed token")
	}
	if err := selected.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg.RuntimeDir, cfg.CacheDir, cfg.TailscaleCLIStateRoot} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("binding created leaf: %v", err)
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.RuntimeDir += "-other" },
		func(c *Config) { c.CacheDir += "-other" },
		func(c *Config) { c.ConfigHome += "-other" },
		func(c *Config) { c.ReleaseRoot += "-other" },
		func(c *Config) { c.TailscaleCLIStateRoot += "-other" },
		func(c *Config) { c.TailscaleCLICoordinationRoot += "-other" },
		func(c *Config) { c.TailscaleCLIBin += "-other" },
		func(c *Config) { c.TailscaleCLIScope = "development" },
		func(c *Config) { c.Port++ },
		func(c *Config) { c.PluginPort++ },
		func(c *Config) { c.InstanceID += "-other" },
		func(c *Config) { c.PhoneAppOrigin = "https://other.example.invalid" },
	} {
		changed := selected.Snapshot()
		mutate(&changed)
		if selected.Validate(&changed) == nil {
			t.Fatal("post-binding config change accepted")
		}
	}
	cfg.AllowedOrigins[0] = "https://changed.example.invalid"
	if selected.Validate(cfg) == nil {
		t.Fatal("post-binding slice change accepted")
	}
	snapshot := selected.Snapshot()
	if err := selected.Validate(&snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Token = "different-token-of-32-bytes------"
	if selected.Validate(&snapshot) == nil {
		t.Fatal("post-binding token change accepted")
	}
	var zero InstalledConfig
	if zero.Validate(&Config{}) == nil {
		t.Fatal("zero binding accepted")
	}
}

func TestInstalledBindingPolicy(t *testing.T) {
	cases := map[string]func(*Config){
		"token": func(c *Config) { c.Token = "short" }, "host": func(c *Config) { c.Host = "0.0.0.0" },
		"scope": func(c *Config) { c.TailscaleCLIScope = "development" }, "ports": func(c *Config) { c.PluginPort = c.Port },
		"origin": func(c *Config) { c.TailscaleCLIOrigin += "/path" }, "app": func(c *Config) { c.PhoneAppOrigin = "http://app.example.invalid" },
		"gateway": func(c *Config) { c.GatewayURL = "wss://gateway.example.invalid" }, "managed": func(c *Config) { c.ManagedRunID = "managed" },
		"control": func(c *Config) { c.ControlRunID = "control" }, "development": func(c *Config) { c.TailscaleCLIDevelopmentRoot = c.ConfigHome },
		"rearm": func(c *Config) { c.RearmBootstrap = true }, "pcp": func(c *Config) { c.PortMappingEnabled = true },
		"registration-coordination": func(c *Config) { c.TailscaleCLICoordinationRoot = c.TailscaleCLIStateRoot },
		"registration-runtime":      func(c *Config) { c.RuntimeDir = filepath.Join(c.TailscaleCLIStateRoot, "runtime") },
		"registration-config":       func(c *Config) { c.ConfigHome = c.TailscaleCLIStateRoot },
		"coordination-cache":        func(c *Config) { c.CacheDir = c.TailscaleCLICoordinationRoot },
		"coordination-release":      func(c *Config) { c.ReleaseRoot = c.TailscaleCLICoordinationRoot },
		"runtime-cache":             func(c *Config) { c.CacheDir = c.RuntimeDir }, "runtime-release": func(c *Config) { c.ReleaseRoot = c.RuntimeDir },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := installedConfigFixture(t)
			mutate(c)
			if _, err := BindInstalledCLI(c); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestInstalledBindingFilesystem(t *testing.T) {
	for _, mode := range []string{"unsafe-leaf", "unsafe-missing-parent", "alias-overlap", "alias-drift"} {
		t.Run(mode, func(t *testing.T) {
			c := installedConfigFixture(t)
			switch mode {
			case "unsafe-leaf":
				if err := os.MkdirAll(c.RuntimeDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(c.RuntimeDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "unsafe-missing-parent":
				if err := os.Mkdir(c.ConfigHome, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(c.ConfigHome, 0777); err != nil {
					t.Fatal(err)
				}
			case "alias-overlap", "alias-drift":
				target := filepath.Dir(c.RuntimeDir)
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				for _, leaf := range []string{"relay", "registration"} {
					if err := os.Mkdir(filepath.Join(target, leaf), 0700); err != nil {
						t.Fatal(err)
					}
				}
				alias := filepath.Join(filepath.Dir(target), "alias")
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				if mode == "alias-overlap" {
					c.TailscaleCLIStateRoot = filepath.Join(alias, "registration")
				} else {
					c.RuntimeDir = filepath.Join(alias, "relay")
					b, err := BindInstalledCLI(c)
					if err != nil {
						t.Fatal(err)
					}
					other := filepath.Join(filepath.Dir(target), "other")
					if err := os.Mkdir(other, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(other, alias); err != nil {
						t.Fatal(err)
					}
					if b.Validate(c) == nil {
						t.Fatal("alias changed after binding")
					}
					return
				}
			}
			if _, err := BindInstalledCLI(c); err == nil {
				t.Fatal("unsafe roots accepted")
			}
		})
	}
}

//go:build linux || darwin

package localcontrol

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestInstalledHandoffPreflightLocator(t *testing.T) {
	good := `{"protocol":"installed-control-v1","installation":"fixture","run":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	for _, name := range []string{"valid", "duplicate", "missing", "unknown", "null", "trailing", "oversize", "wrong-protocol", "foreign", "wrong-run"} {
		t.Run(name, func(t *testing.T) {
			data := good
			switch name {
			case "duplicate":
				data = strings.Replace(good, `"protocol":`, `"protocol":"x","protocol":`, 1)
			case "missing":
				data = `{"protocol":"installed-control-v1","installation":"fixture"}`
			case "unknown":
				data = strings.Replace(good, `"run"`, `"unknown"`, 1)
			case "null":
				data = strings.Replace(good, `"fixture"`, `null`, 1)
			case "trailing":
				data += `{}`
			case "oversize":
				data = strings.Repeat(" ", handoffBytes) + good
			case "wrong-protocol":
				data = strings.Replace(good, "installed-control-v1", "other", 1)
			case "foreign":
				data = strings.Replace(good, "fixture", "foreign", 1)
			case "wrong-run":
				data = strings.Replace(good, strings.Repeat("a", 32), strings.Repeat("A", 32), 1)
			}
			var l handoffLocator
			ok := handoffDecode([]byte(data), &l) == nil && handoffLocatorOK(l, "fixture")
			if ok != (name == "valid") {
				t.Fatal("locator schema decision")
			}
		})
	}
}

func TestInstalledHandoffPreflightProspectiveFinal(t *testing.T) {
	for _, name := range []string{"missing-cancel", "missing-created", "absent-cancel", "namespace-created"} {
		t.Run(name, func(t *testing.T) {
			selected, _, root := handoffFixture(t)
			if strings.HasPrefix(name, "missing") {
				if e := os.Remove(root); e != nil {
					t.Fatal(e)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			got, e := inspectInstalledHandoff(ctx, selected, func(stage string) error {
				if stage != "closed" {
					return nil
				}
				switch name {
				case "missing-cancel", "absent-cancel":
					cancel()
				case "missing-created":
					if e := os.Mkdir(root, 0700); e != nil {
						t.Fatal(e)
					}
				case "namespace-created":
					preflightNamespace(t, selected, root)
				}
				return nil
			})
			if e == nil || got != (InstalledHandoffObservation{}) {
				t.Fatal("prospective final drift accepted")
			}
		})
	}
}

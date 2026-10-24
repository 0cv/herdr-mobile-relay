package slashcmd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOMPMarketplaceDiscoveryOrder(t *testing.T) {
	testOMPMarketplaceDiscoveryOrder(t, false, false)
}

func TestOMPMarketplaceBudgetDiscoveryOrder(t *testing.T) {
	testOMPMarketplaceDiscoveryOrder(t, true, false)
}

func TestOMPMarketplaceIgnoresInvalidIDs(t *testing.T) {
	for _, exhaustBudget := range []bool{false, true} {
		t.Run(fmt.Sprintf("budget=%v", exhaustBudget), func(t *testing.T) {
			testOMPMarketplaceDiscoveryOrder(t, exhaustBudget, true)
		})
	}
}

func testOMPMarketplaceDiscoveryOrder(t *testing.T, exhaustBudget, invalidID bool) {
	t.Helper()
	f := newOMPFixture(t)
	count := 8
	if exhaustBudget {
		count = maxCatalogReadSize/(maxMetadataSize+1) + 10
	}
	var plugins []string
	if invalidID {
		root := filepath.Join(f.home, "marketplace", "invalid")
		writeSkill(t, filepath.Join(root, "skills"), "shared", "Invalid plugin must not win")
		writeSkill(t, filepath.Join(root, "skills"), "invalid-only", "Invalid plugin must not consume budget")
		encoded, err := json.Marshal([]map[string]any{{"installPath": root, "scope": "user"}})
		if err != nil {
			t.Fatal(err)
		}
		plugins = append(plugins, fmt.Sprintf("%q:%s", "zzz-invalid", encoded))
	}
	for i := range count {
		key := fmt.Sprintf("plugin-%03d@acme", count-i-1)
		root := filepath.Join(f.home, "marketplace", fmt.Sprintf("root-%03d", count-i))
		writeSkill(t, filepath.Join(root, "skills"), "shared", fmt.Sprintf("Plugin %03d first install", i))
		name := fmt.Sprintf("entry-%03d", i)
		metadata := "---\nname: " + name + "\ndescription: Marketplace entry\n---\n"
		file := filepath.Join(root, "skills", name, "SKILL.md")
		if exhaustBudget {
			paddedFile(t, file, metadata, 100*1024)
		} else {
			writeFile(t, file, metadata)
		}
		installs := []map[string]any{{"installPath": root, "scope": "user"}}
		if i == 0 {
			second := filepath.Join(f.home, "marketplace", "aaa-second-install")
			writeSkill(t, filepath.Join(second, "skills"), "shared", "Second install must not override first")
			writeSkill(t, filepath.Join(second, "skills"), "second-install-only", "Second install remains discoverable")
			installs = append(installs, map[string]any{"installPath": second, "scope": "user"})
		}
		encoded, err := json.Marshal(installs)
		if err != nil {
			t.Fatal(err)
		}
		plugins = append(plugins, fmt.Sprintf("%q:%s", key, encoded))
	}
	registry := `{"version":2,"plugins":{` + strings.Join(plugins, ",") + `}}`
	writeFile(t, filepath.Join(f.home, ".claude", "plugins", "installed_plugins.json"), registry)

	var first Catalog
	for attempt := range 16 {
		catalog := f.catalog(f.repo)
		if catalog.Truncated != exhaustBudget {
			t.Fatalf("attempt %d: truncated = %v, want %v", attempt, catalog.Truncated, exhaustBudget)
		}
		if _, ok := commandByName(catalog, "/skill:invalid-only"); ok {
			t.Fatal("invalid registry key contributed a skill")
		}
		shared, ok := commandByName(catalog, "/skill:shared")
		if !ok || shared.Description != "Plugin 000 first install" || shared.Source != "personal" {
			t.Fatalf("attempt %d: same-name winner = %+v, present = %v", attempt, shared, ok)
		}
		if _, ok := commandByName(catalog, "/skill:second-install-only"); !ok {
			t.Fatal("later install within a registry key was not discovered")
		}
		entries := 0
		for i := range count {
			_, found := commandByName(catalog, fmt.Sprintf("/skill:entry-%03d", i))
			if found {
				if entries != i {
					t.Fatalf("attempt %d: budget selection skipped entry %03d before entry %03d", attempt, entries, i)
				}
				entries++
			}
		}
		if exhaustBudget && (entries < 250 || entries >= count) || !exhaustBudget && entries != count {
			t.Fatalf("attempt %d: selected %d of %d marketplace entries", attempt, entries, count)
		}
		if attempt == 0 {
			first = catalog
		} else if !reflect.DeepEqual(first, catalog) {
			t.Fatalf("attempt %d: repeated discovery changed the catalog", attempt)
		}
	}
}

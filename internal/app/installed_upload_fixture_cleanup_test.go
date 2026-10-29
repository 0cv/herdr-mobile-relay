package app

import (
	"errors"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/upload"
)

// Factory-only cleanup: these acquisitions have no callers or holders. Snapshot
// both concrete identities before assertions; owner/mode correctness is not a
// prerequisite for containment. Never use this instead of releasing/joining work.
func registerInstalledUploadFixtureCleanup(t *testing.T, returned *upload.Manager, owner *installedUploadOwner) *installedUploadFixtureCleanup {
	t.Helper()
	c := &installedUploadFixtureCleanup{owner: owner}
	c.add(returned)
	if owner != nil {
		if bound, ok := owner.manager.(*upload.Manager); ok {
			c.add(bound)
		}
	}
	t.Cleanup(func() { _ = c.drain() })
	return c
}

type installedUploadFixtureCleanup struct {
	owner        *installedUploadOwner
	managers     []*upload.Manager
	once         sync.Once
	result       error
	closeCalls   map[*upload.Manager]int
	closeResults map[*upload.Manager]error
}

func (c *installedUploadFixtureCleanup) add(m *upload.Manager) {
	if m == nil {
		return
	}
	for _, prior := range c.managers {
		if prior == m {
			return
		}
	}
	c.managers = append(c.managers, m)
}

func (c *installedUploadFixtureCleanup) drain() error {
	c.once.Do(func() {
		// Fence every concrete resource before any owner join, including mismatches.
		installed := make(map[*upload.Manager]bool)
		for _, m := range c.managers {
			installed[m] = m.BeginInstalledDrain() == nil
		}
		c.result = c.owner.drain()
		c.closeCalls = make(map[*upload.Manager]int)
		c.closeResults = make(map[*upload.Manager]error)
		for _, m := range c.managers {
			// Always call actual Close: installed Close returns its cached drain;
			// wrong-mode owner refusal must not prevent ordinary first Close.
			c.closeCalls[m]++
			err := m.Close()
			c.closeResults[m] = err
			// Only the exact bound installed manager's cached result is already
			// represented by owner.drain; equal independent errors remain siblings.
			if c.owner == nil || c.owner.manager != m || !installed[m] {
				c.result = errors.Join(c.result, err)
			}
		}
	})
	return c.result
}

func TestInstalledUploadFixtureCleanupFallbacks(t *testing.T) {
	for _, name := range []string{"absent-owner", "mismatched-owner", "wrong-mode", "valid-installed"} {
		t.Run(name, func(t *testing.T) {
			policy := ordinaryUploadResources
			if name == "valid-installed" {
				policy = cliInstalledUploadResources
			}
			root, otherRoot := t.TempDir(), t.TempDir()
			m, acquired, err := acquireAppUpload(policy, upload.Config{Root: root})
			// Register before inspecting acquisition; the same once-guarded cleanup
			// remains responsible when the test deliberately changes owner topology.
			c := registerInstalledUploadFixtureCleanup(t, m, acquired)
			if err != nil || m == nil {
				t.Fatalf("acquire: %v", err)
			}
			switch name {
			case "wrong-mode":
				c.owner = &installedUploadOwner{manager: m}
			case "mismatched-owner":
				other, otherOwner, otherErr := acquireAppUpload(cliInstalledUploadResources, upload.Config{Root: otherRoot})
				// No assertion between acquisition and registration of every identity.
				c.add(other)
				if otherOwner != nil {
					if bound, ok := otherOwner.manager.(*upload.Manager); ok {
						c.add(bound)
					}
				}
				c.owner = otherOwner
				if otherErr != nil || other == nil {
					t.Fatalf("other: %v", otherErr)
				}
			}
			first := c.drain()
			if c.drain() != first {
				t.Fatal("cleanup result not cached")
			}
			for _, manager := range c.managers {
				if c.closeCalls[manager] != 1 || c.closeResults[manager] != nil {
					t.Fatal("missing or repeated concrete first-close wrapper")
				}
			}
			if name == "wrong-mode" {
				if !errors.Is(first, upload.ErrInstalledDrainRequired) {
					t.Fatal("owner refusal lost")
				}
			} else if first != nil {
				t.Fatal(first)
			}
			if name == "valid-installed" {
				if len(c.managers) != 1 {
					t.Fatal("identity not deduplicated")
				}
				if m.Close() != c.closeResults[m] {
					t.Fatal("installed Close not cached")
				}
				if _, err := m.Begin(upload.BeginRequest{}); err != upload.ErrInstalledDraining {
					t.Fatal("installed admission not fenced")
				}
			}
		})
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
)

func p1OfflineEvidenceContract(t *testing.T) p1fixture.Contract {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "evidence")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return p1fixture.Contract{Root: root, UID: os.Getuid(), EvidenceDirectory: directory}
}

func TestP1OfflineReloadInputReadiness(t *testing.T) {
	ready := "Reloaded keybindings, extensions, skills, prompts, themes, and context files"
	t.Run("bridge restoration does not imply focused editor", func(t *testing.T) {
		screens := []string{"fullscreen bridge restored", "Reloading keybindings, extensions, skills, prompts, themes, and context files...", ready + "\nReloading keybindings", ready}
		reads := 0
		screen, err := p1WaitReloadReady(context.Background(), func(context.Context) (herdr.PaneRead, error) {
			if reads >= len(screens) {
				t.Fatal("readiness did not terminate")
			}
			content := screens[reads]
			reads++
			return herdr.PaneRead{Content: []byte(content)}, nil
		})
		if err != nil || string(screen.Content) != ready || reads != len(screens) {
			t.Fatalf("input admitted before reload completed: %d %s %v", reads, screen.Content, err)
		}
	})
	t.Run("read failure propagates without input or retry", func(t *testing.T) {
		failure := errors.New("offline read failure")
		reads := 0
		_, err := p1WaitReloadReady(context.Background(), func(context.Context) (herdr.PaneRead, error) {
			reads++
			return herdr.PaneRead{}, failure
		})
		if !errors.Is(err, failure) || reads != 1 {
			t.Fatalf("read failure lost: %d %v", reads, err)
		}
	})
	t.Run("incomplete reload fails closed on cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := p1WaitReloadReady(ctx, func(context.Context) (herdr.PaneRead, error) {
			cancel()
			return herdr.PaneRead{Content: []byte("Reloading keybindings")}, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("incomplete reload admitted: %v", err)
		}
	})
	t.Run("canceled context never reads", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p1WaitReloadReady(ctx, func(context.Context) (herdr.PaneRead, error) {
			t.Fatal("read after cancellation")
			return herdr.PaneRead{}, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	})
}

func TestP1OfflineContractRejects(t *testing.T) {
	for _, input := range []string{"", "{", "arbitrary", "{}", `{"uid":-1}`, `{"unexpected":true}`} {
		if _, err := p1fixture.Validate(input, func(string) string { return "" }); err == nil {
			t.Fatalf("invalid contract admitted: %q", input)
		}
	}
	if _, err := p1fixture.Validate("{}", nil); err == nil {
		t.Fatal("nil environment admitted")
	}
}

func TestP1OfflineContractEvidenceQuota(t *testing.T) {
	t.Run("distinct names below quota serialize successfully", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		start := make(chan struct{})
		results := make(chan error, 12)
		var writers sync.WaitGroup
		for index := 0; index < 12; index++ {
			writers.Add(1)
			go func(index int) {
				defer writers.Done()
				<-start
				results <- contract.Save(string(rune('a'+index))+".json", strings.Repeat("x", 2048))
			}(index)
		}
		close(start)
		writers.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(contract.EvidenceDirectory)
		if err != nil || len(entries) != 12 {
			t.Fatalf("lost distinct writes: %d %v", len(entries), err)
		}
	})
	t.Run("distinct names competing for final capacity cannot both succeed", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		for index := 0; index < 7; index++ {
			if err := contract.Save(string(rune('a'+index))+".json", strings.Repeat("x", p1fixture.FileLimit-3)); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, name := range []string{"first.json", "second.json"} {
			go func(name string) {
				<-start
				results <- contract.Save(name, strings.Repeat("x", 400*1024))
			}(name)
		}
		close(start)
		successes := 0
		for index := 0; index < 2; index++ {
			if <-results == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("quota admitted %d competing writers", successes)
		}
		entries, err := os.ReadDir(contract.EvidenceDirectory)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			total += info.Size()
		}
		if total > p1fixture.EvidenceLimit {
			t.Fatal("serialized accounting exceeded Go partition")
		}
	})
	t.Run("duplicate name retains original bytes", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		if err := contract.Save("same.json", "original"); err != nil {
			t.Fatal(err)
		}
		if err := contract.Save("same.json", "replacement"); err == nil {
			t.Fatal("exclusive evidence overwritten")
		}
		data, err := os.ReadFile(filepath.Join(contract.EvidenceDirectory, "same.json"))
		if err != nil {
			t.Fatal(err)
		}
		var value string
		if err := json.Unmarshal(data, &value); err != nil || value != "original" {
			t.Fatal("original evidence changed")
		}
	})
	t.Run("oversized existing evidence fails accounting", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		if err := os.WriteFile(filepath.Join(contract.EvidenceDirectory, "oversized.json"), []byte(strings.Repeat("x", p1fixture.EvidenceFileLimit+1)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := contract.Save("next.json", "next"); err == nil {
			t.Fatal("oversized evidence ignored")
		}
	})
	t.Run("serialized file boundary and aggregate preserve prior evidence", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		value := strings.Repeat("x", p1fixture.EvidenceFileLimit-3)
		if err := contract.Save("maximum.json", value); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(contract.EvidenceDirectory, "maximum.json")
		before, err := os.ReadFile(path)
		if err != nil || len(before) != p1fixture.EvidenceFileLimit {
			t.Fatalf("serialized boundary: %d %v", len(before), err)
		}
		if err := contract.Save("oversized.json", value+"x"); err == nil {
			t.Fatal("oversized serialized evidence admitted")
		}
		if _, err := os.Stat(filepath.Join(contract.EvidenceDirectory, "oversized.json")); !os.IsNotExist(err) {
			t.Fatalf("rejected evidence created a file: %v", err)
		}
		remaining := p1fixture.EvidenceLimit - len(before)
		if err := contract.Save("remaining.json", strings.Repeat("y", remaining-3)); err != nil {
			t.Fatal(err)
		}
		if err := contract.Save("aggregate-overflow.json", nil); err == nil {
			t.Fatal("aggregate overflow admitted")
		}
		if err := contract.Save("maximum.json", "replacement"); err == nil {
			t.Fatal("prior evidence replaced")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("rejected saves changed prior evidence")
		}
		entries, err := os.ReadDir(contract.EvidenceDirectory)
		if err != nil || len(entries) != 2 {
			t.Fatalf("failed saves left files: %d %v", len(entries), err)
		}
	})
	t.Run("serialization error preserves evidence", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		if err := contract.Save("original.json", "original"); err != nil {
			t.Fatal(err)
		}
		if err := contract.Save("invalid.json", make(chan int)); err == nil {
			t.Fatal("unserializable evidence admitted")
		}
		entries, err := os.ReadDir(contract.EvidenceDirectory)
		if err != nil || len(entries) != 1 || entries[0].Name() != "original.json" {
			t.Fatal("serialization failure altered evidence")
		}
	})
	t.Run("foreign directory and unsafe lock refuse", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		other := p1OfflineEvidenceContract(t)
		contract.EvidenceDirectory = other.EvidenceDirectory
		if err := contract.Save("foreign.json", "foreign"); err == nil {
			t.Fatal("foreign root admitted")
		}
		contract.EvidenceDirectory = filepath.Join(contract.Root, "evidence")
		if err := os.Symlink(filepath.Join(other.Root, "lock"), contract.EvidenceDirectory+".lock"); err != nil {
			t.Fatal(err)
		}
		if err := contract.Save("linked.json", "linked"); err == nil {
			t.Fatal("symlink accounting lock admitted")
		}
	})
}

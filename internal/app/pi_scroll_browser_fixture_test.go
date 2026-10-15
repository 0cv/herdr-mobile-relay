package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
)

func p1OfflineEvidenceContract(t *testing.T) p1fixture.Contract {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	if err := os.Chmod(root, 0700); err != nil { t.Fatal(err) }
	directory := filepath.Join(root, "evidence")
	if err := os.Mkdir(directory, 0700); err != nil { t.Fatal(err) }
	return p1fixture.Contract{Root: root, UID: os.Getuid(), EvidenceDirectory: directory}
}

func TestP1OfflineContractRejects(t *testing.T) {
	for _, input := range []string{"", "{", "arbitrary", "{}", `{"uid":-1}`, `{"unexpected":true}`} {
		if _, err := p1fixture.Validate(input, func(string) string { return "" }); err == nil {
			t.Fatalf("invalid contract admitted: %q", input)
		}
	}
	if _, err := p1fixture.Validate("{}", nil); err == nil { t.Fatal("nil environment admitted") }
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
		for err := range results { if err != nil { t.Fatal(err) } }
		entries, err := os.ReadDir(contract.EvidenceDirectory)
		if err != nil || len(entries) != 12 { t.Fatalf("lost distinct writes: %d %v", len(entries), err) }
	})
	t.Run("distinct names competing for final capacity cannot both succeed", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		for index := 0; index < 7; index++ {
			if err := contract.Save(string(rune('a'+index))+".json", strings.Repeat("x", p1fixture.FileLimit-3)); err != nil { t.Fatal(err) }
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
		for index := 0; index < 2; index++ { if <-results == nil { successes++ } }
		if successes != 1 { t.Fatalf("quota admitted %d competing writers", successes) }
		entries, err := os.ReadDir(contract.EvidenceDirectory)
		if err != nil { t.Fatal(err) }
		var total int64
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil { t.Fatal(err) }
			total += info.Size()
		}
		if total > p1fixture.EvidenceLimit { t.Fatal("serialized accounting exceeded Go partition") }
	})
	t.Run("duplicate name retains original bytes", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		if err := contract.Save("same.json", "original"); err != nil { t.Fatal(err) }
		if err := contract.Save("same.json", "replacement"); err == nil { t.Fatal("exclusive evidence overwritten") }
		data, err := os.ReadFile(filepath.Join(contract.EvidenceDirectory, "same.json"))
		if err != nil { t.Fatal(err) }
		var value string
		if err := json.Unmarshal(data, &value); err != nil || value != "original" { t.Fatal("original evidence changed") }
	})
	t.Run("oversized existing evidence fails accounting", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		if err := os.WriteFile(filepath.Join(contract.EvidenceDirectory, "oversized.json"), []byte(strings.Repeat("x", p1fixture.FileLimit+1)), 0600); err != nil { t.Fatal(err) }
		if err := contract.Save("next.json", "next"); err == nil { t.Fatal("oversized evidence ignored") }
	})
	t.Run("foreign directory and unsafe lock refuse", func(t *testing.T) {
		contract := p1OfflineEvidenceContract(t)
		other := p1OfflineEvidenceContract(t)
		contract.EvidenceDirectory = other.EvidenceDirectory
		if err := contract.Save("foreign.json", "foreign"); err == nil { t.Fatal("foreign root admitted") }
		contract.EvidenceDirectory = filepath.Join(contract.Root, "evidence")
		if err := os.Symlink(filepath.Join(other.Root, "lock"), contract.EvidenceDirectory+".lock"); err != nil { t.Fatal(err) }
		if err := contract.Save("linked.json", "linked"); err == nil { t.Fatal("symlink accounting lock admitted") }
	})
}

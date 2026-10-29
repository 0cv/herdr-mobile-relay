package app

import (
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/upload"
)

func TestInstalledUploadFactoryResourceSelection(t *testing.T) {
	// These are resource selections, not transports or invitation policies.
	// Cloudflare/gateway/development all call the ordinary constructor path.
	m, owner, err := acquireAppUpload(ordinaryUploadResources, upload.Config{Root: t.TempDir()})
	registerInstalledUploadFixtureCleanup(t, m, owner)
	if err != nil || owner != nil || m == nil {
		t.Fatalf("ordinary: %v", err)
	}
	if m.BeginInstalledDrain() != upload.ErrInstalledDrainRequired {
		t.Fatal("ordinary acquisition converted")
	}
	installed, owned, err := acquireAppUpload(cliInstalledUploadResources, upload.Config{Root: t.TempDir()})
	registerInstalledUploadFixtureCleanup(t, installed, owned)
	if err != nil || installed == nil || owned == nil || owned.manager != installed {
		t.Fatalf("installed: %v", err)
	}
	owned.begin()
	if _, err := installed.Begin(upload.BeginRequest{}); err != upload.ErrInstalledDraining {
		t.Fatal(err)
	}
	if err := owned.collectResult(); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledUploadFactoryOptionalAndFatal(t *testing.T) {
	manager, owner, err := acquireAppUpload(cliInstalledUploadResources, upload.Config{})
	registerInstalledUploadFixtureCleanup(t, manager, owner)
	if manager != nil {
		t.Fatal("invalid factory succeeded")
	}
	if owner == nil || err == nil || fatalUploadConstruction(err) != nil {
		t.Fatal("clean optional failure made fatal")
	}
	if err := owner.collectResult(); err != nil {
		t.Fatal("absent manager fabricated a drain failure")
	}
	witness := errors.New("physical cleanup")
	fatal := &upload.ConstructionCleanupError{Err: witness}
	if fatalUploadConstruction(fatal) != fatal || !errors.Is(fatalUploadConstruction(fatal), witness) {
		t.Fatal("cleanup identity lost")
	}
}

type uploadOwnerLogicalResource struct {
	err    error
	fenced bool
}

func (r *uploadOwnerLogicalResource) BeginInstalledDrain() error { r.fenced = true; return nil }
func (r *uploadOwnerLogicalResource) DrainInstalled() error      { r.fenced = true; return r.err }

func TestInstalledUploadOwnerRunWrapperCollection(t *testing.T) {
	witness := errors.New("resource")
	sibling := errors.New("control")
	resource := &uploadOwnerLogicalResource{err: witness}
	owner := &installedUploadOwner{manager: resource}
	owner.begin()
	run := errors.Join(sibling, owner.collectResult())
	outer := errors.Join(run, owner.collectResult())
	if !resource.fenced || !errors.Is(outer, witness) || !errors.Is(outer, sibling) {
		t.Fatal("lost error sibling")
	}
	if owner.drain() != witness {
		t.Fatal("collection erased cached result")
	}
	// Deliberately equal independent siblings are NOT errors.Is-deduplicated.
	var leaves func(error) int
	leaves = func(err error) int {
		if err == witness {
			return 1
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			n := 0
			for _, child := range joined.Unwrap() {
				n += leaves(child)
			}
			return n
		}
		return 0
	}
	if leaves(outer) != 1 || leaves(errors.Join(outer, witness)) != 2 {
		t.Fatal("resource duplicated or unrelated sibling laundered")
	}
}

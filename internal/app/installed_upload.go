package app

import (
	"errors"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/upload"
)

// This resource policy is independent of invitations and transport selection.
// Only the setup/STRICT wrappers select it, before constructor acquisition.
type uploadResourcePolicy uint8

const (
	ordinaryUploadResources uploadResourcePolicy = iota
	cliInstalledUploadResources
)

type installedUploadResource interface {
	BeginInstalledDrain() error
	DrainInstalled() error
}

type installedUploadOwner struct {
	manager   installedUploadResource
	collect   sync.Once
	beginOnce sync.Once
	beginErr  error
	drainOnce sync.Once
	drainErr  error
	// Registered before Run starts; joining never depends on Run returning.
	controlJoin func()
}

func acquireAppUpload(policy uploadResourcePolicy, cfg upload.Config) (*upload.Manager, *installedUploadOwner, error) {
	if policy != cliInstalledUploadResources {
		manager, err := upload.NewManager(cfg)
		return manager, nil, err
	}
	// Owner exists before the factory; never convert an ordinary manager.
	owner := &installedUploadOwner{}
	manager, err := upload.NewInstalledManager(cfg)
	if manager != nil {
		owner.manager = manager
	}
	return manager, owner, err
}
func fatalUploadConstruction(err error) error {
	var cleanup *upload.ConstructionCleanupError
	if errors.As(err, &cleanup) {
		return err
	}
	return nil
}
func (o *installedUploadOwner) begin() {
	if o != nil && o.manager != nil {
		o.beginOnce.Do(func() { o.beginErr = o.manager.BeginInstalledDrain() })
	}
}
func (o *installedUploadOwner) drain() error {
	if o == nil {
		return nil
	}
	o.drainOnce.Do(func() {
		o.begin()
		if o.controlJoin != nil {
			o.controlJoin()
		}
		if o.manager != nil {
			o.drainErr = o.manager.DrainInstalled()
		}
		if o.beginErr != nil {
			o.drainErr = errors.Join(o.beginErr, o.drainErr)
		}
	})
	return o.drainErr
}

// Resource results are collected once by explicit owner identity, not by
// errors.Is (which could erase unrelated equal siblings). Run and wrappers
// both drain; whichever returns first carries the single resource result.
// result is shared by Run and its outer wrappers. Collection is owner-scoped;
// independent equal primary/control errors are deliberately retained.
func (o *installedUploadOwner) result(primary error, siblings ...error) error {
	return errors.Join(primary, errors.Join(siblings...), o.collectResult())
}

func (o *installedUploadOwner) collectResult() (result error) {
	if o == nil {
		return nil
	}
	err := o.drain()
	o.collect.Do(func() { result = err })
	return result
}

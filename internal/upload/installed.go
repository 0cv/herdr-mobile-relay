package upload

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
)

var ErrInstalledDrainRequired = errors.New("installed upload ownership required")
var ErrInstalledDraining = errors.New("installed upload is draining")

// ConstructionCleanupError means optional-upload fallback is unsafe.
type ConstructionCleanupError struct{ Err error }

func (e *ConstructionCleanupError) Error() string { return "upload construction cleanup failed" }
func (e *ConstructionCleanupError) Unwrap() error { return e.Err }

type CleanupOverflowError struct{ Count uint64 }

func (e *CleanupOverflowError) Error() string {
	return fmt.Sprintf("upload cleanup witness overflow: %d", e.Count)
}

type installedOwnership struct {
	mu        sync.Mutex
	work      sync.WaitGroup
	closing   bool
	once      sync.Once
	result    error
	witnesses [16]error
	count     int
	overflow  uint64
	// Private fault seam: invoked only after the real close.
	afterClose func(io.Closer) error
	// denyStage returns an additional private pre-I/O denial; nil is concrete I/O.
	// Configured only before use. It never represents a completed effect.
	denyStage func(stage, path string) error
	// beforeWait observes the terminal join boundary, outside the admission lock.
	// Set only before concurrent use; nil in production.
	beforeWait func()
	// afterAdmission is a nil-default receipt outside mu, before resource locks.
	// Configure before concurrent use. Abnormal callback exit releases its ticket.
	afterAdmission func()
}

func NewInstalledManager(config Config) (*Manager, error) {
	return newManager(config, &installedOwnership{})
}

func (m *Manager) admit() (func(), error) {
	if m.installed == nil {
		return func() {}, nil
	}
	o := m.installed
	o.mu.Lock()
	if o.closing {
		o.mu.Unlock()
		return nil, ErrInstalledDraining
	}
	o.work.Add(1)
	o.mu.Unlock()
	transferred := false
	defer func() {
		if !transferred {
			o.work.Done()
		}
	}()
	if o.afterAdmission != nil {
		o.afterAdmission()
	}
	transferred = true
	return o.work.Done, nil
}

func (m *Manager) BeginInstalledDrain() error {
	if m == nil || m.installed == nil || m.root == nil {
		return ErrInstalledDrainRequired
	}
	m.installed.mu.Lock()
	m.installed.closing = true
	m.installed.mu.Unlock()
	return nil
}

// DrainInstalled has no timeout: accepted callbacks and synchronous filesystem
// work must actually return before the root can be released.
func (m *Manager) DrainInstalled() error {
	if err := m.BeginInstalledDrain(); err != nil {
		return err
	}
	o := m.installed
	o.once.Do(func() {
		if o.beforeWait != nil {
			o.beforeWait()
		}
		o.work.Wait()
		for _, current := range m.sessions {
			m.closeSessionFiles(current)
		}
		m.witness(m.physicalClose(m.root))
		o.result = m.cleanupResult()
	})
	return o.result
}

func (m *Manager) witness(err error) {
	if err == nil || m.installed == nil {
		return
	}
	o := m.installed
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.count < len(o.witnesses) {
		o.witnesses[o.count] = err
		o.count++
	} else if o.overflow != ^uint64(0) {
		o.overflow++
	}
}
func (m *Manager) cleanupResult() error {
	o := m.installed
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	result := errors.Join(o.witnesses[:o.count]...)
	if o.overflow != 0 {
		result = errors.Join(result, &CleanupOverflowError{Count: o.overflow})
	}
	return result
}
func (m *Manager) physicalClose(c io.Closer) error {
	err := c.Close()
	if m.installed != nil && m.installed.afterClose != nil {
		err = errors.Join(err, m.installed.afterClose(c))
	}
	return err
}
func (m *Manager) cleanupClose(c io.Closer) { m.witness(m.physicalClose(c)) }
func (m *Manager) removed(err error) {
	if !sweepProvenAbsent(err) {
		m.witness(err)
	}
}
func (m *Manager) closeFile(item *sessionFile) error {
	if item.file == nil {
		return item.closeErr
	}
	item.closeErr = m.physicalClose(item.file)
	item.file = nil
	m.witness(item.closeErr)
	return item.closeErr
}
func (m *Manager) cleanSession(current *session) {
	if m.installed != nil && current.cleaned {
		return
	}
	current.cleaned = true
	m.closeSessionFiles(current)
	m.removed(m.removeSessionDirectory("sessions/" + current.id))
}
func (m *Manager) readDirectory(name string) ([]fs.DirEntry, error) {
	if m.installed == nil {
		return fs.ReadDir(m.root.FS(), name)
	}
	if err := m.deny("sweep-directory-open", name); err != nil {
		return nil, err
	}
	directory, err := m.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer m.cleanupClose(directory)
	if err := m.deny("sweep-directory-read", name); err != nil {
		return nil, err
	}
	return directory.ReadDir(-1)
}

func (m *Manager) constructionFailure(primary error) (*Manager, error) {
	if m.installed == nil {
		_ = m.root.Close()
		return nil, primary
	}
	cleanup := m.DrainInstalled()
	if cleanup != nil {
		return nil, &ConstructionCleanupError{Err: errors.Join(primary, cleanup)}
	}
	return nil, primary
}

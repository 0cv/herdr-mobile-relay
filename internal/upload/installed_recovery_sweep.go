package upload

import (
	"io/fs"
	"syscall"
)

// Literal absence at every leaf is required; Is methods cannot prove siblings.
// Fixed storage and a shared visit budget bound wide trees and cycles.
func sweepProvenAbsent(err error) bool {
	if err == nil {
		return false
	}
	var pending [64]error
	pending[0] = err
	n, budget := 1, len(pending)
	for n > 0 {
		if budget == 0 {
			return false
		}
		budget--
		n--
		current := pending[n]
		pending[n] = nil
		if current == nil {
			return false
		}
		if current == fs.ErrNotExist || current == syscall.ENOENT {
			continue
		}
		switch e := current.(type) {
		case interface{ Unwrap() []error }:
			children := e.Unwrap()
			if len(children) == 0 || len(children) > len(pending)-n {
				return false
			}
			for _, child := range children {
				pending[n] = child
				n++
			}
		case interface{ Unwrap() error }:
			pending[n] = e.Unwrap()
			n++
		default:
			return false
		}
	}
	return true
}

func (m *Manager) sweepInfo(path string, entry fs.DirEntry) (fs.FileInfo, error) {
	if err := m.deny("sweep-entry-info", path); err != nil {
		return nil, err
	}
	return entry.Info()
}

func (m *Manager) sweepRemoveDirectory(path string) error {
	if err := m.deny("sweep-session-remove", path); err != nil {
		return err
	}
	return m.root.RemoveAll(path)
}

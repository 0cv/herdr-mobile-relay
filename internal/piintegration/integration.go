package piintegration

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const Directory = "herdr-mobile-relay-commands"
const Marker = "herdr-mobile-relay-pi-commands-v1"

var Files = []string{"index.ts", "bridge.mjs", "activity.mjs", ".relay-owned"}

type Profile struct {
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

type State struct {
	Instance string    `json:"instance"`
	Enabled  bool      `json:"enabled"`
	Profiles []Profile `json:"profiles"`
}

type Manager struct {
	mu       sync.Mutex
	bundle   fs.FS
	instance string
	dirs     func() []string
}

func New(bundle fs.FS, dirs func() []string) *Manager {
	return &Manager{bundle: bundle, instance: rand.Text(), dirs: dirs}
}

func (m *Manager) Instance() string { return m.instance }

func (m *Manager) Status() State  { return m.apply("status") }
func (m *Manager) Refresh() State { return m.apply("refresh") }
func (m *Manager) Set(enabled bool) State {
	if enabled {
		return m.apply("install")
	}
	return m.apply("remove")
}

func (m *Manager) apply(action string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := State{Instance: m.instance, Profiles: []Profile{}}
	seen := make(map[string]bool)
	for _, dir := range m.dirs() {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		profile := m.profile(dir, action)
		state.Enabled = state.Enabled || profile.Installed
		state.Profiles = append(state.Profiles, profile)
	}
	return state
}

func regular(root *os.Root, name string) ([]byte, os.FileMode, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if len(data) > 1024*1024 {
		return nil, 0, errors.New("file too large")
	}
	return data, info.Mode().Perm(), err
}

func replace(root *os.Root, name string, data []byte) error {
	temp := ".relay-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(temp, name)
}

func (m *Manager) profile(dir, action string) Profile {
	p := Profile{Path: dir, Status: "not installed"}
	refuse := func(reason string) Profile { p.Status, p.Reason = "refused", reason; return p }
	if !filepath.IsAbs(dir) {
		return refuse("Pi agent directory must be absolute")
	}
	extensions := filepath.Join(dir, "extensions")
	target := filepath.Join(extensions, Directory)
	for _, path := range []string{extensions, target} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refuse("Cannot inspect extension directory")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return refuse("Refusing a symlinked or invalid extension directory")
		}
	}
	info, err := os.Lstat(target)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return refuse("Cannot inspect extension directory")
	}
	if missing && action != "install" {
		return p
	}
	bundled := make(map[string][]byte)
	if action != "remove" {
		for _, name := range Files {
			data := []byte(Marker + "\n")
			if name != ".relay-owned" {
				var err error
				data, err = fs.ReadFile(m.bundle, name)
				if err != nil {
					if missing {
						return refuse("Bundled Pi integration is unavailable")
					}
					break
				}
			}
			bundled[name] = data
		}
	}
	if missing {
		if err := os.MkdirAll(extensions, 0700); err != nil {
			return refuse("Cannot create extension directory")
		}
		if err := os.Mkdir(target, 0700); err != nil {
			return refuse("Cannot create integration directory")
		}
		info, err = os.Lstat(target)
		if err != nil || !info.IsDir() {
			return refuse("Integration directory changed")
		}
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return refuse("Cannot open integration directory")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return refuse("Integration directory changed")
	}
	if !missing {
		marker, _, err := regular(root, ".relay-owned")
		if err != nil || strings.TrimRight(string(marker), "\n") != Marker {
			return refuse("Refusing to change an unowned extension directory")
		}
		p.Installed = true
		for _, name := range Files {
			stat, err := root.Lstat(name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !stat.Mode().IsRegular() {
				return refuse("Refusing a symlinked or invalid integration file")
			}
		}
	}
	if action == "remove" {
		for _, name := range Files {
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return refuse("Cannot remove integration file")
			}
		}
		p.Installed, p.Status = false, "removed"
		_ = os.Remove(target)
		return p
	}
	if len(bundled) != len(Files) {
		return refuse("Bundled Pi integration is unavailable")
	}
	current := !missing && info.Mode().Perm() == 0700
	for _, name := range Files {
		installed, mode, err := regular(root, name)
		current = current && err == nil && mode == 0600 && bytes.Equal(bundled[name], installed)
	}
	if current {
		p.Status = "up to date"
		return p
	}
	if action == "status" {
		p.Status = "outdated"
		return p
	}
	if err := root.Chmod(".", 0700); err != nil {
		return refuse("Cannot secure integration directory")
	}
	for _, name := range Files {
		if err := replace(root, name, bundled[name]); err != nil {
			return refuse("Cannot write integration file")
		}
	}
	p.Installed, p.Status = true, "installed"
	return p
}

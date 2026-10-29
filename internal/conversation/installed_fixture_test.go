package conversation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// Explicit logical inventory only. No test dependency reaches the OS bundle.
var sourceFixtureLocation = Location{Path: "/planned/source", Root: "/planned"}

type sourceFixtureInfo struct {
	size int64
	mode os.FileMode
}

func (i sourceFixtureInfo) Name() string       { return "source" }
func (i sourceFixtureInfo) Size() int64        { return i.size }
func (i sourceFixtureInfo) Mode() os.FileMode  { return i.mode }
func (i sourceFixtureInfo) ModTime() time.Time { return time.Time{} }
func (i sourceFixtureInfo) IsDir() bool        { return i.mode.IsDir() }
func (i sourceFixtureInfo) Sys() any           { return nil }

type sourceFixture struct {
	data      []byte
	calls     map[string]int
	unplanned int
	fail      string
	failAt    int
	fault     error
	closeErr  error
	openErr   error
	nilHandle bool
	nilInfo   bool
	mode      os.FileMode
	after     func(string)
}

func newSourceFixture() *sourceFixture {
	return &sourceFixture{data: []byte("first\nsecond\n"), calls: make(map[string]int), fault: errors.New("logical source fault")}
}
func (f *sourceFixture) effect(name string) error {
	f.calls[name]++
	if f.after != nil {
		f.after(name)
	}
	if f.fail == name && (f.failAt == 0 || f.calls[name] == f.failAt) {
		return f.fault
	}
	return nil
}
func (f *sourceFixture) info() os.FileInfo {
	if f.nilInfo {
		return nil
	}
	return sourceFixtureInfo{size: int64(len(f.data)), mode: f.mode}
}
func (f *sourceFixture) planned(path string) bool {
	if path == "/planned" || path == "/planned/source" {
		return true
	}
	f.unplanned++
	return false
}
func (f *sourceFixture) deps() installedSourceDeps {
	return installedSourceDeps{
		open: func(_ context.Context, owner *conversationlife.Owner, path string, onClose func(error)) (*sourceOwnership, error) {
			return acquireSourceHandle(owner, onClose, func() (sourceHandle, error) {
				if !f.planned(path) || path != "/planned/source" {
					f.unplanned++
					return nil, f.fault
				}
				err := f.effect("open")
				if f.nilHandle {
					return nil, err
				}
				if err != nil {
					return f, err
				}
				return f, f.openErr
			})
		},
		eval: func(path string) (string, error) {
			if !f.planned(path) {
				return "", f.fault
			}
			return path, f.effect("eval")
		},
		stat: func(path string) (os.FileInfo, error) {
			if !f.planned(path) || path != "/planned/source" {
				f.unplanned++
				return nil, f.fault
			}
			return f.info(), f.effect("path-stat")
		},
		same: func(a, b os.FileInfo) bool { return f.effect("same") == nil },
	}
}
func (f *sourceFixture) Stat() (os.FileInfo, error) { return f.info(), f.effect("stat") }
func (f *sourceFixture) ReadAt(p []byte, offset int64) (int, error) {
	n, err := bytes.NewReader(f.data).ReadAt(p, offset)
	if fault := f.effect("read"); fault != nil {
		return n, fault
	}
	return n, err
}
func (f *sourceFixture) Close() error {
	if err := f.effect("close"); err != nil {
		return err
	}
	return f.closeErr
}

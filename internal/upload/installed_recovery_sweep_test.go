package upload

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type sweepEmptyError struct{}

func (sweepEmptyError) Error() string   { return "empty" }
func (sweepEmptyError) Unwrap() []error { return nil }

func TestInstalledRecoverySweepStages(t *testing.T) {
	for _, row := range []string{"open", "read-close", "close", "info", "session", "link", "object", "legacy"} {
		t.Run(row, func(t *testing.T) {
			m := installedTestManager(t)
			path, stage := "objects/orphan", "sweep-object-remove"
			switch row {
			case "open":
				stage = "sweep-directory-open"
			case "read-close":
				stage = "sweep-directory-read"
			case "close":
				stage = "unused"
			case "info":
				stage = "sweep-entry-info"
			case "session":
				path, stage = "sessions/stale", "sweep-session-remove"
			case "link":
				path, stage = "objects/link", "sweep-link-remove"
			case "legacy":
				path, stage = "20000101-000000-12345678-old.png", "sweep-legacy-remove"
			}
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			full := filepath.Join(m.rootPath, path)
			var err error
			if row == "session" {
				err = os.Mkdir(full, 0700)
			} else if row == "link" {
				err = os.Symlink(target, full)
			} else {
				err = os.WriteFile(full, []byte("old"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Add(365 * 24 * time.Hour)
			m.now = func() time.Time { return now }
			primary, closeFault := errors.New("private sweep denial"), errors.New("private sweep close")
			denied, closes := 0, 0
			closed := true
			m.installed.denyStage = func(s, p string) error {
				if s == stage && (p == path || p == "objects") {
					denied++
					return primary
				}
				return nil
			}
			m.installed.afterClose = func(c io.Closer) error {
				if f, ok := c.(*os.File); ok && filepath.Base(f.Name()) == "objects" {
					closes++
					_, e := f.Stat()
					closed = closed && errors.Is(e, os.ErrClosed)
					if row == "read-close" || row == "close" {
						return closeFault
					}
				}
				return nil
			}
			count := m.Cleanup()
			result := m.DrainInstalled()
			wantCloses := 1
			if row == "open" {
				wantCloses = 0
			}
			if closes != wantCloses || !closed {
				t.Fatal("first real directory close mismatch")
			}
			if row != "close" && (denied != 1 || terminalMatrixOccurrences(t, result, primary) != 1) {
				t.Fatal("denial lost")
			}
			if row == "read-close" || row == "close" {
				if terminalMatrixOccurrences(t, result, closeFault) != 1 {
					t.Fatal("close sibling lost")
				}
			}
			if row == "session" || row == "link" || row == "object" || row == "legacy" {
				if count != 1 {
					t.Fatal("attempt count changed")
				}
			}
			if row != "close" {
				if _, e := os.Lstat(full); e != nil {
					t.Fatal("failed operation removed artifact", e)
				}
			}
			data, e := os.ReadFile(target)
			if e != nil || string(data) != "untouched" {
				t.Fatal("link target touched")
			}
		})
	}
}

func TestInstalledRecoverySweepAbsence(t *testing.T) {
	for _, row := range []string{"absent", "joined-absent", "joined-unknown", "empty", "over-budget", "overflow"} {
		t.Run(row, func(t *testing.T) {
			m := installedTestManager(t)
			path := "objects/orphan"
			full := filepath.Join(m.rootPath, path)
			if e := os.WriteFile(full, []byte("old"), 0600); e != nil {
				t.Fatal(e)
			}
			now := time.Now().Add(365 * 24 * time.Hour)
			m.now = func() time.Time { return now }
			unknown := errors.New("private unknown")
			var fault error = fs.ErrNotExist
			switch row {
			case "joined-absent":
				fault = errors.Join(fs.ErrNotExist, fmt.Errorf("wrapped: %w", fs.ErrNotExist))
			case "joined-unknown", "overflow":
				fault = errors.Join(fs.ErrNotExist, unknown)
			case "empty":
				fault = sweepEmptyError{}
			case "over-budget":
				for i := 0; i < 65; i++ {
					fault = fmt.Errorf("wrapped: %w", fault)
				}
			}
			calls := 0
			var removeErr error
			m.installed.denyStage = func(s, p string) error {
				if s != "sweep-object-remove" || p != path {
					return nil
				}
				calls++
				if row == "absent" || row == "joined-absent" {
					removeErr = os.Remove(full)
					if removeErr != nil {
						return removeErr
					}
				}
				return fault
			}
			runs := 1
			if row == "overflow" {
				runs = 18
			}
			for i := 0; i < runs; i++ {
				if m.Cleanup() != 1 {
					t.Fatal("attempt count")
				}
			}
			result := m.DrainInstalled()
			if calls != runs || removeErr != nil {
				t.Fatal("removal receipt")
			}
			if row == "absent" || row == "joined-absent" {
				if result != nil {
					t.Fatal("proven absence poisoned")
				}
				if _, e := os.Stat(full); !errors.Is(e, fs.ErrNotExist) {
					t.Fatal("target not absent")
				}
			} else {
				if m.installed.witnesses[0] != fault {
					t.Fatal("original witness replaced")
				}
				want := 1
				overflow := uint64(0)
				if row == "overflow" {
					want = 16
					overflow = 2
				}
				if m.installed.count != want || m.installed.overflow != overflow {
					t.Fatal("bounded accounting")
				}
				if _, e := os.Stat(full); e != nil {
					t.Fatal("unknown removed target")
				}
				if row == "overflow" {
					var e *CleanupOverflowError
					if !errors.As(result, &e) || e.Count != 2 {
						t.Fatal("overflow missing")
					}
				}
			}
		})
	}
}

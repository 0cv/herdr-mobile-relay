package upload

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledUploadRealCleanupOverflow(t *testing.T) {
	for _, name := range []string{"bounded", "saturating"} {
		t.Run(name, func(t *testing.T) {
			m := installedTestManager(t)
			sentinels := make([]error, 19)
			closes := 0
			unexpectedClose := errors.New("unexpected repeated staging close")
			m.installed.afterClose = func(c io.Closer) error {
				f, ok := c.(*os.File)
				if !ok || filepath.Ext(f.Name()) != ".part" {
					return nil
				}
				index := closes
				closes++
				if index >= len(sentinels) {
					return unexpectedClose
				}
				return sentinels[index]
			}
			for i := range sentinels {
				sentinels[i] = errors.New("independent physical staging close")
				started := beginOne(t, m, uploadTarget(0), "a.txt", "text/plain", 1)
				if name == "saturating" && i == 16 {
					// Seed only the impractically large counter; subsequent increments must
					// originate from real cleanup, not direct witness injection.
					m.installed.overflow = ^uint64(0) - 1
				}
				if err := m.Cancel(uploadTarget(0), started.UploadID); err != nil {
					t.Fatal(err)
				}
			}
			result := m.DrainInstalled()
			if closes != len(sentinels) {
				t.Fatalf("physical closes = %d", closes)
			}
			for i, sentinel := range sentinels {
				if errors.Is(result, sentinel) != (i < 16) {
					t.Fatalf("witness %d retention", i)
				}
			}
			var overflow *CleanupOverflowError
			expected := uint64(3)
			if name == "saturating" {
				expected = ^uint64(0)
			}
			if !errors.As(result, &overflow) || overflow.Count != expected {
				t.Fatalf("overflow: %v", result)
			}
			if m.Close() != result || m.DrainInstalled() != result || closes != 19 {
				t.Fatal("terminal result recounted cleanup")
			}
		})
	}
}

package blackbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

type binaries struct {
	fakeHerdr string
	relay     string
	gateway   string
}

var builtBinaries binaries

func TestMain(m *testing.M) {
	if runtime.GOOS == "darwin" {
		if tempDir, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
			_ = os.Setenv("TMPDIR", tempDir)
		}
	}
	if os.Getenv(natPlanEnvVar) != "" || os.Getenv(natRespondEnvVar) != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "blackbox-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create binary dir: %v\n", err)
		os.Exit(1)
	}
	if err := buildBinaries(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func buildBinaries(dir string) error {
	root, err := findRepoRoot()
	if err != nil {
		return err
	}
	builtBinaries = binaries{
		fakeHerdr: filepath.Join(dir, "fake-herdr"),
		relay:     filepath.Join(dir, "herdr-mobile-relay"),
		gateway:   filepath.Join(dir, "herdr-gateway"),
	}
	for out, pkg := range map[string]string{
		builtBinaries.fakeHerdr: "./cmd/fake-herdr",
		builtBinaries.relay:     "./cmd/herdr-mobile-relay",
		builtBinaries.gateway:   "./cmd/herdr-gateway",
	} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Dir = root
		if combined, err := build.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w\n%s", pkg, err, combined)
		}
	}
	return nil
}

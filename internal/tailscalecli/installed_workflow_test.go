package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func installedFixtureBinding(t *testing.T, m *Manager, r PublishRequest) InstalledBinding {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := NewInstalledBinding(InstalledBindingInput{
		StateRoot: m.stateRoot, CoordinationRoot: m.coordinationRoot, Binary: m.client.binary,
		RuntimeRoot: filepath.Join(root, "config", "runtime"), CacheRoot: filepath.Join(root, "cache"),
		ConfigRoot: filepath.Join(root, "config"), ReleaseRoot: filepath.Join(root, "release"),
		Scope: "production", InstallationID: r.InstallationID, Origin: r.Origin, PhoneAppOrigin: r.Origin,
		HTTPSPort: r.HTTPSPort, BackendPort: r.BackendPort, PluginPort: 18378,
	}, "01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInstalledResumeSessionBindingAndRetirement(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "installed-workflow")
	r := fixtureRequest(true)
	r.Scope, r.Consent.Scope = "production", "production"
	if err := m.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var retained *InstalledSession
	sentinel := errors.New("construction failed")
	binding := installedFixtureBinding(t, m, r)
	if err := m.WithInstalledSession(context.Background(), InstalledBinding{}, func(context.Context, *InstalledSession) error { t.Fatal("zero binding entered"); return nil }); err == nil {
		t.Fatal("zero binding accepted")
	}
	err := m.WithInstalledSession(context.Background(), binding, func(ctx context.Context, s *InstalledSession) error {
		retained = s
		if s.ValidateRuntimeBinding(InstalledBinding{}) == nil {
			t.Fatal("zero binding accepted")
		}
		for _, mutate := range []func(*InstalledBindingInput){
			func(in *InstalledBindingInput) { in.Binary += "-other" },
			func(in *InstalledBindingInput) { in.RuntimeRoot += "-other" },
			func(in *InstalledBindingInput) { in.CacheRoot += "-other" },
			func(in *InstalledBindingInput) { in.PluginPort++ },
			func(in *InstalledBindingInput) { in.Origin = "https://other.example.invalid" },
		} {
			changed := binding
			mutate(&changed.input)
			if s.ValidateRuntimeBinding(changed) == nil {
				t.Fatal("changed binding accepted")
			}
		}
		validate := func(root, scope string) error {
			changed := binding
			changed.input.StateRoot, changed.input.Scope = root, scope
			return s.ValidateRuntimeBinding(changed)
		}
		if err := validate(m.stateRoot, "production"); err != nil {
			t.Fatal(err)
		}
		if validate(m.stateRoot, "development") == nil || validate(m.stateRoot+"-other", "production") == nil {
			t.Fatal("mismatch accepted")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if retained.ValidateRuntimeBinding(binding) == nil || retained.PrepareRuntime(binding) == nil {
		t.Fatal("retired binding admitted resources")
	}
	if _, err := retained.VerifyRegisteredRoute(context.Background(), "production", r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort); !errors.Is(err, ErrWorkflowRequired) {
		t.Fatalf("retained authority: %v", err)
	}
	if err := m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func(context.Context, func() (RouteStatus, error)) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

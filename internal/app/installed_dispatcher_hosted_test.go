//go:build herdr_installed_inert_test && herdr_installed_hosted_test

package app

import "testing"

func TestInstalledHostedDispatcherRunStartupFailure(t *testing.T) {
	requireInstalledHosted(t)
	installedHostedLifecycle(t, true, false, func(s *Server) {
		if s.dispatcher == nil || !s.dispatcher.InstalledDrainComplete() {
			t.Fatal("Run returned without joining its installed dispatcher")
		}
	})
}

func TestInstalledHostedDispatcherRunNormalStop(t *testing.T) {
	requireInstalledHosted(t)
	installedHostedLifecycle(t, false, false, func(s *Server) {
		if s.dispatcher == nil || !s.dispatcher.InstalledDrainComplete() {
			t.Fatal("Run returned without joining its installed dispatcher")
		}
	})
}

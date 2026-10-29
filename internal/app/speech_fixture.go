//go:build herdr_installed_inert_test

package app

import "github.com/0cv/herdr-mobile-relay/internal/speech"

// Test-compiled library dependencies cover constructor and refresh callbacks,
// including when app is imported by command tests. Never discover host voices.
func serverSpeechLanguages() []string    { return nil }
func serverSpeechStatus() speech.Catalog { return speech.Catalog{} }

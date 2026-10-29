//go:build !herdr_installed_inert_test

package app

import "github.com/0cv/herdr-mobile-relay/internal/speech"

func serverSpeechLanguages() []string    { return speech.Languages() }
func serverSpeechStatus() speech.Catalog { return speech.Status() }

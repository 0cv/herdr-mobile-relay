package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyPublicRedactsInvalidOriginsFromCLIStderr(t *testing.T) {
	for _, origin := range []string{
		"https://example.test:bad?token=synthetic-query-marker",
		"https://synthetic-user-marker:synthetic-password-marker@example.test:bad?token=synthetic-query-marker",
		"https://example.test/%ZZ?token=synthetic-query-marker",
		"https://synthetic-user-marker:synthetic-password-marker@example.test?token=synthetic-query-marker",
	} {
		root := t.TempDir()
		args := []string{"verify-public", "--web-root", filepath.Join(root, "unavailable"), "--origin", origin}
		code, verifyErr := run(args)
		if code != 1 || verifyErr == nil || strings.Contains(verifyErr.Error(), "synthetic-") {
			t.Fatalf("run() invalid-origin result = %d, %v", code, verifyErr)
		}
		stderr, err := os.CreateTemp(root, "stderr")
		if err != nil {
			t.Fatal(err)
		}
		reportError(stderr, args, verifyErr)
		if err := stderr.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(stderr.Name())
		if err != nil {
			t.Fatal(err)
		}
		const expected = "herdr-mobile-relay: public app origin must be a valid HTTPS origin without credentials, a path, query, or fragment\n"
		if string(data) != expected || strings.Contains(string(data), "synthetic-") {
			t.Fatal("CLI stderr did not contain only the sanitized invalid-origin diagnostic")
		}
	}
}

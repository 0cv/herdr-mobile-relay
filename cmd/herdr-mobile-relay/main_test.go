package main

import (
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestVerifyReleaseIdentity(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = "1.2.3", "candidate-revision"
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	manifest := release.Manifest{
		Version:  "1.2.3",
		Revision: "candidate-revision",
	}
	if err := verifyReleaseIdentity(manifest, "1.2.3", "candidate-revision"); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}

	tests := []struct {
		name             string
		manifest         release.Manifest
		expectedVersion  string
		expectedRevision string
		errorPart        string
	}{
		{
			name:             "workflow version",
			manifest:         manifest,
			expectedVersion:  "1.2.4",
			expectedRevision: "candidate-revision",
			errorPart:        "expected version",
		},
		{
			name:             "workflow revision",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "other-revision",
			errorPart:        "expected revision",
		},
		{
			name: "binary version",
			manifest: release.Manifest{
				Version:  "1.2.4",
				Revision: "candidate-revision",
			},
			errorPart: "binary version",
		},
		{
			name: "binary revision",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "other-revision",
			},
			errorPart: "binary revision",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyReleaseIdentity(test.manifest, test.expectedVersion, test.expectedRevision)
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("identity error = %v, want %q", err, test.errorPart)
			}
		})
	}
}

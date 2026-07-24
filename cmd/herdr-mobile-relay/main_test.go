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
		Target:   release.CurrentTarget(),
	}
	if err := verifyReleaseIdentity(manifest, "1.2.3", "candidate-revision", release.CurrentTarget(), false); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}

	tests := []struct {
		name             string
		manifest         release.Manifest
		expectedVersion  string
		expectedRevision string
		expectedTarget   string
		allowCrossTarget bool
		errorPart        string
	}{
		{
			name:             "workflow version",
			manifest:         manifest,
			expectedVersion:  "1.2.4",
			expectedRevision: "candidate-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected version",
		},
		{
			name:             "workflow revision",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "other-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected revision",
		},
		{
			name:             "workflow target",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "candidate-revision",
			expectedTarget:   "other/target",
			errorPart:        "expected target",
		},
		{
			name: "binary version",
			manifest: release.Manifest{
				Version:  "1.2.4",
				Revision: "candidate-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary version",
		},
		{
			name: "binary revision",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "other-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary revision",
		},
		{
			name: "binary target",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "candidate-revision",
				Target:   "other/target",
			},
			expectedTarget: "other/target",
			errorPart:      "binary target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyReleaseIdentity(
				test.manifest,
				test.expectedVersion,
				test.expectedRevision,
				test.expectedTarget,
				test.allowCrossTarget,
			)
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("identity error = %v, want %q", err, test.errorPart)
			}
		})
	}

	crossTarget := manifest
	crossTarget.Target = "other/target"
	if err := verifyReleaseIdentity(crossTarget, "", "", "other/target", true); err != nil {
		t.Fatalf("cross-target build-host verification rejected: %v", err)
	}
}

func TestVerifyReleaseRejectsCrossTargetCandidateMode(t *testing.T) {
	for _, candidateFlag := range []string{"--version", "--revision"} {
		t.Run(candidateFlag, func(t *testing.T) {
			code, err := run([]string{"verify-release", "--allow-cross-target", candidateFlag, "candidate"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("run() = (%d, %v), want usage error", code, err)
			}
		})
	}
}

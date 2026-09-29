package tailscalecli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const developmentRootMarker = ".herdr-dev-tailscale-cli"

// NewDevelopmentManager binds real development-scope operations to the
// launcher's marked, private root. NewManager remains available for other
// scopes and fixture-only package tests; an unbound manager cannot use the real
// development scope.
func NewDevelopmentManager(developmentRoot, stateRoot, coordinationRoot string, client *Client) (*Manager, error) {
	root, err := validatePrivateDirectory(developmentRoot)
	if err != nil || filepath.Clean(developmentRoot) != root {
		return nil, ErrPermissionDenied
	}
	state, err := validatePrivateDirectory(stateRoot)
	if err != nil || filepath.Clean(stateRoot) != state || state != filepath.Join(root, "registration") {
		return nil, ErrPermissionDenied
	}
	coordination, err := validatePrivateDirectory(coordinationRoot)
	if err != nil || filepath.Clean(coordinationRoot) != coordination || pathsOverlap(root, coordination) {
		return nil, ErrPermissionDenied
	}
	if err := validateDevelopmentRootMarker(root, state, coordination); err != nil {
		return nil, err
	}
	manager, err := NewManager(state, coordination, client)
	if err != nil {
		return nil, err
	}
	manager.developmentRoot = root
	return manager, nil
}

func validateDevelopmentRootMarker(root, state, coordination string) error {
	path := filepath.Join(root, developmentRootMarker)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) {
		return ErrPermissionDenied
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrPermissionDenied
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return ErrPermissionDenied
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 4096 {
		return ErrPermissionDenied
	}
	expected := []byte(fmt.Sprintf(
		"HERDR_DEV_TAILSCALE_CLI_ROOT=1\nHERDR_DEV_TAILSCALE_CLI_STATE_ROOT=%s\nHERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=%s\n",
		state, coordination,
	))
	if !bytes.Equal(data, expected) {
		return ErrPermissionDenied
	}
	return nil
}

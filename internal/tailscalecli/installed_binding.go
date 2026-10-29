package tailscalecli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

// InstalledBindingInput is configuration data, never live route authority.
// Token bytes are checked at construction and are not retained.
type InstalledBindingInput struct {
	StateRoot, CoordinationRoot, Binary, RuntimeRoot, CacheRoot, ConfigRoot, ReleaseRoot string
	Scope, InstallationID, Origin, PhoneAppOrigin                                        string
	HTTPSPort, BackendPort, PluginPort                                                   int
}

// InstalledBinding is an immutable validated configuration snapshot. It does not
// qualify a profile or permit mutations. The zero value is invalid.
type InstalledBinding struct {
	input InstalledBindingInput
	roots [6]string
}

func (b InstalledBinding) String() string { return "installed configuration binding (redacted)" }

func NewInstalledBinding(in InstalledBindingInput, token string) (InstalledBinding, error) {
	if len(token) != 32 {
		return InstalledBinding{}, ErrWorkflowRequired
	}
	roots, err := installedRoots(in)
	if err != nil {
		return InstalledBinding{}, err
	}
	if err := validateInstalledBinding(in, roots); err != nil {
		return InstalledBinding{}, err
	}
	return InstalledBinding{input: in, roots: roots}, nil
}

// Fingerprint pins the original input and all six canonical roots. It is data,
// not ownership or runtime qualification. Validate refuses alias drift first.
func (b InstalledBinding) Fingerprint() ([32]byte, error) {
	if err := b.Validate(); err != nil {
		return [32]byte{}, err
	}
	data, err := json.Marshal(struct {
		Input InstalledBindingInput
		Roots [6]string
	}{b.input, b.roots})
	if err != nil {
		return [32]byte{}, ErrWorkflowRequired
	}
	return sha256.Sum256(data), nil
}

func (b InstalledBinding) Validate() error {
	roots, err := installedRoots(b.input)
	if err != nil {
		return err
	}
	if err := validateInstalledBinding(b.input, roots); err != nil {
		return err
	}
	if roots != b.roots {
		return ErrConflict
	}
	return nil
}

func installedRoots(in InstalledBindingInput) ([6]string, error) {
	var result [6]string
	for i, root := range []string{in.StateRoot, in.CoordinationRoot, in.RuntimeRoot, in.CacheRoot, in.ConfigRoot, in.ReleaseRoot} {
		resolved, err := installedRoot(root, i < 4)
		if err != nil {
			return result, err
		}
		result[i] = resolved
	}
	return result, nil
}

func validateInstalledBinding(in InstalledBindingInput, canonical [6]string) error {
	if in.Scope != "production" || in.InstallationID == "" || len(in.InstallationID) > 128 || !filepath.IsAbs(in.Binary) || filepath.Clean(in.Binary) != in.Binary || strings.ContainsAny(in.Binary, "\x00\r\n") {
		return ErrWorkflowRequired
	}
	for _, c := range in.InstallationID {
		if c < 0x21 || c > 0x7e || c == '/' || c == '\\' {
			return ErrWorkflowRequired
		}
	}
	for _, origin := range []string{in.Origin, in.PhoneAppOrigin} {
		normalized, err := setuphelper.NormalizeExternalHTTPSOrigin(origin)
		if err != nil || normalized != origin {
			return ErrWorkflowRequired
		}
	}
	u, err := url.Parse(in.Origin)
	if err != nil {
		return ErrWorkflowRequired
	}
	port := 443
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return ErrWorkflowRequired
		}
	}
	if port != in.HTTPSPort {
		return ErrWorkflowRequired
	}
	for _, p := range []int{in.HTTPSPort, in.BackendPort, in.PluginPort} {
		if p < 1 || p > 65535 {
			return ErrWorkflowRequired
		}
	}
	if in.HTTPSPort == in.BackendPort || in.HTTPSPort == in.PluginPort || in.BackendPort == in.PluginPort {
		return ErrWorkflowRequired
	}
	if pathsOverlap(canonical[0], canonical[1]) {
		return ErrConflict
	}
	for _, retained := range canonical[:2] {
		for _, excluded := range canonical[2:] {
			if pathsOverlap(retained, excluded) {
				return ErrConflict
			}
		}
	}
	// Runtime inside ConfigHome is normal. Cache/release ownership is not.
	if pathsOverlap(canonical[2], canonical[3]) || pathsOverlap(canonical[2], canonical[5]) || pathsOverlap(canonical[3], canonical[5]) {
		return ErrConflict
	}
	return nil
}

// Resolve aliases before comparing roots. Missing leaves inherit no assurance:
// their nearest existing ancestor must be owned and not writable by others.
// Existing non-sensitive parents need not be 0700. No creation or repair occurs.
// This has the same cooperative filesystem-race limits as manager root checks.
func installedRoot(path string, sensitive bool) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return "", ErrPermissionDenied
	}
	ancestor := path
	var suffix []string
	for {
		info, err := os.Lstat(ancestor)
		if errors.Is(err, os.ErrNotExist) {
			suffix = append(suffix, filepath.Base(ancestor))
			ancestor = filepath.Dir(ancestor)
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", ErrPermissionDenied
		}
		if sensitive && len(suffix) == 0 {
			if _, err := validatePrivateDirectory(ancestor); err != nil {
				return "", err
			}
		} else if !ownedByCurrentUser(info) || info.Mode().Perm()&0022 != 0 {
			return "", ErrPermissionDenied
		}
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err != nil {
			return "", ErrPermissionDenied
		}
		for i := len(suffix) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, suffix[i])
		}
		return resolved, nil
	}
}

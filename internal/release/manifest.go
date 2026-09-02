package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	relayprotocol "github.com/0cv/herdr-mobile-relay/internal/protocol"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	ManifestName   = "release-manifest.json"
	ManifestSchema = 1
)

type Manifest struct {
	Schema          int               `json:"schema"`
	Version         string            `json:"version"`
	Revision        string            `json:"revision"`
	Target          string            `json:"target"`
	WebHash         string            `json:"web_hash,omitempty"`
	AppTransports   []string          `json:"app_transports,omitempty"`
	RelayTransports []string          `json:"relay_transports,omitempty"`
	Files           map[string]string `json:"files"`
}

func CurrentTarget() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func Load(root string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("read release manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse release manifest: %w", err)
	}
	if manifest.Schema != ManifestSchema {
		return Manifest{}, fmt.Errorf("unsupported release manifest schema %d", manifest.Schema)
	}
	if strings.TrimSpace(manifest.Version) == "" || strings.TrimSpace(manifest.Revision) == "" {
		return Manifest{}, errors.New("release manifest version and revision are required")
	}
	if manifest.Target == "" || len(manifest.Files) == 0 {
		return Manifest{}, errors.New("release manifest target and files are required")
	}
	return manifest, nil
}

func Verify(root, expectedTarget string) (Manifest, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := Load(root)
	if err != nil {
		return Manifest{}, err
	}
	if expectedTarget != "" && manifest.Target != expectedTarget {
		return Manifest{}, fmt.Errorf("release target %q does not match %q", manifest.Target, expectedTarget)
	}

	listed := make(map[string]bool, len(manifest.Files))
	for name, expected := range manifest.Files {
		clean, err := cleanRelative(name)
		if err != nil {
			return Manifest{}, fmt.Errorf("invalid manifest path %q: %w", name, err)
		}
		if clean != name {
			return Manifest{}, fmt.Errorf("manifest path %q is not canonical", name)
		}
		if !validSHA256(expected) {
			return Manifest{}, fmt.Errorf("invalid SHA-256 for %s", name)
		}
		actual, err := hashRegularFile(root, clean)
		if err != nil {
			return Manifest{}, fmt.Errorf("verify %s: %w", name, err)
		}
		if actual != strings.ToLower(expected) {
			return Manifest{}, fmt.Errorf("hash mismatch for %s", name)
		}
		listed[clean] = true
	}
	computedWebHash := hashFileMap(manifest.Files, "web/")
	if computedWebHash == "" || manifest.WebHash != computedWebHash {
		return Manifest{}, errors.New("release manifest web hash does not match its web files")
	}

	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release contains symlink %s", relative)
		}
		if entry.IsDir() {
			return nil
		}
		if relative == ManifestName {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("release contains non-regular file %s", relative)
		}
		if !listed[relative] {
			return fmt.Errorf("release file is not listed in manifest: %s", relative)
		}
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	for _, required := range []string{
		"herdr-mobile-relay",
		"web/index.html",
		"LICENSE",
		"README.md",
		"relay/common.sh",
		"relay/herdr-mobile-relay-service.sh",
		"relay/plugin-on-event.sh",
		"relay/setup-link.sh",
		"relay/stable-setup.sh",
		"relay/stable-teardown.sh",
		"relay/start.sh",
	} {
		if !listed[required] {
			return Manifest{}, fmt.Errorf("release manifest is missing %s", required)
		}
	}
	binary, err := os.Stat(filepath.Join(root, "herdr-mobile-relay"))
	if err != nil || binary.Mode()&0o111 == 0 {
		return Manifest{}, errors.New("release relay binary is not executable")
	}
	return manifest, nil
}

// Seal removes write permission from a verified release tree. Runtime state
// belongs outside releases, and keeping installed bundles immutable prevents
// platform metadata such as Finder's .DS_Store from invalidating the manifest.
func Seal(root string) error {
	if _, err := Verify(root, ""); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if err := os.Chmod(filename, mode); err != nil {
			return fmt.Errorf("seal %s: %w", filename, err)
		}
		return nil
	})
}

func Build(root, version, revision, target string) (Manifest, error) {
	if strings.TrimSpace(version) == "" || strings.TrimSpace(revision) == "" || strings.TrimSpace(target) == "" {
		return Manifest{}, errors.New("version, revision, and target are required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Manifest{}, err
	}
	files := make(map[string]string)
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release contains symlink %s", relative)
		}
		if entry.IsDir() || relative == ManifestName {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("release contains non-regular file %s", relative)
		}
		hash, err := hashRegularFile(root, relative)
		if err != nil {
			return err
		}
		files[relative] = hash
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	// This release supports both encrypted WebSocket and hybrid v2 paths. It
	// intentionally does not claim compatibility with the retired E2EE v1.
	manifest := Manifest{
		Schema:   ManifestSchema,
		Version:  version,
		Revision: revision,
		Target:   target,
		AppTransports: []string{
			relayprotocol.EncryptedWebSocketSubprotocol,
			relayprotocol.HybridTransportCapability,
		},
		RelayTransports: []string{
			relayprotocol.EncryptedWebSocketSubprotocol,
			relayprotocol.HybridTransportCapability,
		},
		Files: files,
	}
	manifest.WebHash = hashFileMap(files, "web/")
	if err := writeManifest(root, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func writeManifest(root string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	destination := filepath.Join(root, ManifestName)
	temp, err := os.CreateTemp(root, "."+ManifestName+".")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, destination)
}

func cleanRelative(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\\') || path.IsAbs(name) {
		return "", errors.New("path must be a non-empty slash-separated relative path")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes release root")
	}
	return clean, nil
}

func hashRegularFile(root, name string) (string, error) {
	filename := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(filename)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func hashFileMap(files map[string]string, prefix string) string {
	keys := make([]string, 0)
	for name := range files {
		if strings.HasPrefix(name, prefix) {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	hasher := sha256.New()
	for _, name := range keys {
		fmt.Fprintf(hasher, "%s\x00%s\n", name, files[name])
	}
	if len(keys) == 0 {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func WebHashFS(filesystem fs.FS) (string, error) {
	files := make(map[string]string)
	err := fs.WalkDir(filesystem, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("web bundle contains non-regular file %s", name)
		}
		file, err := filesystem.Open(name)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		files["web/"+path.Clean(name)] = hex.EncodeToString(hasher.Sum(nil))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hashFileMap(files, "web/"), nil
}

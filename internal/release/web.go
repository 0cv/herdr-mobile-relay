package release

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing/fstest"
)

const WebDescriptorSchema = 1

var ErrWebDescriptorMissing = errors.New("web release descriptor is missing")

// WebDescriptor is the small, public identity document emitted with every
// frontend bundle. It deliberately describes the entry and executable/style
// bytes rather than treating version.json as proof that those bytes arrived.
type WebDescriptor struct {
	Schema  int                          `json:"schema"`
	Version string                       `json:"version"`
	Assets  int                          `json:"assets"`
	Build   string                       `json:"build"`
	Entry   string                       `json:"entry"`
	Files   map[string]WebDescriptorFile `json:"files"`
}

type WebDescriptorFile struct {
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	Integrity       string `json:"integrity"`
	BrotliSHA256    string `json:"brotli_sha256,omitempty"`
	BrotliIntegrity string `json:"brotli_integrity,omitempty"`
}

var (
	webBuildEntryPattern = regexp.MustCompile(`^builds/[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-[a-f0-9]{16,64}/index\.html$`)
	webScriptPathPattern = regexp.MustCompile(`^assets/app-[a-f0-9]{64}\.js$`)
	webStylePathPattern  = regexp.MustCompile(`^assets/app-[a-f0-9]{64}\.css$`)
	webVersionPattern    = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$`)
	webScriptTagPattern  = regexp.MustCompile(`(?is)<script\b[^>]*>`)
	webStyleTagPattern   = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	webAttributePattern  = regexp.MustCompile(`(?i)([a-zA-Z_:][a-zA-Z0-9_.:-]*)\s*=\s*["']([^"']*)["']`)
)

func LoadWebDescriptor(filesystem fs.FS) (WebDescriptor, error) {
	data, err := fs.ReadFile(filesystem, "release.json")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return WebDescriptor{}, ErrWebDescriptorMissing
		}
		return WebDescriptor{}, fmt.Errorf("read web release descriptor: %w", err)
	}
	var descriptor WebDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return WebDescriptor{}, fmt.Errorf("parse web release descriptor: %w", err)
	}
	return descriptor, nil
}

// VerifyWebDescriptorData verifies a descriptor and the exact bytes fetched
// for its entry, JavaScript, and stylesheet. It is used by the deployment
// worker after fetching an untrusted public origin; keeping the same verifier
// for local and public files prevents the two paths from accepting different
// release contracts.
func VerifyWebDescriptorData(descriptorData []byte, files map[string][]byte, expectedVersion string) (WebDescriptor, error) {
	mapped := fstest.MapFS{
		"release.json": &fstest.MapFile{Data: descriptorData},
	}
	for name, data := range files {
		if _, err := webDescriptorPath(name); err != nil {
			return WebDescriptor{}, fmt.Errorf("map web descriptor file %q: %w", name, err)
		}
		mapped[name] = &fstest.MapFile{Data: data}
	}
	return VerifyWebDescriptor(mapped, expectedVersion)
}

func VerifyWebDescriptor(filesystem fs.FS, expectedVersion string) (WebDescriptor, error) {
	descriptor, err := LoadWebDescriptor(filesystem)
	if err != nil {
		return WebDescriptor{}, err
	}
	if descriptor.Schema != WebDescriptorSchema {
		return WebDescriptor{}, fmt.Errorf("unsupported web release descriptor schema %d", descriptor.Schema)
	}
	if !webVersionPattern.MatchString(descriptor.Version) {
		return WebDescriptor{}, errors.New("web release descriptor version is invalid")
	}
	if expectedVersion != "" && descriptor.Version != expectedVersion {
		return WebDescriptor{}, fmt.Errorf("web release version %q does not match %q", descriptor.Version, expectedVersion)
	}
	if !validWebBuildID(descriptor.Build) || descriptor.Assets < 0 {
		return WebDescriptor{}, errors.New("web release descriptor identity is invalid")
	}
	entryPath, err := webDescriptorEntryPath(descriptor.Entry)
	if err != nil {
		return WebDescriptor{}, err
	}
	if descriptor.Files == nil || len(descriptor.Files) != 3 {
		return WebDescriptor{}, errors.New("web release descriptor must contain exactly entry, javascript, and stylesheet files")
	}
	for _, name := range []string{"entry", "javascript", "stylesheet"} {
		file, ok := descriptor.Files[name]
		if !ok {
			return WebDescriptor{}, fmt.Errorf("web release descriptor is missing %s", name)
		}
		if err := validateWebDescriptorFilePath(name, file.Path, entryPath); err != nil {
			return WebDescriptor{}, err
		}
		if err := verifyWebDescriptorFile(filesystem, file); err != nil {
			return WebDescriptor{}, fmt.Errorf("verify web %s: %w", name, err)
		}
	}
	if descriptor.Files["entry"].Path != entryPath {
		return WebDescriptor{}, errors.New("web release entry descriptor does not match entry URL")
	}
	entry, err := readWebDescriptorFile(filesystem, descriptor.Files["entry"])
	if err != nil {
		return WebDescriptor{}, fmt.Errorf("read web entry: %w", err)
	}
	javascript := descriptor.Files["javascript"]
	stylesheet := descriptor.Files["stylesheet"]
	if !webReferenceMatches(entry, webScriptTagPattern, javascript) {
		return WebDescriptor{}, errors.New("web entry does not reference its integrity-checked JavaScript")
	}
	if !webReferenceMatches(entry, webStyleTagPattern, stylesheet) {
		return WebDescriptor{}, errors.New("web entry does not reference its integrity-checked stylesheet")
	}
	return descriptor, nil
}

func webDescriptorEntryPath(value string) (string, error) {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "?#") {
		return "", errors.New("web release entry must be a root-relative path without a query or fragment")
	}
	entryPath, err := webDescriptorPath(strings.TrimPrefix(value, "/"))
	if err != nil || !webBuildEntryPattern.MatchString(entryPath) {
		return "", errors.New("web release entry is not a build-specific same-origin path")
	}
	return entryPath, nil
}

func validateWebDescriptorFilePath(name, value, entryPath string) error {
	clean, err := webDescriptorPath(value)
	if err != nil {
		return fmt.Errorf("web %s path: %w", name, err)
	}
	if clean != value {
		return fmt.Errorf("web %s path is not canonical", name)
	}
	valid := (name == "entry" && value == entryPath) ||
		(name == "javascript" && webScriptPathPattern.MatchString(value)) ||
		(name == "stylesheet" && webStylePathPattern.MatchString(value))
	if !valid {
		return fmt.Errorf("web %s path is not a content-addressed release file", name)
	}
	return nil
}

func verifyWebDescriptorFile(filesystem fs.FS, file WebDescriptorFile) error {
	if _, err := webDescriptorPath(file.Path); err != nil {
		return err
	}
	if !validSHA256(file.SHA256) {
		return errors.New("file SHA-256 is invalid")
	}
	expectedIntegrity := integrityForSHA256(file.SHA256)
	if file.Integrity != expectedIntegrity {
		return errors.New("file integrity does not match its SHA-256")
	}
	if file.BrotliSHA256 != "" || file.BrotliIntegrity != "" {
		if !validSHA256(file.BrotliSHA256) {
			return errors.New("Brotli SHA-256 is invalid")
		}
		if file.BrotliIntegrity != integrityForSHA256(file.BrotliSHA256) {
			return errors.New("Brotli integrity does not match its SHA-256")
		}
	}
	data, err := readWebDescriptorFile(filesystem, file)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != file.SHA256 {
		return errors.New("file digest does not match its descriptor")
	}
	return nil
}

func readWebDescriptorFile(filesystem fs.FS, file WebDescriptorFile) ([]byte, error) {
	name, err := webDescriptorPath(file.Path)
	if err != nil {
		return nil, err
	}
	opened, err := filesystem.Open(name)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	const maxWebDescriptorFileBytes = 32 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(opened, maxWebDescriptorFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWebDescriptorFileBytes {
		return nil, errors.New("file exceeds descriptor verification limit")
	}
	return data, nil
}

func webDescriptorPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") {
		return "", errors.New("web descriptor path must be a non-empty relative path")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", errors.New("web descriptor path is not canonical")
	}
	return clean, nil
}

func webReferenceMatches(entry []byte, tagPattern *regexp.Regexp, file WebDescriptorFile) bool {
	wantPath := "/" + file.Path
	for _, tag := range tagPattern.FindAll(entry, -1) {
		attributes := map[string]string{}
		for _, match := range webAttributePattern.FindAllSubmatch(tag, -1) {
			if len(match) == 3 {
				attributes[strings.ToLower(string(match[1]))] = string(match[2])
			}
		}
		if attributes["src"] == wantPath || attributes["href"] == wantPath {
			if attributes["integrity"] == file.Integrity {
				return true
			}
		}
	}
	return false
}

func validWebBuildID(value string) bool {
	return validSHA256(value)
}

func integrityForSHA256(value string) string {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return ""
	}
	return "sha256-" + base64.StdEncoding.EncodeToString(decoded)
}

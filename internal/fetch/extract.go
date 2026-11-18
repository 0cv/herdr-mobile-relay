package fetch

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxArchiveEntries = 4096
	maxExtractedBytes = 256 * 1024 * 1024
)

// ExtractTarGz unpacks a gzipped tar archive beneath destination. It refuses
// paths that leave destination, caps the entry count and the unpacked size, and
// accepts only directories, regular files and, when allowSymlinks is set,
// relative symlinks that stay below their own directory.
func ExtractTarGz(archive, destination string, allowSymlinks bool) error {
	name := filepath.Base(archive)
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()
	stream, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	defer stream.Close()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open extraction root: %w", err)
	}
	defer root.Close()

	reader := tar.NewReader(stream)
	var extracted int64
	for entries := 1; ; entries++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if entries > maxArchiveEntries {
			return fmt.Errorf("%s has too many entries", name)
		}
		if header.Size < 0 || header.Size > maxExtractedBytes-extracted {
			return fmt.Errorf("%s exceeds the extracted size limit", name)
		}
		extracted += header.Size

		entryPath, err := cleanPath(header.Name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if entryPath == "" {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("%s has an empty file path", name)
		}
		if err := extractEntry(root, reader, header, entryPath, allowSymlinks); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
}

func extractEntry(root *os.Root, reader io.Reader, header *tar.Header, entryPath string, allowSymlinks bool) error {
	switch header.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(entryPath, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", entryPath, err)
		}
		return nil
	case tar.TypeReg:
		return extractRegularFile(root, reader, header, entryPath)
	case tar.TypeSymlink:
		if !allowSymlinks {
			return fmt.Errorf("unsupported entry %q", header.Name)
		}
		if !safeLinkTarget(header.Linkname) {
			return fmt.Errorf("unsafe link target %q for %s", header.Linkname, entryPath)
		}
		if err := root.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
			return fmt.Errorf("create parent for %s: %w", entryPath, err)
		}
		if err := root.Symlink(filepath.FromSlash(header.Linkname), entryPath); err != nil {
			return fmt.Errorf("create link %s: %w", entryPath, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported entry %q", header.Name)
	}
}

func cleanPath(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes its destination", name)
	}
	return filepath.FromSlash(clean), nil
}

func safeLinkTarget(target string) bool {
	if target == "" || path.IsAbs(target) || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return false
	}
	for _, component := range strings.Split(filepath.ToSlash(target), "/") {
		if component == ".." {
			return false
		}
	}
	return true
}

func extractRegularFile(root *os.Root, reader io.Reader, header *tar.Header, entryPath string) error {
	if err := root.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
		return fmt.Errorf("create parent for %s: %w", entryPath, err)
	}
	mode := os.FileMode(0o644)
	if header.FileInfo().Mode()&0o111 != 0 {
		mode = 0o755
	}
	file, err := root.OpenFile(entryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", entryPath, err)
	}
	written, copyErr := io.CopyN(file, reader, header.Size)
	chmodErr := file.Chmod(mode)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write %s: %w", entryPath, copyErr)
	}
	if chmodErr != nil {
		return chmodErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != header.Size {
		return io.ErrUnexpectedEOF
	}
	return nil
}

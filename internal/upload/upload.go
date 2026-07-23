package upload

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MaxBytes   = 10 * 1024 * 1024
	MaxAgeDays = 7
)

var mimeExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
	"image/heic": ".heic",
	"image/heif": ".heif",
}

var unsafeStemChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Result struct {
	OK    bool
	Error string
	Path  string
}

func Store(uploadDir, filename, mime, data string) Result {
	if !strings.HasPrefix(mime, "image/") {
		return Result{Error: "unsupported file type"}
	}

	content, err := decodeImageData(data)
	if err != nil {
		return Result{Error: "invalid image data"}
	}

	if len(content) > MaxBytes {
		mb := MaxBytes / (1024 * 1024)
		return Result{Error: fmt.Sprintf("Image is larger than %d MB", mb)}
	}

	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return Result{Error: "cannot create upload directory"}
	}

	ext := resolveExtension(mime, filename)
	stem := safeStem(filename)
	name := fmt.Sprintf("%s-%s-%s%s",
		time.Now().Format("20060102-150405"),
		randomHex(4),
		stem,
		ext,
	)

	path := filepath.Join(uploadDir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return Result{Error: "failed to write file"}
	}

	return Result{OK: true, Path: path}
}

func Prune(uploadDir string) int {
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		return 0
	}

	cutoff := time.Now().AddDate(0, 0, -MaxAgeDays)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(uploadDir, e.Name()))
			removed++
		}
	}
	return removed
}

func decodeImageData(data string) ([]byte, error) {
	if strings.HasPrefix(data, "data:") {
		idx := strings.Index(data, ";base64,")
		if idx < 0 {
			return nil, fmt.Errorf("data URL missing ;base64 marker")
		}
		data = data[idx+len(";base64,"):]
	}
	return base64.StdEncoding.DecodeString(data)
}

func resolveExtension(mime, filename string) string {
	if ext, ok := mimeExtensions[mime]; ok {
		return ext
	}
	if ext := filepath.Ext(filename); ext != "" {
		lower := strings.ToLower(ext)
		for _, v := range mimeExtensions {
			if v == lower {
				return lower
			}
		}
	}
	return ".img"
}

func safeStem(filename string) string {
	stem := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	stem = unsafeStemChars.ReplaceAllString(stem, "-")
	if len(stem) > 60 {
		stem = stem[:60]
	}
	if stem == "" {
		stem = "image"
	}
	return stem
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

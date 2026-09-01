package speech

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishedVoices serves the pinned catalog from a local server, keeping the
// digests the code checks in charge of the outcome.
func publishedVoices(t *testing.T, corrupt map[string]bool) *httptest.Server {
	t.Helper()
	files := map[string][]byte{}
	for language, entry := range catalog {
		model := []byte("model for " + language)
		config := []byte(`{"language":"` + language + `"}`)
		files["/"+entry.path+"/"+entry.name+".onnx"] = model
		files["/"+entry.path+"/"+entry.name+".onnx.json"] = config
		if corrupt[language] {
			files["/"+entry.path+"/"+entry.name+".onnx"] = []byte("tampered")
		}
		entry.modelSHA = digestOf(model)
		entry.configSHA = digestOf(config)
		if corrupt[language] {
			// Leave the pinned digest describing the honest bytes.
			entry.modelSHA = digestOf(model)
		}
		catalog[language] = entry
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, published := files[request.URL.Path]
		if !published {
			http.NotFound(writer, request)
			return
		}
		writer.Write(body)
	}))
	t.Cleanup(server.Close)
	t.Setenv("HERDR_PIPER_VOICE_BASE_URL", server.URL)
	return server
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// publishedRuntime serves an engine archive shaped like the real one: a piper
// directory holding the executable.
func publishedRuntime(t *testing.T) {
	t.Helper()
	var archive bytes.Buffer
	compressor := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressor)
	body := []byte("#!/bin/sh\nexit 0\n")
	for _, entry := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"piper/", 0o755, nil},
		{"piper/piper", 0o755, body},
		{"piper/espeak-ng-data/phontab", 0o644, []byte("data")},
	} {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}
		if entry.body == nil {
			header.Typeflag = tar.TypeDir
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	payload := archive.Bytes()
	for target, asset := range runtimeAssets {
		asset.digest = digestOf(payload)
		runtimeAssets[target] = asset
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	t.Setenv("HERDR_PIPER_RUNTIME_BASE_URL", server.URL)
}

// restoreCatalog keeps a test's fixture digests from leaking into the next one.
func restoreCatalog(t *testing.T) {
	t.Helper()
	voices := map[string]voice{}
	for language, entry := range catalog {
		voices[language] = entry
	}
	assets := map[string]struct{ name, digest string }{}
	for target, asset := range runtimeAssets {
		assets[target] = asset
	}
	t.Cleanup(func() {
		for language, entry := range voices {
			catalog[language] = entry
		}
		for target, asset := range assets {
			runtimeAssets[target] = asset
		}
	})
}

func TestInstallCachesTheEngineAndVoiceOnce(t *testing.T) {
	restoreCatalog(t)
	binDir := t.TempDir()
	hermeticEnv(t, binDir)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	publishedVoices(t, nil)
	publishedRuntime(t)

	if got := strings.Join(missing([]string{"fr"}), ","); got != "runtime,fr" {
		t.Fatalf("missing() = %q, want \"runtime,fr\"", got)
	}
	if err := Install(context.Background(), "fr"); err != nil {
		t.Fatalf("Install(fr) error = %v", err)
	}

	engine := filepath.Join(cache, "herdr-mobile-relay", "speech", "runtime", "piper", "piper")
	if info, err := os.Stat(engine); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("cached engine = %v (%v), want an executable", info, err)
	}
	if items := missing([]string{"fr"}); len(items) != 0 {
		t.Fatalf("missing() after install = %v, want none", items)
	}
	status := Status()
	if !status.EngineInstalled {
		t.Fatal("Status() reports no engine after installing one")
	}
	french := voiceStatus(t, status, "fr")
	if !french.Installed || french.Engine != "piper" || french.Bytes <= 0 {
		t.Fatalf("French status = %+v, want an installed piper voice", french)
	}
	if english := voiceStatus(t, status, "en"); english.Installed || english.Engine != "" {
		t.Fatalf("English status = %+v, want no voice and no engine for it", english)
	}
	if got := strings.Join(status.Languages, ","); got != "fr" {
		t.Fatalf("Status().Languages = %q, want \"fr\"", got)
	}

	// A second install is a no-op: this is what keeps a relay update from
	// downloading the voices again.
	before, err := os.Stat(filepath.Join(cache, "herdr-mobile-relay", "speech", "voices", "fr_FR-siwis-medium.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), "fr"); err != nil {
		t.Fatalf("second Install(fr) error = %v", err)
	}
	after, err := os.Stat(filepath.Join(cache, "herdr-mobile-relay", "speech", "voices", "fr_FR-siwis-medium.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("Install() re-downloaded a cached voice")
	}

	if err := Remove("fr"); err != nil {
		t.Fatalf("Remove(fr) error = %v", err)
	}
	if items := strings.Join(missing([]string{"fr"}), ","); items != "fr" {
		t.Fatalf("missing() after remove = %q, want \"fr\"", items)
	}
	if err := Remove("fr"); err != nil {
		t.Fatalf("Remove() on an absent voice error = %v", err)
	}
}

func TestInstallRejectsTamperedBytesAndUnknownLanguages(t *testing.T) {
	restoreCatalog(t)
	binDir := t.TempDir()
	writeExecutable(t, binDir, "piper", ":")
	hermeticEnv(t, binDir)
	t.Setenv("PATH", binDir)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	publishedVoices(t, map[string]bool{"de": true})

	err := Install(context.Background(), "de")
	if err == nil || !strings.Contains(err.Error(), "published checksum") {
		t.Fatalf("Install(de) error = %v, want a checksum rejection", err)
	}
	voices := filepath.Join(cache, "herdr-mobile-relay", "speech", "voices")
	for _, leftover := range []string{"de_DE-thorsten-medium.onnx", "de_DE-thorsten-medium.onnx.part"} {
		if _, err := os.Stat(filepath.Join(voices, leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived a failed download", leftover)
		}
	}
	if err := Install(context.Background(), "ja"); !errors.Is(err, ErrUsage) {
		t.Fatalf("Install(ja) error = %v, want a usage error", err)
	}
	if err := Remove("ja"); !errors.Is(err, ErrUsage) {
		t.Fatalf("Remove(ja) error = %v, want a usage error", err)
	}
}

func TestRunReportsAndInstallsFromTheCommandLine(t *testing.T) {
	restoreCatalog(t)
	binDir := t.TempDir()
	hermeticEnv(t, binDir)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	publishedVoices(t, nil)
	publishedRuntime(t)

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"missing", "--languages", "es,zh"}, &out, &errOut); err != nil {
		t.Fatalf("missing error = %v", err)
	}
	if got := out.String(); got != "runtime\nes\nzh\n" {
		t.Fatalf("missing output = %q", got)
	}

	out.Reset()
	if err := Run(context.Background(), []string{"install", "--languages", "es"}, &out, &errOut); err != nil {
		t.Fatalf("install error = %v", err)
	}
	if !strings.Contains(out.String(), "Downloading the es voice") || !strings.Contains(out.String(), "cached in") {
		t.Fatalf("install output = %q", out.String())
	}

	out.Reset()
	if err := Run(context.Background(), []string{"install", "--languages", "es"}, &out, &errOut); err != nil {
		t.Fatalf("second install error = %v", err)
	}
	if !strings.Contains(out.String(), "already cached") {
		t.Fatalf("second install output = %q", out.String())
	}

	out.Reset()
	if err := Run(context.Background(), []string{"list"}, &out, &errOut); err != nil {
		t.Fatalf("list error = %v", err)
	}
	if !strings.Contains(out.String(), "es es_ES-davefx-medium (cached") ||
		!strings.Contains(out.String(), "spoken by piper") ||
		!strings.Contains(out.String(), "fr fr_FR-siwis-medium (not downloaded") {
		t.Fatalf("list output = %q", out.String())
	}

	out.Reset()
	if err := Run(context.Background(), []string{"remove", "--languages", "es"}, &out, &errOut); err != nil {
		t.Fatalf("remove error = %v", err)
	}
	if !strings.Contains(out.String(), "Removed the es voice") {
		t.Fatalf("remove output = %q", out.String())
	}

	for _, args := range [][]string{{}, {"list", "--languages", "tlh"}, {"explode"}, {"list", "extra"}} {
		if err := Run(context.Background(), args, &out, &errOut); !errors.Is(err, ErrUsage) {
			t.Fatalf("Run(%v) error = %v, want a usage error", args, err)
		}
	}
}

func voiceStatus(t *testing.T, status Catalog, language string) VoiceStatus {
	t.Helper()
	for _, current := range status.Voices {
		if current.Language == language {
			return current
		}
	}
	t.Fatalf("Status() has no entry for %s", language)
	return VoiceStatus{}
}

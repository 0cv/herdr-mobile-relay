package release

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func webTestDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func webTestIntegrity(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256-" + base64.StdEncoding.EncodeToString(digest[:])
}

func webDescriptorFixture(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	javascriptPath := "assets/app-" + webTestDigest([]byte("console.log('release');")) + ".js"
	stylesheetPath := "assets/app-" + webTestDigest([]byte("body{color:red}")) + ".css"
	entryPath := "builds/0.20.10-363-0123456789abcdef/index.html"
	javascript := []byte("console.log('release');")
	stylesheet := []byte("body{color:red}")
	entry := []byte(`<link rel="stylesheet" href="/` + stylesheetPath + `" integrity="` + webTestIntegrity(stylesheet) + `"><script src="/` + javascriptPath + `" integrity="` + webTestIntegrity(javascript) + `"></script>`)
	files := map[string][]byte{
		entryPath:      entry,
		javascriptPath: javascript,
		stylesheetPath: stylesheet,
	}
	descriptor := WebDescriptor{
		Schema:  WebDescriptorSchema,
		Version: "0.20.10",
		Assets:  363,
		Build:   strings.Repeat("a", 64),
		Entry:   "/" + entryPath,
		Files: map[string]WebDescriptorFile{
			"entry":      {Path: entryPath, SHA256: webTestDigest(entry), Integrity: webTestIntegrity(entry)},
			"javascript": {Path: javascriptPath, SHA256: webTestDigest(javascript), Integrity: webTestIntegrity(javascript)},
			"stylesheet": {Path: stylesheetPath, SHA256: webTestDigest(stylesheet), Integrity: webTestIntegrity(stylesheet)},
		},
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return data, files
}

func TestVerifyWebDescriptorData(t *testing.T) {
	descriptor, files := webDescriptorFixture(t)
	verified, err := VerifyWebDescriptorData(descriptor, files, "0.20.10")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Entry == "" || verified.Files["javascript"].Integrity == "" {
		t.Fatalf("verified descriptor = %#v", verified)
	}
}

func TestVerifyWebDescriptorRejectsStaleOrMixedFiles(t *testing.T) {
	descriptor, files := webDescriptorFixture(t)
	files["assets/app-"+webTestDigest([]byte("console.log('release');"))+".js"] = []byte("console.log('stale');")
	if _, err := VerifyWebDescriptorData(descriptor, files, "0.20.10"); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("stale JavaScript error = %v", err)
	}

	descriptor, files = webDescriptorFixture(t)
	var parsed WebDescriptor
	if err := json.Unmarshal(descriptor, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.Files["javascript"] = WebDescriptorFile{
		Path:      "assets/app-" + strings.Repeat("b", 64) + ".js",
		SHA256:    webTestDigest([]byte("other")),
		Integrity: webTestIntegrity([]byte("other")),
	}
	files[parsed.Files["javascript"].Path] = []byte("other")
	mixed, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWebDescriptorData(mixed, files, "0.20.10"); err == nil || !strings.Contains(err.Error(), "JavaScript") {
		t.Fatalf("mixed asset error = %v", err)
	}
}

func TestVerifyWebDescriptorRejectsUnsafeEntry(t *testing.T) {
	descriptor, files := webDescriptorFixture(t)
	var parsed WebDescriptor
	if err := json.Unmarshal(descriptor, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.Entry = "/builds/../index.html"
	invalid, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWebDescriptorData(invalid, files, "0.20.10"); err == nil {
		t.Fatal("unsafe entry was accepted")
	}
}

func TestVerifyWebDescriptorValidatesBrotliMetadata(t *testing.T) {
	descriptor, files := webDescriptorFixture(t)
	var parsed WebDescriptor
	if err := json.Unmarshal(descriptor, &parsed); err != nil {
		t.Fatal(err)
	}
	file := parsed.Files["javascript"]
	file.BrotliSHA256 = strings.Repeat("c", 64)
	file.BrotliIntegrity = webTestIntegrity([]byte("wrong"))
	parsed.Files["javascript"] = file
	invalid, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyWebDescriptorData(invalid, files, "0.20.10"); err == nil || !strings.Contains(err.Error(), "Brotli integrity") {
		t.Fatalf("invalid Brotli metadata error = %v", err)
	}
}

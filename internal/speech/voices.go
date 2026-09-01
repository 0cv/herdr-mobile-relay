package speech

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrUsage marks a command line the caller got wrong, which the relay reports
// with exit status 2.
var ErrUsage = errors.New("usage")

// voice is one published Piper voice, pinned to the revision its digests were
// taken from so a tampered or truncated download is rejected.
type voice struct {
	path       string
	name       string
	modelSHA   string
	configSHA  string
	totalBytes int64
}

var catalog = map[string]voice{
	"en": {"en/en_US/lessac/medium", "en_US-lessac-medium",
		"5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f",
		"efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0", 63206179},
	"fr": {"fr/fr_FR/siwis/medium", "fr_FR-siwis-medium",
		"641d1ab097da2b81128c076810edb052b385decc8be3381814802a64a73baf99",
		"39479916c2db192b5ac9764daddd0c744d83e023ad890c6976c0633ae4df8959", 63206169},
	"de": {"de/de_DE/thorsten/medium", "de_DE-thorsten-medium",
		"7e64762d8e5118bb578f2eea6207e1a35a8e0c30595010b666f983fc87bb7819",
		"974adee790533adb273a1ac88f49027d2a1b8f0f2cf4905954a4791e79264e85", 63206113},
	"es": {"es/es_ES/davefx/medium", "es_ES-davefx-medium",
		"6658b03b1a6c316ee4c265a9896abc1393353c2d9e1bca7d66c2c442e222a917",
		"0e0dda87c732f6f38771ff274a6380d9252f327dca77aa2963d5fbdf9ec54842", 63206111},
	"zh": {"zh/zh_CN/huayan/medium", "zh_CN-huayan-medium",
		"9929917bf8cabb26fd528ea44d3a6699c11e87317a14765312420be230be0f3d",
		"d521dc45504a8ccc99e325822b35946dd701840bfb07e3dbb31a40929ed6a82b", 63206116},
}

// Apple publishes no arm64 build of the standalone engine, so Apple Silicon
// runs the Intel one through Rosetta.
var runtimeAssets = map[string]struct{ name, digest string }{
	"linux/amd64":  {"piper_linux_x86_64.tar.gz", "a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992"},
	"linux/arm64":  {"piper_linux_aarch64.tar.gz", "fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb"},
	"darwin/amd64": {"piper_macos_x64.tar.gz", "ced85c0a3df13945b1e623b878a48fdc2854d5c485b4b67f62857cf551deaf8b"},
	"darwin/arm64": {"piper_macos_x64.tar.gz", "ced85c0a3df13945b1e623b878a48fdc2854d5c485b4b67f62857cf551deaf8b"},
}

// VoiceStatus is what one language looks like on this computer.
type VoiceStatus struct {
	Language string
	Name     string
	// Installed reports the cached neural voice, not whether the language can
	// be spoken: Engine names the engine that would speak it right now.
	Installed bool
	Bytes     int64
	Engine    string
}

type Catalog struct {
	CacheDir        string
	EngineInstalled bool
	Languages       []string
	Voices          []VoiceStatus
}

func cacheDir() string {
	home, _ := os.UserHomeDir()
	return speechCache(home)
}

func voiceDir() string {
	return filepath.Join(cacheDir(), "voices")
}

func runtimeBinary() string {
	return filepath.Join(cacheDir(), "runtime", "piper", "piper")
}

func voiceBaseURL() string {
	if base := os.Getenv("HERDR_PIPER_VOICE_BASE_URL"); base != "" {
		return base
	}
	return "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0"
}

func runtimeBaseURL() string {
	if base := os.Getenv("HERDR_PIPER_RUNTIME_BASE_URL"); base != "" {
		return base
	}
	return "https://github.com/rhasspy/piper/releases/download/2023.11.14-2"
}

// Status reports every Offered language, which engine would speak it, and how
// much its voice costs to download or already occupies.
func Status() Catalog {
	installed := engines()
	status := Catalog{
		CacheDir:        cacheDir(),
		Languages:       Languages(),
		EngineInstalled: piperInstalled(installed),
	}
	for _, language := range Offered {
		entry := catalog[language]
		current := VoiceStatus{Language: language, Name: entry.name, Bytes: entry.totalBytes}
		if selected, ok := selectEngine(installed, language); ok {
			current.Engine = selected.engine.binary
		}
		model := filepath.Join(voiceDir(), entry.name+".onnx")
		if modelInfo, err := os.Stat(model); err == nil {
			if configInfo, err := os.Stat(model + ".json"); err == nil {
				current.Installed = true
				current.Bytes = modelInfo.Size() + configInfo.Size()
			}
		}
		status.Voices = append(status.Voices, current)
	}
	return status
}

func piperInstalled(candidates []engine) bool {
	for _, candidate := range candidates {
		if candidate.binary == "piper" {
			_, found := lookup(candidate)
			return found
		}
	}
	return false
}

// Install downloads one language's neural voice, and the engine itself when
// this computer does not have it yet.
func Install(ctx context.Context, language string) error {
	entry, known := catalog[language]
	if !known {
		return fmt.Errorf("%w: unknown speech language %q", ErrUsage, language)
	}
	if !piperInstalled(engines()) {
		if err := installRuntime(ctx); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(voiceDir(), 0o755); err != nil {
		return fmt.Errorf("create voice cache: %w", err)
	}
	model := filepath.Join(voiceDir(), entry.name+".onnx")
	base := voiceBaseURL() + "/" + entry.path + "/" + entry.name
	if err := download(ctx, base+".onnx", model, entry.modelSHA); err != nil {
		return err
	}
	return download(ctx, base+".onnx.json", model+".json", entry.configSHA)
}

// Remove deletes a cached voice. The language keeps working wherever a system
// engine can still speak it.
func Remove(language string) error {
	entry, known := catalog[language]
	if !known {
		return fmt.Errorf("%w: unknown speech language %q", ErrUsage, language)
	}
	model := filepath.Join(voiceDir(), entry.name+".onnx")
	for _, path := range []string{model, model + ".json"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func installRuntime(ctx context.Context) error {
	asset, published := runtimeAssets[runtime.GOOS+"/"+runtime.GOARCH]
	if !published {
		return fmt.Errorf("no speech engine is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	work, err := os.MkdirTemp("", "herdr-speech-runtime-")
	if err != nil {
		return fmt.Errorf("create download workspace: %w", err)
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, asset.name)
	if err := download(ctx, runtimeBaseURL()+"/"+asset.name, archive, asset.digest); err != nil {
		return err
	}
	if err := extractTarGz(archive, work); err != nil {
		return err
	}
	engineDir := filepath.Dir(filepath.Dir(runtimeBinary()))
	if err := os.MkdirAll(engineDir, 0o755); err != nil {
		return fmt.Errorf("create engine cache: %w", err)
	}
	replaced := filepath.Join(engineDir, "piper.replaced")
	os.RemoveAll(replaced)
	current := filepath.Join(engineDir, "piper")
	if _, err := os.Stat(current); err == nil {
		if err := os.Rename(current, replaced); err != nil {
			return fmt.Errorf("replace the installed engine: %w", err)
		}
	}
	if err := os.Rename(filepath.Join(work, "piper"), current); err != nil {
		return fmt.Errorf("install the speech engine: %w", err)
	}
	os.RemoveAll(replaced)
	return nil
}

// extractTarGz unpacks the engine archive, which holds one piper directory of
// regular files and no links.
func extractTarGz(archive, destination string) error {
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(archive), err)
	}
	defer file.Close()
	stream, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(archive), err)
	}
	defer stream.Close()
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(archive), err)
		}
		name := filepath.Clean(header.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("%s contains an unsafe path: %s", filepath.Base(archive), header.Name)
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode).Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, reader); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

// fileDigest returns the file's SHA-256, or an empty string when it cannot be
// read: a truncated cached file then simply looks like a missing one.
func fileDigest(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// download writes the verified bytes into place, so a cached file is never a
// partial or tampered one, and an already valid file is left alone.
func download(ctx context.Context, url, destination, digest string) error {
	if fileDigest(destination) == digest {
		return nil
	}
	partial := destination + ".part"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("request %s: %w", filepath.Base(destination), err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", filepath.Base(destination), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", filepath.Base(destination), response.Status)
	}
	out, err := os.Create(partial)
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(destination), err)
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, sum), response.Body); err != nil {
		out.Close()
		os.Remove(partial)
		return fmt.Errorf("download %s: %w", filepath.Base(destination), err)
	}
	if err := out.Close(); err != nil {
		os.Remove(partial)
		return fmt.Errorf("write %s: %w", filepath.Base(destination), err)
	}
	if actual := hex.EncodeToString(sum.Sum(nil)); actual != digest {
		os.Remove(partial)
		return fmt.Errorf("%s does not match its published checksum", filepath.Base(destination))
	}
	if err := os.Rename(partial, destination); err != nil {
		os.Remove(partial)
		return fmt.Errorf("install %s: %w", filepath.Base(destination), err)
	}
	return nil
}

// Run is the relay's speech-voices command, which relay setup and the
// Makefile use to cache voices from a shell.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: herdr-mobile-relay speech-voices {list|missing|install|remove} [--languages en,fr]", ErrUsage)
	}
	operation, args := args[0], args[1:]
	flags := flag.NewFlagSet("speech-voices", flag.ContinueOnError)
	flags.SetOutput(stderr)
	requested := flags.String("languages", "", "comma-separated languages")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %s", ErrUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: unexpected argument %q", ErrUsage, flags.Arg(0))
	}
	if *requested == "" && operation == "remove" {
		return fmt.Errorf("%w: remove needs --languages", ErrUsage)
	}
	// English is what a computer downloads on its own; every other language is
	// asked for, from the phone or with this flag.
	if *requested == "" {
		*requested = DefaultLanguage
	}
	languages := strings.Split(*requested, ",")
	for _, language := range languages {
		if _, known := catalog[language]; !known {
			return fmt.Errorf("%w: unknown speech language %q", ErrUsage, language)
		}
	}

	switch operation {
	case "list":
		status := Status()
		fmt.Fprintf(stdout, "Cache: %s\n", status.CacheDir)
		for _, current := range status.Voices {
			state := fmt.Sprintf("not downloaded, %d MB", current.Bytes>>20)
			if current.Installed {
				state = fmt.Sprintf("cached, %d MB", current.Bytes>>20)
			}
			engine := current.Engine
			if engine == "" {
				engine = "no engine"
			}
			fmt.Fprintf(stdout, "  %s %s (%s, spoken by %s)\n", current.Language, current.Name, state, engine)
		}
		return nil
	case "missing":
		for _, item := range missing(languages) {
			fmt.Fprintln(stdout, item)
		}
		return nil
	case "install":
		items := missing(languages)
		if len(items) == 0 {
			fmt.Fprintf(stdout, "Speech voices are already cached in %s\n", cacheDir())
			return nil
		}
		for _, item := range items {
			if item == "runtime" {
				fmt.Fprintln(stdout, "Downloading the speech engine...")
				if err := installRuntime(ctx); err != nil {
					return err
				}
				continue
			}
			entry := catalog[item]
			fmt.Fprintf(stdout, "Downloading the %s voice (%s, about %d MB)...\n", item, entry.name, entry.totalBytes>>20)
			if err := Install(ctx, item); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "Speech voices are cached in %s\n", cacheDir())
		return nil
	case "remove":
		for _, language := range languages {
			if err := Remove(language); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Removed the %s voice\n", language)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown speech-voices operation %q", ErrUsage, operation)
}

// missing names what an install would download: the engine first, since a
// voice without it cannot be spoken.
func missing(languages []string) []string {
	status := Status()
	installed := map[string]bool{}
	for _, current := range status.Voices {
		installed[current.Language] = current.Installed
	}
	var items []string
	if !status.EngineInstalled {
		items = append(items, "runtime")
	}
	for _, language := range Offered {
		for _, requested := range languages {
			if requested == language && !installed[language] {
				items = append(items, language)
				break
			}
		}
	}
	return items
}

// InstallTimeout bounds one phone-driven download; a medium voice is about
// 63 MB, which a slow connection can stretch out.
const InstallTimeout = 5 * time.Minute

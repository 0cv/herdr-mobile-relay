// Package speech synthesizes short text fragments into WAV audio with a
// text-to-speech engine installed on the relay host. The phone plays the
// result as ordinary media, which survives a locked screen where the
// browser's own speech API does not.
package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// MaxTextRunes bounds one synthesis request. The phone splits responses into
// sentence-sized pieces well under this, and the cap keeps every encrypted
// audio frame under the transport's 1 MiB payload limit.
const MaxTextRunes = 400

// maxWAVBytes keeps the base64-encoded result inside one transport frame.
// Output past the cap is decimated to a lower sample rate instead of failing.
const maxWAVBytes = 900 << 10

// Offered lists the languages the phone can ask for, in the order they appear
// in its settings.
var Offered = []string{"en", "fr", "de", "es", "zh"}

// DefaultLanguage is the one voice a computer downloads on its own; the phone
// asks for any other language it wants.
const DefaultLanguage = "en"

var labels = map[string]string{
	"en": "English",
	"fr": "French",
	"de": "German",
	"es": "Spanish",
	"zh": "Chinese",
}

// LanguageLabel names a language for the messages the phone shows.
func LanguageLabel(language string) string {
	if label, known := labels[language]; known {
		return label
	}
	return language
}

func offers(language string) bool {
	for _, candidate := range Offered {
		if candidate == language {
			return true
		}
	}
	return false
}

type engine struct {
	binary string
	// fallback paths are tried when PATH misses the binary: the relay often
	// runs as a service with a minimal PATH that excludes user installs.
	fallback []string
	// voice names the engine's voice for one language, reporting false when
	// the engine cannot speak it.
	voice func(language string) (string, bool)
	// argv builds the command line writing a WAV to outPath; text arrives on
	// stdin unless textArg is true.
	argv    func(voice, outPath string) []string
	textArg bool
}

func engines() []engine {
	// Piper's neural voices sound close to cloud TTS and synthesize many
	// times faster than realtime on a CPU; every other engine is a fallback.
	home, _ := os.UserHomeDir()
	cache := speechCache(home)
	models := piperVoices(home, cache)
	candidates := []engine{{
		binary: "piper",
		fallback: []string{
			filepath.Join(cache, "runtime", "piper", "piper"),
			filepath.Join(home, ".local", "bin", "piper"),
			"/usr/local/bin/piper",
			"/opt/piper/piper",
		},
		voice: func(language string) (string, bool) {
			model, ok := models[language]
			return model, ok
		},
		argv: func(voice, out string) []string {
			return []string{"--model", voice, "--output_file", out}
		},
	}}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, engine{
			binary: "say",
			voice: func(language string) (string, bool) {
				name, ok := sayVoices()[language]
				return name, ok
			},
			argv: func(voice, out string) []string {
				return []string{"-v", voice, "-o", out, "--file-format=WAVE", "--data-format=LEI16@22050"}
			},
		})
	}
	return append(candidates,
		engine{binary: "espeak-ng", voice: espeakVoice, argv: espeakArgv},
		engine{binary: "espeak", voice: espeakVoice, argv: espeakArgv},
		engine{
			binary:  "flite",
			voice:   func(language string) (string, bool) { return "", language == "en" },
			argv:    func(_, out string) []string { return []string{"-o", out} },
			textArg: true,
		},
	)
}

// espeakVoice maps a language to an espeak voice. Mandarin is named after the
// dialect rather than the language.
func espeakVoice(language string) (string, bool) {
	if !offers(language) {
		return "", false
	}
	if language == "zh" {
		return "cmn", true
	}
	return language, true
}

func espeakArgv(voice, out string) []string {
	return []string{"-v", voice, "-s", "175", "-w", out, "--stdin"}
}

// speechCache is where relay setup downloads the engine and its voices: a
// directory outside any release, so an update never re-downloads them. The
// setup scripts resolve the same path.
func speechCache(home string) string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "herdr-mobile-relay", "speech")
	}
	return filepath.Join(home, ".cache", "herdr-mobile-relay", "speech")
}

// piperVoices maps each language to its installed model. Voice files keep
// their published names, so es_ES-davefx-medium.onnx is the Spanish voice,
// and the model's sidecar config has to sit beside it.
func piperVoices(home, cache string) map[string]string {
	dirs := []string{}
	if configured := os.Getenv("HERDR_PIPER_VOICES"); configured != "" {
		dirs = append(dirs, configured)
	}
	dirs = append(dirs,
		filepath.Join(cache, "voices"),
		filepath.Join(home, ".local", "share", "piper-voices"),
		"/usr/local/share/piper-voices",
		"/usr/share/piper-voices",
	)
	found := map[string]string{}
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.onnx"))
		sort.Strings(matches)
		for _, model := range matches {
			language, _, _ := strings.Cut(filepath.Base(model), "_")
			if !offers(language) {
				continue
			}
			if _, taken := found[language]; taken {
				continue
			}
			if info, err := os.Stat(model + ".json"); err != nil || info.IsDir() {
				continue
			}
			found[language] = model
		}
	}
	return found
}

var (
	sayVoiceOnce  sync.Once
	sayVoiceNames map[string]string
)

func sayVoices() map[string]string {
	sayVoiceOnce.Do(func() {
		listing, err := exec.Command("say", "-v", "?").Output()
		if err != nil {
			sayVoiceNames = map[string]string{}
			return
		}
		sayVoiceNames = parseSayVoices(string(listing))
	})
	return sayVoiceNames
}

// parseSayVoices reads macOS voice listings, where a voice name is followed by
// at least two spaces, its locale, then a sample sentence.
func parseSayVoices(listing string) map[string]string {
	found := map[string]string{}
	for _, line := range strings.Split(listing, "\n") {
		name, rest, split := strings.Cut(line, "  ")
		if !split {
			continue
		}
		name = strings.TrimSpace(name)
		locale := strings.Fields(rest)
		if name == "" || len(locale) == 0 {
			continue
		}
		language, _, _ := strings.Cut(locale[0], "_")
		if !offers(language) {
			continue
		}
		if _, taken := found[language]; !taken {
			found[language] = name
		}
	}
	return found
}

func lookup(candidate engine) (string, bool) {
	if path, err := exec.LookPath(candidate.binary); err == nil {
		return path, true
	}
	for _, fallback := range candidate.fallback {
		if info, err := os.Stat(fallback); err == nil && !info.IsDir() {
			return fallback, true
		}
	}
	return "", false
}

type selection struct {
	engine engine
	binary string
	voice  string
}

func selectEngine(candidates []engine, language string) (selection, bool) {
	for _, candidate := range candidates {
		voice, speaks := candidate.voice(language)
		if !speaks {
			continue
		}
		if path, installed := lookup(candidate); installed {
			if candidate.binary == "piper" &&
				filepath.Clean(path) == filepath.Clean(runtimeBinary()) &&
				!runtimeReady(path) {
				continue
			}
			return selection{engine: candidate, binary: path, voice: voice}, true
		}
	}
	return selection{}, false
}

// Languages reports which Offered languages this host can synthesize.
func Languages() []string {
	candidates := engines()
	var available []string
	for _, language := range Offered {
		if _, ok := selectEngine(candidates, language); ok {
			available = append(available, language)
		}
	}
	return available
}

// Synthesize renders text in one language with the best engine installed and
// returns a canonical PCM16 WAV small enough for one transport frame.
func Synthesize(ctx context.Context, text, language string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errors.New("text is required")
	}
	if utf8.RuneCountInString(trimmed) > MaxTextRunes {
		return nil, fmt.Errorf("text exceeds %d characters", MaxTextRunes)
	}
	selected, ok := selectEngine(engines(), language)
	if !ok {
		return nil, fmt.Errorf("no installed engine speaks %s", language)
	}
	dir, err := os.MkdirTemp("", "herdr-speech-")
	if err != nil {
		return nil, fmt.Errorf("create synthesis workspace: %w", err)
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "speech.wav")
	args := selected.engine.argv(selected.voice, outPath)
	if selected.engine.textArg {
		args = append(args, "-t", trimmed)
	}
	cmd := exec.CommandContext(ctx, selected.binary, args...)
	if !selected.engine.textArg {
		cmd.Stdin = strings.NewReader(trimmed)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s: %s", selected.engine.binary, detail)
		}
		return nil, fmt.Errorf("%s: %w", selected.engine.binary, err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read synthesized audio: %w", err)
	}
	sampleRate, samples, err := parsePCM16Mono(raw)
	if err != nil {
		return nil, fmt.Errorf("%s output: %w", selected.engine.binary, err)
	}
	for 44+len(samples)*2 > maxWAVBytes && sampleRate > 8000 {
		samples = decimate(samples)
		sampleRate /= 2
	}
	if 44+len(samples)*2 > maxWAVBytes {
		return nil, errors.New("synthesized audio exceeds the transport frame budget")
	}
	return encodeWAV(sampleRate, samples), nil
}

// parsePCM16Mono walks the RIFF chunks; engines that stream to a pipe leave
// placeholder sizes, so the actual data length is taken from the file itself.
func parsePCM16Mono(raw []byte) (int, []int16, error) {
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return 0, nil, errors.New("not a RIFF WAVE file")
	}
	sampleRate := 0
	var data []byte
	offset := 12
	for offset+8 <= len(raw) {
		id := string(raw[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		body := offset + 8
		if size < 0 || body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return 0, nil, errors.New("truncated fmt chunk")
			}
			format := binary.LittleEndian.Uint16(raw[body : body+2])
			channels := binary.LittleEndian.Uint16(raw[body+2 : body+4])
			bits := binary.LittleEndian.Uint16(raw[body+14 : body+16])
			if format != 1 || channels != 1 || bits != 16 {
				return 0, nil, fmt.Errorf("unsupported format %d/%d channels/%d bits", format, channels, bits)
			}
			sampleRate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
		case "data":
			data = raw[body : body+size]
		}
		offset = body + size + size%2
	}
	if sampleRate == 0 || len(data) < 2 {
		return 0, nil, errors.New("missing audio data")
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	return sampleRate, samples, nil
}

func decimate(samples []int16) []int16 {
	half := make([]int16, len(samples)/2)
	for i := range half {
		half[i] = samples[i*2]
	}
	return half
}

func encodeWAV(sampleRate int, samples []int16) []byte {
	dataLen := len(samples) * 2
	out := make([]byte, 44+dataLen)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataLen))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 1)
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataLen))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[44+i*2:46+i*2], uint16(sample))
	}
	return out
}

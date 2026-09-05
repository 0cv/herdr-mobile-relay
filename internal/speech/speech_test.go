package speech

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Keep host-installed engines and voices out of fake-engine tests.
func hermeticEnv(t *testing.T, binDir string) {
	t.Helper()
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HERDR_PIPER_VOICES", t.TempDir())
}

func installVoice(t *testing.T, dir, name string, withConfig bool) string {
	t.Helper()
	model := filepath.Join(dir, name)
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withConfig {
		if err := os.WriteFile(model+".json", []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return model
}

func fakeWAV(sampleRate, sampleCount int, riffSize uint32) []byte {
	wav := encodeWAV(sampleRate, make([]int16, sampleCount))
	binary.LittleEndian.PutUint32(wav[4:8], riffSize)
	return wav
}

// fakeEngine writes a canned WAV to the path following flag, recording its
// argv so tests can prove which voice was requested.
func fakeEngine(t *testing.T, binDir, name, flag string, wav []byte) {
	t.Helper()
	source := filepath.Join(binDir, name+"-source.wav")
	if err := os.WriteFile(source, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binDir, name, `if [ "$1" = "--help" ]; then exit 0; fi
echo "$@" > `+binDir+`/`+name+`-args
while [ "$1" != "`+flag+`" ]; do shift; done
/bin/cat `+source+` > "$2"`)
}

func engineArgs(t *testing.T, binDir, name string) string {
	t.Helper()
	args, err := os.ReadFile(filepath.Join(binDir, name+"-args"))
	if err != nil {
		t.Fatal(err)
	}
	return string(args)
}

func TestSynthesizeNormalizesStreamedHeaderSizes(t *testing.T) {
	binDir := t.TempDir()
	// espeak-ng streaming to a file it cannot seek leaves placeholder RIFF
	// sizes; the parser must trust the actual byte count instead.
	fakeEngine(t, binDir, "espeak-ng", "-w", fakeWAV(22050, 1000, 0x7fffffff))
	hermeticEnv(t, binDir)

	wav, err := Synthesize(context.Background(), "hello relay", "en")
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	sampleRate, samples, err := parsePCM16Mono(wav)
	if err != nil {
		t.Fatalf("parse normalized output: %v", err)
	}
	if sampleRate != 22050 || len(samples) != 1000 {
		t.Fatalf("normalized output = %d Hz %d samples, want 22050 Hz 1000 samples", sampleRate, len(samples))
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(36+len(samples)*2) {
		t.Fatalf("RIFF size = %d, want %d", got, 36+len(samples)*2)
	}
}

func TestSynthesizeDecimatesOversizedAudio(t *testing.T) {
	binDir := t.TempDir()
	// ~1.5 MB of samples: one decimation pass lands under the frame budget.
	fakeEngine(t, binDir, "espeak-ng", "-w", fakeWAV(22050, 800_000, 0))
	hermeticEnv(t, binDir)

	wav, err := Synthesize(context.Background(), "long response", "en")
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(wav) > maxWAVBytes {
		t.Fatalf("output %d bytes exceeds frame budget %d", len(wav), maxWAVBytes)
	}
	sampleRate, samples, err := parsePCM16Mono(wav)
	if err != nil {
		t.Fatal(err)
	}
	if sampleRate != 11025 || len(samples) != 400_000 {
		t.Fatalf("decimated output = %d Hz %d samples, want 11025 Hz 400000 samples", sampleRate, len(samples))
	}
}

func TestSynthesizeRejectsBadInputAndReportsEngineFailure(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "espeak-ng", `echo "voice data missing" >&2; exit 1`)
	hermeticEnv(t, binDir)

	if _, err := Synthesize(context.Background(), "   ", "en"); err == nil {
		t.Fatal("Synthesize() accepted blank text")
	}
	if _, err := Synthesize(context.Background(), strings.Repeat("a", MaxTextRunes+1), "en"); err == nil {
		t.Fatal("Synthesize() accepted oversized text")
	}
	if _, err := Synthesize(context.Background(), "hello", "ja"); err == nil {
		t.Fatal("Synthesize() accepted a language the app never offers")
	}
	_, err := Synthesize(context.Background(), "hello", "en")
	if err == nil || !strings.Contains(err.Error(), "voice data missing") {
		t.Fatalf("Synthesize() error = %v, want engine stderr surfaced", err)
	}
}

func TestLanguagesFollowInstalledVoices(t *testing.T) {
	binDir := t.TempDir()
	voiceDir := t.TempDir()
	hermeticEnv(t, binDir)
	t.Setenv("HERDR_PIPER_VOICES", voiceDir)

	if languages := Languages(); len(languages) != 0 {
		t.Fatalf("Languages() with no engine = %v, want none", languages)
	}

	writeExecutable(t, binDir, "piper", ":")
	installVoice(t, voiceDir, "fr_FR-siwis-medium.onnx", true)
	installVoice(t, voiceDir, "zh_CN-huayan-medium.onnx", true)
	// A model without its sidecar config cannot be loaded, so the language
	// must not be advertised.
	installVoice(t, voiceDir, "de_DE-thorsten-medium.onnx", false)
	if got := strings.Join(Languages(), ","); got != "fr,zh" {
		t.Fatalf("Languages() with two piper voices = %q, want \"fr,zh\"", got)
	}

	// espeak-ng speaks every offered language, so it fills the gaps.
	writeExecutable(t, binDir, "espeak-ng", ":")
	if got := strings.Join(Languages(), ","); got != "en,fr,de,es,zh" {
		t.Fatalf("Languages() with espeak-ng = %q, want every offered language", got)
	}

	// flite reads English only.
	t.Setenv("HERDR_PIPER_VOICES", t.TempDir())
	if err := os.Remove(filepath.Join(binDir, "espeak-ng")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(binDir, "piper")); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binDir, "flite", ":")
	if got := strings.Join(Languages(), ","); got != "en" {
		t.Fatalf("Languages() with flite = %q, want \"en\"", got)
	}
}

// Relay setup downloads the engine and its voices into the cache directory,
// which survives every release update. Nothing else has to be installed.
func TestCachedSetupDownloadIsEnough(t *testing.T) {
	cache := t.TempDir()
	hermeticEnv(t, t.TempDir())
	t.Setenv("XDG_CACHE_HOME", cache)
	speechCache := filepath.Join(cache, "herdr-mobile-relay", "speech")
	voices := filepath.Join(speechCache, "voices")
	runtime := filepath.Join(speechCache, "runtime", "piper")
	if err := os.MkdirAll(voices, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	model := installVoice(t, voices, "es_ES-davefx-medium.onnx", true)
	fakeEngine(t, runtime, "piper", "--output_file", fakeWAV(22050, 500, 0))

	if got := strings.Join(Languages(), ","); got != "es" {
		t.Fatalf("Languages() from the cache = %q, want \"es\"", got)
	}
	if _, err := Synthesize(context.Background(), "hola", "es"); err != nil {
		t.Fatalf("Synthesize(es) error = %v", err)
	}
	if args := engineArgs(t, runtime, "piper"); !strings.Contains(args, "--model "+model) {
		t.Fatalf("piper args = %q, want the cached Spanish model", args)
	}
}

func TestSynthesizeRoutesEachLanguageToItsVoice(t *testing.T) {
	binDir := t.TempDir()
	voiceDir := t.TempDir()
	fakeEngine(t, binDir, "piper", "--output_file", fakeWAV(22050, 500, 0))
	fakeEngine(t, binDir, "espeak-ng", "-w", fakeWAV(22050, 500, 0))
	hermeticEnv(t, binDir)
	t.Setenv("HERDR_PIPER_VOICES", voiceDir)
	french := installVoice(t, voiceDir, "fr_FR-siwis-medium.onnx", true)

	if _, err := Synthesize(context.Background(), "bonjour", "fr"); err != nil {
		t.Fatalf("Synthesize(fr) error = %v", err)
	}
	if args := engineArgs(t, binDir, "piper"); !strings.Contains(args, "--model "+french) {
		t.Fatalf("piper args = %q, want the French model", args)
	}

	// Without a neural model the language falls through to espeak-ng, whose
	// Mandarin voice is named after the dialect.
	if _, err := Synthesize(context.Background(), "你好", "zh"); err != nil {
		t.Fatalf("Synthesize(zh) error = %v", err)
	}
	if args := engineArgs(t, binDir, "espeak-ng"); !strings.Contains(args, "-v cmn") {
		t.Fatalf("espeak-ng args = %q, want the Mandarin voice", args)
	}
	if _, err := Synthesize(context.Background(), "hola", "es"); err != nil {
		t.Fatalf("Synthesize(es) error = %v", err)
	}
	if args := engineArgs(t, binDir, "espeak-ng"); !strings.Contains(args, "-v es") {
		t.Fatalf("espeak-ng args = %q, want the Spanish voice", args)
	}
}

func TestParseSayVoicesKeepsTheFirstVoicePerLanguage(t *testing.T) {
	voices := parseSayVoices(strings.Join([]string{
		"Alva                   sv_SE    # Hej, jag heter Alva.",
		"Amelie                 fr_CA    # Bonjour, je m’appelle Amelie.",
		"Eddy (French (France)) fr_FR    # Bonjour, je m’appelle Eddy.",
		"Samantha               en_US    # Hello, my name is Samantha.",
		"malformed-line",
	}, "\n"))
	if voices["fr"] != "Amelie" {
		t.Fatalf("French voice = %q, want Amelie", voices["fr"])
	}
	if voices["en"] != "Samantha" {
		t.Fatalf("English voice = %q, want Samantha", voices["en"])
	}
	if _, offered := voices["sv"]; offered {
		t.Fatal("parseSayVoices kept a language the app never offers")
	}
}

func TestSynthesizeWithRealEngine(t *testing.T) {
	languages := Languages()
	if len(languages) == 0 {
		t.Skip("no speech engine is installed")
	}
	for _, language := range languages {
		text := map[string]string{
			"en": "The relay confirmed every change landed.",
			"fr": "Le relais a confirmé chaque modification.",
			"de": "Die Übertragung hat jede Änderung bestätigt.",
			"es": "El relé confirmó cada cambio.",
			"zh": "中继已确认每一项更改。",
		}[language]
		wav, err := Synthesize(context.Background(), text, language)
		if err != nil {
			t.Fatalf("Synthesize(%s) error = %v", language, err)
		}
		sampleRate, samples, err := parsePCM16Mono(wav)
		if err != nil {
			t.Fatalf("parse %s output: %v", language, err)
		}
		if sampleRate < 8000 || len(samples) < sampleRate/2 {
			t.Fatalf("%s output = %d Hz %d samples, want at least half a second of audio", language, sampleRate, len(samples))
		}
		if len(wav) > maxWAVBytes {
			t.Fatalf("%s output %d bytes exceeds frame budget", language, len(wav))
		}
	}
}

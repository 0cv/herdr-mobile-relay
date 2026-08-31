package speech

import (
	"bytes"
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
	t.Setenv("HERDR_PIPER_VOICE", "")
}

func fakeWAV(sampleRate, sampleCount int, riffSize uint32) []byte {
	wav := encodeWAV(sampleRate, make([]int16, sampleCount))
	binary.LittleEndian.PutUint32(wav[4:8], riffSize)
	return wav
}

func TestSynthesizeNormalizesStreamedHeaderSizes(t *testing.T) {
	binDir := t.TempDir()
	// espeak-ng streaming to a file it cannot seek leaves placeholder RIFF
	// sizes; the parser must trust the actual byte count instead.
	source := filepath.Join(binDir, "source.wav")
	if err := os.WriteFile(source, fakeWAV(22050, 1000, 0x7fffffff), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binDir, "espeak-ng", `while [ "$1" != "-w" ]; do shift; done; /bin/cat `+source+` > "$2"`)
	hermeticEnv(t, binDir)

	wav, err := Synthesize(context.Background(), "hello relay")
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
	source := filepath.Join(binDir, "source.wav")
	// ~1.5 MB of samples: one decimation pass lands under the frame budget.
	if err := os.WriteFile(source, fakeWAV(22050, 800_000, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binDir, "espeak-ng", `while [ "$1" != "-w" ]; do shift; done; /bin/cat `+source+` > "$2"`)
	hermeticEnv(t, binDir)

	wav, err := Synthesize(context.Background(), "long response")
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

	if _, err := Synthesize(context.Background(), "   "); err == nil {
		t.Fatal("Synthesize() accepted blank text")
	}
	if _, err := Synthesize(context.Background(), strings.Repeat("a", MaxTextRunes+1)); err == nil {
		t.Fatal("Synthesize() accepted oversized text")
	}
	_, err := Synthesize(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "voice data missing") {
		t.Fatalf("Synthesize() error = %v, want engine stderr surfaced", err)
	}
}

func TestDiscoverPrefersInstalledEngine(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "flite", ":")
	hermeticEnv(t, binDir)
	name, ok := Discover()
	if !ok || name != "flite" {
		t.Fatalf("Discover() = (%q, %v), want flite", name, ok)
	}
	t.Setenv("PATH", t.TempDir())
	if _, ok := Discover(); ok {
		t.Fatal("Discover() found an engine on an empty PATH")
	}
}

func TestSynthesizePrefersPiperWhenVoiceInstalled(t *testing.T) {
	binDir := t.TempDir()
	source := filepath.Join(binDir, "source.wav")
	if err := os.WriteFile(source, fakeWAV(22050, 500, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, binDir, "piper",
		`echo "$@" > `+binDir+`/piper-args; while [ "$1" != "--output_file" ]; do shift; done; /bin/cat `+source+` > "$2"`)
	writeExecutable(t, binDir, "espeak-ng", `exit 7`)
	hermeticEnv(t, binDir)
	voice := filepath.Join(binDir, "voice.onnx")
	if err := os.WriteFile(voice, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PIPER_VOICE", voice)

	if name, ok := Discover(); !ok || name != "piper" {
		t.Fatalf("Discover() = (%q, %v), want piper", name, ok)
	}
	if _, err := Synthesize(context.Background(), "neural please"); err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	args, err := os.ReadFile(filepath.Join(binDir, "piper-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--model "+voice) {
		t.Fatalf("piper args = %q, want the configured voice model", args)
	}

	// A configured voice that does not exist rules piper out entirely.
	t.Setenv("HERDR_PIPER_VOICE", filepath.Join(binDir, "missing.onnx"))
	if name, ok := Discover(); !ok || name != "espeak-ng" {
		t.Fatalf("Discover() without a voice = (%q, %v), want espeak-ng", name, ok)
	}
}

func TestSynthesizeWithRealEngine(t *testing.T) {
	if _, ok := Discover(); !ok {
		t.Skip("no speech engine is installed")
	}
	wav, err := Synthesize(context.Background(), "The relay confirmed every change landed.")
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Fatal("output is not a RIFF file")
	}
	sampleRate, samples, err := parsePCM16Mono(wav)
	if err != nil {
		t.Fatal(err)
	}
	if sampleRate < 8000 || len(samples) < sampleRate/2 {
		t.Fatalf("output = %d Hz %d samples, want at least half a second of audio", sampleRate, len(samples))
	}
	if len(wav) > maxWAVBytes {
		t.Fatalf("output %d bytes exceeds frame budget", len(wav))
	}
}

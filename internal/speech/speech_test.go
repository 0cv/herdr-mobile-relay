package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
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
	t.Setenv("PATH", binDir)

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
	t.Setenv("PATH", binDir)

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
		t.Fatalf("decimated output = %d Hz %d samples, want 11025 Hz 500000 samples", sampleRate, len(samples))
	}
}

func TestSynthesizeRejectsBadInputAndReportsEngineFailure(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "espeak-ng", `echo "voice data missing" >&2; exit 1`)
	t.Setenv("PATH", binDir)

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
	t.Setenv("PATH", binDir)
	name, ok := Discover()
	if !ok || name != "flite" {
		t.Fatalf("Discover() = (%q, %v), want flite", name, ok)
	}
	t.Setenv("PATH", t.TempDir())
	if _, ok := Discover(); ok {
		t.Fatal("Discover() found an engine on an empty PATH")
	}
}

func TestSynthesizeWithRealEngine(t *testing.T) {
	if _, err := exec.LookPath("espeak-ng"); err != nil {
		t.Skip("espeak-ng is not installed")
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
	if fmt.Sprint(len(wav)) == "" || len(wav) > maxWAVBytes {
		t.Fatalf("output %d bytes exceeds frame budget", len(wav))
	}
}

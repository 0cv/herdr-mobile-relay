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
	"unicode/utf8"
)

// MaxTextRunes bounds one synthesis request. The phone splits responses into
// sentence-sized pieces well under this, and the cap keeps every encrypted
// audio frame under the transport's 1 MiB payload limit.
const MaxTextRunes = 400

// maxWAVBytes keeps the base64-encoded result inside one transport frame.
// Output past the cap is decimated to a lower sample rate instead of failing.
const maxWAVBytes = 900 << 10

type engine struct {
	binary string
	// fallback paths are tried when PATH misses the binary: the relay often
	// runs as a service with a minimal PATH that excludes user installs.
	fallback []string
	// args builds the argv writing a WAV to outPath; text arrives on stdin
	// unless textArg is true.
	args    func(outPath string) []string
	textArg bool
}

func engines() []engine {
	var candidates []engine
	// Piper's neural voices sound close to cloud TTS and synthesize many
	// times faster than realtime on a CPU; every other engine is a fallback.
	home, _ := os.UserHomeDir()
	if voice, ok := piperVoice(home); ok {
		candidates = append(candidates, engine{
			binary: "piper",
			fallback: []string{
				filepath.Join(home, ".local", "bin", "piper"),
				"/usr/local/bin/piper",
				"/opt/piper/piper",
			},
			args: func(out string) []string { return []string{"--model", voice, "--output_file", out} },
		})
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, engine{
			binary: "say",
			args: func(out string) []string {
				return []string{"-o", out, "--file-format=WAVE", "--data-format=LEI16@22050"}
			},
		})
	}
	return append(candidates,
		engine{
			binary: "espeak-ng",
			args:   func(out string) []string { return []string{"-s", "175", "-w", out, "--stdin"} },
		},
		engine{
			binary: "espeak",
			args:   func(out string) []string { return []string{"-s", "175", "-w", out, "--stdin"} },
		},
		engine{
			binary:  "flite",
			args:    func(out string) []string { return []string{"-o", out} },
			textArg: true,
		},
	)
}

// piperVoice finds a voice model: an explicit HERDR_PIPER_VOICE path wins,
// otherwise the alphabetically first *.onnx in the conventional voice
// directories.
func piperVoice(home string) (string, bool) {
	if path := os.Getenv("HERDR_PIPER_VOICE"); path != "" {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
		return "", false
	}
	for _, dir := range []string{
		filepath.Join(home, ".local", "share", "piper-voices"),
		"/usr/local/share/piper-voices",
		"/usr/share/piper-voices",
	} {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.onnx"))
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches[0], true
		}
	}
	return "", false
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

// Discover reports the first available engine's binary name.
func Discover() (string, bool) {
	for _, candidate := range engines() {
		if _, ok := lookup(candidate); ok {
			return candidate.binary, true
		}
	}
	return "", false
}

// Synthesize renders text with the first available engine and returns a
// canonical PCM16 WAV small enough for one transport frame.
func Synthesize(ctx context.Context, text string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errors.New("text is required")
	}
	if utf8.RuneCountInString(trimmed) > MaxTextRunes {
		return nil, fmt.Errorf("text exceeds %d characters", MaxTextRunes)
	}
	var selected *engine
	binary := ""
	for _, candidate := range engines() {
		if path, ok := lookup(candidate); ok {
			selected = &candidate
			binary = path
			break
		}
	}
	if selected == nil {
		return nil, errors.New("no speech engine is available")
	}
	dir, err := os.MkdirTemp("", "herdr-speech-")
	if err != nil {
		return nil, fmt.Errorf("create synthesis workspace: %w", err)
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "speech.wav")
	args := selected.args(outPath)
	if selected.textArg {
		args = append(args, "-t", trimmed)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if !selected.textArg {
		cmd.Stdin = strings.NewReader(trimmed)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s: %s", selected.binary, detail)
		}
		return nil, fmt.Errorf("%s: %w", selected.binary, err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read synthesized audio: %w", err)
	}
	sampleRate, samples, err := parsePCM16Mono(raw)
	if err != nil {
		return nil, fmt.Errorf("%s output: %w", selected.binary, err)
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

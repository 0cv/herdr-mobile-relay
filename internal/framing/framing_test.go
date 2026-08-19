package framing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

// startBody and contBody are the body capacities of the first and of every
// following chunk for a given chunk-size ceiling.
func startBody(maxChunk int) int { return maxChunk - HeaderSize - LengthPrefixSize }

func contBody(maxChunk int) int { return maxChunk - HeaderSize }

// oversizedChunk builds an otherwise well formed single-chunk frame of size
// bytes, so only its length can be what a reassembler rejects.
func oversizedChunk(size int) []byte {
	part := make([]byte, size)
	part[0] = Version
	part[1] = FlagStart | FlagEnd
	binary.BigEndian.PutUint32(part[HeaderSize:], uint32(size-HeaderSize-LengthPrefixSize))
	return part
}

// roundTrip chunks a logical frame, asserts the wire invariants of every chunk,
// and returns the frame rebuilt by a reassembler.
func roundTrip(t *testing.T, logical []byte, maxChunk int) []byte {
	t.Helper()

	parts := Chunk(nil, logical, maxChunk)
	if want := Count(len(logical), maxChunk); len(parts) != want {
		t.Fatalf("chunk count = %d, want %d", len(parts), want)
	}

	var (
		assembler = NewReassembler(maxChunk)
		frame     []byte
	)
	for index, part := range parts {
		if len(part) > maxChunk {
			t.Fatalf("chunk %d size = %d, want <= %d", index, len(part), maxChunk)
		}
		if part[0] != Version {
			t.Fatalf("chunk %d version = %d, want %d", index, part[0], Version)
		}
		if start := part[1]&FlagStart != 0; start != (index == 0) {
			t.Fatalf("chunk %d start flag = %v", index, start)
		}
		if end := part[1]&FlagEnd != 0; end != (index == len(parts)-1) {
			t.Fatalf("chunk %d end flag = %v", index, end)
		}
		if index == 0 {
			declared := binary.BigEndian.Uint32(part[HeaderSize:])
			if int(declared) != len(logical) {
				t.Fatalf("declared length = %d, want %d", declared, len(logical))
			}
		}

		out, err := assembler.Push(part)
		if err != nil {
			t.Fatalf("push chunk %d: %v", index, err)
		}
		if index < len(parts)-1 {
			if out != nil {
				t.Fatalf("chunk %d completed a frame early", index)
			}
			continue
		}
		if out == nil {
			t.Fatal("final chunk did not complete the frame")
		}
		frame = out
	}
	if assembler.active {
		t.Fatal("reassembler still holds a partial frame")
	}
	return frame
}

func TestChunkRoundTrip(t *testing.T) {
	random := rand.NewChaCha8([32]byte{1})
	tests := []struct {
		name     string
		size     int
		maxChunk int
	}{
		{name: "one byte", size: 1, maxChunk: WebRTCChunkSize},
		{name: "exact single chunk", size: startBody(WebRTCChunkSize), maxChunk: WebRTCChunkSize},
		{name: "one byte past single chunk", size: startBody(WebRTCChunkSize) + 1, maxChunk: WebRTCChunkSize},
		{name: "exact two chunks", size: startBody(WebRTCChunkSize) + contBody(WebRTCChunkSize), maxChunk: WebRTCChunkSize},
		{name: "ten mebibytes", size: 10 << 20, maxChunk: WebRTCChunkSize},
		{name: "gateway chunk size", size: 10 << 20, maxChunk: GatewayChunkSize},
		{name: "gateway exact single chunk", size: startBody(GatewayChunkSize), maxChunk: GatewayChunkSize},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logical := make([]byte, test.size)
			if _, err := random.Read(logical); err != nil {
				t.Fatalf("fill payload: %v", err)
			}

			frame := roundTrip(t, logical, test.maxChunk)
			if len(frame) != len(logical) {
				t.Fatalf("logical length = %d, want %d", len(frame), len(logical))
			}
			if !bytes.Equal(frame, logical) {
				t.Fatal("round tripped frame differs from the original")
			}
		})
	}
}

// TestChunkSizeParameterization pins that the ceiling passed to Chunk, not a
// package-level constant, decides how one frame is split.
func TestChunkSizeParameterization(t *testing.T) {
	logical := bytes.Repeat([]byte{0x5a}, 10<<20)

	small := Chunk(nil, logical, WebRTCChunkSize)
	large := Chunk(nil, logical, GatewayChunkSize)
	if len(small) <= len(large) {
		t.Fatalf("chunk counts = %d and %d, want the smaller ceiling to produce more", len(small), len(large))
	}
	if want := Count(len(logical), GatewayChunkSize); len(large) != want {
		t.Fatalf("gateway chunk count = %d, want %d", len(large), want)
	}
	for index, part := range large {
		if len(part) > GatewayChunkSize {
			t.Fatalf("chunk %d size = %d, want <= %d", index, len(part), GatewayChunkSize)
		}
	}
}

func TestChunkReusesDestination(t *testing.T) {
	first := bytes.Repeat([]byte{0xa1}, startBody(WebRTCChunkSize)+10)
	second := bytes.Repeat([]byte{0xb2}, 4)

	parts := Chunk(nil, first, WebRTCChunkSize)
	parts = Chunk(parts[:0], second, WebRTCChunkSize)
	if len(parts) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(parts))
	}

	frame, err := NewReassembler(WebRTCChunkSize).Push(parts[0])
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !bytes.Equal(frame, second) {
		t.Fatal("reused destination produced the wrong frame")
	}
}

func TestReassemblerRejectsOversizedDeclaredLength(t *testing.T) {
	part := make([]byte, HeaderSize+LengthPrefixSize)
	part[0] = Version
	part[1] = FlagStart
	binary.BigEndian.PutUint32(part[HeaderSize:], MaxLogicalBytes+1)

	assembler := NewReassembler(WebRTCChunkSize)
	if _, err := assembler.Push(part); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("push oversized start = %v, want %v", err, ErrFrameTooLarge)
	}
	if assembler.active {
		t.Fatal("rejected start chunk started an assembly")
	}
}

func TestReassemblerRejectsSequenceViolations(t *testing.T) {
	start := Chunk(nil, bytes.Repeat([]byte{7}, startBody(WebRTCChunkSize)+1), WebRTCChunkSize)[0]

	tests := []struct {
		name   string
		chunks [][]byte
		want   error
	}{
		{
			name:   "end without start",
			chunks: [][]byte{{Version, FlagEnd, 1, 2, 3}},
			want:   ErrChunkSequence,
		},
		{
			name:   "continuation without start",
			chunks: [][]byte{{Version, 0, 1, 2, 3}},
			want:   ErrChunkSequence,
		},
		{
			name:   "start while assembling",
			chunks: [][]byte{start, start},
			want:   ErrChunkSequence,
		},
		{
			name:   "unknown flag bit",
			chunks: [][]byte{{Version, FlagStart | FlagEnd | 0x04, 0, 0, 0, 0}},
			want:   ErrChunkMalformed,
		},
		{
			name:   "unknown version",
			chunks: [][]byte{{Version + 1, FlagStart | FlagEnd, 0, 0, 0, 0}},
			want:   ErrChunkMalformed,
		},
		{
			name:   "short header",
			chunks: [][]byte{{Version}},
			want:   ErrChunkMalformed,
		},
		{
			name:   "body longer than declared",
			chunks: [][]byte{{Version, FlagStart | FlagEnd, 0, 0, 0, 1, 0xaa, 0xbb}},
			want:   ErrChunkMalformed,
		},
		{
			name:   "end before declared length",
			chunks: [][]byte{{Version, FlagStart, 0, 0, 0, 4, 0xaa}, {Version, FlagEnd}},
			want:   ErrChunkMalformed,
		},
		{
			name:   "chunk larger than the ceiling",
			chunks: [][]byte{oversizedChunk(WebRTCChunkSize + 1)},
			want:   ErrChunkMalformed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assembler := NewReassembler(WebRTCChunkSize)
			var err error
			for _, part := range test.chunks {
				_, err = assembler.Push(part)
				if err != nil {
					break
				}
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("push = %v, want %v", err, test.want)
			}
		})
	}
}

func TestReassemblerStall(t *testing.T) {
	parts := Chunk(nil, bytes.Repeat([]byte{9}, startBody(WebRTCChunkSize)+1), WebRTCChunkSize)
	if len(parts) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(parts))
	}

	assembler := NewReassembler(WebRTCChunkSize)
	if _, err := assembler.Push(parts[0]); err != nil {
		t.Fatalf("push start: %v", err)
	}
	arrival := assembler.updated
	if assembler.Expired(arrival) {
		t.Fatal("assembly expired immediately")
	}
	if assembler.Expired(arrival.Add(StallTimeout)) {
		t.Fatal("assembly expired at the timeout boundary")
	}
	if !assembler.Expired(arrival.Add(StallTimeout + time.Nanosecond)) {
		t.Fatal("stalled assembly did not expire")
	}

	if _, err := assembler.Push(parts[1]); err != nil {
		t.Fatalf("push end: %v", err)
	}
	if assembler.Expired(arrival.Add(24 * time.Hour)) {
		t.Fatal("completed assembly still expires")
	}
}

func TestReassemblerReset(t *testing.T) {
	parts := Chunk(nil, bytes.Repeat([]byte{3}, startBody(WebRTCChunkSize)+1), WebRTCChunkSize)

	assembler := NewReassembler(WebRTCChunkSize)
	if _, err := assembler.Push(parts[0]); err != nil {
		t.Fatalf("push start: %v", err)
	}
	assembler.Reset()
	if assembler.Expired(time.Now().Add(StallTimeout + time.Second)) {
		t.Fatal("reset assembly still expires")
	}
	if _, err := assembler.Push(parts[0]); err != nil {
		t.Fatalf("push start after reset: %v", err)
	}
}

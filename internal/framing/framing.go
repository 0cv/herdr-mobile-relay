// Package framing implements the chunk framing shared by every transport that
// carries binary Herdr E2EE frames.
//
// Two paths need it for different reasons and both need the same wire format:
// a WebRTC DataChannel has a practical per-message ceiling around 64 KiB, and
// the blind gateway caps one relayed frame so a shared VPS cannot be forced to
// buffer a multi-megabyte message per phone. A single logical frame — up to the
// relay's 21 MiB read limit, which an image upload approaches — therefore
// travels as a sequence of bounded chunks:
//
//	chunk = [u8 version=1][u8 flags][payload]
//	START chunk payload = [u32be logical_len][body...]
//
// Both transports are reliable and ordered and carry exactly one logical frame
// at a time per direction, so the state machine needs neither message
// identifiers nor reorder buffers: anything that does not follow START..END in
// order is a protocol violation that closes the connection.
package framing

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	// Version is the only chunk version this relay speaks.
	Version = 1
	// HeaderSize covers [u8 version][u8 flags].
	HeaderSize = 2
	// LengthPrefixSize covers the [u32be logical_len] a START chunk carries.
	LengthPrefixSize = 4

	// FlagStart marks the first chunk of a logical frame, FlagEnd the last. A
	// frame that fits one chunk sets both.
	FlagStart = 1 << 0
	FlagEnd   = 1 << 1
	flagKnown = FlagStart | FlagEnd

	// MaxLogicalBytes mirrors the WebSocket read limit of the legacy path so
	// no transport can buy more memory than another.
	MaxLogicalBytes = 21 * 1024 * 1024

	// StallTimeout abandons a partially received frame whose remaining chunks
	// stopped arriving.
	StallTimeout = 30 * time.Second

	// WebRTCChunkSize stays well under the SCTP message limit every browser
	// implements.
	WebRTCChunkSize = 16384
	// GatewayChunkSize keeps one relayed fragment far below the gateway's
	// per-frame cap, bounding the memory a phone can pin on shared hardware.
	GatewayChunkSize = 262144
)

var (
	ErrChunkMalformed = errors.New("framing: malformed chunk")
	ErrChunkSequence  = errors.New("framing: chunk out of sequence")
	ErrFrameTooLarge  = errors.New("framing: logical frame exceeds size limit")
	ErrChunkSize      = errors.New("framing: chunk size limit is too small")
)

// Count reports how many chunks a logical frame of n bytes needs.
func Count(n, maxChunk int) int {
	start := maxChunk - HeaderSize - LengthPrefixSize
	if n <= start {
		return 1
	}
	cont := maxChunk - HeaderSize
	return 1 + (n-start+cont-1)/cont
}

// Chunk splits one logical frame into chunks appended to dst, which callers
// pass as a reused slice header (dst[:0]). Every chunk body is carved out of a
// single allocation, so a large frame costs one copy rather than one allocation
// per chunk. The returned chunks alias that allocation and stay valid until the
// next Chunk call that reuses dst.
func Chunk(dst [][]byte, logical []byte, maxChunk int) [][]byte {
	startBody := maxChunk - HeaderSize - LengthPrefixSize
	contBody := maxChunk - HeaderSize
	count := Count(len(logical), maxChunk)
	buf := make([]byte, len(logical)+LengthPrefixSize+count*HeaderSize)
	remaining := logical
	offset := 0
	for index := range count {
		body := contBody
		size := HeaderSize
		if index == 0 {
			body = startBody
			size += LengthPrefixSize
		}
		if body > len(remaining) {
			body = len(remaining)
		}
		size += body

		part := buf[offset : offset+size]
		offset += size

		part[0] = Version
		flags := byte(0)
		if index == 0 {
			flags |= FlagStart
			binary.BigEndian.PutUint32(part[HeaderSize:], uint32(len(logical)))
		}
		if index == count-1 {
			flags |= FlagEnd
		}
		part[1] = flags

		copy(part[size-body:], remaining[:body])
		remaining = remaining[body:]
		dst = append(dst, part)
	}
	return dst
}

// Reassembler rebuilds logical frames from chunks of one direction.
type Reassembler struct {
	maxChunk int
	buf      []byte
	want     int
	active   bool
	updated  time.Time
}

// NewReassembler builds a reassembler that rejects chunks larger than maxChunk.
func NewReassembler(maxChunk int) *Reassembler {
	return &Reassembler{maxChunk: maxChunk}
}

// Push consumes one chunk and returns the completed logical frame, or nil while
// the frame is still incomplete. A returned frame owns its bytes.
func (r *Reassembler) Push(part []byte) ([]byte, error) {
	if r.maxChunk <= HeaderSize+LengthPrefixSize {
		return nil, ErrChunkSize
	}
	if len(part) < HeaderSize || len(part) > r.maxChunk || part[0] != Version {
		return nil, ErrChunkMalformed
	}
	flags := part[1]
	if flags&^flagKnown != 0 {
		return nil, ErrChunkMalformed
	}
	payload := part[HeaderSize:]
	start := flags&FlagStart != 0
	end := flags&FlagEnd != 0

	if start {
		if r.active {
			return nil, ErrChunkSequence
		}
		if len(payload) < LengthPrefixSize {
			return nil, ErrChunkMalformed
		}
		declared := binary.BigEndian.Uint32(payload)
		if declared > MaxLogicalBytes {
			return nil, ErrFrameTooLarge
		}
		body := payload[LengthPrefixSize:]
		r.want = int(declared)
		if len(body) > r.want {
			return nil, ErrChunkMalformed
		}
		r.buf = make([]byte, 0, r.want)
		r.buf = append(r.buf, body...)
		r.active = true
		r.updated = time.Now()
	} else {
		if !r.active {
			return nil, ErrChunkSequence
		}
		if len(r.buf)+len(payload) > r.want {
			return nil, ErrChunkMalformed
		}
		r.buf = append(r.buf, payload...)
		r.updated = time.Now()
	}

	if !end {
		return nil, nil
	}
	if len(r.buf) != r.want {
		return nil, ErrChunkMalformed
	}
	frame := r.buf
	r.buf = nil
	r.want = 0
	r.active = false
	return frame, nil
}

// Expired reports whether an incomplete frame has been stalled long enough to
// abandon the connection.
func (r *Reassembler) Expired(now time.Time) bool {
	return r.active && now.Sub(r.updated) > StallTimeout
}

// Reset drops any partial assembly.
func (r *Reassembler) Reset() {
	r.buf = nil
	r.want = 0
	r.active = false
}

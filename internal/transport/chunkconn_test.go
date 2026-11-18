package transport

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
)

const testChunkSize = 64

func newStubChunkConn(sendPart func(ctx context.Context, part []byte) error) *ChunkConn {
	return NewChunkConn(ChunkConnOptions{
		Name:      TransportGateway,
		ChunkSize: testChunkSize,
		SendPart:  sendPart,
	})
}

func newChunkConnPair() (*ChunkConn, *ChunkConn) {
	var a, b *ChunkConn
	a = newStubChunkConn(func(_ context.Context, part []byte) error { return b.Receive(part) })
	b = newStubChunkConn(func(_ context.Context, part []byte) error { return a.Receive(part) })
	return a, b
}

func testFrame(size int, fill byte) []byte {
	return bytes.Repeat([]byte{fill}, size)
}

func startChunk(t *testing.T, size int) []byte {
	t.Helper()
	parts := framing.Chunk(nil, testFrame(size, 1), testChunkSize)
	if len(parts) < 2 {
		t.Fatalf("frame of %d bytes fits one chunk", size)
	}
	return parts[0]
}

func TestChunkConnImplementsFrameConn(t *testing.T) {
	var conn FrameConn = newStubChunkConn(nil)
	if conn.Codec() != CodecBinary {
		t.Fatalf("codec = %v, want binary", conn.Codec())
	}
	if conn.TransportName() != TransportGateway {
		t.Fatalf("transport name = %q", conn.TransportName())
	}
}

func TestChunkConnRoundTrip(t *testing.T) {
	for name, size := range map[string]int{"one chunk": 10, "many chunks": 10_000} {
		t.Run(name, func(t *testing.T) {
			a, b := newChunkConnPair()
			var sender, receiver FrameConn = a, b
			want := testFrame(size, 7)
			if err := sender.WriteFrame(t.Context(), want); err != nil {
				t.Fatal(err)
			}
			got, err := receiver.ReadFrame(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("frame of %d bytes changed in transit", size)
			}
		})
	}
}

func TestChunkConnConcurrentWritersKeepChunkSequencesContiguous(t *testing.T) {
	var mu sync.Mutex
	var parts [][]byte
	conn := newStubChunkConn(func(_ context.Context, part []byte) error {
		runtime.Gosched()
		mu.Lock()
		defer mu.Unlock()
		parts = append(parts, bytes.Clone(part))
		return nil
	})

	const writers = 8
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if err := conn.WriteFrame(t.Context(), testFrame(2_000, byte(i+1))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	reassembler := framing.NewReassembler(testChunkSize)
	seen := map[byte]bool{}
	for _, part := range parts {
		frame, err := reassembler.Push(part)
		if err != nil {
			t.Fatalf("chunks interleaved: %v", err)
		}
		if frame == nil {
			continue
		}
		if !bytes.Equal(frame, testFrame(2_000, frame[0])) {
			t.Fatal("frame content corrupted")
		}
		seen[frame[0]] = true
	}
	if len(seen) != writers {
		t.Fatalf("reassembled %d distinct frames, want %d", len(seen), writers)
	}
}

func TestChunkConnRejectsFrameAboveLogicalLimit(t *testing.T) {
	var calls atomic.Int32
	conn := newStubChunkConn(func(context.Context, []byte) error {
		calls.Add(1)
		return nil
	})
	err := conn.WriteFrame(t.Context(), make([]byte, framing.MaxLogicalBytes+1))
	if !errors.Is(err, framing.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if calls.Load() != 0 {
		t.Fatal("an oversized frame reached SendPart")
	}
}

func TestChunkConnWriteReturnsSendPartError(t *testing.T) {
	want := errors.New("link down")
	var calls int
	conn := newStubChunkConn(func(context.Context, []byte) error {
		calls++
		return want
	})
	if err := conn.WriteFrame(t.Context(), testFrame(1_000, 1)); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("SendPart called %d times after its first error", calls)
	}
}

func TestChunkConnReceiveReportsFramingViolationsAndResetsAssembly(t *testing.T) {
	good := framing.Chunk(nil, []byte("hello"), testChunkSize)[0]
	start := startChunk(t, 500)
	tooLarge := []byte{framing.Version, framing.FlagStart | framing.FlagEnd, 0, 0, 0, 0}
	tooLarge[2] = 0xff

	tests := []struct {
		name    string
		prelude [][]byte
		part    []byte
		want    error
	}{
		{"short chunk", nil, []byte{framing.Version}, framing.ErrChunkMalformed},
		{"unknown version", nil, []byte{9, framing.FlagStart | framing.FlagEnd, 0, 0, 0, 0}, framing.ErrChunkMalformed},
		{"unknown flag", nil, []byte{framing.Version, 0x80, 0, 0, 0, 0}, framing.ErrChunkMalformed},
		{"chunk above chunk size", nil, bytes.Repeat([]byte{framing.Version}, testChunkSize+1), framing.ErrChunkMalformed},
		{"continuation without start", nil, []byte{framing.Version, 0, 1}, framing.ErrChunkSequence},
		{"start inside a frame", [][]byte{start}, start, framing.ErrChunkSequence},
		{"declared length above limit", nil, tooLarge, framing.ErrFrameTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := newStubChunkConn(nil)
			for _, part := range tt.prelude {
				if err := conn.Receive(part); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Receive(tt.part); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if conn.Stalled(time.Now().Add(framing.StallTimeout + time.Second)) {
				t.Fatal("violation left a partial assembly behind")
			}
			if err := conn.Receive(good); err != nil {
				t.Fatalf("connection unusable after a violation: %v", err)
			}
			frame, err := conn.ReadFrame(t.Context())
			if err != nil || string(frame) != "hello" {
				t.Fatalf("frame = %q, err = %v", frame, err)
			}
		})
	}
}

func TestChunkConnFullQueueReturnsErrInboundQueueFull(t *testing.T) {
	conn := newStubChunkConn(nil)
	for i := range inboundQueueDepth {
		part := framing.Chunk(nil, []byte{byte(i)}, testChunkSize)[0]
		if err := conn.Receive(part); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	overflow := framing.Chunk(nil, []byte("overflow"), testChunkSize)[0]
	if err := conn.Receive(overflow); !errors.Is(err, ErrInboundQueueFull) {
		t.Fatalf("err = %v, want ErrInboundQueueFull", err)
	}
	for i := range inboundQueueDepth {
		frame, err := conn.ReadFrame(t.Context())
		if err != nil || len(frame) != 1 || frame[0] != byte(i) {
			t.Fatalf("frame %d = %v, err = %v", i, frame, err)
		}
	}
}

func TestChunkConnReceiveAfterCloseReturnsNil(t *testing.T) {
	for name, end := range map[string]func(*ChunkConn){
		"close":    func(c *ChunkConn) { c.Close(CloseNormal, "") },
		"shutdown": func(c *ChunkConn) { c.Shutdown() },
	} {
		t.Run(name, func(t *testing.T) {
			conn := newStubChunkConn(nil)
			end(conn)
			good := framing.Chunk(nil, []byte("late"), testChunkSize)[0]
			if err := conn.Receive(good); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if err := conn.Receive([]byte{0}); err != nil {
				t.Fatalf("malformed chunk after close: err = %v, want nil", err)
			}
		})
	}
}

func TestChunkConnStalledFollowsStallTimeout(t *testing.T) {
	conn := newStubChunkConn(nil)
	if conn.Stalled(time.Now().Add(time.Hour)) {
		t.Fatal("idle connection reported stalled")
	}
	if err := conn.Receive(startChunk(t, 500)); err != nil {
		t.Fatal(err)
	}
	if conn.Stalled(time.Now()) {
		t.Fatal("fresh partial frame reported stalled")
	}
	if !conn.Stalled(time.Now().Add(framing.StallTimeout + time.Second)) {
		t.Fatal("partial frame not reported stalled after the stall timeout")
	}
}

func TestChunkConnBlockedSendPartEndsAtWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newStubChunkConn(func(ctx context.Context, _ []byte) error {
			<-ctx.Done()
			return ctx.Err()
		})
		start := time.Now()
		err := conn.WriteFrame(context.Background(), testFrame(10, 1))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline exceeded", err)
		}
		if elapsed := time.Since(start); elapsed != frameWriteTimeout {
			t.Fatalf("write ended after %v, want %v", elapsed, frameWriteTimeout)
		}
	})
}

type closeRecorder struct {
	calls  atomic.Int32
	status CloseStatus
	reason string
}

func (r *closeRecorder) onClose(status CloseStatus, reason string) {
	r.calls.Add(1)
	r.status = status
	r.reason = reason
}

func newRecordedChunkConn(r *closeRecorder) *ChunkConn {
	return NewChunkConn(ChunkConnOptions{
		Name:      TransportWebRTC,
		ChunkSize: testChunkSize,
		SendPart:  func(context.Context, []byte) error { return nil },
		OnClose:   r.onClose,
	})
}

func TestChunkConnCloseRunsOnCloseOnce(t *testing.T) {
	var recorder closeRecorder
	conn := newRecordedChunkConn(&recorder)
	var frameConn FrameConn = conn

	frameConn.Close(CloseUnauthorized, "denied")
	frameConn.Close(CloseGoingAway, "again")
	frameConn.CloseNow()
	conn.Shutdown()

	if recorder.calls.Load() != 1 {
		t.Fatalf("OnClose ran %d times, want 1", recorder.calls.Load())
	}
	if recorder.status != CloseUnauthorized || recorder.reason != "denied" {
		t.Fatalf("OnClose got (%v, %q)", recorder.status, recorder.reason)
	}
}

func TestChunkConnCloseNowClosesNormally(t *testing.T) {
	var recorder closeRecorder
	var conn FrameConn = newRecordedChunkConn(&recorder)
	conn.CloseNow()
	if recorder.calls.Load() != 1 || recorder.status != CloseNormal || recorder.reason != "" {
		t.Fatalf("OnClose = %d calls, (%v, %q)", recorder.calls.Load(), recorder.status, recorder.reason)
	}
}

func TestChunkConnShutdownNeverRunsOnClose(t *testing.T) {
	var recorder closeRecorder
	conn := newRecordedChunkConn(&recorder)
	conn.Shutdown()
	conn.Close(CloseNormal, "late")
	conn.CloseNow()
	if recorder.calls.Load() != 0 {
		t.Fatalf("OnClose ran %d times after Shutdown", recorder.calls.Load())
	}
}

func TestChunkConnOnCloseMayShutdownAndCloseSameConnection(t *testing.T) {
	var conn *ChunkConn
	var calls atomic.Int32
	conn = NewChunkConn(ChunkConnOptions{
		Name:      TransportWebRTC,
		ChunkSize: testChunkSize,
		SendPart:  func(context.Context, []byte) error { return nil },
		OnClose: func(CloseStatus, string) {
			calls.Add(1)
			conn.Shutdown()
			conn.Close(CloseNormal, "nested")
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.Close(CloseNormal, "")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked when OnClose called Shutdown and Close")
	}
	if calls.Load() != 1 {
		t.Fatalf("OnClose ran %d times, want 1", calls.Load())
	}
}

func TestChunkConnEndedConnectionReadsClosedAndSignalsDone(t *testing.T) {
	for name, end := range map[string]func(FrameConn){
		"close":     func(c FrameConn) { c.Close(CloseNormal, "") },
		"close now": func(c FrameConn) { c.CloseNow() },
		"shutdown":  func(c FrameConn) { c.(*ChunkConn).Shutdown() },
	} {
		t.Run(name, func(t *testing.T) {
			conn := newStubChunkConn(func(context.Context, []byte) error { return nil })
			var frameConn FrameConn = conn
			select {
			case <-conn.Done():
				t.Fatal("Done closed before the connection ended")
			default:
			}

			end(frameConn)

			if _, err := frameConn.ReadFrame(t.Context()); !errors.Is(err, ErrFrameConnClosed) {
				t.Fatalf("ReadFrame err = %v, want ErrFrameConnClosed", err)
			}
			if err := frameConn.WriteFrame(t.Context(), []byte("x")); !errors.Is(err, ErrFrameConnClosed) {
				t.Fatalf("WriteFrame err = %v, want ErrFrameConnClosed", err)
			}
			select {
			case <-conn.Done():
			default:
				t.Fatal("Done not closed after the connection ended")
			}
		})
	}
}

func TestChunkConnReadFrameHonoursContext(t *testing.T) {
	conn := newStubChunkConn(nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := conn.ReadFrame(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

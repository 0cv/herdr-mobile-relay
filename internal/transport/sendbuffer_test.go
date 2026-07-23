package transport

import (
	"testing"
)

func TestSendBufferItemLimit(t *testing.T) {
	buf := newSendBuffer(3, 1<<20)

	for i := 0; i < 3; i++ {
		if !buf.Push([]byte("x")) {
			t.Fatalf("push %d failed, want success (under item limit)", i)
		}
	}
	if buf.Push([]byte("x")) {
		t.Fatal("push beyond item limit succeeded, want rejection")
	}
	if buf.Len() != 3 {
		t.Fatalf("len = %d, want 3", buf.Len())
	}
}

func TestSendBufferByteLimit(t *testing.T) {
	buf := newSendBuffer(100, 10)

	if !buf.Push([]byte("12345")) {
		t.Fatal("first push failed, want success (5 <= 10 bytes)")
	}
	if !buf.Push([]byte("12345")) {
		t.Fatal("second push failed, want success (10 <= 10 bytes)")
	}
	if buf.Push([]byte("1")) {
		t.Fatal("push beyond byte budget succeeded, want rejection (11 > 10)")
	}
	if buf.Bytes() != 10 {
		t.Fatalf("bytes = %d, want 10", buf.Bytes())
	}
}

func TestSendBufferByteReleaseOnPop(t *testing.T) {
	buf := newSendBuffer(100, 10)

	buf.Push([]byte("12345"))
	buf.Push([]byte("12345"))

	data, ok := buf.Pop()
	if !ok || string(data) != "12345" {
		t.Fatalf("pop = %q, %v; want 12345, true", data, ok)
	}
	if buf.Bytes() != 5 {
		t.Fatalf("bytes after pop = %d, want 5", buf.Bytes())
	}

	if !buf.Push([]byte("12345")) {
		t.Fatal("push after pop failed, want success (budget freed)")
	}
	if buf.Bytes() != 10 {
		t.Fatalf("bytes = %d, want 10", buf.Bytes())
	}
}

func TestSendBufferCloseStopsPop(t *testing.T) {
	buf := newSendBuffer(4, 1<<20)
	buf.Push([]byte("a"))
	buf.Close()

	data, ok := buf.Pop()
	if !ok || string(data) != "a" {
		t.Fatalf("pop buffered item = %q, %v; want a, true", data, ok)
	}
	_, ok = buf.Pop()
	if ok {
		t.Fatal("pop after close+drain succeeded, want false")
	}
}

func TestSendBufferByteOverflowEvicts(t *testing.T) {
	buf := newSendBuffer(1000, 64)

	big := make([]byte, 65)
	if buf.Push(big) {
		t.Fatal("single message exceeding byte budget was accepted, want rejection")
	}
	if buf.Len() != 0 {
		t.Fatalf("len = %d, want 0 (nothing enqueued)", buf.Len())
	}
}

func TestAgentUpdatesNeverCoalesce(t *testing.T) {
	buf := newSendBuffer(4, 1<<20)
	first, kind, replaceable, err := encodeMessage(map[string]any{
		"type": "agent_update", "pane_id": "pane-1", "status": "working",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, secondKind, secondReplaceable, err := encodeMessage(map[string]any{
		"type": "agent_update", "pane_id": "pane-1", "status": "blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replaceable || secondReplaceable {
		t.Fatal("agent_update was marked replaceable")
	}
	if got := buf.pushTyped(first, kind, replaceable); got != pushQueued {
		t.Fatalf("first push = %v, want queued", got)
	}
	if got := buf.pushTyped(second, secondKind, secondReplaceable); got != pushQueued {
		t.Fatalf("second push = %v, want queued", got)
	}
	if buf.Len() != 2 {
		t.Fatalf("buffer length = %d, want both agent_update messages", buf.Len())
	}
}

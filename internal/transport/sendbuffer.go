package transport

import (
	"encoding/json"
	"sync"
)

const (
	clientOutboundMaxItems = 64
	clientOutboundMaxBytes = 4 * 1024 * 1024
)

type bufferedMessage struct {
	data        []byte
	messageType string
	replaceable bool
}

type sendBuffer struct {
	mu       sync.Mutex
	ready    *sync.Cond
	items    []bufferedMessage
	bytes    int
	maxItems int
	maxBytes int
	closed   bool
}

type pushResult uint8

const (
	pushRejected pushResult = iota
	pushQueued
	pushCoalesced
)

func newSendBuffer(maxItems, maxBytes int) *sendBuffer {
	buffer := &sendBuffer{maxItems: maxItems, maxBytes: maxBytes}
	buffer.ready = sync.NewCond(&buffer.mu)
	return buffer
}

func (b *sendBuffer) Push(data []byte) bool {
	return b.PushTyped(data, messageType(data), false)
}

func (b *sendBuffer) PushTyped(data []byte, kind string, replaceable bool) bool {
	return b.pushTyped(data, kind, replaceable) != pushRejected
}

func (b *sendBuffer) pushTyped(data []byte, kind string, replaceable bool) pushResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return pushRejected
	}
	copyData := append([]byte(nil), data...)
	if replaceable && len(b.items) > 0 {
		tail := &b.items[len(b.items)-1]
		if tail.replaceable && tail.messageType == kind {
			nextBytes := b.bytes - len(tail.data) + len(copyData)
			if nextBytes > b.maxBytes {
				return pushRejected
			}
			b.bytes = nextBytes
			tail.data = copyData
			return pushCoalesced
		}
	}
	if len(b.items) >= b.maxItems || b.bytes+len(copyData) > b.maxBytes {
		return pushRejected
	}
	b.items = append(b.items, bufferedMessage{data: copyData, messageType: kind, replaceable: replaceable})
	b.bytes += len(copyData)
	b.ready.Signal()
	return pushQueued
}

func (b *sendBuffer) Pop() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.items) == 0 && !b.closed {
		b.ready.Wait()
	}
	if len(b.items) == 0 {
		return nil, false
	}
	item := b.items[0]
	copy(b.items, b.items[1:])
	b.items[len(b.items)-1] = bufferedMessage{}
	b.items = b.items[:len(b.items)-1]
	b.bytes -= len(item.data)
	return item.data, true
}

func (b *sendBuffer) Close() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		b.ready.Broadcast()
	}
	b.mu.Unlock()
}

func (b *sendBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

func (b *sendBuffer) Bytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
}

func messageType(data []byte) string {
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &envelope)
	return envelope.Type
}

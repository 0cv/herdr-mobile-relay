package hc

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const maxFramePayload = 4 << 20

const (
	frameData byte = iota
	frameCredit
	frameClose
	frameResize
)

type attachment struct {
	conn    net.Conn
	channel uint32
	mu      sync.Mutex
	done    chan struct{}
}

func readFrame(r io.Reader) (uint32, byte, []byte, error) {
	var h [9]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, 0, nil, err
	}
	n := binary.LittleEndian.Uint32(h[5:])
	if n > maxFramePayload {
		return 0, 0, nil, errors.New("core frame too large")
	}
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return binary.LittleEndian.Uint32(h[:4]), h[4], p, err
}

func (a *attachment) write(channel uint32, kind byte, p []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(p) > maxFramePayload {
		return errors.New("core frame too large")
	}
	if err := a.conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	h := make([]byte, 9+len(p))
	binary.LittleEndian.PutUint32(h, channel)
	h[4] = kind
	binary.LittleEndian.PutUint32(h[5:], uint32(len(p)))
	copy(h[9:], p)
	n, err := a.conn.Write(h)
	if err == nil && n != len(h) {
		err = io.ErrShortWrite
	}
	return err
}

func (c *Client) attach(ctx context.Context, terminal string, cols, rows int) (*attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	if err := hello(ctx, conn, reader, true); err != nil {
		return nil, err
	}
	a := &attachment{conn: conn, done: make(chan struct{})}
	p, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "attach", "method": "terminal.attach", "params": map[string]any{"terminal": terminal, "cols": cols, "rows": rows, "observe": false, "history": 0}})
	if err != nil {
		return nil, err
	}
	if err := a.write(0, frameData, p); err != nil {
		return nil, err
	}
	ch, kind, p, err := readFrame(reader)
	if err != nil {
		return nil, err
	}
	var response struct {
		JSONRPC string
		ID      string
		Result  struct{ Channel uint32 }
		Error   json.RawMessage
	}
	if err := json.Unmarshal(p, &response); err != nil {
		return nil, err
	}
	if ch != 0 || kind != frameData || response.JSONRPC != "2.0" || response.ID != "attach" || response.Result.Channel == 0 {
		return nil, errors.New("core attach rejected")
	}
	a.channel = response.Result.Channel
	if !stop() || ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	success = true
	go func() {
		defer close(a.done)
		defer conn.Close()
		for {
			ch, kind, p, err := readFrame(reader)
			if err != nil {
				return
			}
			if ch != a.channel {
				continue
			}
			switch kind {
			case frameData:
				var credit [4]byte
				binary.LittleEndian.PutUint32(credit[:], uint32(len(p)))
				if a.write(ch, frameCredit, credit[:]) != nil {
					return
				}
			case frameClose:
				return
			}
		}
	}()
	return a, nil
}

func (a *attachment) Resize(cols, rows int) error {
	var p [4]byte
	binary.LittleEndian.PutUint16(p[:], uint16(cols))
	binary.LittleEndian.PutUint16(p[2:], uint16(rows))
	return a.write(a.channel, frameResize, p[:])
}

func (a *attachment) Close() error { return a.conn.Close() }

func (a *attachment) ended() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

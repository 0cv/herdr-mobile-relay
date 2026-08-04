package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const socketAPIBufferBytes = 64 * 1024

type socketAPIClient struct {
	path   string
	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	seq    uint64
}

type PaneRead struct {
	Content   []byte
	Truncated bool
}

type socketAPIResponse struct {
	ID     string `json:"id"`
	Result struct {
		Type string `json:"type"`
		Read struct {
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		} `json:"read"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newSocketAPIClient(path string) *socketAPIClient {
	return &socketAPIClient{path: path}
}

func (c *socketAPIClient) available(ctx context.Context) bool {
	if c == nil || c.path == "" {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connect(checkCtx) == nil
}

func (c *socketAPIClient) readPane(
	ctx context.Context,
	paneID string,
	lines int,
	format string,
	source string,
) (PaneRead, error) {
	if c == nil || c.path == "" {
		return PaneRead{}, errors.New("Herdr socket path is unavailable")
	}
	if source == "recent-unwrapped" {
		source = "recent_unwrapped"
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for range 2 {
		if err := c.connect(ctx); err != nil {
			lastErr = err
			break
		}
		content, err := c.readPaneConnected(ctx, paneID, lines, format, source)
		if err == nil {
			return content, nil
		}
		lastErr = err
		_ = c.closeLocked()
	}
	return PaneRead{}, lastErr
}

func (c *socketAPIClient) connect(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.path)
	if err != nil {
		return fmt.Errorf("connect to Herdr socket API: %w", err)
	}
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, socketAPIBufferBytes)
	return nil
}

func (c *socketAPIClient) readPaneConnected(
	ctx context.Context,
	paneID string,
	lines int,
	format string,
	source string,
) (PaneRead, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return PaneRead{}, fmt.Errorf("set Herdr socket API deadline: %w", err)
	}

	c.seq++
	requestID := fmt.Sprintf("mobile-relay-read-%d", c.seq)
	request := map[string]any{
		"id":     requestID,
		"method": "pane.read",
		"params": map[string]any{
			"pane_id":    paneID,
			"source":     source,
			"lines":      lines,
			"format":     format,
			"strip_ansi": format != "ansi",
		},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return PaneRead{}, fmt.Errorf("encode Herdr socket API request: %w", err)
	}
	payload = append(payload, '\n')
	if _, err := c.conn.Write(payload); err != nil {
		return PaneRead{}, fmt.Errorf("write Herdr socket API request: %w", err)
	}
	line, err := readSocketAPILine(c.reader)
	if err != nil {
		return PaneRead{}, fmt.Errorf("read Herdr socket API response: %w", err)
	}
	var response socketAPIResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return PaneRead{}, fmt.Errorf("decode Herdr socket API response: %w", err)
	}
	if response.ID != requestID {
		return PaneRead{}, errors.New("Herdr socket API response ID mismatch")
	}
	if response.Error != nil {
		return PaneRead{}, fmt.Errorf("Herdr socket API %s: %s", response.Error.Code, response.Error.Message)
	}
	if response.Result.Type != "pane_read" {
		return PaneRead{}, fmt.Errorf("Herdr socket API returned %q for pane.read", response.Result.Type)
	}
	return PaneRead{
		Content:   []byte(response.Result.Read.Text),
		Truncated: response.Result.Read.Truncated,
	}, nil
}

func readSocketAPILine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, socketAPIBufferBytes)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxOutputBytes {
			return nil, fmt.Errorf("response exceeds %d bytes", maxOutputBytes)
		}
		line = append(line, fragment...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func (c *socketAPIClient) close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *socketAPIClient) closeLocked() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.reader = nil
	return err
}

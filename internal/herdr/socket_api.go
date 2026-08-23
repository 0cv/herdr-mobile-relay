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
	params := map[string]any{
		"pane_id":    paneID,
		"source":     source,
		"lines":      lines,
		"format":     format,
		"strip_ansi": format != "ansi",
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for range 2 {
		if err := c.connect(ctx); err != nil {
			lastErr = err
			break
		}
		response, _, err := c.requestConnected(ctx, "pane.read", params)
		if err == nil && response.Result.Type != "pane_read" {
			err = fmt.Errorf("Herdr socket API returned %q for pane.read", response.Result.Type)
		}
		if err == nil {
			return PaneRead{
				Content:   []byte(response.Result.Read.Text),
				Truncated: response.Result.Read.Truncated,
			}, nil
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

// requestConnected writes one request and reads its response. The returned
// bool reports whether any request bytes reached the socket: once bytes are
// on the wire the peer may execute the request even when no response comes
// back, so callers use it as the retry-safety boundary for mutations. A
// structured Herdr error response is surfaced as a *CLIError so callers can
// classify the refusal by code.
func (c *socketAPIClient) requestConnected(
	ctx context.Context,
	method string,
	params map[string]any,
) (socketAPIResponse, bool, error) {
	var response socketAPIResponse
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return response, false, fmt.Errorf("set Herdr socket API deadline: %w", err)
	}

	c.seq++
	requestID := fmt.Sprintf("mobile-relay-api-%d", c.seq)
	payload, err := json.Marshal(map[string]any{
		"id": requestID, "method": method, "params": params,
	})
	if err != nil {
		return response, false, fmt.Errorf("encode Herdr socket API request: %w", err)
	}
	payload = append(payload, '\n')
	if written, err := c.conn.Write(payload); err != nil {
		return response, written > 0, fmt.Errorf("write Herdr socket API request: %w", err)
	}
	line, err := readSocketAPILine(c.reader)
	if err != nil {
		return response, true, fmt.Errorf("read Herdr socket API response: %w", err)
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return response, true, fmt.Errorf("decode Herdr socket API response: %w", err)
	}
	if response.ID != requestID {
		return response, true, errors.New("Herdr socket API response ID mismatch")
	}
	if response.Error != nil {
		return response, true, fmt.Errorf("Herdr socket API: %w", &CLIError{
			Code:    response.Error.Code,
			Message: response.Error.Message,
		})
	}
	return response, true, nil
}

// moveRequest sends one topology move. It retries once on transport errors:
// Herdr closes the socket after every response, so a connection cached by an
// earlier request reads EOF, and repeating a satisfied move is a Herdr no-op.
// The final error carries the dispatch boundary: once any attempt wrote
// request bytes the move may have applied, so the failure wraps
// ErrDispatchedUnknown; failures before the first written byte wrap
// ErrNotStarted. A structured Herdr refusal is definitive — the move did not
// apply — and is returned as-is so callers keep the refusal code.
func (c *socketAPIClient) moveRequest(
	ctx context.Context,
	method string,
	wantType string,
	params map[string]any,
) error {
	if c == nil || c.path == "" {
		return fmt.Errorf("%w: Herdr socket path is unavailable", ErrNotStarted)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dispatched := false
	var lastErr error
	for range 2 {
		if err := c.connect(ctx); err != nil {
			lastErr = err
			break
		}
		response, wrote, err := c.requestConnected(ctx, method, params)
		dispatched = dispatched || wrote
		if err == nil {
			if response.Result.Type != wantType {
				return errors.Join(
					ErrDispatchedUnknown,
					fmt.Errorf("Herdr socket API returned %q for %s", response.Result.Type, method),
				)
			}
			return nil
		}
		var cliErr *CLIError
		if errors.As(err, &cliErr) {
			return err
		}
		lastErr = err
		_ = c.closeLocked()
	}
	if dispatched {
		return errors.Join(ErrDispatchedUnknown, lastErr)
	}
	return errors.Join(ErrNotStarted, lastErr)
}

func (c *socketAPIClient) tabMove(ctx context.Context, tabID string, insertIndex int) error {
	return c.moveRequest(ctx, "tab.move", "tab_list", map[string]any{
		"tab_id":       tabID,
		"insert_index": insertIndex,
	})
}

func (c *socketAPIClient) workspaceMove(ctx context.Context, workspaceID string, insertIndex int) error {
	return c.moveRequest(ctx, "workspace.move", "workspace_list", map[string]any{
		"workspace_id": workspaceID,
		"insert_index": insertIndex,
	})
}

func (c *socketAPIClient) workspaceMoveBlock(
	ctx context.Context,
	workspaceIDs []string,
	beforeWorkspaceID string,
) error {
	params := map[string]any{"workspace_ids": workspaceIDs}
	if beforeWorkspaceID != "" {
		params["before_workspace_id"] = beforeWorkspaceID
	}
	return c.moveRequest(ctx, "workspace.move_block", "workspace_list", params)
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

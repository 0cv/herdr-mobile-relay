package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
)

const socketAPIBufferBytes = 64 * 1024

var ErrServerNotRunning = errors.New("herdr: server is not running")

type socketAPIClient struct {
	path string
	seq  atomic.Uint64
}

type PaneRead struct {
	Content   []byte
	Truncated bool
}

func newSocketAPIClient(path string) *socketAPIClient {
	return &socketAPIClient{path: path}
}

type socketAPIEnvelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *socketAPIClient) requestUnary(
	ctx context.Context,
	method string,
	params map[string]any,
) (json.RawMessage, bool, error) {
	if c == nil || c.path == "" {
		return nil, false, errors.New("Herdr socket path is unavailable")
	}
	requestCtx := ctx
	cancel := func() {}
	if _, ok := requestCtx.Deadline(); !ok {
		requestCtx, cancel = context.WithTimeout(requestCtx, defaultTimeout)
	}
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(requestCtx, "unix", c.path)
	if err != nil {
		if socketServerNotRunning(c.path, err) {
			err = errors.Join(err, ErrServerNotRunning)
		}
		return nil, false, fmt.Errorf("connect to Herdr socket API: %w", err)
	}
	stopWatch := context.AfterFunc(requestCtx, func() { _ = conn.Close() })
	defer stopWatch()
	defer conn.Close()
	deadline, _ := requestCtx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, false, fmt.Errorf("set Herdr socket API deadline: %w", err)
	}

	requestID := fmt.Sprintf("mobile-relay-api-%d", c.seq.Add(1))
	payload, err := json.Marshal(map[string]any{
		"id": requestID, "method": method, "params": params,
	})
	if err != nil {
		return nil, false, fmt.Errorf("encode Herdr socket API request: %w", err)
	}
	payload = append(payload, '\n')
	if err := CheckDispatch(requestCtx); err != nil {
		return nil, false, errors.Join(ErrNotStarted, err)
	}
	written, err := conn.Write(payload)
	if err != nil {
		return nil, written > 0, fmt.Errorf("write Herdr socket API request: %w", err)
	}
	line, err := readSocketAPILine(bufio.NewReaderSize(conn, socketAPIBufferBytes))
	if err != nil {
		return nil, true, fmt.Errorf("read Herdr socket API response: %w", err)
	}
	var response socketAPIEnvelope
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, true, fmt.Errorf("decode Herdr socket API response: %w", err)
	}
	if response.Error != nil {
		if response.ID != requestID && !isPreDispatchRequestError(response.ID, response.Error.Code, response.Error.Message) {
			return nil, true, errors.New("Herdr socket API response ID mismatch")
		}
		return nil, true, fmt.Errorf("Herdr socket API: %w", &CLIError{
			Code:    response.Error.Code,
			Message: response.Error.Message,
		})
	}
	if response.ID != requestID {
		return nil, true, errors.New("Herdr socket API response ID mismatch")
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return nil, true, errors.New("Herdr socket API response has no result")
	}
	return response.Result, true, nil
}

func socketServerNotRunning(path string, dialErr error) bool {
	missing := errors.Is(dialErr, syscall.ENOENT)
	refused := errors.Is(dialErr, syscall.ECONNREFUSED)
	if !missing && !refused {
		return false
	}
	info, err := os.Lstat(path)
	if missing {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func isPreDispatchRequestError(id, code, message string) bool {
	return id == "" && code == "invalid_request" && strings.HasPrefix(message, "invalid request:")
}

func decodeSocketResult(raw json.RawMessage, wantType string, target any) error {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode Herdr socket API result: %w", err)
	}
	if envelope.Type != wantType {
		return fmt.Errorf("Herdr socket API returned %q, want %q", envelope.Type, wantType)
	}
	if target == nil {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode Herdr %s result: %w", wantType, err)
	}
	return nil
}

func (c *socketAPIClient) requestResult(
	ctx context.Context,
	method string,
	params map[string]any,
	wantType string,
	target any,
) error {
	raw, wrote, err := c.requestUnary(ctx, method, params)
	if err != nil {
		var cliErr *CLIError
		if errors.As(err, &cliErr) {
			return err
		}
		if wrote {
			return errors.Join(ErrDispatchedUnknown, err)
		}
		return errors.Join(ErrNotStarted, err)
	}
	if err := decodeSocketResult(raw, wantType, target); err != nil {
		return errors.Join(ErrDispatchedUnknown, err)
	}
	return nil
}

func (c *socketAPIClient) readPane(
	ctx context.Context,
	paneID string,
	lines int,
	format string,
	source string,
) (PaneRead, error) {
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
	var result struct {
		Read struct {
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		} `json:"read"`
	}
	if err := c.requestResult(ctx, "pane.read", params, "pane_read", &result); err != nil {
		return PaneRead{}, err
	}
	return PaneRead{Content: []byte(result.Read.Text), Truncated: result.Read.Truncated}, nil
}

func (c *socketAPIClient) tabMove(ctx context.Context, tabID string, insertIndex int) error {
	return c.requestResult(ctx, "tab.move", map[string]any{
		"tab_id":       tabID,
		"insert_index": insertIndex,
	}, "tab_list", nil)
}

func (c *socketAPIClient) workspaceMove(ctx context.Context, workspaceID string, insertIndex int) error {
	return c.requestResult(ctx, "workspace.move", map[string]any{
		"workspace_id": workspaceID,
		"insert_index": insertIndex,
	}, "workspace_list", nil)
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
	return c.requestResult(ctx, "workspace.move_block", params, "workspace_list", nil)
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

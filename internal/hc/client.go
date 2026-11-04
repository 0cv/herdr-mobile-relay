package hc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type Client struct {
	socketPath string
	mu         sync.Mutex
	revisions  map[string]int
}

func New(socketPath string) *Client {
	return &Client{socketPath: socketPath, revisions: make(map[string]int)}
}

func (c *Client) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, errors.Join(herdr.ErrNotStarted, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, errors.Join(herdr.ErrNotStarted, err)
	}
	reader := bufio.NewReader(conn)
	if err := hello(ctx, conn, reader, false); err != nil {
		return nil, errors.Join(herdr.ErrNotStarted, err)
	}
	raw, started, err := request(ctx, conn, reader, "request", method, params, true)
	if err == nil {
		return raw, nil
	}
	if !started {
		return nil, errors.Join(herdr.ErrNotStarted, err)
	}
	var cliErr *herdr.CLIError
	if errors.As(err, &cliErr) {
		return nil, err
	}
	return nil, errors.Join(herdr.ErrDispatchedUnknown, err)
}

func hello(ctx context.Context, conn net.Conn, reader *bufio.Reader, framed bool) error {
	params := map[string]any{"generation": 1}
	if framed {
		params["framed"] = true
	}
	raw, _, err := request(ctx, conn, reader, "hello", "hello", params, false)
	if err != nil {
		return err
	}
	var result struct {
		Generation int  `json:"generation"`
		Framed     bool `json:"framed"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Generation != 1 || (framed && !result.Framed) {
		return &herdr.CLIError{Code: "protocol_mismatch"}
	}
	return nil
}

func request(ctx context.Context, conn net.Conn, reader *bufio.Reader, id, method string, params map[string]any, dispatch bool) (json.RawMessage, bool, error) {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, false, err
	}
	if dispatch {
		if err := herdr.CheckDispatch(ctx); err != nil {
			return nil, false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	payload = append(payload, '\n')
	n, err := conn.Write(payload)
	if err == nil && n != len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return nil, n > 0, err
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, true, err
	}
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Kind string `json:"kind"`
				Hint string `json:"hint"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, true, err
	}
	if response.ID != id {
		return nil, true, errors.New("core response ID mismatch")
	}
	if response.JSONRPC != "2.0" {
		return nil, true, errors.New("core response is not JSON-RPC 2.0")
	}
	if response.Error != nil {
		code := response.Error.Data.Kind
		if code == "" && response.Error.Code == -32602 {
			code = "invalid_params"
		}
		if code == "not_found" {
			code = "pane_not_found"
		}
		return nil, true, &herdr.CLIError{Code: code, Message: response.Error.Message}
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return nil, true, fmt.Errorf("core response has no result")
	}
	return response.Result, true, nil
}

package pibridge

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

type bridgeConnection struct {
	net.Conn
	path   string
	before os.FileInfo
	stop   func() bool
}

func openBridgeTransport(ctx context.Context, directory, endpointName string) (*bridgeConnection, error) {
	if _, err := owned(directory, false); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, endpointName)
	before, err := owned(path, true)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	bridge := &bridgeConnection{Conn: conn, path: path, before: before,
		stop: context.AfterFunc(ctx, func() { _ = conn.Close() })}
	return bridge, nil
}

func openBridgeConnection(ctx context.Context, directory string, pid int) (*bridgeConnection, error) {
	bridge, err := openBridgeTransport(ctx, directory, fmt.Sprintf("%d.sock", pid))
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := bridge.SetDeadline(deadline); err != nil {
		_ = bridge.Close()
		return nil, err
	}
	if err := checkPeer(bridge.Conn.(*net.UnixConn), pid); err != nil {
		_ = bridge.Close()
		return nil, err
	}
	return bridge, nil
}

func (conn *bridgeConnection) Close() error {
	conn.stop()
	return conn.Conn.Close()
}

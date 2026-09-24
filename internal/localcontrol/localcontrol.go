// Package localcontrol exposes the deliberately tiny local pairing control
// protocol used by a managed foreground relay. It is a Unix-socket protocol,
// never an HTTP or WebSocket endpoint.
package localcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	ProtocolVersion = 1
	MaxRequestBytes = 4096
	IOTimeout       = 2 * time.Second
)

type Status struct {
	Ready               bool   `json:"ready"`
	RunID               string `json:"run_id"`
	Instance            string `json:"instance"`
	Transport           string `json:"transport,omitempty"`
	Version             string `json:"version,omitempty"`
	Revision            string `json:"revision,omitempty"`
	BundleHash          string `json:"bundle_hash,omitempty"`
	InvitationArmed     bool   `json:"invitation_armed"`
	InvitationPending   bool   `json:"invitation_pending"`
	InvitationExpiresAt string `json:"invitation_expires_at,omitempty"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Status
}

type request struct {
	Protocol int    `json:"protocol"`
	Op       string `json:"op"`
	RunID    string `json:"run_id"`
	Instance string `json:"instance"`
}

type Server struct {
	path       string
	runID      string
	instance   string
	listener   net.Listener
	socketInfo os.FileInfo
	status     func() Status
	arm        func() (Status, error)
}

func New(path, runID, instance string, status func() Status, arm func() (Status, error)) (*Server, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("pairing control socket path must be absolute")
	}
	if !validIdentity(runID) || !validIdentity(instance) {
		return nil, errors.New("pairing control identity is invalid")
	}
	if status == nil || arm == nil {
		return nil, errors.New("pairing control callbacks are required")
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateDirectory(parent); err != nil {
		return nil, fmt.Errorf("prepare pairing control directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("pairing control socket is a symlink")
		}
		return nil, errors.New("pairing control socket already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect pairing control socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on pairing control socket: %w", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		// A non-Unix listener cannot be trusted to own the leaf; close it and
		// only unlink the object when it is still the one we just created.
		if info, statErr := os.Lstat(path); statErr == nil {
			_ = (&Server{path: path}).removeOwnedSocket(info)
		}
		_ = listener.Close()
		return nil, errors.New("pairing control listener is not Unix")
	}
	// net.Listen enables unlink-on-close by default. Disable it immediately so
	// that closing the listener never removes a pathname this server does not
	// own; all removal then goes through removeOwnedSocket.
	unixListener.SetUnlinkOnClose(false)
	// Capture the identity of the freshly created socket before changing its
	// mode. chmod does not alter the inode, so this FileInfo stays authoritative.
	socketInfo, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("record pairing control socket: %w", err)
	}
	server := &Server{
		path: path, runID: runID, instance: instance, listener: listener,
		status: status, arm: arm,
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		removeErr := server.removeOwnedSocket(socketInfo)
		return nil, errors.Join(fmt.Errorf("protect pairing control socket: %w", err), removeErr)
	}
	if err := verifyOwnedRegularSocket(path); err != nil {
		_ = listener.Close()
		removeErr := server.removeOwnedSocket(socketInfo)
		return nil, errors.Join(err, removeErr)
	}
	if _, err := os.Lstat(path); err != nil {
		_ = listener.Close()
		removeErr := server.removeOwnedSocket(socketInfo)
		return nil, errors.Join(fmt.Errorf("record pairing control socket: %w", err), removeErr)
	}
	server.socketInfo = socketInfo
	return server, nil
}

func (s *Server) Run(ctx context.Context) error {
	if s == nil || s.listener == nil {
		return errors.New("pairing control server is not initialized")
	}
	listener, ok := s.listener.(*net.UnixListener)
	if !ok {
		return errors.New("pairing control listener is not Unix")
	}
	for {
		if err := listener.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			return err
		}
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var netError net.Error
			if errors.As(err, &netError) && netError.Timeout() {
				continue
			}
			return err
		}
		s.handle(ctx, connection)
	}
}

func (s *Server) Close() error {
	if s == nil || s.listener == nil {
		return nil
	}
	// The listener was created with unlink-on-close disabled, so closing it
	// only releases the descriptor. Removal is separate and identity-checked.
	err := s.listener.Close()
	s.listener = nil
	removeErr := s.removeOwnedSocket(s.socketInfo)
	return errors.Join(err, removeErr)
}

// removeOwnedSocket removes the socket at s.path only while the object there is
// still the socket this server created. It returns nil when the path is already
// gone and a descriptive error when the path now holds a foreign object, which
// is deliberately left in place.
//
// This check is inherently time-of-check/time-of-use: a same-UID,
// noncooperating writer can still replace the leaf between the final Lstat and
// the unlink, and portable filesystem primitives cannot make conditional
// deletion atomic. That residual risk is the open DR-2 product decision; this
// helper does not claim to eliminate it.
func (s *Server) removeOwnedSocket(info os.FileInfo) error {
	if info == nil {
		return errors.New("pairing control socket identity is unknown")
	}
	current, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect pairing control socket: %w", err)
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(info, current) {
		return errors.New("pairing control socket was replaced; refusing to remove it")
	}
	if err := os.Remove(s.path); err != nil {
		return fmt.Errorf("remove pairing control socket: %w", err)
	}
	return nil
}

func (s *Server) Path() string { return s.path }

func Request(ctx context.Context, path, op, runID, instance string) (Response, error) {
	if !filepath.IsAbs(path) || !validIdentity(runID) || !validIdentity(instance) {
		return Response{}, errors.New("invalid pairing control request identity")
	}
	if op != "status" && op != "arm_bootstrap" {
		return Response{}, errors.New("unsupported pairing control operation")
	}
	dialer := net.Dialer{Timeout: IOTimeout}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("connect to pairing control socket: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(IOTimeout))
	payload, err := json.Marshal(request{Protocol: ProtocolVersion, Op: op, RunID: runID, Instance: instance})
	if err != nil {
		return Response{}, err
	}
	if _, err := connection.Write(append(payload, '\n')); err != nil {
		return Response{}, fmt.Errorf("write pairing control request: %w", err)
	}
	data, err := bufio.NewReader(io.LimitReader(connection, MaxRequestBytes+1)).ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("read pairing control response: %w", err)
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) > MaxRequestBytes {
		return Response{}, errors.New("pairing control response is too large")
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		return Response{}, fmt.Errorf("decode pairing control response: %w", err)
	}
	if !response.OK {
		return response, errors.New(response.Error)
	}
	return response, nil
}

func (s *Server) handle(ctx context.Context, connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(IOTimeout))
	data, err := bufio.NewReader(io.LimitReader(connection, MaxRequestBytes+1)).ReadBytes('\n')
	data = []byte(strings.TrimSpace(string(data)))
	if err != nil || len(data) > MaxRequestBytes {
		s.writeResponse(connection, Response{Error: "invalid or oversized request"})
		return
	}
	var incoming request
	if err := json.Unmarshal(data, &incoming); err != nil {
		s.writeResponse(connection, Response{Error: "invalid request"})
		return
	}
	if incoming.Protocol != ProtocolVersion || incoming.RunID != s.runID || incoming.Instance != s.instance {
		s.writeResponse(connection, Response{Error: "pairing control identity mismatch"})
		return
	}
	if ctx.Err() != nil {
		s.writeResponse(connection, Response{Error: "relay is shutting down"})
		return
	}
	switch incoming.Op {
	case "status":
		status := s.status()
		status.RunID = s.runID
		status.Instance = s.instance
		s.writeResponse(connection, Response{OK: true, Status: status})
	case "arm_bootstrap":
		status, err := s.arm()
		if err != nil {
			s.writeResponse(connection, Response{Error: "bootstrap invitation could not be persisted"})
			return
		}
		status.RunID = s.runID
		status.Instance = s.instance
		if !status.InvitationArmed || status.InvitationExpiresAt == "" {
			s.writeResponse(connection, Response{Error: "bootstrap invitation persistence was not acknowledged"})
			return
		}
		s.writeResponse(connection, Response{OK: true, Status: status})
	default:
		s.writeResponse(connection, Response{Error: "unsupported pairing control operation"})
	}
}

func (s *Server) writeResponse(connection net.Conn, response Response) {
	data, err := json.Marshal(response)
	if err == nil {
		_, _ = connection.Write(append(data, '\n'))
	}
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("pairing control parent is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return verifyOwned(path)
}

func verifyOwnedRegularSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&0o077 != 0 {
		return errors.New("pairing control socket is not private")
	}
	return verifyOwned(path)
}

func verifyOwned(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uint64(os.Getuid()) {
		return errors.New("pairing control path is not owned by the current user")
	}
	return nil
}

func validIdentity(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e || character == '/' || character == '\\' {
			return false
		}
	}
	return true
}
